// Package cirunner runs GitHub Actions jobs on bean sandboxes: one job, one sandbox.
//
// The design and its verification plan are in docs/ci-runner.md.
package cirunner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Labels bean-ci-runner puts on every sandbox it creates. Reap finds its own sandboxes
// by these and touches nothing else.
const (
	LabelPurpose = "purpose"
	PurposeValue = "ci-runner"
	LabelRunner  = "runner"
	LabelRepo    = "repo"
)

// RunnerNamePrefix marks runners this controller registered, so Reap never removes a
// runner someone else added to the repository.
const RunnerNamePrefix = "bean-ci-"

// MaxJobTimeout keeps a job inside bean's exec ceiling of 3600 s, with room for the
// runner to start. A longer job would be cut off by bean-api, not by its own timeout.
const MaxJobTimeout = 3540 * time.Second

// Config is everything one controller needs. It is resolved by cmd/bean-ci-runner.
type Config struct {
	Repo        string        // owner/name
	Labels      []string      // runner labels; a workflow selects with runs-on
	Image       string        // OCI reference of the runner image
	JobTimeout  time.Duration // how long one job may run
	IdleTimeout time.Duration // server-side backstop for a sandbox whose controller died
	CPU         float64
	MemoryMiB   int64
}

// Validate refuses a configuration that would fail later, in a worse place.
func (c Config) Validate() error {
	if parts := strings.Split(c.Repo, "/"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("repo must be owner/name, got %q", c.Repo)
	}
	if len(c.Labels) == 0 {
		return errors.New("at least one runner label is required")
	}
	if c.Image == "" {
		return errors.New("runner image is required")
	}
	if c.JobTimeout <= 0 || c.JobTimeout > MaxJobTimeout {
		return fmt.Errorf("job timeout must be in (0, %s], got %s", MaxJobTimeout, c.JobTimeout)
	}
	if c.IdleTimeout != 0 && c.IdleTimeout <= c.JobTimeout {
		// A shorter idle timeout could reclaim a sandbox whose job is still running, if
		// bean counts only API calls as activity. Requiring it to exceed the job timeout
		// makes that impossible whichever way bean counts.
		return fmt.Errorf("idle timeout (%s) must exceed the job timeout (%s)", c.IdleTimeout, c.JobTimeout)
	}
	return nil
}

// Controller provisions runners.
type Controller struct {
	Config Config
	GitHub *GitHub
	Bean   *Bean
	Log    *slog.Logger
	// Now and NewName are replaceable for tests.
	Now     func() time.Time
	NewName func() string
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) newName() string {
	if c.NewName != nil {
		return c.NewName()
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return RunnerNamePrefix + hex.EncodeToString(b)
}

// RunOnce provisions one runner, waits for it to take and finish one job, and cleans
// up. It returns the runner's exit code.
//
// Cleanup runs on every path, with its own context: a cancelled parent context is the
// common reason to be here, and cleanup that inherited it would fail immediately and
// leave the sandbox and the registration behind.
func (c *Controller) RunOnce(ctx context.Context) (code int, err error) {
	name := c.newName()
	log := c.Log.With("runner", name)

	jit, err := c.GitHub.GenerateJIT(ctx, c.Config.Repo, name, c.Config.Labels)
	if err != nil {
		return -1, err
	}
	// From here the runner is registered, so every exit path must remove it unless it
	// removed itself by completing a job.
	var sandboxID string
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if sandboxID != "" {
			if derr := c.Bean.Delete(cctx, sandboxID); derr != nil && !errors.Is(derr, ErrNotFound) {
				log.Error("delete sandbox failed; its idle timeout will reclaim it", "sandbox", sandboxID, "err", derr)
			}
		}
		if rerr := c.GitHub.RemoveRunner(cctx, c.Config.Repo, jit.ID); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			log.Error("remove runner registration failed; reap will retry", "runner_id", jit.ID, "err", rerr)
		}
	}()

	sb, err := c.Bean.Create(ctx, CreateSpec{
		ImageRef: c.Config.Image,
		Labels: map[string]string{
			LabelPurpose: PurposeValue,
			LabelRunner:  name,
			LabelRepo:    c.Config.Repo,
		},
		IdleTimeout: c.Config.IdleTimeout,
		CPU:         c.Config.CPU,
		MemoryMiB:   c.Config.MemoryMiB,
	})
	if err != nil {
		return -1, err
	}
	sandboxID = sb.ID
	log = log.With("sandbox", sb.ID)
	log.Info("runner sandbox created", "image", c.Config.Image)

	start := c.now()
	res, err := c.Bean.Exec(ctx, sb.ID, []string{"/home/runner/run.sh"}, map[string]string{
		// The runner reads any argument from ACTIONS_RUNNER_INPUT_<NAME>, so the JIT
		// config reaches it without appearing on a command line.
		"ACTIONS_RUNNER_INPUT_JITCONFIG": jit.EncodedConfig,
		// bean runs exec as root; the runner refuses root unless told this is intended.
		"RUNNER_ALLOW_RUNASROOT": "1",
	}, c.Config.JobTimeout)
	if err != nil {
		return -1, err
	}
	log.Info("runner exited", "exit_code", res.ExitCode, "duration", c.now().Sub(start).Round(time.Second))
	if res.ExitCode != 0 {
		// The tail only: enough to see why, short enough not to flood the controller log.
		log.Warn("runner output", "stdout_tail", tail(res.Stdout, 2000), "stderr_tail", tail(res.Stderr, 2000))
	}
	return res.ExitCode, nil
}

// Serve keeps n runners waiting until ctx is cancelled. A failed provision backs off
// so a broken token or an unreachable bean-api does not spin.
func (c *Controller) Serve(ctx context.Context, n int) {
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(slot int) {
			defer func() { done <- struct{}{} }()
			backoff := time.Second
			for ctx.Err() == nil {
				_, err := c.RunOnce(ctx)
				if err == nil {
					backoff = time.Second
					continue
				}
				if ctx.Err() != nil {
					return
				}
				c.Log.Error("provision failed", "slot", slot, "err", err, "retry_in", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 2*time.Minute)
			}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}

// ReapResult reports what Reap removed or would remove.
type ReapResult struct {
	Sandboxes []string
	Runners   []string
}

// Reap removes what a crashed controller left behind: sandboxes it labelled that are
// older than maxAge, and offline runners it registered. A busy or online runner is
// never removed, whatever its age. With dryRun nothing is deleted.
func (c *Controller) Reap(ctx context.Context, maxAge time.Duration, dryRun bool) (ReapResult, error) {
	var out ReapResult
	sandboxes, err := c.Bean.List(ctx, LabelPurpose, PurposeValue)
	if err != nil {
		return out, err
	}
	cutoff := c.now().Add(-maxAge)
	// Runners whose sandbox is still alive. A freshly minted JIT runner is "offline"
	// until its sandbox has pulled the image and connected, which takes tens of seconds;
	// removing it then would break a provision that is working. So a runner is only
	// abandoned when its sandbox is gone too.
	alive := map[string]bool{}
	for _, sb := range sandboxes {
		// A zero timestamp is treated as old: a record that cannot say when it was made
		// is more likely abandoned than fresh, and keeping it forever is the worse error.
		if !sb.CreatedAt.IsZero() && sb.CreatedAt.After(cutoff) {
			alive[sb.Labels[LabelRunner]] = true
			continue
		}
		out.Sandboxes = append(out.Sandboxes, sb.ID)
		if !dryRun {
			if err := c.Bean.Delete(ctx, sb.ID); err != nil && !errors.Is(err, ErrNotFound) {
				return out, err
			}
		}
	}
	runners, err := c.GitHub.ListRunners(ctx, c.Config.Repo)
	if err != nil {
		return out, err
	}
	for _, r := range runners {
		if !strings.HasPrefix(r.Name, RunnerNamePrefix) || r.Status != "offline" || r.Busy || alive[r.Name] {
			continue
		}
		out.Runners = append(out.Runners, r.Name+"#"+strconv.FormatInt(r.ID, 10))
		if !dryRun {
			if err := c.GitHub.RemoveRunner(ctx, c.Config.Repo, r.ID); err != nil && !errors.Is(err, ErrNotFound) {
				return out, err
			}
		}
	}
	return out, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
