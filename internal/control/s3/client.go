// Package s3 is a minimal S3 client covering what the platform needs:
// object put, get, range-get, head, delete and multipart upload.
//
// It speaks the protocol directly rather than pulling in a cloud SDK. The
// reasons are practical: the SDK is tens of megabytes of dependency for a
// handful of verbs, and compatibility with MinIO, Ceph RGW and the like
// then rests on SDK behaviour rather than on the wire format. Range reads
// matter especially — that is how overlaybd fetches blocks on demand — so
// the request shape is something the platform should own.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrNotFound reports a missing object or bucket.
var ErrNotFound = errors.New("s3: not found")

// Config describes an endpoint and its credentials.
type Config struct {
	// Endpoint is the service URL, e.g. "https://s3.amazonaws.com" or
	// "http://127.0.0.1:9000" for MinIO.
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	// PathStyle addresses buckets as /bucket/key instead of a subdomain.
	// MinIO and most self-hosted gateways need this.
	PathStyle bool
	// HTTPClient allows callers to supply timeouts and connection pooling
	// suited to their workload.
	HTTPClient *http.Client
}

// Client talks to one S3-compatible endpoint.
type Client struct {
	cfg      Config
	endpoint *url.URL
	http     *http.Client
}

func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3: endpoint required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: credentials required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: parse endpoint: %w", err)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}
	return &Client{cfg: cfg, endpoint: u, http: hc}, nil
}

// objectURL builds the request URL for a key.
func (c *Client) objectURL(bucket, key string) string {
	// Each path segment is escaped individually so slashes in the key stay
	// slashes in the URL, which is what S3 expects for nested keys.
	escaped := make([]string, 0, 8)
	for _, seg := range strings.Split(key, "/") {
		escaped = append(escaped, url.PathEscape(seg))
	}
	keyPath := strings.Join(escaped, "/")
	if c.cfg.PathStyle {
		return fmt.Sprintf("%s://%s/%s/%s", c.endpoint.Scheme, c.endpoint.Host, bucket, keyPath)
	}
	return fmt.Sprintf("%s://%s.%s/%s", c.endpoint.Scheme, bucket, c.endpoint.Host, keyPath)
}

// PutObject uploads body as a single object. Callers with large or
// unknown-length payloads should use the multipart writer instead.
func (c *Client) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.objectURL(bucket, key), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	resp, err := c.send(req, body)
	if err != nil {
		return err
	}
	defer drain(resp)
	return checkStatus(resp, http.StatusOK)
}

// GetObject fetches a whole object.
func (c *Client) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	return c.getRange(ctx, bucket, key, "")
}

// GetRange fetches bytes [offset, offset+length). This is the read pattern
// lazy block loading depends on, so it is a first-class operation rather
// than something callers assemble from headers.
func (c *Client) GetRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return nil, errors.New("s3: range length must be positive")
	}
	return c.getRange(ctx, bucket, key,
		fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
}

func (c *Client) getRange(ctx context.Context, bucket, key, rangeHeader string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.objectURL(bucket, key), nil)
	if err != nil {
		return nil, err
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.send(req, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, bucket, key)
	}
	// A range request answers 206; a whole-object request answers 200.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer drain(resp)
		return nil, statusError(resp)
	}
	return resp.Body, nil
}

// HeadObject returns an object's size, or ErrNotFound.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.objectURL(bucket, key), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.send(req, nil)
	if err != nil {
		return 0, err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("%w: %s/%s", ErrNotFound, bucket, key)
	}
	if err := checkStatus(resp, http.StatusOK); err != nil {
		return 0, err
	}
	return resp.ContentLength, nil
}

// DeleteObject removes an object. Deleting a missing object is not an
// error, so cleanup paths stay idempotent.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.objectURL(bucket, key), nil)
	if err != nil {
		return err
	}
	resp, err := c.send(req, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK ||
		resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return statusError(resp)
}

// EnsureBucket creates a bucket if it does not exist, so a fresh
// deployment does not require an out-of-band setup step.
func (c *Client) EnsureBucket(ctx context.Context, bucket string) error {
	// A HEAD on the bucket distinguishes "exists" from "must create".
	headURL := c.bucketURL(bucket)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, headURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.send(req, nil)
	if err != nil {
		return err
	}
	drain(resp)
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodPut, headURL, nil)
	if err != nil {
		return err
	}
	resp, err = c.send(req, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	// A concurrent creator is not an error.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return statusError(resp)
}

func (c *Client) bucketURL(bucket string) string {
	if c.cfg.PathStyle {
		return fmt.Sprintf("%s://%s/%s", c.endpoint.Scheme, c.endpoint.Host, bucket)
	}
	return fmt.Sprintf("%s://%s.%s/", c.endpoint.Scheme, bucket, c.endpoint.Host)
}

// ListObjects returns keys under a prefix. It follows continuation tokens,
// so callers do not have to page.
func (c *Client) ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		reqURL := c.bucketURL(bucket)
		if !strings.HasSuffix(reqURL, "/") {
			reqURL += "/"
		}
		reqURL += "?" + q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.send(req, nil)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		drain(resp)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("s3: list %s: HTTP %d: %s", bucket, resp.StatusCode, body)
		}

		var result listBucketResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("s3: parse list response: %w", err)
		}
		for _, obj := range result.Contents {
			out = append(out, ObjectInfo{Key: obj.Key, Size: obj.Size, ETag: obj.ETag})
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			break
		}
		token = result.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ObjectInfo describes a listed object.
type ObjectInfo struct {
	Key  string
	Size int64
	ETag string
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
		ETag string `xml:"ETag"`
	} `xml:"Contents"`
}

// send signs and performs a request. payload may be nil for bodyless
// requests; for streaming bodies the caller uses the multipart writer,
// which signs each part separately.
func (c *Client) send(req *http.Request, payload []byte) (*http.Response, error) {
	if err := c.sign(req, payload); err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

func checkStatus(resp *http.Response, want int) error {
	if resp.StatusCode != want {
		return statusError(resp)
	}
	return nil
}

func statusError(resp *http.Response) error {
	// The body carries S3's error code and message, which is far more
	// useful than the status alone when diagnosing a policy or signing
	// problem.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("s3: %s %s: HTTP %d: %s",
		resp.Request.Method, resp.Request.URL.Path, resp.StatusCode,
		strings.TrimSpace(string(body)))
}

// drain consumes and closes a response body so the connection can be
// reused rather than dropped.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// hmacSHA256 is the primitive SigV4 is built from.
func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
