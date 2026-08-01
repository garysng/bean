// Package obs provides the platform's metrics surface. It implements the
// Prometheus text exposition format directly so the binaries stay
// dependency-free; an OTLP exporter can wrap the same registry later.
package obs

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry collects counters, gauges and histograms.
type Registry struct {
	mu         sync.RWMutex
	counters   map[string]*metricFamily
	gauges     map[string]*metricFamily
	histograms map[string]*histogramFamily
}

type metricFamily struct {
	name   string
	help   string
	series map[string]*series // key = encoded labels
}

type series struct {
	labels map[string]string
	value  float64
}

type histogramFamily struct {
	name    string
	help    string
	buckets []float64
	series  map[string]*histogramSeries
}

type histogramSeries struct {
	labels map[string]string
	counts []uint64 // len(buckets)+1, last is +Inf
	sum    float64
	count  uint64
}

// DefaultBucketsSeconds covers sandbox create latency: the cached path
// lands under a second, cold pulls take several (docs/security-and-startup B1).
var DefaultBucketsSeconds = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60}

func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*metricFamily{},
		gauges:     map[string]*metricFamily{},
		histograms: map[string]*histogramFamily{},
	}
}

// IncCounter adds delta (>=0 by convention) to a counter series.
func (r *Registry) IncCounter(name, help string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.counters[name]
	if !ok {
		f = &metricFamily{name: name, help: help, series: map[string]*series{}}
		r.counters[name] = f
	}
	key := encodeLabels(labels)
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: copyLabels(labels)}
		f.series[key] = s
	}
	s.value += delta
}

// SetGauge replaces a gauge series value.
func (r *Registry) SetGauge(name, help string, labels map[string]string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.gauges[name]
	if !ok {
		f = &metricFamily{name: name, help: help, series: map[string]*series{}}
		r.gauges[name] = f
	}
	key := encodeLabels(labels)
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: copyLabels(labels)}
		f.series[key] = s
	}
	s.value = v
}

// Observe records a value in a histogram series.
func (r *Registry) Observe(name, help string, buckets []float64, labels map[string]string, v float64) {
	if len(buckets) == 0 {
		buckets = DefaultBucketsSeconds
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.histograms[name]
	if !ok {
		f = &histogramFamily{name: name, help: help, buckets: buckets,
			series: map[string]*histogramSeries{}}
		r.histograms[name] = f
	}
	key := encodeLabels(labels)
	s, ok := f.series[key]
	if !ok {
		s = &histogramSeries{labels: copyLabels(labels), counts: make([]uint64, len(f.buckets)+1)}
		f.series[key] = s
	}
	s.sum += v
	s.count++
	idx := sort.SearchFloat64s(f.buckets, v)
	// Cumulative buckets: everything at or above the matched index counts.
	for i := idx; i < len(s.counts); i++ {
		s.counts[i]++
	}
}

// ObserveDuration is the common case for latency histograms.
func (r *Registry) ObserveDuration(name, help string, labels map[string]string, d time.Duration) {
	r.Observe(name, help, DefaultBucketsSeconds, labels, d.Seconds())
}

// WritePrometheus renders the registry in text exposition format.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, name := range sortedKeys(r.counters) {
		f := r.counters[name]
		if err := writeFamily(w, f, "counter"); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(r.gauges) {
		f := r.gauges[name]
		if err := writeFamily(w, f, "gauge"); err != nil {
			return err
		}
	}
	for _, name := range sortedHistKeys(r.histograms) {
		f := r.histograms[name]
		if f.help != "" {
			if _, err := fmt.Fprintf(w, "# HELP %s %s\n", f.name, f.help); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s histogram\n", f.name); err != nil {
			return err
		}
		for _, key := range sortedSeriesKeys(f.series) {
			s := f.series[key]
			for i, b := range f.buckets {
				lbls := withLabel(s.labels, "le", formatFloat(b))
				if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, renderLabels(lbls), s.counts[i]); err != nil {
					return err
				}
			}
			infLabels := withLabel(s.labels, "le", "+Inf")
			if _, err := fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, renderLabels(infLabels), s.counts[len(f.buckets)]); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_sum%s %s\n", f.name, renderLabels(s.labels), formatFloat(s.sum)); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "%s_count%s %d\n", f.name, renderLabels(s.labels), s.count); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeFamily(w io.Writer, f *metricFamily, typ string) error {
	if f.help != "" {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", f.name, f.help); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", f.name, typ); err != nil {
		return err
	}
	for _, key := range sortedSeriesKeys(f.series) {
		s := f.series[key]
		if _, err := fmt.Fprintf(w, "%s%s %s\n", f.name, renderLabels(s.labels), formatFloat(s.value)); err != nil {
			return err
		}
	}
	return nil
}

func encodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('\x00')
		b.WriteString(labels[k])
		b.WriteByte('\x00')
	}
	return b.String()
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabelValue(labels[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabelValue applies the Prometheus exposition escaping rules:
// backslash, double quote and newline.
func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func withLabel(labels map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for lk, lv := range labels {
		out[lk] = lv
	}
	out[k] = v
	return out
}

func copyLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func sortedKeys(m map[string]*metricFamily) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedHistKeys(m map[string]*histogramFamily) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSeriesKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
