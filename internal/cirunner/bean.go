package cirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Bean is the slice of bean-api the controller uses: create a sandbox from an image,
// run one command in it, delete it, and list the ones it labelled.
type Bean struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Sandbox is the part of a sandbox record the controller reads.
type Sandbox struct {
	ID        string            `json:"id"`
	State     string            `json:"state"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"createdAt"`
}

// CreateSpec is what the controller asks for. IdleTimeout is the server-side backstop:
// a controller killed mid-job never sends the delete, and this reclaims the sandbox.
type CreateSpec struct {
	ImageRef    string
	Labels      map[string]string
	IdleTimeout time.Duration
	CPU         float64
	MemoryMiB   int64
}

// Create starts a sandbox from an OCI image.
func (b *Bean) Create(ctx context.Context, spec CreateSpec) (*Sandbox, error) {
	body := map[string]any{"imageRef": spec.ImageRef, "labels": spec.Labels}
	if spec.IdleTimeout > 0 {
		body["lifecycle"] = map[string]string{
			"idleTimeout": fmt.Sprintf("%ds", int(spec.IdleTimeout.Seconds())),
			"onIdle":      "delete",
		}
	}
	if spec.CPU > 0 || spec.MemoryMiB > 0 {
		body["resources"] = map[string]any{"cpu": spec.CPU, "memoryMiB": spec.MemoryMiB}
	}
	var out struct {
		Sandbox Sandbox `json:"sandbox"`
	}
	if err := b.do(ctx, http.MethodPost, "/v1/sandboxes", body, &out); err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	if out.Sandbox.ID == "" {
		return nil, fmt.Errorf("create sandbox: response has no id")
	}
	return &out.Sandbox, nil
}

// ExecResult is a finished command. Output is captured by bean-api and may be truncated.
type ExecResult struct {
	ExitCode  int    `json:"exitCode"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

// Exec runs cmd in the sandbox and waits for it. env is passed to the process only; it
// is the channel for the JIT credential, so neither the command line nor any file in
// the sandbox carries it.
func (b *Bean) Exec(ctx context.Context, id string, cmd []string, env map[string]string, timeout time.Duration) (*ExecResult, error) {
	body := map[string]any{"cmd": cmd, "env": env, "timeoutSeconds": int64(timeout.Seconds())}
	var out ExecResult
	if err := b.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", body, &out); err != nil {
		return nil, fmt.Errorf("exec in %s: %w", id, err)
	}
	return &out, nil
}

// Delete removes a sandbox. ErrNotFound means it is already gone, which a cleanup path
// treats as done.
func (b *Bean) Delete(ctx context.Context, id string) error {
	return b.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

// List returns sandboxes carrying label key=value.
func (b *Bean) List(ctx context.Context, key, value string) ([]Sandbox, error) {
	var out struct {
		Sandboxes []Sandbox `json:"sandboxes"`
	}
	path := "/v1/sandboxes?label=" + url.QueryEscape(key+"="+value)
	if err := b.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out.Sandboxes, nil
}

func (b *Bean) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.BaseURL, "/")+path, reader)
	if err != nil {
		return err
	}
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("bean %s %s: %d %s %s", method, path, resp.StatusCode, e.Error.Code, e.Error.Message)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
