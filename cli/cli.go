// Package cli implements the bean CLI against the REST API.
package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Client is a minimal REST client for bean-api.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	timeout := 15 * time.Minute
	// BEAN_TIMEOUT accepts a Go duration (e.g. "30s"); mainly for tests and
	// scripted use where a hung endpoint should fail fast.
	if v := os.Getenv("BEAN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey,
		HTTP: &http.Client{Timeout: timeout}}
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A request that never got an answer is worth retrying, unlike one the
		// platform rejected. Wrapping it here is what lets the exit code say so.
		return nil, &transportError{err: err}
	}
	return resp, nil
}

func (c *Client) doJSON(method, path string, body any, out any) error {
	resp, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Code != "" {
			return &apiError{Code: e.Error.Code, Message: e.Error.Message,
				Status: resp.StatusCode}
		}
		return &apiError{Message: strings.TrimSpace(string(data)),
			Status: resp.StatusCode}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Run dispatches a CLI invocation. Returns process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, usage)
		return ExitUsage
	}
	baseURL := envOr("BEAN_BASE_URL", "http://127.0.0.1:8080")
	apiKey := os.Getenv("BEAN_API_KEY")
	c := NewClient(baseURL, apiKey)

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(c, rest, stdout)
	case "ls":
		err = cmdLs(c, rest, stdout)
	case "exec":
		return cmdExec(c, rest, stdout, stderr)
	case "kill":
		err = cmdKill(c, rest, stdout)
	case "pause":
		err = cmdSimplePost(c, rest, "pause")
	case "resume":
		err = cmdSimplePost(c, rest, "resume")
	case "logs":
		err = cmdLogs(c, rest, stdout)
	case "cp":
		err = cmdCp(c, rest, stdout)
	case "events":
		err = cmdEvents(c, rest, stdout)
	case "snapshot":
		err = cmdSnapshot(c, rest, stdout)
	case "commit":
		err = cmdCommit(c, rest, stdout)
	case "build":
		err = cmdBuild(c, rest, stdout)
	case "image":
		err = cmdImage(c, rest, stdout)
	case "version":
		fmt.Fprintln(stdout, "bean CLI (dev)")
	default:
		fmt.Fprintln(stderr, usage)
		return ExitUsage
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCodeFor(err)
	}
	return ExitOK
}

const usage = `usage: bean <command> [args]
commands:
  run --image IMG [--label k=v] [--idle-timeout 300s] [--on-idle pause|kill]
  ls [--label k=v]
  exec SBX -- CMD...
  kill SBX [--force]
  pause SBX | resume SBX
  logs SBX [--tail N]
  cp LOCAL sbx:SBX:/path | sbx:SBX:/path LOCAL
  events SBX | events -f [SBX] [--label k=v]    # -f follows the live stream
  build --tag REF [--file Dockerfile] [CONTEXT] [-f]
                                                # build an image on the platform
                                                # -f follows the build output
  build logs REF                                # watch a build in progress
  build cancel REF                              # stop a build; -f alone does not
  commit SBX --tag REF                          # freeze the filesystem as an image
  snapshot create SBX [--name N] [--no-keep-running] [--no-memory] [--base SNAP]
                                                # --no-memory: filesystem only,
                                                # restores on any CPU but reboots
                                                # --base: store only what changed
                                                # since SNAP (needs guest memory)
  snapshot ls [--label k=v] | snapshot rm SNAP
  run --snapshot SNAP                           # restore instead of image
  image ls | image status REF | image prewarm REF... [--replicas N]

output: --json for machine-readable output, --quiet for identifiers only
exit:   0 ok, 64 not found, 69 unavailable (retry may help), 70 failed,
        125 usage error
env:    BEAN_BASE_URL (default http://127.0.0.1:8080), BEAN_API_KEY`

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseFlags(args []string) (map[string]string, []string) {
	flags := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		// Short flags (-f, -it) are boolean-only; long flags may take a value.
		if len(a) > 1 && a[0] == '-' && a[1] != '-' {
			for _, r := range a[1:] {
				flags[string(r)] = "true"
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			key := strings.TrimPrefix(a, "--")
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				flags[key[:eq]] = key[eq+1:]
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[key] = args[i+1]
				i++
			} else {
				flags[key] = "true"
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

func cmdRun(c *Client, args []string, stdout io.Writer) error {
	flags, _ := parseFlags(args)
	image, snap := flags["image"], flags["snapshot"]
	if (image == "") == (snap == "") {
		return usagef("provide exactly one of --image or --snapshot")
	}
	body := map[string]any{}
	if image != "" {
		body["image"] = image
	} else {
		body["snapshot"] = snap
	}
	if lbl := flags["label"]; lbl != "" {
		parts := strings.SplitN(lbl, "=", 2)
		if len(parts) == 2 {
			body["labels"] = map[string]string{parts[0]: parts[1]}
		}
	}
	if it := flags["idle-timeout"]; it != "" {
		lc := map[string]string{"idleTimeout": it}
		if oi := flags["on-idle"]; oi != "" {
			lc["onIdle"] = oi
		}
		body["lifecycle"] = lc
	}
	var out struct {
		Sandbox struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Image string `json:"image"`
		} `json:"sandbox"`
	}
	if err := c.doJSON("POST", "/v1/sandboxes", body, &out); err != nil {
		return err
	}
	// One line, id first: the existing shape, which scripts already cut on.
	return newPrinter(stdout, flags).result(out.Sandbox.ID,
		field{"state", out.Sandbox.State},
		field{"image", out.Sandbox.Image})
}

func cmdLs(c *Client, args []string, stdout io.Writer) error {
	flags, _ := parseFlags(args)
	path := "/v1/sandboxes"
	if lbl := flags["label"]; lbl != "" {
		path += "?label=" + url.QueryEscape(lbl)
	}
	var out struct {
		Sandboxes []struct {
			ID        string            `json:"id"`
			Image     string            `json:"image"`
			State     string            `json:"state"`
			Labels    map[string]string `json:"labels"`
			CreatedAt time.Time         `json:"createdAt"`
		} `json:"sandboxes"`
	}
	if err := c.doJSON("GET", path, nil, &out); err != nil {
		return err
	}
	p := newPrinter(stdout, flags)
	rows := make([]row, 0, len(out.Sandboxes))
	for _, sb := range out.Sandboxes {
		r := newRow("id", sb.ID).
			with("image", sb.Image).
			with("state", sb.State)
		// A person reads an age; a script wants a timestamp it can compare.
		if p.json {
			r = r.with("createdAt", sb.CreatedAt.UTC().Format(time.RFC3339))
		} else {
			r = r.with("age", time.Since(sb.CreatedAt).Truncate(time.Second))
		}
		// A script wants the map it can index; a person wants one column.
		if p.json {
			r = r.with("labels", orEmptyMap(sb.Labels))
		} else {
			r = r.with("labels", sortedLabels(sb.Labels))
		}
		rows = append(rows, r)
	}
	return p.table("sandboxes", rows)
}

func cmdExec(c *Client, args []string, stdout, stderr io.Writer) int {
	_, pos := parseFlags(args)
	if len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: bean exec SBX -- CMD...")
		return ExitUsage
	}
	id, cmd := pos[0], pos[1:]
	var out struct {
		ExitCode  int    `json:"exitCode"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
	}
	if err := c.doJSON("POST", "/v1/sandboxes/"+id+"/exec", map[string]any{"cmd": cmd}, &out); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitCodeFor(err)
	}
	io.WriteString(stdout, out.Stdout)
	io.WriteString(stderr, out.Stderr)
	if out.Truncated {
		fmt.Fprintln(stderr, "[output truncated]")
	}
	return out.ExitCode
}

func cmdKill(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) < 1 {
		return usagef("usage: bean kill SBX")
	}
	path := "/v1/sandboxes/" + pos[0]
	if flags["force"] == "true" {
		path += "?force=true"
	}
	if err := c.doJSON("DELETE", path, nil, nil); err != nil {
		return err
	}
	return newPrinter(stdout, flags).result(pos[0], field{"state", "killed"})
}

func cmdSimplePost(c *Client, args []string, action string) error {
	_, pos := parseFlags(args)
	if len(pos) < 1 {
		return usagef("usage: bean %s SBX", action)
	}
	return c.doJSON("POST", "/v1/sandboxes/"+pos[0]+"/"+action, nil, nil)
}

func cmdLogs(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) < 1 {
		return usagef("usage: bean logs SBX")
	}
	path := "/v1/sandboxes/" + pos[0] + "/logs"
	if t := flags["tail"]; t != "" {
		path += "?tailLines=" + url.QueryEscape(t)
	}
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
}

// cmdBuild builds an image from a Dockerfile on the platform.
//
// The build context is packed and uploaded, so the user needs no local Docker
// and the build cache is shared across the cluster rather than sitting on one
// laptop.
func cmdBuild(c *Client, args []string, stdout io.Writer) error {
	// Long flags only: the shared parser treats short flags as boolean, which
	// `events -f` depends on, so `-t REF` would silently lose its value.
	flags, pos := parseFlags(args)
	// logs and cancel are subcommands rather than sibling top-level commands so
	// that everything about one build is reached by one word. They are dispatched
	// on the positional arguments, not on args[0], so a leading --json still
	// reaches them.
	if len(pos) > 0 {
		switch pos[0] {
		case "logs":
			return cmdBuildLogs(c, flags, pos[1:], stdout)
		case "cancel":
			return cmdBuildCancel(c, flags, pos[1:], stdout)
		}
	}
	p := newPrinter(stdout, flags)
	tag := flags["tag"]
	if tag == "" {
		return usagef("usage: bean build --tag REF [--file Dockerfile] [CONTEXT_DIR]")
	}

	dockerfile := flags["file"]
	contextDir := "."
	if len(pos) > 0 {
		contextDir = pos[0]
	}
	if dockerfile == "" {
		dockerfile = filepath.Join(contextDir, "Dockerfile")
	}

	content, err := os.ReadFile(dockerfile)
	if err != nil {
		return fmt.Errorf("read %s: %w", dockerfile, err)
	}

	body := map[string]any{"tag": tag, "dockerfile": string(content)}

	// The context is only packed when the Dockerfile needs it: a build that
	// only runs commands should not upload the working directory.
	if needsContext(string(content)) {
		tarball, err := packContext(contextDir, dockerfile)
		if err != nil {
			return err
		}
		body["contextTar"] = base64.StdEncoding.EncodeToString(tarball)
		p.note("uploading %d KiB of build context", len(tarball)>>10)
	}

	if bargs := parseBuildArgs(flags["build-arg"]); len(bargs) > 0 {
		body["buildArgs"] = bargs
	}

	var out struct {
		ImageRef string `json:"imageRef"`
		State    string `json:"state"`
	}
	if err := c.doJSON("POST", "/v1/images/build", body, &out); err != nil {
		return err
	}

	// --follow prints the build's output and waits for it. It is opt-in rather
	// than the default because the accepted response is the contract a script
	// depends on, and because Ctrl-C on a followed build stops watching without
	// stopping the build — `bean build cancel` is what stops it.
	if flags["follow"] == "true" || flags["f"] == "true" {
		if err := p.result(out.ImageRef, field{"state", out.State}); err != nil {
			return err
		}
		return streamBuildLogs(c, out.ImageRef, stdout)
	}

	if err := p.result(out.ImageRef, field{"state", out.State}); err != nil {
		return err
	}
	// A build takes minutes, so the next step is worth naming — but only for a
	// person: a script already knows what it is going to poll.
	p.note("follow with: bean build logs %s", out.ImageRef)
	return nil
}

// streamBuildLogs prints a build's output until it finishes.
//
// The body is copied through rather than parsed: it is a log, and the server's
// trailing "build failed: ..." line is meant for the same eyes as the rest of it.
func streamBuildLogs(c *Client, ref string, stdout io.Writer) error {
	resp, err := c.do("GET", "/v1/images/build/logs?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// Copied in small reads so output appears as the build produces it; io.Copy's
	// buffer would hold a partial line back until the next flush filled it.
	buf := make([]byte, 4096)
	var tail strings.Builder
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := stdout.Write(buf[:n]); werr != nil {
				return werr
			}
			tail.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	// A failed build has to exit non-zero, or `bean build --follow && deploy`
	// would deploy from an image that was never produced. The outcome is in the
	// body because the status line was sent before it was known.
	if strings.Contains(tail.String(), "\nbuild failed:") {
		return &apiError{Code: "BUILD_FAILED", Message: "build " + ref + " failed",
			Status: http.StatusInternalServerError}
	}
	return nil
}

// cmdBuildLogs prints a build's retained output, following by default: someone
// asking for a running build's logs is asking to watch it.
func cmdBuildLogs(c *Client, _ map[string]string, pos []string, stdout io.Writer) error {
	if len(pos) < 1 {
		return usagef("usage: bean build logs REF")
	}
	return streamBuildLogs(c, pos[0], stdout)
}

// cmdBuildCancel stops a running build.
func cmdBuildCancel(c *Client, flags map[string]string, pos []string, stdout io.Writer) error {
	if len(pos) < 1 {
		return usagef("usage: bean build cancel REF")
	}
	var out struct {
		ImageRef string `json:"imageRef"`
		State    string `json:"state"`
	}
	if err := c.doJSON("POST", "/v1/images/build/cancel?ref="+url.QueryEscape(pos[0]),
		nil, &out); err != nil {
		return err
	}
	return newPrinter(stdout, flags).result(out.ImageRef, field{"state", out.State})
}

// needsContext reports whether a Dockerfile references the build context. COPY
// and ADD are the only instructions that read it.
func needsContext(dockerfile string) bool {
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "COPY", "ADD":
			return true
		}
	}
	return false
}

// parseBuildArgs reads "K=V,K2=V2".
func parseBuildArgs(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			out[k] = v
		}
	}
	return out
}

// packContext tars a directory for upload, skipping the Dockerfile itself
// (which travels inline) and anything .dockerignore excludes.
func packContext(dir, dockerfilePath string) ([]byte, error) {
	ignore, err := loadDockerignore(dir)
	if err != nil {
		return nil, err
	}
	dfAbs, _ := filepath.Abs(dockerfilePath)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil || rel == "." {
			return rerr
		}
		if abs, _ := filepath.Abs(path); abs == dfAbs {
			return nil
		}
		if ignore(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks are recorded as links rather than followed, so a link out of
		// the context does not drag in whatever it points at.
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if werr := tw.WriteHeader(hdr); werr != nil {
			return werr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		_, cerr := io.Copy(tw, f)
		return cerr
	})
	if err != nil {
		return nil, fmt.Errorf("pack build context: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// loadDockerignore returns a predicate matching excluded paths. The matching is
// prefix and glob based, which covers the patterns in practice without
// reimplementing Docker's full rule set.
func loadDockerignore(dir string) (func(string) bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		if os.IsNotExist(err) {
			// .git is always excluded: it is large, never needed by a build,
			// and shipping it would leak history into the context.
			return func(rel string) bool {
				return rel == ".git" || strings.HasPrefix(rel, ".git/")
			}, nil
		}
		return nil, err
	}

	var patterns []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, strings.TrimSuffix(line, "/"))
	}
	return func(rel string) bool {
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
			return true
		}
		for _, p := range patterns {
			if rel == p || strings.HasPrefix(rel, p+"/") {
				return true
			}
			if ok, _ := filepath.Match(p, rel); ok {
				return true
			}
			if ok, _ := filepath.Match(p, filepath.Base(rel)); ok {
				return true
			}
		}
		return false
	}, nil
}

// cmdCommit turns a sandbox's filesystem into a reusable base image.
//
// Distinct from snapshot: a snapshot restores this one sandbox including its
// memory, on the tier that made it. A committed image is a filesystem anyone can
// start from — the "set it up interactively, then share it" path.
func cmdCommit(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) == 0 || flags["tag"] == "" {
		return usagef("usage: bean commit SBX --tag REF")
	}
	var out struct {
		ImageRef string `json:"imageRef"`
	}
	if err := c.doJSON("POST", "/v1/sandboxes/"+pos[0]+"/commit",
		map[string]any{"tag": flags["tag"]}, &out); err != nil {
		return err
	}
	return newPrinter(stdout, flags).result(out.ImageRef,
		field{"sandboxId", pos[0]})
}

// cmdSnapshot handles snapshot create/ls/rm.
func cmdSnapshot(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) == 0 {
		return usagef("usage: bean snapshot create SBX | ls | rm SNAP")
	}
	switch pos[0] {
	case "create":
		if len(pos) < 2 {
			return usagef("usage: bean snapshot create SBX [--name N] [--base SNAP]")
		}
		body := map[string]any{"name": flags["name"]}
		// keepRunning defaults true; --no-keep-running stops the source once
		// the snapshot is safely stored.
		if flags["no-keep-running"] == "true" {
			body["keepRunning"] = false
		}
		// Guest memory is included by default, so a restore resumes the guest.
		// --no-memory captures only the filesystem: the restore boots fresh but
		// can land on any CPU, where guest memory pins a snapshot to a
		// compatible vendor and family.
		if flags["no-memory"] == "true" {
			body["includeMemory"] = false
		}
		// --base captures only the memory written since that snapshot, which is
		// what makes repeated checkpoints of one sandbox cheap. The server may
		// answer with a full snapshot anyway once the chain is deep enough, which
		// the reported base makes visible.
		if base := flags["base"]; base != "" {
			body["base"] = base
		}
		var out struct {
			SnapshotID string `json:"snapshotId"`
			Snapshot   struct {
				State     string `json:"state"`
				SizeBytes int64  `json:"sizeBytes"`
				// A pointer so a snapshot taken before the server reported this
				// is shown as unknown rather than as "no memory" — the latter
				// would be a confident wrong answer about whether a restore
				// resumes the guest.
				IncludeMemory *bool  `json:"includeMemory"`
				BaseID        string `json:"baseId"`
				ChainDepth    int    `json:"chainDepth"`
			} `json:"snapshot"`
		}
		if err := c.doJSON("POST", "/v1/sandboxes/"+pos[1]+"/snapshot", body, &out); err != nil {
			return err
		}
		memory := "unknown"
		if out.Snapshot.IncludeMemory != nil {
			memory = "no"
			if *out.Snapshot.IncludeMemory {
				memory = "yes"
			}
		}
		// A requested base that comes back empty means the chain hit its limit and
		// this was taken in full. Reporting it is how a caller sees that without
		// having to know the limit.
		base := out.Snapshot.BaseID
		if base == "" {
			base = "-"
		}
		return newPrinter(stdout, flags).result(out.SnapshotID,
			field{"state", out.Snapshot.State},
			field{"sizeBytes", out.Snapshot.SizeBytes},
			// Whether a restore resumes or reboots is the snapshot's most
			// consequential property, so it is reported rather than inferred
			// from the size.
			field{"memory", memory},
			field{"base", base},
			field{"chainDepth", out.Snapshot.ChainDepth})

	case "ls":
		path := "/v1/snapshots"
		if lbl := flags["label"]; lbl != "" {
			path += "?label=" + url.QueryEscape(lbl)
		}
		var out struct {
			Snapshots []struct {
				ID        string    `json:"id"`
				Name      string    `json:"name"`
				State     string    `json:"state"`
				SandboxID string    `json:"sandboxId"`
				Image     string    `json:"image"`
				SizeBytes int64     `json:"sizeBytes"`
				CreatedAt time.Time `json:"createdAt"`
			} `json:"snapshots"`
		}
		if err := c.doJSON("GET", path, nil, &out); err != nil {
			return err
		}
		p := newPrinter(stdout, flags)
		rows := make([]row, 0, len(out.Snapshots))
		for _, s := range out.Snapshots {
			r := newRow("id", s.ID).
				with("name", s.Name).
				with("state", s.State).
				with("sandboxId", s.SandboxID).
				with("image", s.Image).
				with("sizeBytes", s.SizeBytes)
			if p.json {
				r = r.with("createdAt", s.CreatedAt.UTC().Format(time.RFC3339))
			} else {
				r = r.with("age", time.Since(s.CreatedAt).Truncate(time.Second))
			}
			rows = append(rows, r)
		}
		return p.table("snapshots", rows)

	case "rm":
		if len(pos) < 2 {
			return usagef("usage: bean snapshot rm SNAP")
		}
		if err := c.doJSON("DELETE", "/v1/snapshots/"+pos[1], nil, nil); err != nil {
			return err
		}
		return newPrinter(stdout, flags).result(pos[1], field{"state", "deleted"})

	default:
		return usagef("unknown snapshot subcommand %q", pos[0])
	}
}

// cmdImage handles image ls/status/prewarm.
func cmdImage(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) == 0 {
		return usagef("usage: bean image ls | status REF | prewarm REF...")
	}
	switch pos[0] {
	case "ls":
		var out struct {
			Images []struct {
				Ref       string `json:"ref"`
				State     string `json:"state"`
				SizeBytes int64  `json:"sizeBytes"`
			} `json:"images"`
		}
		if err := c.doJSON("GET", "/v1/images", nil, &out); err != nil {
			return err
		}
		rows := make([]row, 0, len(out.Images))
		for _, i := range out.Images {
			rows = append(rows, newRow("ref", i.Ref).
				with("state", i.State).
				with("sizeBytes", i.SizeBytes))
		}
		return newPrinter(stdout, flags).table("images", rows)

	case "status":
		if len(pos) < 2 {
			return usagef("usage: bean image status REF")
		}
		var out map[string]any
		if err := c.doJSON("GET", "/v1/images/status?ref="+url.QueryEscape(pos[1]), nil, &out); err != nil {
			return err
		}
		r := newRow("ref", fmt.Sprint(out["ref"]))
		for _, k := range []string{"digest", "state", "format", "sizeBytes"} {
			if v, ok := out[k]; ok {
				r = r.with(k, v)
			}
		}
		return newPrinter(stdout, flags).record(r)

	case "prewarm":
		if len(pos) < 2 {
			return usagef("usage: bean image prewarm REF... [--replicas N]")
		}
		body := map[string]any{"refs": pos[1:]}
		// How widely to warm an image is a capacity decision a caller can act
		// on, so it stays. It is named for the copies rather than the machines
		// holding them: nodes are not part of the platform's vocabulary.
		if n := flags["replicas"]; n != "" {
			parsed, err := strconv.Atoi(n)
			if err != nil {
				return usagef("--replicas %q: not a number", n)
			}
			body["targetNodes"] = parsed
		}
		var out struct {
			JobID string         `json:"jobId"`
			Ready map[string]int `json:"ready"`
		}
		if err := c.doJSON("POST", "/v1/images/prewarm", body, &out); err != nil {
			return err
		}
		p := newPrinter(stdout, flags)
		if p.json {
			// Readiness per image, and the job to poll. Whether an image is
			// ready is the caller's concern; how many machines hold it is not
			// something they can act on.
			refs := make(map[string]string, len(out.Ready))
			for ref, n := range out.Ready {
				refs[ref] = readyState(n)
			}
			return p.encode(map[string]any{"jobId": out.JobID, "images": refs})
		}
		if err := p.result(out.JobID); err != nil {
			return err
		}
		names := make([]string, 0, len(out.Ready))
		for ref := range out.Ready {
			names = append(names, ref)
		}
		sort.Strings(names)
		for _, ref := range names {
			p.note("  %s: %s", ref, readyState(out.Ready[ref]))
		}
		return nil

	default:
		return usagef("unknown image subcommand %q", pos[0])
	}
}

// streamEvents follows the SSE event stream until interrupted.
func streamEvents(c *Client, sandbox, label string, stdout io.Writer) error {
	q := url.Values{}
	if sandbox != "" {
		q.Set("sandbox", sandbox)
	}
	if label != "" {
		q.Set("label", label)
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		// Skip comments (": connected"/": keepalive"), blanks and the
		// event: name line; the data: line carries the full payload.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev struct {
			Type      string            `json:"type"`
			SandboxID string            `json:"sandboxId"`
			Timestamp time.Time         `json:"timestamp"`
			Data      map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		extra := ""
		if reason := ev.Data["reason"]; reason != "" {
			extra = "  " + reason
		}
		fmt.Fprintf(stdout, "%s  %s  %s%s\n",
			ev.Timestamp.Format(time.RFC3339), ev.SandboxID, ev.Type, extra)
	}
	return sc.Err()
}

// cmdCp copies LOCAL -> sbx:ID:/path or sbx:ID:/path -> LOCAL.
func cmdCp(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) != 2 {
		return usagef("usage: bean cp SRC DST")
	}
	src, dst := pos[0], pos[1]
	switch {
	case strings.HasPrefix(dst, "sbx:"):
		id, remote, err := splitSbxPath(dst)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		req, err := http.NewRequest("PUT",
			c.BaseURL+"/v1/sandboxes/"+id+"/files?mkdirs=true&path="+url.QueryEscape(remote),
			bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		newPrinter(stdout, flags).note("copied %s -> %s:%s", src, id, remote)
		return nil
	case strings.HasPrefix(src, "sbx:"):
		id, remote, err := splitSbxPath(src)
		if err != nil {
			return err
		}
		resp, err := c.do("GET", "/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(remote), nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(f, resp.Body); err != nil {
			return err
		}
		newPrinter(stdout, flags).note("copied %s:%s -> %s", id, remote, dst)
		return nil
	default:
		return fmt.Errorf("one side must be sbx:ID:/path")
	}
}

func splitSbxPath(s string) (id, path string, err error) {
	rest := strings.TrimPrefix(s, "sbx:")
	i := strings.Index(rest, ":")
	if i < 0 {
		return "", "", fmt.Errorf("invalid sandbox path %q, want sbx:ID:/path", s)
	}
	return rest[:i], rest[i+1:], nil
}

func cmdEvents(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if flags["follow"] == "true" || flags["f"] == "true" {
		// Follow streams cluster-wide unless a sandbox or label narrows it.
		sandbox := ""
		if len(pos) > 0 {
			sandbox = pos[0]
		}
		return streamEvents(c, sandbox, flags["label"], stdout)
	}
	if len(pos) < 1 {
		return usagef("usage: bean events SBX [-f] [--label k=v]")
	}
	var out struct {
		Events []struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"events"`
	}
	if err := c.doJSON("GET", "/v1/sandboxes/"+pos[0]+"/events", nil, &out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tTYPE")
	for _, e := range out.Events {
		fmt.Fprintf(tw, "%s\t%s\n", e.Timestamp.Format(time.RFC3339), e.Type)
	}
	return tw.Flush()
}
