package cirunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GitHub is the slice of the GitHub REST API a JIT runner needs: mint a single-use
// runner, and remove one that never got to run.
type GitHub struct {
	APIURL string
	Token  string
	HTTP   *http.Client
}

// JITRunner is what generate-jitconfig returns. EncodedConfig is a bearer
// credential: it registers exactly one runner, and must not be logged.
type JITRunner struct {
	ID            int64
	Name          string
	EncodedConfig string
}

// ErrNotFound reports a 404. A JIT runner that finished its job deregisters itself,
// so removing it afterwards finds nothing, and that is success rather than failure.
var ErrNotFound = errors.New("not found")

// GenerateJIT mints a runner bound to one job. Repository level, runner group 1:
// organisation runners and custom groups are out of scope (docs/ci-runner.md).
func (g *GitHub) GenerateJIT(ctx context.Context, repo, name string, labels []string) (*JITRunner, error) {
	body := map[string]any{
		"name":            name,
		"runner_group_id": 1,
		"labels":          labels,
		"work_folder":     "_work",
	}
	var out struct {
		Runner struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := g.do(ctx, http.MethodPost, "/repos/"+repo+"/actions/runners/generate-jitconfig", body, &out); err != nil {
		return nil, fmt.Errorf("generate jit config: %w", err)
	}
	if out.EncodedJITConfig == "" || out.Runner.ID == 0 {
		// Checked rather than trusted: a runner with no config cannot start, and one
		// with no id cannot be removed if the sandbox fails, so it would leak.
		return nil, errors.New("generate jit config: response has no config or runner id")
	}
	return &JITRunner{ID: out.Runner.ID, Name: out.Runner.Name, EncodedConfig: out.EncodedJITConfig}, nil
}

// RemoveRunner deregisters a runner. ErrNotFound means it is already gone.
func (g *GitHub) RemoveRunner(ctx context.Context, repo string, id int64) error {
	return g.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/actions/runners/%d", repo, id), nil, nil)
}

// Runner is one entry of the repository's runner list.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// ListRunners returns the repository's self-hosted runners, every page.
func (g *GitHub) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	var all []Runner
	for page := 1; ; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		path := fmt.Sprintf("/repos/%s/actions/runners?per_page=100&page=%d", repo, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, fmt.Errorf("list runners: %w", err)
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < 100 {
			return all, nil
		}
	}
}

func (g *GitHub) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.APIURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "bean-ci-runner")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		// Only GitHub's message field: the body of a failed generate-jitconfig is not a
		// credential, but echoing whole bodies is how one eventually ends up in a log.
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("github %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
