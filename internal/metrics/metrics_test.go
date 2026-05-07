package metrics

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fresh registry helper — every test uses an isolated registry so
// metric registrations across tests don't collide on the package
// Default.
func freshReg(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry()
}

func TestCounter_IncAndAdd(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "test_total", "test counter", "outcome")

	c.Inc("hit")
	c.Inc("hit")
	c.Inc("miss")
	c.Add(5, "hit")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	wants := []string{
		"# HELP test_total test counter\n",
		"# TYPE test_total counter\n",
		`test_total{outcome="hit"} 7` + "\n",
		`test_total{outcome="miss"} 1` + "\n",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\nfull output:\n%s", w, got)
		}
	}
}

func TestCounter_NegativeDeltaDropped(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "test_total", "test counter")

	c.Add(5)
	c.Add(-3) // dropped
	c.Add(2)

	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "test_total 7\n") {
		t.Errorf("expected counter at 7 (negative delta dropped), got:\n%s", buf.String())
	}
}

func TestCounter_LabelArityPanic(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "test_total", "test counter", "outcome")

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on label-arity mismatch")
		}
	}()
	c.Inc("too", "many")
}

func TestHistogram_BucketCounts(t *testing.T) {
	r := freshReg(t)
	h := NewHistogramIn(r, "test_seconds", "test histogram",
		[]float64{0.1, 1.0, 10.0}, "outcome")

	// Observations: 0.05 → buckets {0.1, 1.0, 10.0, +Inf}
	//               0.5  → buckets {1.0, 10.0, +Inf}
	//               5.0  → buckets {10.0, +Inf}
	//               100  → buckets {+Inf}
	h.Observe(0.05, "hit")
	h.Observe(0.5, "hit")
	h.Observe(5.0, "hit")
	h.Observe(100, "hit")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	wants := []string{
		`test_seconds_bucket{outcome="hit",le="0.1"} 1`,
		`test_seconds_bucket{outcome="hit",le="1"} 2`,
		`test_seconds_bucket{outcome="hit",le="10"} 3`,
		`test_seconds_bucket{outcome="hit",le="+Inf"} 4`,
		`test_seconds_count{outcome="hit"} 4`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q\nfull output:\n%s", w, got)
		}
	}

	// Sum check (0.05 + 0.5 + 5.0 + 100 = 105.55)
	if !strings.Contains(got, `test_seconds_sum{outcome="hit"} 105.55`) {
		t.Errorf("expected sum=105.55, got:\n%s", got)
	}
}

func TestHistogram_NaNDropped(t *testing.T) {
	r := freshReg(t)
	h := NewHistogramIn(r, "test_seconds", "h",
		[]float64{1.0})

	h.Observe(0.5)
	h.Observe(nanValue())
	h.Observe(0.7)

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()
	if !strings.Contains(got, "test_seconds_count 2\n") {
		t.Errorf("expected count=2 (NaN dropped), got:\n%s", got)
	}
}

func TestHistogram_AscendingBucketsRequired(t *testing.T) {
	r := freshReg(t)
	defer func() {
		if recover() == nil {
			t.Error("expected panic on non-ascending buckets")
		}
	}()
	NewHistogramIn(r, "test_seconds", "h", []float64{1.0, 0.5})
}

func TestGauge_SetIncDec(t *testing.T) {
	r := freshReg(t)
	g := NewGaugeIn(r, "test_value", "test gauge")

	g.Set(10)
	g.Inc()
	g.Inc()
	g.Dec()
	g.Add(0.5)

	var buf bytes.Buffer
	r.Render(&buf)
	if !strings.Contains(buf.String(), "test_value 11.5\n") {
		t.Errorf("expected gauge at 11.5, got:\n%s", buf.String())
	}
}

func TestGauge_Reset(t *testing.T) {
	r := freshReg(t)
	g := NewGaugeIn(r, "test_value", "g", "host")

	g.Set(5, "host1")
	g.Set(7, "host2")
	g.Reset()
	g.Set(9, "host3")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()
	if strings.Contains(got, "host1") || strings.Contains(got, "host2") {
		t.Errorf("Reset should drop prior series; got:\n%s", got)
	}
	if !strings.Contains(got, `test_value{host="host3"} 9`) {
		t.Errorf("expected host3=9 after reset, got:\n%s", got)
	}
}

func TestRegistry_DuplicateRegistrationPanics(t *testing.T) {
	r := freshReg(t)
	NewCounterIn(r, "dup_total", "first")
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate name")
		}
	}()
	NewCounterIn(r, "dup_total", "second")
}

func TestEscaping_LabelValueWithSpecialChars(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "test_total", "h", "url")
	c.Inc(`a"b\c` + "\n" + "d")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()
	want := `test_total{url="a\"b\\c\nd"} 1`
	if !strings.Contains(got, want) {
		t.Errorf("expected escaping %q in output:\n%s", want, got)
	}
}

func TestRender_StableSeriesOrder(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "test_total", "h", "outcome")

	// Insert in non-alphabetical order
	c.Inc("zulu")
	c.Inc("alpha")
	c.Inc("mike")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	// Series should render alpha → mike → zulu
	iAlpha := strings.Index(got, `outcome="alpha"`)
	iMike := strings.Index(got, `outcome="mike"`)
	iZulu := strings.Index(got, `outcome="zulu"`)
	if !(iAlpha < iMike && iMike < iZulu) {
		t.Errorf("series not in sorted order: alpha=%d mike=%d zulu=%d\n%s",
			iAlpha, iMike, iZulu, got)
	}
}

// TestCounter_SeriesCap exercises SPEC5 §3.2 / §10.4 — at the cap,
// new label tuples are silently dropped while existing series keep
// updating.
func TestCounter_SeriesCap(t *testing.T) {
	r := freshReg(t)
	c := NewCounterWithCapIn(r, "test_total", "h", 3, "outcome")

	// Fill the cap with three distinct outcomes.
	c.Inc("a")
	c.Inc("b")
	c.Inc("c")

	// A 4th distinct outcome should be dropped.
	c.Inc("d")
	c.Inc("d") // and again, still dropped.

	// Existing series keep working.
	c.Inc("a")
	c.Add(5, "b")

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	if !strings.Contains(got, `test_total{outcome="a"} 2`) {
		t.Errorf("a-series should be 2 (one initial + one post-cap), got:\n%s", got)
	}
	if !strings.Contains(got, `test_total{outcome="b"} 6`) {
		t.Errorf("b-series should be 6 (one initial + 5 post-cap), got:\n%s", got)
	}
	if !strings.Contains(got, `test_total{outcome="c"} 1`) {
		t.Errorf("c-series should be 1, got:\n%s", got)
	}
	if strings.Contains(got, `test_total{outcome="d"}`) {
		t.Errorf("d-series should not exist (cap reached); got:\n%s", got)
	}
}

// TestHistogram_SeriesCap mirrors the Counter test for histograms.
func TestHistogram_SeriesCap(t *testing.T) {
	r := freshReg(t)
	h := NewHistogramWithCapIn(r, "test_seconds", "h",
		[]float64{1.0}, 2, "host")

	h.Observe(0.5, "a")
	h.Observe(0.5, "b")
	h.Observe(0.5, "c") // dropped
	h.Observe(0.5, "a") // existing series keeps updating

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	if !strings.Contains(got, `test_seconds_count{host="a"} 2`) {
		t.Errorf("a should have 2 observations, got:\n%s", got)
	}
	if !strings.Contains(got, `test_seconds_count{host="b"} 1`) {
		t.Errorf("b should have 1 observation, got:\n%s", got)
	}
	if strings.Contains(got, `test_seconds_count{host="c"}`) {
		t.Errorf("c should not exist (cap reached); got:\n%s", got)
	}
}

// TestGauge_SeriesCap mirrors the Counter test for gauges.
func TestGauge_SeriesCap(t *testing.T) {
	r := freshReg(t)
	g := NewGaugeWithCapIn(r, "test_value", "h", 2, "host")

	g.Set(1, "a")
	g.Set(2, "b")
	g.Set(3, "c")  // dropped
	g.Set(99, "a") // existing series keeps updating

	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()

	if !strings.Contains(got, `test_value{host="a"} 99`) {
		t.Errorf("a should be 99, got:\n%s", got)
	}
	if !strings.Contains(got, `test_value{host="b"} 2`) {
		t.Errorf("b should be 2, got:\n%s", got)
	}
	if strings.Contains(got, `test_value{host="c"}`) {
		t.Errorf("c should not exist (cap reached); got:\n%s", got)
	}
}

// TestGauge_ResetClearsCapLog verifies that Reset re-enables the
// one-shot cap-reached log so a refresher-driven gauge can flag a
// fresh cap event after the refresher dropped its old series.
// TestGauge_ResetSwapsSeriesAndKeepsCap verifies that Reset clears
// the prior series map and that the cap is still enforced against
// the new series set. (The cap-warning's once-per-process semantic
// is locked in by TestGauge_CapWarningStaysOneShotAcrossResets.)
func TestGauge_ResetSwapsSeriesAndKeepsCap(t *testing.T) {
	r := freshReg(t)
	g := NewGaugeWithCapIn(r, "test_value", "h", 1, "host")

	g.Set(1, "a")
	g.Set(2, "b") // dropped, cap reached
	g.Reset()

	g.Set(3, "c")
	g.Set(4, "d") // dropped — cap still enforced after Reset
	var buf bytes.Buffer
	r.Render(&buf)
	got := buf.String()
	if !strings.Contains(got, `test_value{host="c"} 3`) {
		t.Errorf("c should be 3 after Reset, got:\n%s", got)
	}
	if strings.Contains(got, `test_value{host="a"}`) ||
		strings.Contains(got, `test_value{host="b"}`) ||
		strings.Contains(got, `test_value{host="d"}`) {
		t.Errorf("only c should remain, got:\n%s", got)
	}
}

// TestGauge_CapWarningStaysOneShotAcrossResets confirms that
// Reset preserves the cap-logged guard so an upstream that
// continuously exceeds the cap does not spam metrics_series_cap_reached
// every refresh tick. SPEC5 §10.2: the warn is once-per-process.
func TestGauge_CapWarningStaysOneShotAcrossResets(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := freshReg(t)
	g := NewGaugeWithCapIn(r, "test_value", "h", 1, "host")

	g.Set(1, "a")
	g.Set(2, "b") // dropped → first cap-warn fires
	g.Reset()

	g.Set(3, "c")
	g.Set(4, "d") // dropped → second cap-warn must NOT fire after Reset

	got := buf.String()
	count := strings.Count(got, "metrics_series_cap_reached")
	if count != 1 {
		t.Errorf("metrics_series_cap_reached fired %d times, want exactly 1; log:\n%s",
			count, got)
	}
}

// TestNegativeMaxSeriesPanics verifies the guardrail: a negative cap
// is a programming error and must not silently degrade.
func TestNegativeMaxSeriesPanics(t *testing.T) {
	r := freshReg(t)
	defer func() {
		if recover() == nil {
			t.Error("expected panic on negative maxSeries")
		}
	}()
	NewCounterWithCapIn(r, "bad", "h", -1)
}

// TestCounter_UnboundedCap (cap=0) keeps the legacy behavior — every
// distinct tuple gets its own series.
func TestCounter_UnboundedCap(t *testing.T) {
	r := freshReg(t)
	c := NewCounterWithCapIn(r, "test_total", "h", 0, "outcome")

	for i := 0; i < 100; i++ {
		c.Inc(itoa(int64(i)))
	}
	var buf bytes.Buffer
	r.Render(&buf)
	if strings.Count(buf.String(), "test_total{outcome=") != 100 {
		t.Errorf("unbounded counter should have 100 series; output:\n%s",
			buf.String())
	}
}

func TestConcurrent_IncAndRender(t *testing.T) {
	r := freshReg(t)
	c := NewCounterIn(r, "concurrent_total", "h")
	const goroutines = 50
	const incs = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incs; j++ {
				c.Inc()
				if j%100 == 0 {
					var buf bytes.Buffer
					r.Render(&buf) // exercises RLock/Lock interleaving
				}
			}
		}()
	}
	wg.Wait()

	var buf bytes.Buffer
	r.Render(&buf)
	want := goroutines * incs
	wantLine := "concurrent_total " + itoa(int64(want))
	if !strings.Contains(buf.String(), wantLine) {
		t.Errorf("expected total %d, got:\n%s", want, buf.String())
	}
}

// nanValue returns a NaN float64 (math.NaN() is the canonical source;
// kept tiny here to avoid the import in the test file otherwise).
func nanValue() float64 {
	var z float64
	return z / z
}

// itoa avoids strconv import in the one place we'd use it.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
