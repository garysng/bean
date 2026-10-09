// bean-ci-runner runs a repository's GitHub Actions jobs on bean sandboxes.
//
//	bean-ci-runner serve --concurrency 4   keep 4 runners waiting
//	bean-ci-runner once                     provision one runner, run one job, exit
//	bean-ci-runner reap [--dry-run]         remove what a crashed controller left
//
// Credentials come from the environment: GITHUB_TOKEN (repository admin, to mint JIT
// runners) and BEAN_API_KEY. They are never taken as flags, because a flag is visible in
// the process list. See docs/ci-runner.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/garysng/bean/internal/cirunner"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub, args := os.Args[1], os.Args[2:]

	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	repo := fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name of the repository")
	labels := fs.String("labels", "self-hosted,bean", "comma-separated runner labels")
	image := fs.String("image", "ghcr.io/actions/actions-runner:latest", "runner image")
	jobTimeout := fs.Duration("job-timeout", 55*time.Minute, "longest a job may run (bean caps exec at 1h)")
	idleTimeout := fs.Duration("idle-timeout", 2*time.Hour, "server-side reclaim for a sandbox whose controller died")
	cpu := fs.Float64("cpu", 0, "vCPUs per runner (0 = bean default)")
	mem := fs.Int64("memory-mib", 0, "memory per runner in MiB (0 = bean default)")
	beanURL := fs.String("bean-url", envOr("BEAN_BASE_URL", "http://127.0.0.1:8080"), "bean-api base URL")
	githubAPI := fs.String("github-api", envOr("GITHUB_API_URL", "https://api.github.com"), "GitHub API URL")
	concurrency := fs.Int("concurrency", 2, "serve: runners kept waiting")
	maxAge := fs.Duration("max-age", 2*time.Hour, "reap: remove runner sandboxes older than this")
	dryRun := fs.Bool("dry-run", false, "reap: report without deleting")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fail(log, "GITHUB_TOKEN is required (repository admin, to mint JIT runners)")
	}
	cfg := cirunner.Config{
		Repo:        *repo,
		Labels:      splitNonEmpty(*labels),
		Image:       *image,
		JobTimeout:  *jobTimeout,
		IdleTimeout: *idleTimeout,
		CPU:         *cpu,
		MemoryMiB:   *mem,
	}
	if err := cfg.Validate(); err != nil {
		fail(log, err.Error())
	}
	ctl := &cirunner.Controller{
		Config: cfg,
		GitHub: &cirunner.GitHub{APIURL: *githubAPI, Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}},
		// The exec holds its request open for the whole job, so the client timeout must
		// exceed the job timeout or every long job would be cut off by the controller.
		Bean: &cirunner.Bean{BaseURL: *beanURL, APIKey: os.Getenv("BEAN_API_KEY"),
			HTTP: &http.Client{Timeout: *jobTimeout + 2*time.Minute}},
		Log: log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "serve":
		log.Info("serving", "repo", cfg.Repo, "labels", cfg.Labels, "concurrency", *concurrency)
		ctl.Serve(ctx, *concurrency)
	case "once":
		code, err := ctl.RunOnce(ctx)
		if err != nil {
			fail(log, err.Error())
		}
		os.Exit(code)
	case "reap":
		res, err := ctl.Reap(ctx, *maxAge, *dryRun)
		verb := "removed"
		if *dryRun {
			verb = "would remove"
		}
		fmt.Printf("%s %d sandboxes %v and %d runners %v\n", verb, len(res.Sandboxes), res.Sandboxes, len(res.Runners), res.Runners)
		if err != nil {
			fail(log, err.Error())
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bean-ci-runner serve|once|reap [flags]   (see -h on any subcommand)")
}

func fail(log *slog.Logger, msg string) {
	log.Error(msg)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
