package freshness

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// flatRigHost is the flat rig's upstream host.
const flatRigHost = "fh"

// flatRig is retryRig for a flat suite: a real cache, Adopter and Checker
// over suite on host fh, the checker's probes and conditional GETs
// answered by a rigUpstream (200 with an ETag, 304 to it, 404 for a URL
// without a body), the adopter's members by a fakeFetcher. The adopter
// runs with adoption.adopt_unsigned on, production's default (rulings
// PG-9, PG-17: with it off the anchorless refresh is refused, ruling
// PG-1); a test turns it off by hand. The suite has a row whose interest
// stamp is the rig clock's now; no url_path row is seeded: each test
// caches what its clients fetched (cacheRow). The clock starts at
// rigStart and moves only when the test advances it.
type flatRig struct {
	t       *testing.T
	cache   *cache.Cache
	adopter *Adopter
	checker *Checker
	members *fakeFetcher
	up      *rigUpstream
	ver     *rigVerifier
	clock   *rigClock
	logs    *lockedBuffer
	suite   SuiteRef
}

func newFlatRig(t *testing.T, suitePath string) *flatRig {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r := &flatRig{
		t: t, cache: c, members: newFakeFetcher(),
		up:    &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}},
		ver:   &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		clock: &rigClock{t: time.Unix(rigStart, 0)},
		logs:  &lockedBuffer{},
		suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: flatRigHost, SuitePath: suitePath},
	}
	interest := int64(rigStart)
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: flatRigHost,
		SuitePath: suitePath, LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r.adopter, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: r.ver,
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, AdoptUnsigned: true, Logger: discardLogger(), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lifeCtx, cancel := context.WithCancel(ctx)
	r.checker, err = New(Config{Cache: c, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Refresh: 15 * time.Minute, Adopter: r.adopter, LifetimeCtx: lifeCtx, AdoptionRing: observability.NewRing(50),
		Logger: slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:    r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.checker.WaitForAdoptions() })
	return r
}

// url is the upstream URL of the suite's file name.
func (r *flatRig) url(name string) string {
	return "http://" + flatRigHost + r.suite.SuitePath + "/" + name
}

// poolBlob writes body into c's pool (with its blob row) and returns its
// hash.
func poolBlob(t *testing.T, c *cache.Cache, body []byte) string {
	t.Helper()
	w, err := c.NewTempBlob()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	h, err := w.Finalize(int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutBlob(context.Background(), h, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	return h
}

// cacheRow stores body as the url_path row of the suite's file name, as a
// client's miss stored it: requests is its request_count (0: written by
// no client), fetched its last_fetched_at, etag its upstream_etag ("" for
// none). It returns the blob's hash.
func (r *flatRig) cacheRow(name string, body []byte, requests, fetched int64, etag string) string {
	r.t.Helper()
	h := poolBlob(r.t, r.cache, body)
	u := cache.URLPath{CanonicalScheme: "http", CanonicalHost: flatRigHost, Path: r.suite.SuitePath + "/" + name,
		BlobHash: &h, UpstreamURL: r.url(name), IsMetadata: true, RequestCount: requests, LastFetchedAt: &fetched}
	if requests > 0 {
		u.LastRequestedAt = &fetched
	}
	if etag != "" {
		u.UpstreamETag = &etag
	}
	if err := r.cache.PutURLPath(context.Background(), u); err != nil {
		r.t.Fatal(err)
	}
	return h
}

// check runs one freshness check of the suite and waits for any adoption
// it started.
func (r *flatRig) check() {
	r.checker.Check(context.Background(), "http", flatRigHost, r.suite.SuitePath)
	r.checker.WaitForAdoptions()
}

func (r *flatRig) row() cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", flatRigHost, r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return *s
}

// requests is how many GETs the checker sent for the suite's file name.
func (r *flatRig) requests(name string) int { return len(r.up.etagsSent(r.url(name))) }

// lastResult is the result of the rig's last check line: a
// freshness_check line's result, "failed" for a failed check's line, ""
// for none.
func (r *flatRig) lastResult() string {
	var res string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line == "" || json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m["msg"] {
		case "freshness_check":
			res, _ = m["result"].(string)
		case "freshness check failed":
			res = "failed"
		}
	}
	return res
}

// lastRows is the rows_replaced and rows_deleted of the rig's last
// freshness_check line (an anchorless refresh's counts), -1 for a field
// the line lacks.
func (r *flatRig) lastRows() (replaced, deleted float64) {
	r.t.Helper()
	recs := logRecords(r.t, r.logs, "freshness_check")
	if len(recs) == 0 {
		r.t.Fatal("no freshness_check line")
	}
	get := func(k string) float64 {
		if v, ok := recs[len(recs)-1][k].(float64); ok {
			return v
		}
		return -1
	}
	return get("rows_replaced"), get("rows_deleted")
}
