// SPEC6 §10.4 status-page support.
//
// Tracks two cheap rolling signals the §10.4 status page consumes:
//
//   - The 60s cert-cache hit rate (60 one-second buckets, indexed by
//     monotonic uptime second). RecordCertCacheLookup bumps the
//     bucket synchronously on every leaf-cert Get.
//   - The last cert issued — host literal + timestamp. NoteCertIssued
//     is called from main.go's wrapped GenFunc on every successful
//     leaf generation.
//
// Both pieces of state are package globals because they are
// strictly observational and there is exactly one CONNECT pipeline
// per process. Bucketing follows the same monotonic-uptime pattern
// as ConnectStats so an NTP jump cannot corrupt the window.
package proxy

import (
	"sync"
	"time"
)

type lookupBucket struct {
	second int64 // uptime second this bucket represents; -1 = empty
	hits   int
	misses int
}

// CertHitRate is a 60-bucket sliding-window hit/miss counter for the
// MITM leaf-cert cache. Bucket index = uptime_second mod 60.
type CertHitRate struct {
	mu      sync.Mutex
	buckets [60]lookupBucket
	start   time.Time
	nowFn   func() time.Time
}

// NewCertHitRate constructs an empty 60s rolling counter using nowFn
// (defaults to time.Now). The construction time is the monotonic
// baseline.
func NewCertHitRate(nowFn func() time.Time) *CertHitRate {
	if nowFn == nil {
		nowFn = time.Now
	}
	c := &CertHitRate{nowFn: nowFn, start: nowFn()}
	for i := range c.buckets {
		c.buckets[i].second = -1
	}
	return c
}

// Note bumps the appropriate bucket. Idempotent on bucket rollover —
// the second-of-uptime mismatch zeroes the bucket before counting.
func (c *CertHitRate) Note(hit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sec := int64(c.nowFn().Sub(c.start) / time.Second)
	idx := sec % 60
	if idx < 0 {
		idx += 60
	}
	if c.buckets[idx].second != sec {
		c.buckets[idx] = lookupBucket{second: sec}
	}
	if hit {
		c.buckets[idx].hits++
	} else {
		c.buckets[idx].misses++
	}
}

// Last60s returns the (hits, misses) totals across the last 60s
// window. Buckets older than the window are excluded.
func (c *CertHitRate) Last60s() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := int64(c.nowFn().Sub(c.start) / time.Second)
	cutoff := now - 60
	for _, b := range c.buckets {
		if b.second > cutoff {
			hits += b.hits
			misses += b.misses
		}
	}
	return
}

// SetClockForTest replaces the clock and resets the baseline +
// buckets. Used by tests to inject a fake clock.
func (c *CertHitRate) SetClockForTest(nowFn func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nowFn = nowFn
	c.start = nowFn()
	for i := range c.buckets {
		c.buckets[i] = lookupBucket{second: -1}
	}
}

// CertIssuance is the package-global last-cert-issued tracker fed by
// NoteCertIssued. Reads are cheap (mutex-guarded snapshot).
type CertIssuance struct {
	mu       sync.Mutex
	host     string
	at       time.Time
	recorded bool
}

// Note records a successful leaf-cert issuance.
func (i *CertIssuance) Note(host string, at time.Time) {
	i.mu.Lock()
	i.host = host
	i.at = at
	i.recorded = true
	i.mu.Unlock()
}

// Last returns the most recent (host, at, ok). ok is false when no
// issuance has been recorded yet.
func (i *CertIssuance) Last() (host string, at time.Time, ok bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.host, i.at, i.recorded
}

// ResetForTest zeroes the last-issued state.
func (i *CertIssuance) ResetForTest() {
	i.mu.Lock()
	i.host, i.at, i.recorded = "", time.Time{}, false
	i.mu.Unlock()
}

// Package globals: there is exactly one CONNECT pipeline per
// process, and these are observational only. Both initialize lazily
// at package init so RecordCertCacheLookup / NoteCertIssued can be
// called before main.go has wired anything else.
var (
	certHitRate    = NewCertHitRate(time.Now)
	certIssuance   = &CertIssuance{}
	certHitRateMu  sync.RWMutex
	certIssuanceMu sync.RWMutex
)

// NoteCertIssued records a successful leaf-cert issuance for the
// §10.4 status-page "last cert issued" field. host is the literal
// CONNECT host the cert was generated for. main.go's wrapped
// GenFunc calls this after each successful tlsmitm.GenerateLeaf.
func NoteCertIssued(host string, at time.Time) {
	certIssuanceMu.RLock()
	tracker := certIssuance
	certIssuanceMu.RUnlock()
	tracker.Note(host, at)
}

// LastCertIssued returns the most recent recorded issuance.
func LastCertIssued() (host string, at time.Time, ok bool) {
	certIssuanceMu.RLock()
	tracker := certIssuance
	certIssuanceMu.RUnlock()
	return tracker.Last()
}

// CertHitRate60s returns (hits, misses) over the last 60s window.
// Used by the §10.4 status page to compute the percentage.
func CertHitRate60s() (hits, misses int) {
	certHitRateMu.RLock()
	r := certHitRate
	certHitRateMu.RUnlock()
	return r.Last60s()
}

// SwapCertHitRateForTest replaces the package-global rate counter
// with one that uses the supplied clock. Returns a restore closure.
// Tests call this when they need deterministic bucket alignment.
func SwapCertHitRateForTest(nowFn func() time.Time) func() {
	certHitRateMu.Lock()
	prev := certHitRate
	certHitRate = NewCertHitRate(nowFn)
	certHitRateMu.Unlock()
	return func() {
		certHitRateMu.Lock()
		certHitRate = prev
		certHitRateMu.Unlock()
	}
}

// ResetCertIssuanceForTest zeroes the package-global last-issued
// state.
func ResetCertIssuanceForTest() {
	certIssuanceMu.RLock()
	tracker := certIssuance
	certIssuanceMu.RUnlock()
	tracker.ResetForTest()
}
