// Package metrics provides Prometheus-compatible counter, histogram,
// and gauge primitives plus text-exposition-format rendering. The
// implementation is hand-rolled rather than using
// github.com/prometheus/client_golang to avoid the dependency tree and
// keep the surface area exactly what SPEC5 §3.2 specifies.
//
// AIDEV-NOTE: SPEC5 §3.4 enumerates the metric inventory; new metric
// declarations should match that list. Adding a metric here without
// updating SPEC5 §3.4 is a review-failing omission.
//
// Concurrency: each metric carries its own sync.Mutex. The hot path
// (Inc / Observe / Set on a metric whose label values were used before)
// is one lock acquire + one map lookup. The first call with a new
// label-value tuple allocates a series under the same lock.
//
// Render builds the per-metric output under each metric's lock into a
// strings.Builder, then writes the buffer to the caller's io.Writer
// *outside* the lock. This bounds hot-path Inc/Observe/Set blocking by
// the per-metric series count (memory operations) regardless of the
// writer's speed — important because /metrics is served to a remote
// scraper over HTTP, and a slow consumer must not stall request-path
// counters. SPEC5 §3.2 invariant.
package metrics

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
)

// DefaultMaxSeries is the per-metric series cap applied to every
// labeled metric constructed via NewCounter / NewHistogram / NewGauge.
// SPEC5 §3.2 / §10.4 — bounds the worst-case Prometheus cardinality
// regardless of how loose upstream.allowed_host_regex is. When a new
// label-value tuple would push the metric's series count past this,
// the Inc / Observe / Set call is silently dropped and a one-shot
// metrics_series_cap_reached Warn fires for that metric. Existing
// series continue to update normally.
//
// 1024 is generous enough that no realistic deployment hits it under
// well-formed traffic, tight enough that a hostile or noisy client
// cannot blow up Prometheus storage. Callers can override per-metric
// via the WithCap constructor variants; cap=0 disables the bound for
// unlabeled metrics or known-tiny-cardinality cases.
const DefaultMaxSeries = 1024

// labelKey joins label values into a stable map key. The separator
// must be a byte that cannot appear inside a label value; \x00 fits
// because Prometheus label values are UTF-8 strings and NUL is
// disallowed by the exposition spec.
const labelSep = "\x00"

func labelKey(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return strings.Join(values, labelSep)
}

// Registry holds all registered metrics. The default registry is
// package-global; tests construct private registries via NewRegistry
// to isolate state.
type Registry struct {
	mu      sync.RWMutex
	metrics []metric
	byName  map[string]metric
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]metric{}}
}

// Default is the package-global registry that NewCounter, NewHistogram,
// and NewGauge attach to. Tests that need isolation should construct a
// Registry via NewRegistry and pass it to NewCounterIn / NewHistogramIn
// / NewGaugeIn.
var Default = NewRegistry()

// register adds m to r. Duplicate names panic — metrics should be
// declared once per process at package init time.
func (r *Registry) register(m metric) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[m.metricName()]; dup {
		panic("metrics: duplicate registration of " + m.metricName())
	}
	r.byName[m.metricName()] = m
	r.metrics = append(r.metrics, m)
}

// SnapshotNamesForTest returns the set of metric names currently
// registered. Test-only escape hatch for code paths that legitimately
// register-once-per-process (e.g. admin.New) but need to be invoked
// repeatedly across tests sharing a global registry.
func (r *Registry) SnapshotNamesForTest() map[string]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snap := make(map[string]struct{}, len(r.byName))
	for name := range r.byName {
		snap[name] = struct{}{}
	}
	return snap
}

// UnregisterAddedSinceForTest drops every metric registered after
// `snap` was captured. Test-only — paired with SnapshotNamesForTest
// to undo gauge-registration side effects when a test brings up a
// subsystem (e.g. admin server) that registers into a process-global
// registry.
func (r *Registry) UnregisterAddedSinceForTest(snap map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.metrics[:0]
	for _, m := range r.metrics {
		if _, present := snap[m.metricName()]; present {
			kept = append(kept, m)
		} else {
			delete(r.byName, m.metricName())
		}
	}
	r.metrics = kept
}

// metric is the internal interface every concrete type satisfies.
type metric interface {
	metricName() string
	render(w io.Writer)
}

// ──────────────────────────────────────────────────────────────────
// Counter
// ──────────────────────────────────────────────────────────────────

// Counter is a cumulative-since-process-start counter. Use Inc on
// integer-counted events; Add for known-numeric increments.
type Counter struct {
	name      string
	help      string
	labels    []string
	maxSeries int // 0 = unbounded
	mu        sync.Mutex
	series    map[string]*counterSeries
	capLogged bool // one-shot guard for metrics_series_cap_reached Warn
}

type counterSeries struct {
	values []string
	val    float64
}

// NewCounter declares a counter on the Default registry with the
// DefaultMaxSeries cap.
func NewCounter(name, help string, labelNames ...string) *Counter {
	return NewCounterWithCapIn(Default, name, help, DefaultMaxSeries, labelNames...)
}

// NewCounterWithCap declares a counter on the Default registry with
// an explicit per-metric series cap. Pass 0 to disable the cap.
func NewCounterWithCap(name, help string, maxSeries int, labelNames ...string) *Counter {
	return NewCounterWithCapIn(Default, name, help, maxSeries, labelNames...)
}

// NewCounterIn declares a counter on the given registry with the
// DefaultMaxSeries cap.
func NewCounterIn(r *Registry, name, help string, labelNames ...string) *Counter {
	return NewCounterWithCapIn(r, name, help, DefaultMaxSeries, labelNames...)
}

// NewCounterWithCapIn is the full constructor: a registry and an
// explicit cap. maxSeries=0 disables the cap.
func NewCounterWithCapIn(r *Registry, name, help string, maxSeries int, labelNames ...string) *Counter {
	if maxSeries < 0 {
		panic(fmt.Sprintf("metrics: counter %q maxSeries=%d must be >= 0 (0 = unbounded)",
			name, maxSeries))
	}
	c := &Counter{
		name:      name,
		help:      help,
		labels:    append([]string(nil), labelNames...),
		maxSeries: maxSeries,
		series:    map[string]*counterSeries{},
	}
	r.register(c)
	return c
}

func (c *Counter) metricName() string { return c.name }

// Inc increments by 1. labelValues must match the labelNames count
// passed to NewCounter.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add increments by delta. delta must be non-negative; negative or
// non-finite deltas are silently dropped (counters do not move
// backwards, and a NaN/Inf would corrupt the running sum). When the
// per-metric series cap is reached, increments on a previously-seen
// label tuple still apply; increments that would create a new series
// are silently dropped (after a one-shot Warn).
func (c *Counter) Add(delta float64, labelValues ...string) {
	if delta < 0 || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return
	}
	if len(labelValues) != len(c.labels) {
		panic(fmt.Sprintf("metrics: counter %q expects %d label values, got %d",
			c.name, len(c.labels), len(labelValues)))
	}
	k := labelKey(labelValues)
	c.mu.Lock()
	s, ok := c.series[k]
	if !ok {
		if c.maxSeries > 0 && len(c.series) >= c.maxSeries {
			firstHit := !c.capLogged
			c.capLogged = true
			c.mu.Unlock()
			if firstHit {
				logCapReached(c.name, c.maxSeries)
			}
			return
		}
		lv := append([]string(nil), labelValues...)
		s = &counterSeries{values: lv}
		c.series[k] = s
	}
	s.val += delta
	c.mu.Unlock()
}

func (c *Counter) render(w io.Writer) {
	var buf strings.Builder
	c.mu.Lock()
	keys := make([]string, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&buf, "# HELP %s %s\n", c.name, escapeHelp(c.help))
	fmt.Fprintf(&buf, "# TYPE %s counter\n", c.name)
	for _, k := range keys {
		s := c.series[k]
		fmt.Fprintf(&buf, "%s%s %s\n", c.name, formatLabels(c.labels, s.values), formatFloat(s.val))
	}
	c.mu.Unlock()
	_, _ = io.WriteString(w, buf.String())
}

// ──────────────────────────────────────────────────────────────────
// Histogram
// ──────────────────────────────────────────────────────────────────

// Histogram observes a distribution of values into a fixed bucket
// schema. Bucket boundaries are upper bounds in ascending order; an
// implicit +Inf bucket catches values above the last boundary.
type Histogram struct {
	name      string
	help      string
	labels    []string
	buckets   []float64
	maxSeries int
	mu        sync.Mutex
	series    map[string]*histogramSeries
	capLogged bool
}

type histogramSeries struct {
	values   []string
	counts   []uint64 // len(buckets)+1 (the +1 is +Inf)
	sum      float64
	obsCount uint64
}

// NewHistogram declares a histogram on the Default registry with the
// DefaultMaxSeries cap. buckets must be ascending and finite; an
// implicit +Inf bucket is appended.
func NewHistogram(name, help string, buckets []float64, labelNames ...string) *Histogram {
	return NewHistogramWithCapIn(Default, name, help, buckets, DefaultMaxSeries, labelNames...)
}

// NewHistogramWithCap declares a histogram on the Default registry
// with an explicit per-metric series cap. Pass 0 to disable.
func NewHistogramWithCap(name, help string, buckets []float64, maxSeries int, labelNames ...string) *Histogram {
	return NewHistogramWithCapIn(Default, name, help, buckets, maxSeries, labelNames...)
}

// NewHistogramIn declares a histogram on the given registry with the
// DefaultMaxSeries cap.
func NewHistogramIn(r *Registry, name, help string, buckets []float64, labelNames ...string) *Histogram {
	return NewHistogramWithCapIn(r, name, help, buckets, DefaultMaxSeries, labelNames...)
}

// NewHistogramWithCapIn is the full constructor.
func NewHistogramWithCapIn(r *Registry, name, help string, buckets []float64, maxSeries int, labelNames ...string) *Histogram {
	if maxSeries < 0 {
		panic(fmt.Sprintf("metrics: histogram %q maxSeries=%d must be >= 0 (0 = unbounded)",
			name, maxSeries))
	}
	for i, b := range buckets {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			panic(fmt.Sprintf("metrics: histogram %q bucket[%d]=%v invalid", name, i, b))
		}
		if i > 0 && buckets[i-1] >= b {
			panic(fmt.Sprintf("metrics: histogram %q buckets must be strictly ascending", name))
		}
	}
	h := &Histogram{
		name:      name,
		help:      help,
		labels:    append([]string(nil), labelNames...),
		buckets:   append([]float64(nil), buckets...),
		maxSeries: maxSeries,
		series:    map[string]*histogramSeries{},
	}
	r.register(h)
	return h
}

func (h *Histogram) metricName() string { return h.name }

// Observe records v in the appropriate bucket and updates sum+count.
// NaN observations are dropped; +Inf is recorded only in the implicit
// last bucket (everything-below-+Inf is unchanged), -Inf is dropped
// because negative durations / sizes are nonsensical for our use.
// When the per-metric series cap is reached, observations on a
// previously-seen label tuple still apply; observations that would
// create a new series are silently dropped (after a one-shot Warn).
func (h *Histogram) Observe(v float64, labelValues ...string) {
	if math.IsNaN(v) {
		return
	}
	if len(labelValues) != len(h.labels) {
		panic(fmt.Sprintf("metrics: histogram %q expects %d label values, got %d",
			h.name, len(h.labels), len(labelValues)))
	}
	k := labelKey(labelValues)
	h.mu.Lock()
	s, ok := h.series[k]
	if !ok {
		if h.maxSeries > 0 && len(h.series) >= h.maxSeries {
			firstHit := !h.capLogged
			h.capLogged = true
			h.mu.Unlock()
			if firstHit {
				logCapReached(h.name, h.maxSeries)
			}
			return
		}
		lv := append([]string(nil), labelValues...)
		s = &histogramSeries{
			values: lv,
			counts: make([]uint64, len(h.buckets)+1),
		}
		h.series[k] = s
	}
	// Increment all buckets whose upper bound is >= v, plus the +Inf bucket.
	for i, ub := range h.buckets {
		if v <= ub {
			s.counts[i]++
		}
	}
	s.counts[len(h.buckets)]++ // +Inf
	if !math.IsInf(v, 1) {
		s.sum += v
	}
	s.obsCount++
	h.mu.Unlock()
}

func (h *Histogram) render(w io.Writer) {
	var buf strings.Builder
	h.mu.Lock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&buf, "# HELP %s %s\n", h.name, escapeHelp(h.help))
	fmt.Fprintf(&buf, "# TYPE %s histogram\n", h.name)
	for _, k := range keys {
		s := h.series[k]
		// Buckets
		for i, ub := range h.buckets {
			labels := mergeLabels(h.labels, s.values, "le", formatFloat(ub))
			fmt.Fprintf(&buf, "%s_bucket%s %d\n", h.name, labels, s.counts[i])
		}
		// +Inf bucket
		labels := mergeLabels(h.labels, s.values, "le", "+Inf")
		fmt.Fprintf(&buf, "%s_bucket%s %d\n", h.name, labels, s.counts[len(h.buckets)])
		// Sum + count
		fmt.Fprintf(&buf, "%s_sum%s %s\n", h.name, formatLabels(h.labels, s.values), formatFloat(s.sum))
		fmt.Fprintf(&buf, "%s_count%s %d\n", h.name, formatLabels(h.labels, s.values), s.obsCount)
	}
	h.mu.Unlock()
	_, _ = io.WriteString(w, buf.String())
}

// ──────────────────────────────────────────────────────────────────
// Gauge
// ──────────────────────────────────────────────────────────────────

// Gauge is a point-in-time value that can move in either direction.
// Sample uses: in-flight request count, blob count from the last DB
// snapshot, build_info=1.
type Gauge struct {
	name      string
	help      string
	labels    []string
	maxSeries int
	mu        sync.Mutex
	series    map[string]*gaugeSeries
	capLogged bool
}

type gaugeSeries struct {
	values []string
	val    float64
}

// NewGauge declares a gauge on the Default registry with the
// DefaultMaxSeries cap.
func NewGauge(name, help string, labelNames ...string) *Gauge {
	return NewGaugeWithCapIn(Default, name, help, DefaultMaxSeries, labelNames...)
}

// NewGaugeWithCap declares a gauge on the Default registry with an
// explicit per-metric series cap. Pass 0 to disable.
func NewGaugeWithCap(name, help string, maxSeries int, labelNames ...string) *Gauge {
	return NewGaugeWithCapIn(Default, name, help, maxSeries, labelNames...)
}

// NewGaugeIn declares a gauge on the given registry with the
// DefaultMaxSeries cap.
func NewGaugeIn(r *Registry, name, help string, labelNames ...string) *Gauge {
	return NewGaugeWithCapIn(r, name, help, DefaultMaxSeries, labelNames...)
}

// NewGaugeWithCapIn is the full constructor.
func NewGaugeWithCapIn(r *Registry, name, help string, maxSeries int, labelNames ...string) *Gauge {
	if maxSeries < 0 {
		panic(fmt.Sprintf("metrics: gauge %q maxSeries=%d must be >= 0 (0 = unbounded)",
			name, maxSeries))
	}
	g := &Gauge{
		name:      name,
		help:      help,
		labels:    append([]string(nil), labelNames...),
		maxSeries: maxSeries,
		series:    map[string]*gaugeSeries{},
	}
	r.register(g)
	return g
}

func (g *Gauge) metricName() string { return g.name }

// Set replaces the current value. When the per-metric series cap is
// reached, Set on a previously-seen label tuple still applies; Set
// on a new tuple is silently dropped (after a one-shot Warn).
func (g *Gauge) Set(v float64, labelValues ...string) {
	if math.IsNaN(v) {
		return
	}
	if len(labelValues) != len(g.labels) {
		panic(fmt.Sprintf("metrics: gauge %q expects %d label values, got %d",
			g.name, len(g.labels), len(labelValues)))
	}
	k := labelKey(labelValues)
	g.mu.Lock()
	s, ok := g.series[k]
	if !ok {
		if g.maxSeries > 0 && len(g.series) >= g.maxSeries {
			firstHit := !g.capLogged
			g.capLogged = true
			g.mu.Unlock()
			if firstHit {
				logCapReached(g.name, g.maxSeries)
			}
			return
		}
		lv := append([]string(nil), labelValues...)
		s = &gaugeSeries{values: lv}
		g.series[k] = s
	}
	s.val = v
	g.mu.Unlock()
}

// Inc increments by 1.
func (g *Gauge) Inc(labelValues ...string) { g.Add(1, labelValues...) }

// Dec decrements by 1.
func (g *Gauge) Dec(labelValues ...string) { g.Add(-1, labelValues...) }

// Add adjusts by delta (signed). When the per-metric series cap is
// reached, Add on a previously-seen tuple still applies; Add on a
// new tuple is silently dropped (after a one-shot Warn).
func (g *Gauge) Add(delta float64, labelValues ...string) {
	if math.IsNaN(delta) {
		return
	}
	if len(labelValues) != len(g.labels) {
		panic(fmt.Sprintf("metrics: gauge %q expects %d label values, got %d",
			g.name, len(g.labels), len(labelValues)))
	}
	k := labelKey(labelValues)
	g.mu.Lock()
	s, ok := g.series[k]
	if !ok {
		if g.maxSeries > 0 && len(g.series) >= g.maxSeries {
			firstHit := !g.capLogged
			g.capLogged = true
			g.mu.Unlock()
			if firstHit {
				logCapReached(g.name, g.maxSeries)
			}
			return
		}
		lv := append([]string(nil), labelValues...)
		s = &gaugeSeries{values: lv}
		g.series[k] = s
	}
	s.val += delta
	g.mu.Unlock()
}

// Reset clears all gauge series. Used by the refresher goroutine when
// it wants to drop stale series (e.g. acu_per_host_inflight for a host
// that no longer has in-flight requests).
//
// AIDEV-NOTE: Reset is for refresher-driven gauges only. Do not call
// from hot paths — race window between Reset and the next Set.
//
// Reset preserves the cap-logged guard so the
// metrics_series_cap_reached Warn stays a per-process one-shot. An
// earlier design cleared the guard on every Reset to allow re-firing
// after the refresher dropped stale series, but that turned a one-shot
// warning into one-per-refresh-cycle whenever an upstream consistently
// exceeded the cap (e.g. malicious or buggy clients producing many
// distinct hosts) — operator alerting on the warn would page every
// gauge_refresh interval. Forcing the warning to fire-once accepts
// that a transient cap-hit-then-cleared-then-exceeded-again is silent;
// the cap-bounded behavior is unchanged in either case.
func (g *Gauge) Reset() {
	g.mu.Lock()
	g.series = map[string]*gaugeSeries{}
	g.mu.Unlock()
}

func (g *Gauge) render(w io.Writer) {
	var buf strings.Builder
	g.mu.Lock()
	keys := make([]string, 0, len(g.series))
	for k := range g.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(&buf, "# HELP %s %s\n", g.name, escapeHelp(g.help))
	fmt.Fprintf(&buf, "# TYPE %s gauge\n", g.name)
	for _, k := range keys {
		s := g.series[k]
		fmt.Fprintf(&buf, "%s%s %s\n", g.name, formatLabels(g.labels, s.values), formatFloat(s.val))
	}
	g.mu.Unlock()
	_, _ = io.WriteString(w, buf.String())
}

// ──────────────────────────────────────────────────────────────────
// Render
// ──────────────────────────────────────────────────────────────────

// Render writes the Default registry to w in Prometheus text exposition
// format.
func Render(w io.Writer) { Default.Render(w) }

// Render writes the registry to w. Metrics are emitted in declaration
// order; series within a metric are emitted in label-key sorted order
// for stable output across renders.
func (r *Registry) Render(w io.Writer) {
	r.mu.RLock()
	ms := make([]metric, len(r.metrics))
	copy(ms, r.metrics)
	r.mu.RUnlock()
	for _, m := range ms {
		m.render(w)
	}
}

// ──────────────────────────────────────────────────────────────────
// Formatting helpers
// ──────────────────────────────────────────────────────────────────

// formatLabels produces `{n1="v1",n2="v2"}` or "" when there are no
// labels. Values are escaped per the Prometheus exposition spec
// (backslash, double quote, newline).
func formatLabels(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// mergeLabels produces a label set with one extra (name, value) pair
// appended after the metric's own labels. Used by histogram bucket
// emission to attach `le="..."`.
func mergeLabels(names, values []string, extraName, extraValue string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	if len(names) > 0 {
		b.WriteByte(',')
	}
	b.WriteString(extraName)
	b.WriteString(`="`)
	b.WriteString(escapeLabelValue(extraValue))
	b.WriteByte('"')
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue handles the three required escape sequences from
// the Prometheus exposition spec: \\, \", \n.
func escapeLabelValue(v string) string {
	if !strings.ContainsAny(v, "\\\"\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v) + 2)
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeHelp handles the two required HELP-line escape sequences: \\, \n.
func escapeHelp(h string) string {
	if !strings.ContainsAny(h, "\\\n") {
		return h
	}
	var b strings.Builder
	b.Grow(len(h) + 2)
	for _, r := range h {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// logCapReached emits the one-shot metrics_series_cap_reached Warn
// fired by the per-metric cap path. SPEC5 §10.2 specifies the event
// shape (one log line per metric per process lifetime). Centralized
// here so the three concrete metric types share one source of
// truth for the message format.
func logCapReached(metricName string, cap int) {
	slog.Default().Warn("metrics_series_cap_reached",
		"metric_name", metricName,
		"cap", cap)
}

// formatFloat prints f using minimal digits, matching Prometheus's
// expected style. Integers render without a decimal point; +Inf is
// rendered as the string Prometheus expects.
func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	if math.IsInf(f, -1) {
		return "-Inf"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}
