// Package cli implements the bean CLI against the REST API.
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	return c.HTTP.Do(req)
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
			return fmt.Errorf("%s: %s", e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
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
		return 125
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
	case "image":
		err = cmdImage(c, rest, stdout)
	case "version":
		fmt.Fprintln(stdout, "bean CLI (dev)")
	default:
		fmt.Fprintln(stderr, usage)
		return 125
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 125
	}
	return 0
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
  snapshot create SBX [--name N] [--no-keep-running]
  snapshot ls [--label k=v] | snapshot rm SNAP
  run --snapshot SNAP                           # restore instead of image
  image ls | image status REF | image prewarm REF... [--nodes N]
env: BEAN_BASE_URL (default http://127.0.0.1:8080), BEAN_API_KEY`

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
		return fmt.Errorf("provide exactly one of --image or --snapshot")
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
		} `json:"sandbox"`
	}
	if err := c.doJSON("POST", "/v1/sandboxes", body, &out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\t%s\n", out.Sandbox.ID, out.Sandbox.State)
	return nil
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
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tIMAGE\tSTATE\tAGE\tLABELS")
	for _, sb := range out.Sandboxes {
		var lbls []string
		for k, v := range sb.Labels {
			lbls = append(lbls, k+"="+v)
		}
		sort.Strings(lbls)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sb.ID, sb.Image, sb.State,
			time.Since(sb.CreatedAt).Truncate(time.Second), strings.Join(lbls, ","))
	}
	return tw.Flush()
}

func cmdExec(c *Client, args []string, stdout, stderr io.Writer) int {
	_, pos := parseFlags(args)
	if len(pos) < 2 {
		fmt.Fprintln(stderr, "usage: bean exec SBX -- CMD...")
		return 125
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
		return 125
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
		return fmt.Errorf("usage: bean kill SBX")
	}
	path := "/v1/sandboxes/" + pos[0]
	if flags["force"] == "true" {
		path += "?force=true"
	}
	if err := c.doJSON("DELETE", path, nil, nil); err != nil {
		return err
	}
	fmt.Fprintln(stdout, pos[0], "killed")
	return nil
}

func cmdSimplePost(c *Client, args []string, action string) error {
	_, pos := parseFlags(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: bean %s SBX", action)
	}
	return c.doJSON("POST", "/v1/sandboxes/"+pos[0]+"/"+action, nil, nil)
}

func cmdLogs(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: bean logs SBX")
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

// cmdSnapshot handles snapshot create/ls/rm.
func cmdSnapshot(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) == 0 {
		return fmt.Errorf("usage: bean snapshot create SBX | ls | rm SNAP")
	}
	switch pos[0] {
	case "create":
		if len(pos) < 2 {
			return fmt.Errorf("usage: bean snapshot create SBX [--name N]")
		}
		body := map[string]any{"name": flags["name"]}
		// keepRunning defaults true; --no-keep-running stops the source once
		// the snapshot is safely stored.
		if flags["no-keep-running"] == "true" {
			body["keepRunning"] = false
		}
		var out struct {
			SnapshotID string `json:"snapshotId"`
			Snapshot   struct {
				State     string `json:"state"`
				SizeBytes int64  `json:"sizeBytes"`
			} `json:"snapshot"`
		}
		if err := c.doJSON("POST", "/v1/sandboxes/"+pos[1]+"/snapshot", body, &out); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\t%s\t%d bytes\n",
			out.SnapshotID, out.Snapshot.State, out.Snapshot.SizeBytes)
		return nil

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
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTATE\tSANDBOX\tIMAGE\tSIZE\tAGE")
		for _, s := range out.Snapshots {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
				s.ID, s.Name, s.State, s.SandboxID, s.Image, s.SizeBytes,
				time.Since(s.CreatedAt).Truncate(time.Second))
		}
		return tw.Flush()

	case "rm":
		if len(pos) < 2 {
			return fmt.Errorf("usage: bean snapshot rm SNAP")
		}
		if err := c.doJSON("DELETE", "/v1/snapshots/"+pos[1], nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(stdout, pos[1], "deleted")
		return nil

	default:
		return fmt.Errorf("unknown snapshot subcommand %q", pos[0])
	}
}

// cmdImage handles image ls/status/prewarm.
func cmdImage(c *Client, args []string, stdout io.Writer) error {
	flags, pos := parseFlags(args)
	if len(pos) == 0 {
		return fmt.Errorf("usage: bean image ls | status REF | prewarm REF...")
	}
	switch pos[0] {
	case "ls":
		var out struct {
			Images []struct {
				Ref         string `json:"ref"`
				State       string `json:"state"`
				CachedNodes int    `json:"cachedNodes"`
				SizeBytes   int64  `json:"sizeBytes"`
			} `json:"images"`
		}
		if err := c.doJSON("GET", "/v1/images", nil, &out); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "REF\tSTATE\tCACHED NODES\tSIZE")
		for _, i := range out.Images {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\n", i.Ref, i.State, i.CachedNodes, i.SizeBytes)
		}
		return tw.Flush()

	case "status":
		if len(pos) < 2 {
			return fmt.Errorf("usage: bean image status REF")
		}
		var out map[string]any
		if err := c.doJSON("GET", "/v1/images/status?ref="+url.QueryEscape(pos[1]), nil, &out); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
		for _, k := range []string{"ref", "digest", "state", "format", "cachedNodes", "sizeBytes"} {
			if v, ok := out[k]; ok {
				fmt.Fprintf(tw, "%s:\t%v\n", k, v)
			}
		}
		return tw.Flush()

	case "prewarm":
		if len(pos) < 2 {
			return fmt.Errorf("usage: bean image prewarm REF... [--nodes N]")
		}
		body := map[string]any{"refs": pos[1:]}
		if n := flags["nodes"]; n != "" {
			if parsed, err := strconv.Atoi(n); err == nil {
				body["targetNodes"] = parsed
			}
		}
		var out struct {
			JobID string         `json:"jobId"`
			Ready map[string]int `json:"ready"`
		}
		if err := c.doJSON("POST", "/v1/images/prewarm", body, &out); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s\n", out.JobID)
		for ref, n := range out.Ready {
			fmt.Fprintf(stdout, "  %s: cached on %d node(s)\n", ref, n)
		}
		return nil

	default:
		return fmt.Errorf("unknown image subcommand %q", pos[0])
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
	_, pos := parseFlags(args)
	if len(pos) != 2 {
		return fmt.Errorf("usage: bean cp SRC DST")
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
		fmt.Fprintf(stdout, "copied %s -> %s:%s\n", src, id, remote)
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
		fmt.Fprintf(stdout, "copied %s:%s -> %s\n", id, remote, dst)
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
		return fmt.Errorf("usage: bean events SBX [-f] [--label k=v]")
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
