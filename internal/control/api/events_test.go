package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

func TestParseLabelFilter(t *testing.T) {
	cases := []struct{ in, key, val string }{
		{"", "", ""},
		{"run=abc", "run", "abc"},
		{"run", "run", ""},
		{"run=a=b", "run", "a=b"},
	}
	for _, tc := range cases {
		k, v := parseLabelFilter(tc.in)
		if k != tc.key || v != tc.val {
			t.Errorf("%q -> (%q,%q), want (%q,%q)", tc.in, k, v, tc.key, tc.val)
		}
	}
}

func TestEventBusFilters(t *testing.T) {
	b := newEventBus()
	all, stopAll := b.subscribe("", "", "")
	defer stopAll()
	bySandbox, stopSbx := b.subscribe("sbx_1", "", "")
	defer stopSbx()
	byLabel, stopLbl := b.subscribe("", "run", "a")
	defer stopLbl()

	if got := b.subscriberCount(); got != 3 {
		t.Fatalf("subscribers = %d", got)
	}

	b.publish(&store.Event{Type: "x", SandboxID: "sbx_1"}, map[string]string{"run": "a"})
	b.publish(&store.Event{Type: "y", SandboxID: "sbx_2"}, map[string]string{"run": "b"})

	if got := drain(all.ch); len(got) != 2 {
		t.Errorf("unfiltered got %d events, want 2", len(got))
	}
	got := drain(bySandbox.ch)
	if len(got) != 1 || got[0].SandboxID != "sbx_1" {
		t.Errorf("sandbox filter got %+v", got)
	}
	got = drain(byLabel.ch)
	if len(got) != 1 || got[0].Type != "x" {
		t.Errorf("label filter got %+v", got)
	}
}

func TestEventBusDropsSlowSubscriber(t *testing.T) {
	b := newEventBus()
	sub, stop := b.subscribe("", "", "")
	defer stop()
	// Overfill without reading: publishes must not block.
	for i := 0; i < subscriberBuffer+20; i++ {
		b.publish(&store.Event{Type: "flood"}, nil)
	}
	if got := sub.dropped.Load(); got == 0 {
		t.Error("expected drops for a subscriber that never reads")
	}
	if got := len(sub.ch); got != subscriberBuffer {
		t.Errorf("buffered = %d, want %d", got, subscriberBuffer)
	}
}

func TestEventBusUnsubscribe(t *testing.T) {
	b := newEventBus()
	sub, stop := b.subscribe("", "", "")
	stop()
	if got := b.subscriberCount(); got != 0 {
		t.Errorf("subscribers after stop = %d", got)
	}
	if _, open := <-sub.ch; open {
		t.Error("channel should be closed")
	}
	// Double stop is safe, and publishing with no subscribers is a no-op.
	stop()
	b.publish(&store.Event{Type: "x"}, nil)
}

func drain(ch chan *store.Event) []*store.Event {
	var out []*store.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

func TestEventStreamDeliversLifecycleEvents(t *testing.T) {
	env := startEnv(t, envOpts{})

	req, err := http.NewRequest("GET", env.Server.URL+"/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	// The stream opens with a comment line.
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("first line = %q err=%v", line, err)
	}

	// Creating a sandbox must produce events on the stream.
	go func() {
		time.Sleep(100 * time.Millisecond)
		env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "img:1"})
	}()

	seen := map[string]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event: ") {
			seen[strings.TrimPrefix(line, "event: ")] = true
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			var ev store.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Errorf("bad event payload: %v", err)
			} else if ev.SandboxID == "" || ev.Version != "v1" {
				t.Errorf("event = %+v", ev)
			}
		}
	}
	if !seen["sandbox.lifecycle.created"] {
		t.Errorf("missing created event; saw %v", seen)
	}
	if !seen["sandbox.lifecycle.running"] {
		t.Errorf("missing running event; saw %v", seen)
	}
}

func TestEventStreamRequiresAuth(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, err := http.Get(env.Server.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestEventStreamUnsubscribesOnDisconnect(t *testing.T) {
	env := startEnv(t, envOpts{})
	req, _ := http.NewRequest("GET", env.Server.URL+"/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read the preamble so the handler is definitely subscribed.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The server must drop the subscription; nothing to assert directly from
	// outside, so ensure subsequent requests still work (no leaked lock).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := env.do("GET", "/v1/sandboxes", nil); code.StatusCode == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("API unusable after stream disconnect")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
