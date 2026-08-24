package obs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCounterAccumulates(t *testing.T) {
	r := NewRegistry()
	r.IncCounter("wizard_calls_total", "Calls.", map[string]string{"outcome": "ok"}, 1)
	r.IncCounter("wizard_calls_total", "Calls.", map[string]string{"outcome": "ok"}, 2)
	r.IncCounter("wizard_calls_total", "Calls.", map[string]string{"outcome": "err"}, 1)

	out := render(t, r)
	if !strings.Contains(out, "# TYPE wizard_calls_total counter") {
		t.Errorf("missing TYPE line:\n%s", out)
	}
	if !strings.Contains(out, `wizard_calls_total{outcome="ok"} 3`) {
		t.Errorf("counter not accumulated:\n%s", out)
	}
	if !strings.Contains(out, `wizard_calls_total{outcome="err"} 1`) {
		t.Errorf("second series missing:\n%s", out)
	}
	if !strings.Contains(out, "# HELP wizard_calls_total Calls.") {
		t.Errorf("missing HELP:\n%s", out)
	}
}

func TestGaugeReplaces(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("wizard_sandboxes", "Sandboxes.", map[string]string{"state": "RUNNING"}, 5)
	r.SetGauge("wizard_sandboxes", "Sandboxes.", map[string]string{"state": "RUNNING"}, 2)
	out := render(t, r)
	if !strings.Contains(out, `wizard_sandboxes{state="RUNNING"} 2`) {
		t.Errorf("gauge not replaced:\n%s", out)
	}
	if strings.Contains(out, "} 5") {
		t.Errorf("stale gauge value retained:\n%s", out)
	}
}

func TestGaugeWithoutLabels(t *testing.T) {
	r := NewRegistry()
	r.SetGauge("wizard_subscribers", "Subs.", nil, 3)
	out := render(t, r)
	if !strings.Contains(out, "wizard_subscribers 3") {
		t.Errorf("unlabelled gauge:\n%s", out)
	}
}

func TestHistogramCumulativeBuckets(t *testing.T) {
	r := NewRegistry()
	buckets := []float64{1, 5, 10}
	for _, v := range []float64{0.5, 2, 7, 50} {
		r.Observe("wizard_lat_seconds", "Latency.", buckets, nil, v)
	}
	out := render(t, r)
	// Cumulative: le=1 has 1 (0.5), le=5 has 2 (0.5,2), le=10 has 3, +Inf 4.
	for _, want := range []string{
		`wizard_lat_seconds_bucket{le="1"} 1`,
		`wizard_lat_seconds_bucket{le="5"} 2`,
		`wizard_lat_seconds_bucket{le="10"} 3`,
		`wizard_lat_seconds_bucket{le="+Inf"} 4`,
		`wizard_lat_seconds_count 4`,
		`wizard_lat_seconds_sum 59.5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "# TYPE wizard_lat_seconds histogram") {
		t.Errorf("missing TYPE:\n%s", out)
	}
}

func TestHistogramBoundaryIsInclusive(t *testing.T) {
	r := NewRegistry()
	// A value exactly on a boundary belongs to that bucket (le semantics).
	r.Observe("wizard_b_seconds", "", []float64{1, 2}, nil, 1)
	out := render(t, r)
	if !strings.Contains(out, `wizard_b_seconds_bucket{le="1"} 1`) {
		t.Errorf("boundary value excluded:\n%s", out)
	}
}

func TestObserveDurationUsesDefaultBuckets(t *testing.T) {
	r := NewRegistry()
	r.ObserveDuration("wizard_create_seconds", "Create.", map[string]string{"outcome": "success"},
		300*time.Millisecond)
	out := render(t, r)
	if !strings.Contains(out, `wizard_create_seconds_bucket{le="0.5",outcome="success"} 1`) {
		t.Errorf("expected 300ms in the 0.5s bucket:\n%s", out)
	}
	if !strings.Contains(out, `wizard_create_seconds_bucket{le="0.25",outcome="success"} 0`) {
		t.Errorf("300ms must not land in 0.25s:\n%s", out)
	}
}

func TestHistogramPerLabelSeries(t *testing.T) {
	r := NewRegistry()
	r.Observe("wizard_x_seconds", "", []float64{1}, map[string]string{"outcome": "ok"}, 0.5)
	r.Observe("wizard_x_seconds", "", []float64{1}, map[string]string{"outcome": "err"}, 2)
	out := render(t, r)
	if !strings.Contains(out, `wizard_x_seconds_count{outcome="ok"} 1`) ||
		!strings.Contains(out, `wizard_x_seconds_count{outcome="err"} 1`) {
		t.Errorf("label series not separated:\n%s", out)
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	build := func() string {
		r := NewRegistry()
		r.IncCounter("b_total", "", map[string]string{"z": "1", "a": "2"}, 1)
		r.IncCounter("a_total", "", nil, 1)
		r.SetGauge("g", "", map[string]string{"k": "v"}, 1)
		r.Observe("h_seconds", "", []float64{1}, nil, 0.5)
		return render(t, r)
	}
	first := build()
	for i := 0; i < 5; i++ {
		if got := build(); got != first {
			t.Fatalf("output not deterministic:\n%s\n---\n%s", first, got)
		}
	}
	// Families are emitted in name order within each type, labels sorted.
	if !strings.Contains(first, `b_total{a="2",z="1"}`) {
		t.Errorf("labels not sorted:\n%s", first)
	}
	if strings.Index(first, "a_total") > strings.Index(first, "b_total") {
		t.Errorf("families not name-ordered:\n%s", first)
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	r.IncCounter("wizard_e_total", "", map[string]string{"reason": "a\\b\nc"}, 1)
	out := render(t, r)
	if !strings.Contains(out, `reason="a\\b\nc"`) {
		t.Errorf("label not escaped: %q", out)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.IncCounter("wizard_c_total", "", nil, 1)
			r.SetGauge("wizard_g", "", nil, 1)
			r.Observe("wizard_h_seconds", "", []float64{1}, nil, 0.5)
		}()
	}
	wg.Wait()
	out := render(t, r)
	if !strings.Contains(out, "wizard_c_total 50") {
		t.Errorf("lost counter increments:\n%s", out)
	}
	if !strings.Contains(out, "wizard_h_seconds_count 50") {
		t.Errorf("lost observations:\n%s", out)
	}
}
