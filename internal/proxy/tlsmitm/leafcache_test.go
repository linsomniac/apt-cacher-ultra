package tlsmitm

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCert builds a minimal *tls.Certificate whose Leaf has the given
// host as Subject.CommonName and a controllable NotAfter. The cert is
// NOT cryptographically valid — these tests exercise the cache, not
// signing. Cache code only inspects Leaf.NotAfter and the public
// fields.
func fakeCert(host string, notAfter time.Time) *tls.Certificate {
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotAfter:     notAfter,
		Raw:          []byte(host), // arbitrary non-nil bytes
	}
	return &tls.Certificate{
		Certificate: [][]byte{[]byte(host)},
		Leaf:        leaf,
	}
}

func TestCache_NewCache_RejectsBadInputs(t *testing.T) {
	if _, err := NewCache(0, func(string) (*tls.Certificate, error) { return nil, nil }); err == nil {
		t.Error("capacity 0 should be rejected")
	}
	if _, err := NewCache(-1, func(string) (*tls.Certificate, error) { return nil, nil }); err == nil {
		t.Error("negative capacity should be rejected")
	}
	if _, err := NewCache(8, nil); err == nil {
		t.Error("nil GenFunc should be rejected")
	}
}

func TestCache_HitMissBasic(t *testing.T) {
	calls := 0
	gen := func(host string) (*tls.Certificate, error) {
		calls++
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, err := NewCache(8, gen)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	cert1, err := c.Get("foo.example.com")
	if err != nil {
		t.Fatalf("Get foo: %v", err)
	}
	cert2, err := c.Get("foo.example.com")
	if err != nil {
		t.Fatalf("Get foo (hit): %v", err)
	}
	if cert1 != cert2 {
		t.Error("hit should return the same *tls.Certificate")
	}
	if calls != 1 {
		t.Errorf("gen called %d times, want 1", calls)
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
	if c.Capacity() != 8 {
		t.Errorf("capacity = %d, want 8", c.Capacity())
	}
}

func TestCache_LRUEvictionOrder(t *testing.T) {
	gen := func(host string) (*tls.Certificate, error) {
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(3, gen)

	type evict struct {
		host   string
		reason EvictReason
	}
	var evicted []evict
	c.SetOnEvict(func(host string, reason EvictReason, _ float64) {
		evicted = append(evicted, evict{host, reason})
	})

	for _, h := range []string{"a", "b", "c"} {
		if _, err := c.Get(h); err != nil {
			t.Fatalf("Get %s: %v", h, err)
		}
	}
	// LRU order now (LRU → MRU): a, b, c.
	// Touch "a" so it becomes MRU: order becomes b, c, a.
	if _, err := c.Get("a"); err != nil {
		t.Fatal(err)
	}
	// Insert "d" — should evict "b" (now LRU).
	if _, err := c.Get("d"); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0].host != "b" || evicted[0].reason != EvictReasonLRU {
		t.Errorf("evicted: got %+v, want [{b lru}]", evicted)
	}
	if c.Size() != 3 {
		t.Errorf("size = %d, want 3", c.Size())
	}
}

func TestCache_ExpiredEntryRefreshedOnLookup(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	calls := 0
	gen := func(host string) (*tls.Certificate, error) {
		calls++
		// Each generation produces a cert valid for 1h from "now".
		return fakeCert(host, clock().Add(time.Hour)), nil
	}
	c, _ := NewCache(8, gen)
	c.SetClockForTest(clock)

	cert1, _ := c.Get("foo")
	if calls != 1 {
		t.Fatalf("first call: gen invoked %d times, want 1", calls)
	}
	// Advance the clock past the cert's NotAfter.
	now = now.Add(2 * time.Hour)
	cert2, _ := c.Get("foo")
	if calls != 2 {
		t.Errorf("after expiry: gen invoked %d times, want 2", calls)
	}
	if cert1 == cert2 {
		t.Error("expired cert should have been replaced")
	}
}

func TestCache_Singleflight_OneGenerationForConcurrentCallers(t *testing.T) {
	const N = 100
	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	gate := make(chan struct{})

	gen := func(host string) (*tls.Certificate, error) {
		cur := inFlight.Add(1)
		// Track the peak concurrency observed inside gen.
		for {
			peak := maxConcurrent.Load()
			if cur <= peak {
				break
			}
			if maxConcurrent.CompareAndSwap(peak, cur) {
				break
			}
		}
		<-gate
		inFlight.Add(-1)
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(8, gen)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.Get("foo"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}()
	}
	// Give callers time to pile up on the singleflight, then release gen.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	if maxConcurrent.Load() != 1 {
		t.Errorf("singleflight let %d concurrent gens through, want 1", maxConcurrent.Load())
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
}

func TestCache_Singleflight_FailureNotCached(t *testing.T) {
	calls := 0
	var failNext = true
	gen := func(host string) (*tls.Certificate, error) {
		calls++
		if failNext {
			return nil, errors.New("synthetic gen failure")
		}
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(8, gen)

	if _, err := c.Get("foo"); err == nil {
		t.Fatal("first call: expected error")
	}
	if calls != 1 {
		t.Errorf("calls after first: %d, want 1", calls)
	}
	if c.Size() != 0 {
		t.Errorf("failure was cached: size = %d", c.Size())
	}

	// Second call should retry — no failure caching.
	failNext = false
	cert, err := c.Get("foo")
	if err != nil {
		t.Fatalf("retry call: %v", err)
	}
	if cert == nil {
		t.Fatal("retry call returned nil cert")
	}
	if calls != 2 {
		t.Errorf("calls after retry: %d, want 2", calls)
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
}

func TestCache_DistinctHostsGenerateConcurrently(t *testing.T) {
	const N = 16
	var peak atomic.Int32
	var inFlight atomic.Int32
	gate := make(chan struct{})
	gen := func(host string) (*tls.Certificate, error) {
		cur := inFlight.Add(1)
		for {
			p := peak.Load()
			if cur <= p {
				break
			}
			if peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-gate
		inFlight.Add(-1)
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(N, gen)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			host := fmt.Sprintf("host%d.example.com", i)
			if _, err := c.Get(host); err != nil {
				t.Errorf("Get %s: %v", host, err)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	if peak.Load() < 2 {
		t.Errorf("distinct-host gens should overlap (peak=%d)", peak.Load())
	}
	if c.Size() != N {
		t.Errorf("size = %d, want %d", c.Size(), N)
	}
}

func TestCache_EmptyHostRejected(t *testing.T) {
	gen := func(host string) (*tls.Certificate, error) {
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(8, gen)
	if _, err := c.Get(""); err == nil {
		t.Error("empty host should be rejected")
	}
}

// TestCache_Singleflight_SurvivesPanicInGen exercises the panic-safety
// contract on sfGroup.Do: a panicking gen must (a) surface a typed
// error to every waiter, (b) allow the next call to lead a fresh
// execution. Without panic recovery, every future Get for the panicked
// host would block on wg.Wait() forever.
func TestCache_Singleflight_SurvivesPanicInGen(t *testing.T) {
	const N = 16
	var (
		shouldPanic atomic.Bool
		called      atomic.Int32
	)
	shouldPanic.Store(true)
	gen := func(host string) (*tls.Certificate, error) {
		called.Add(1)
		if shouldPanic.Load() {
			panic("synthetic gen panic")
		}
		return fakeCert(host, time.Now().Add(time.Hour)), nil
	}
	c, _ := NewCache(8, gen)

	// Pile up N concurrent waiters on the same host so we exercise both
	// the leader path (which panics) and the wg.Wait waiter path (which
	// must wake up rather than hang).
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := c.Get("foo.example.com")
			errs <- err
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("singleflight wedged waiting on a panicking gen")
	}
	close(errs)

	for err := range errs {
		if err == nil {
			t.Error("expected error from panicked gen, got nil")
			continue
		}
		if !strings.Contains(err.Error(), "panicked") {
			t.Errorf("unexpected error: %v", err)
		}
	}

	// Subsequent Get with a non-panicking gen must succeed — the
	// in-flight entry was deleted; failure was not cached.
	shouldPanic.Store(false)
	cert, err := c.Get("foo.example.com")
	if err != nil {
		t.Fatalf("retry after panic: %v", err)
	}
	if cert == nil {
		t.Fatal("retry returned nil cert")
	}
	if c.Size() != 1 {
		t.Errorf("size = %d, want 1", c.Size())
	}
}
