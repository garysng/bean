package beand

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The agent learns which callers to trust from Firecracker's metadata service rather
// than from its own command line. Two reasons, and the second is the one that shapes
// this file.
//
// The kernel command line is readable inside the sandbox, so a hash placed there would
// be visible -- harmless in itself, since a hash cannot be used as a credential. What
// it cannot do is change. A sandbox restored from a snapshot was booted long ago under
// a hash that no longer applies, so a value fixed at boot is the wrong value for every
// restore. MMDS is writable while the guest runs, which is what lets each restore
// publish a fresh one.
//
// That is why the hash is read per handshake instead of cached at startup: the value
// is expected to change underneath a running agent.

const (
	// mmdsAddr is Firecracker's metadata service. Inside the sandbox's namespace this
	// is the VMM answering, not a cloud provider: the guest has no route off its /30
	// except through the tap, and the host filter drops the link-local range.
	mmdsAddr = "169.254.169.254"

	// mmdsSessionTTL bounds a V2 session token. Short because a token is taken per
	// read and never reused across them.
	mmdsSessionTTL = 60 * time.Second

	mmdsTimeout = 2 * time.Second
)

// mmdsDoc is the document noded publishes. Only the field this agent needs is
// declared; MMDS is a shared channel and unknown keys must not make a read fail.
type mmdsDoc struct {
	AgentTokenHash string `json:"agentTokenHash"`
}

// mmdsClient reads the metadata document.
type mmdsClient struct {
	http *http.Client
	addr string

	// The document is re-read per handshake, so a burst of connections would
	// otherwise produce a burst of identical round trips. This caches for a short
	// window rather than for the process's life: caching forever would defeat the
	// point of reading late, and not caching at all makes the metadata service part
	// of the per-request path.
	mu       sync.Mutex
	cached   string
	cachedAt time.Time
	cacheFor time.Duration
}

func newMMDSClient() *mmdsClient {
	return &mmdsClient{
		addr: mmdsAddr,
		http: &http.Client{
			Timeout: mmdsTimeout,
			Transport: &http.Transport{
				// No connection reuse. The metadata service is contacted rarely, and
				// a pooled connection to a link-local address survives across a
				// snapshot restore as a descriptor to a machine that no longer
				// exists.
				DisableKeepAlives: true,
				DialContext:       (&net.Dialer{Timeout: mmdsTimeout}).DialContext,
			},
		},
		cacheFor: 5 * time.Second,
	}
}

// AgentTokenHash returns the hash the agent should require of its callers.
//
// An error is returned rather than an empty string when the service cannot be reached,
// so a caller cannot mistake "could not ask" for "no credential is required". The
// distinction is the whole security property: the fallback for an unreachable metadata
// service must be to refuse requests, not to serve them unauthenticated.
func (c *mmdsClient) AgentTokenHash(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.cached != "" && time.Since(c.cachedAt) < c.cacheFor {
		h := c.cached
		c.mu.Unlock()
		return h, nil
	}
	c.mu.Unlock()

	doc, err := c.read(ctx)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.cached = doc.AgentTokenHash
	c.cachedAt = time.Now()
	c.mu.Unlock()
	return doc.AgentTokenHash, nil
}

// read performs the V2 exchange: acquire a session token, then read with it.
//
// V2 rather than V1 deliberately. V1 answers a bare GET, which means any process in
// the guest that can be induced to fetch a URL -- a library following a redirect, a
// build script given a hostile input -- reads the metadata as a side effect. V2
// requires a PUT first, which a redirect cannot produce.
func (c *mmdsClient) read(ctx context.Context) (*mmdsDoc, error) {
	session, err := c.sessionToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.addr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-metadata-token", session)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("beand: read metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("beand: read metadata: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("beand: read metadata body: %w", err)
	}
	var doc mmdsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("beand: parse metadata: %w", err)
	}
	return &doc, nil
}

func (c *mmdsClient) sessionToken(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://"+c.addr+"/latest/api/token", new(bytes.Buffer))
	if err != nil {
		return "", err
	}
	req.Header.Set("X-metadata-token-ttl-seconds",
		strconv.Itoa(int(mmdsSessionTTL.Seconds())))

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("beand: metadata session token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("beand: metadata session token: unexpected status %d",
			resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", fmt.Errorf("beand: metadata session token body: %w", err)
	}
	if len(body) == 0 {
		return "", fmt.Errorf("beand: metadata session token is empty")
	}
	return string(body), nil
}
