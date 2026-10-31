package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// checkTotal returns acu_freshness_check_total for result on host.
func checkTotal(t *testing.T, result, host string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "acu_freshness_check_total{") &&
			strings.Contains(line, `result="`+result+`"`) && strings.Contains(line, `host="`+host+`"`) {
			v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return 0
}

// logRecords returns the JSON log records in buf whose msg is msg.
func logRecords(t *testing.T, buf *lockedBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// seedDetachedSuite gives the fake cache's /r/dists/x on host a current
// detached snapshot with cached Release and Release.gpg anchors.
func seedDetachedSuite(fc *fakeCache, host string) {
	rel, sig := testHex64, hashOf([]byte("a signature"))
	id := int64(7)
	fc.putSnapshot(cache.SuiteSnapshot{SnapshotID: id, CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x",
		Form: cache.FormDetached, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	fc.putSuite(cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x", CurrentSnapshotID: &id})
	for path, h := range map[string]string{"/r/dists/x/Release": rel, "/r/dists/x/Release.gpg": sig} {
		h := h
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: path, BlobHash: &h,
			UpstreamURL: "http://" + host + path, IsMetadata: true})
	}
}

// Ruling R-E3 (overview, B carry-forward; deferred minors B-34, B-35): a
// success status other than 200 and 304 — a fetch.Conditional contract
// violation — is a failed check on the one failed-check line, with its
// form, counted as failed, extending the streak and leaving
// last_success_at alone, for both forms.
func TestCheck_UnexpectedStatusIsAFailedCheck(t *testing.T) {
	for _, form := range []string{"inline", "detached"} {
		t.Run(form, func(t *testing.T) {
			host := "unexpected-" + form
			fc := newFakeCache()
			url := "http://" + host + "/r/dists/x/InRelease"
			if form == "inline" {
				stale := testHex64
				fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: host, Path: "/r/dists/x/InRelease",
					BlobHash: &stale, UpstreamURL: url, IsMetadata: true})
				fc.track("http", host, "/r/dists/x")
			} else {
				seedDetachedSuite(fc, host)
				url = "http://" + host + "/r/dists/x/Release"
			}
			success := int64(1_799_000_000)
			s, _ := fc.suite("http", host, "/r/dists/x")
			s.LastSuccessAt = &success
			fc.putSuite(s)
			up := simtest.NewUpstream()
			up.Set(url, func(context.Context, string) (*fetch.ConditionalResult, error) {
				return &fetch.ConditionalResult{Status: 206}, nil
			})
			logs := &lockedBuffer{}
			c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8),
				Logger: slog.New(slog.NewJSONHandler(logs, nil)), now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
			if err != nil {
				t.Fatal(err)
			}
			before := checkTotal(t, "failed", host)
			rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: host, SuitePath: "/r/dists/x"}, checkOpts{})
			if rep.result != "failed" {
				t.Fatalf("report = %q, want failed", rep.result)
			}
			if got := checkTotal(t, "failed", host) - before; got != 1 {
				t.Fatalf("acu_freshness_check_total{result=failed} rose by %v, want 1", got)
			}
			lines := logRecords(t, logs, "freshness check failed")
			if len(lines) != 1 || lines[0]["form"] != form || !strings.Contains(lines[0]["err"].(string), "unexpected success status 206") {
				t.Fatalf("failed-check lines %v, want one for form %s naming the status", lines, form)
			}
			if n := len(logRecords(t, logs, "freshness_check")); n != 0 {
				t.Fatalf("%d freshness_check lines for a failed check, want 0", n)
			}
			row, _ := fc.suite("http", host, "/r/dists/x")
			if row.CheckFailCount != 1 || row.LastCheckAt == nil || *row.LastCheckAt != 1_800_000_000 || i64Of(row.LastSuccessAt) != success {
				t.Fatalf("row = %+v; want the streak at 1, last_check_at now, last_success_at kept", row)
			}
		})
	}
}

// Deferred minor B-34: a transport failure counts as failed too.
func TestCheck_TransportFailureCountsAsFailed(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	fc.track("http", "h", "/r/dists/x")
	up := simtest.NewUpstream()
	up.Set("http://h/r/dists/x/InRelease", func(context.Context, string) (*fetch.ConditionalResult, error) {
		return nil, fetch.ErrUpstreamServerError
	})
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	before := checkTotal(t, "failed", "h")
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := checkTotal(t, "failed", "h") - before; got != 1 {
		t.Fatalf("acu_freshness_check_total{result=failed} rose by %v, want 1", got)
	}
}

// Deferred minor B-33: the Release.gpg GET of a detached check runs under
// the check deadline: a signature that stalls ends the check at
// checkDeadline (spec C4: D in the latency bound), not later.
func TestCheck_DetachedSignatureStallEndsAtTheCheckDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fc := newFakeCache()
		seedDetachedSuite(fc, "h")
		up := simtest.NewUpstream()
		up.Set("http://h/r/dists/x/Release", simtest.Serve([]byte("a new Release\n")))
		up.Set("http://h/r/dists/x/Release.gpg", simtest.Stall())
		c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		rep := c.check(context.Background(), SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}, checkOpts{})
		if d := time.Since(start); d != checkDeadline {
			t.Fatalf("the check took %v, want the %v check deadline", d, checkDeadline)
		}
		if rep.result != "changed" {
			t.Fatalf("result %q, want changed (the signature fetch failed; retried later)", rep.result)
		}
	})
}

// Deferred minor B-23: a next_retry_at left far in the future by a
// backward clock step is clamped once per check, also when the inline
// check falls back to the detached path.
func TestCheck_FutureRetryIsClampedOncePerCheck(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	for path, h := range map[string]string{"/r/dists/x/Release": testHex64, "/r/dists/x/Release.gpg": hashOf([]byte("sig"))} {
		h := h
		fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path, BlobHash: &h,
			UpstreamURL: "http://h" + path, IsMetadata: true})
	}
	fc.track("http", "h", "/r/dists/x")
	now := time.Unix(1_800_000_000, 0)
	fc.interest("http", "h", "/r/dists/x", now.Unix())
	if err := fc.RecordAdoptionFailure(context.Background(), "http", "h", "/r/dists/x", cache.AdoptionFailure{
		Hash: testHex64, Reason: "gpg_failed", Count: 1, FirstAt: now.Unix(), LastAt: now.Unix(),
		NextRetryAt: now.Add(10 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	up := simtest.NewUpstream() // InRelease 404s: the never-adopted suite falls back to detached
	up.Set("http://h/r/dists/x/Release", simtest.Serve([]byte("a Release\n")))
	up.Set("http://h/r/dists/x/Release.gpg", simtest.Serve([]byte("a signature\n")))
	ad, err := NewAdopter(AdoptionConfig{Cache: simtest.OpenCache(t), Fetcher: newFakeFetcher(), Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad, Logger: discardLogger(),
		now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	parse := func() float64 {
		v, _ := strconv.ParseFloat(renderedMetric("acu_clock_steps_detected_total"), 64)
		return v
	}
	before := parse()
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if got := parse() - before; got != 1 {
		t.Fatalf("clock steps detected: %v, want 1 (one clamp per check)", got)
	}
}

// notModifiedTo answers every request for url with a bare 304, whatever
// validators it carried (recorded like the rig's): an upstream that
// answers 304 to unconditional GETs.
type notModifiedTo struct {
	*rigUpstream
	url string
}

func (u notModifiedTo) Conditional(ctx context.Context, target *fetch.Target, etag, lastmod string, limit int64) (*fetch.ConditionalResult, error) {
	if target.URL != u.url {
		return u.rigUpstream.Conditional(ctx, target, etag, lastmod, limit)
	}
	u.mu.Lock()
	u.sent[target.URL] = append(u.sent[target.URL], etag+lastmod)
	u.mu.Unlock()
	return &fetch.ConditionalResult{Status: 304}, nil
}

// Spec C4 (design note 21; final spec review M5 (c), final fix wave FW-3
// (c)): a 304 is present only as the answer to a conditional request. A
// current form's GET that carried no validators (none stored, or the
// record due) and got a 304 is a failed check — not not_modified, which
// bumped last_success_at and froze the suite silently behind an upstream
// that answers 304 to unconditional GETs. Phase 1 (no adopter) runs
// unchanged and still takes it as not_modified.
func TestCheck_NotModifiedWithoutValidatorsIsAFailedCheck(t *testing.T) {
	for _, form := range []string{"inline", "detached", "unsigned", "phase 1"} {
		t.Run(form, func(t *testing.T) {
			r := newRetryRig(t, 0)
			url := rigReleaseURL
			switch form {
			case "inline", "phase 1":
				url = rigAnchorURL
				if form == "inline" {
					r.adopt()
				}
			case "detached":
				r.adoptDetached()
			default:
				r.adoptUnsigned("u1")
			}
			row := r.row()
			row.InReleaseETag, row.InReleaseLastMod = nil, nil // the next GET carries no validators
			if err := r.cache.UpdateSuiteFreshness(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			success := i64Of(row.LastSuccessAt)
			fetcher := notModifiedTo{rigUpstream: r.up, url: url}
			r.checker.fetcher = fetcher
			if form == "phase 1" {
				c, err := New(Config{Cache: r.cache, Fetcher: fetcher, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
					Logger: slog.New(slog.NewJSONHandler(r.logs, nil)), now: r.clock.now})
				if err != nil {
					t.Fatal(err)
				}
				r.checker = c
			}
			sentBefore := len(r.up.etagsSent(url))
			before := checkTotal(t, "failed", rigHost)
			r.clock.advance(time.Minute)
			r.check()
			if sent := r.up.etagsSent(url)[sentBefore:]; len(sent) != 1 || sent[0] != "" {
				t.Fatalf("setup: requests for %s carried %q, want one without validators", url, sent)
			}
			got := r.row()
			if form == "phase 1" {
				if got.CheckFailCount != 0 || lastResult(r) != "not_modified" {
					t.Fatalf("phase 1: check_fail_count %d, result %q; want not_modified as before phase E", got.CheckFailCount, lastResult(r))
				}
				return
			}
			if got.CheckFailCount != 1 || i64Of(got.LastSuccessAt) != success {
				t.Fatalf("check_fail_count %d, last_success_at %d (was %d); want a failed check that keeps last_success_at",
					got.CheckFailCount, i64Of(got.LastSuccessAt), success)
			}
			if n := checkTotal(t, "failed", rigHost) - before; n != 1 {
				t.Fatalf("failed checks counted %v, want 1", n)
			}
			if ev := r.events("freshness check failed"); len(ev) != 1 || ev[0]["form"] != form || !strings.Contains(ev[0]["err"].(string), "304") {
				t.Fatalf("failed-check lines %v, want one for form %s naming the 304", ev, form)
			}
		})
	}
}
