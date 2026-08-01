package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Registry authentication is a challenge-response: the first request gets a 401
// carrying a WWW-Authenticate header naming a token service, and the client
// exchanges its credentials there for a short-lived bearer token scoped to one
// repository. Anonymous pulls work the same way — the exchange just happens
// without a credential.
//
// Tokens are cached per scope because a pull makes one manifest request and one
// request per layer, and re-authenticating for each would triple the round trips.

// tokenCache holds bearer tokens by scope.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func (c *tokenCache) get(scope string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tokens[scope]
	// A token about to expire is treated as absent: a pull can take minutes,
	// and a token that dies mid-transfer fails the whole layer.
	if !ok || time.Now().Add(30*time.Second).After(t.expires) {
		return "", false
	}
	return t.token, true
}

func (c *tokenCache) put(scope, token string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = map[string]cachedToken{}
	}
	c.tokens[scope] = cachedToken{token: token, expires: time.Now().Add(ttl)}
}

// do performs a request, handling the token challenge. A 401 is answered once:
// the token is fetched and the request retried, so callers see either a real
// response or a real failure rather than a challenge.
func (r *Registry) do(ctx context.Context, req *http.Request, ref Reference) (*http.Response, error) {
	scope := fmt.Sprintf("repository:%s:pull", ref.Repository)
	if token, ok := r.tokens.get(scope); ok {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image: request %s: %w", req.URL.Host, err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	closeBody(resp)
	if challenge == "" {
		return nil, fmt.Errorf("image: %s refused the request without a challenge", ref.Host)
	}

	token, ttl, err := r.fetchToken(ctx, ref, challenge, scope)
	if err != nil {
		return nil, err
	}
	r.tokens.put(scope, token, ttl)

	// The body was already consumed by the first attempt, so the request is
	// rebuilt rather than reused. Pulls are GETs with no body, which is what
	// makes this safe.
	retry, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	retry.Header = req.Header.Clone()
	retry.Header.Set("Authorization", "Bearer "+token)

	resp, err = r.Client.Do(retry)
	if err != nil {
		return nil, fmt.Errorf("image: retry %s: %w", req.URL.Host, err)
	}
	return resp, nil
}

// fetchToken exchanges credentials at the service named by the challenge.
func (r *Registry) fetchToken(ctx context.Context, ref Reference, challenge, scope string) (string, time.Duration, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", 0, fmt.Errorf("image: challenge from %s has no realm", ref.Host)
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", 0, fmt.Errorf("image: parse token realm: %w", err)
	}
	q := u.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	if r.Auth != nil {
		if user, pass, ok := r.Auth.Credential(ref.Host); ok {
			req.SetBasicAuth(user, pass)
		}
	}

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("image: fetch token: %w", err)
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return "", 0, statusError(resp, "fetch token for "+ref.Repository)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, err
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", 0, fmt.Errorf("image: parse token response: %w", err)
	}

	// Registries differ on which field carries the token.
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", 0, fmt.Errorf("image: token response from %s had no token", ref.Host)
	}

	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		// The spec's default when expires_in is absent.
		ttl = 60 * time.Second
	}
	return token, ttl, nil
}

// parseChallenge reads the key="value" pairs from a WWW-Authenticate header.
func parseChallenge(header string) map[string]string {
	out := map[string]string{}
	rest := strings.TrimSpace(header)
	// Drop the scheme ("Bearer"), keeping the parameters.
	if space := strings.Index(rest, " "); space >= 0 {
		rest = rest[space+1:]
	}
	for _, part := range splitUnquoted(rest) {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.ToLower(key)] = strings.Trim(value, `"`)
	}
	return out
}

// splitUnquoted splits on commas outside quotes: a scope value legitimately
// contains commas, so a plain Split would break it apart.
func splitUnquoted(s string) []string {
	var parts []string
	var current strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}
