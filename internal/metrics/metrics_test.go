package metrics

import (
	"bytes"
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
