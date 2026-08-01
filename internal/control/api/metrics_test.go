package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	// /metrics is unauthenticated: it is scraped locally and carries no
	// sandbox contents.
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestMetricsEndpointNeedsNoAuth(t *testing.T) {
	ts := startStack(t)
	if got := scrape(t, ts); got == "" {
		t.Error("empty metrics output")
	}
}

func TestMetricsRecordCreateAndExec(t *testing.T) {
	ts := startStack(t)

	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "img:1"})
	id := out["sandbox"].(map[string]any)["id"].(string)
	doReq(t, ts, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{"cmd": []string{"true"}})

	body := scrape(t, ts)
	for _, want := range []string{
		`bean_sandbox_creates_total{outcome="success"} 1`,
		"bean_sandbox_create_duration_seconds_count",
		"bean_exec_duration_seconds_count",
		`bean_sandboxes{state="RUNNING"} 1`,
		`bean_events_total{type="sandbox.lifecycle.created"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in metrics:\n%s", want, body)
		}
	}
}

func TestMetricsStateGaugesReflectStore(t *testing.T) {
	ts := startStack(t)
	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "img:1"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	if body := scrape(t, ts); !strings.Contains(body, `bean_sandboxes{state="RUNNING"} 1`) {
		t.Errorf("running gauge wrong:\n%s", body)
	}
	doReq(t, ts, "DELETE", "/v1/sandboxes/"+id, nil)

	body := scrape(t, ts)
	// The sandbox moved to STOPPED, and RUNNING must drop back to zero
	// rather than keep its stale value.
	if !strings.Contains(body, `bean_sandboxes{state="STOPPED"} 1`) {
		t.Errorf("stopped gauge missing:\n%s", body)
	}
	if !strings.Contains(body, `bean_sandboxes{state="RUNNING"} 0`) {
		t.Errorf("running gauge not reset:\n%s", body)
	}
}

func TestMetricsRecordFailedCreate(t *testing.T) {
	ts := startStack(t)
	// Invalid request never reaches a node: it counts as an error outcome.
	doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{})
	if body := scrape(t, ts); !strings.Contains(body, `bean_sandbox_creates_total{outcome="error"} 1`) {
		t.Errorf("error outcome not counted:\n%s", body)
	}
}

func TestMetricsRegistryExposed(t *testing.T) {
	ts, srv := startStackWithServer(t)
	// Binaries can add their own series to the same registry.
	srv.Metrics().IncCounter("bean_custom_total", "Custom.", nil, 3)
	if body := scrape(t, ts); !strings.Contains(body, "bean_custom_total 3") {
		t.Errorf("custom metric missing:\n%s", body)
	}
}
