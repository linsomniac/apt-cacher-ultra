package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const (
	rigHost      = "h"
	rigSuite     = "/r/dists/x"
	rigAnchorURL = "http://h/r/dists/x/InRelease"
	rigPkgsURL   = "http://h/r/dists/x/main/binary-amd64/Packages"
)

// rigClock is the settable wall clock shared by the rig's checker and
// adopter.
type rigClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *rigClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *rigClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// rigETag is the ETag the rig upstream gives a body.
func rigETag(body []byte) string { return "e-" + hashOf(body)[:12] }

// rigUpstream answers the checker's conditional GETs by URL: 200 with the
// body and its ETag, or 304 when If-None-Match equals that ETag (unless
// validators are ignored). A URL without a body answers 404. While
// stalled, every request blocks until its context ends.
type rigUpstream struct {
	mu               sync.Mutex
	bodies           map[string][]byte
	ignoreValidators bool
	stall            bool
	fail             map[string]error    // per URL: the error Conditional returns (setFail)
	sent             map[string][]string // If-None-Match per URL, in order
}

func (u *rigUpstream) set(body []byte) { u.setAt(rigAnchorURL, body) }

// setFail makes every request for url fail with err (nil restores the
// body or the default 404).
func (u *rigUpstream) setFail(url string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fail == nil {
		u.fail = map[string]error{}
	}
	if err == nil {
		delete(u.fail, url)
		return
	}
	u.fail[url] = err
}

func (u *rigUpstream) setAt(url string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bodies[url] = body
}

func (u *rigUpstream) etagsSent(url string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.sent[url]...)
}

func (u *rigUpstream) Conditional(ctx context.Context, target *fetch.Target, etag, _ string, _ int64) (*fetch.ConditionalResult, error) {
	u.mu.Lock()
	u.sent[target.URL] = append(u.sent[target.URL], etag)
	body, ok := u.bodies[target.URL]
	stall, ignore, failErr := u.stall, u.ignoreValidators, u.fail[target.URL]
	u.mu.Unlock()
	if stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if failErr != nil {
		return nil, failErr
	}
	if !ok {
		return nil, &fetch.StatusError{Code: 404}
	}
	tag := rigETag(body)
	if !ignore && etag != "" && etag == tag {
		return &fetch.ConditionalResult{Status: 304, ETag: tag}, nil
	}
	return &fetch.ConditionalResult{Status: 200, Body: body, ETag: tag}, nil
}

var errRigUntrusted = errors.New("rig: untrusted signer")

// rigVerifier passes every body through as its own plaintext, except
// content keys marked rejected (error) or panicking. blockNext makes the
// next call signal entered and wait for its context to end.
type rigVerifier struct {
	mu       sync.Mutex
	rejected map[string]bool
	panics   map[string]bool
	block    bool
	n        int
	entered  chan struct{}
}

func (v *rigVerifier) reject(key string)  { v.mu.Lock(); v.rejected[key] = true; v.mu.Unlock() }
func (v *rigVerifier) panicOn(key string) { v.mu.Lock(); v.panics[key] = true; v.mu.Unlock() }
func (v *rigVerifier) blockNext()         { v.mu.Lock(); v.block = true; v.mu.Unlock() }
func (v *rigVerifier) calls() int         { v.mu.Lock(); defer v.mu.Unlock(); return v.n }

func (v *rigVerifier) verify(ctx context.Context, key string, plain []byte) ([]byte, error) {
	v.mu.Lock()
	v.n++
	rej, pan, blk := v.rejected[key], v.panics[key], v.block
	v.block = false
	v.mu.Unlock()
	if blk {
		v.entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if pan {
		panic("rig verifier panic")
	}
	if rej {
		return nil, errRigUntrusted
	}
	return plain, nil
}

func (v *rigVerifier) VerifyInline(ctx context.Context, _ SuiteRef, b []byte) ([]byte, error) {
	return v.verify(ctx, contentKey(adoptionFormInline, b, nil, nil), b)
}

func (v *rigVerifier) VerifyDetached(ctx context.Context, _ SuiteRef, rel, sig []byte) ([]byte, error) {
	return v.verify(ctx, contentKey(adoptionFormDetached, nil, rel, sig), rel)
}

// retryRig wires a real cache, Adopter and Checker for suite /r/dists/x
// on host h, whose cached InRelease anchor is stale (testHex64), so any
// body the upstream serves is "changed". The clock starts at
// 1_800_000_000 and moves only when the test advances it; jitter is off
// (rand = 0.5).
type retryRig struct {
	t              *testing.T
	cache          *cache.Cache
	adopter        *Adopter
	checker        *Checker
	members        *fakeFetcher
	up             *rigUpstream
	ver            *rigVerifier
	clock          *rigClock
	ring           *observability.Ring
	logs           *lockedBuffer
	pkgs           []byte
	cancelLifetime context.CancelFunc
}

func newRetryRig(t *testing.T, cooldown time.Duration) *retryRig {
	t.Helper()
	ctx := context.Background()
	c, err := cache.Open(ctx, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	seedStaleAnchor(t, c)
	// Client interest at the rig clock's now (spec C4 Dispatch 3).
	interest := int64(1_800_000_000)
	if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: rigHost, SuitePath: rigSuite,
		LastRequestedAt: &interest}); err != nil {
		t.Fatal(err)
	}
	r := &retryRig{
		t: t, cache: c, members: newFakeFetcher(),
		up:    &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}},
		ver:   &rigVerifier{rejected: map[string]bool{}, panics: map[string]bool{}, entered: make(chan struct{}, 1)},
		clock: &rigClock{t: time.Unix(1_800_000_000, 0)},
		ring:  observability.NewRing(50), logs: &lockedBuffer{},
		pkgs: fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64}),
	}
	r.members.put(rigPkgsURL, r.pkgs)
	r.adopter, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: r.ver,
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: discardLogger(), now: r.clock.now})
	if err != nil {
		t.Fatal(err)
	}
	lifeCtx, cancel := context.WithCancel(ctx)
	r.cancelLifetime = cancel
	r.checker, err = New(Config{Cache: c, Fetcher: r.up, HostLimiter: hostsem.New(8),
		Cooldown: cooldown, Refresh: 15 * time.Minute, Adopter: r.adopter, LifetimeCtx: lifeCtx,
		AdoptionRing: r.ring,
		Logger:       slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		now:          r.clock.now, rand: func() float64 { return 0.5 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); r.checker.WaitForAdoptions() })
	return r
}

// release is an InRelease body the rig can adopt: the Release text of
// releaseText in a clearsign envelope (spec C3's sniff passes it, the rig
// verifier hands it to ParseRelease, which skips the armor lines).
func (r *retryRig) release(label string, date int64) []byte {
	return simtest.ClearsignedFixture(string(r.releaseText(label, date)))
}

// releaseText is a Release body the rig can adopt (detached and unsigned
// forms): label makes it distinct; date != 0 adds a Date field.
func (r *retryRig) releaseText(label string, date int64) []byte {
	return datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": r.pkgs})
}

// armoredSig is a Release.gpg body that passes spec C3's sniff (an armor
// header); label makes it distinct. The rig verifier never checks it.
func armoredSig(label string) []byte {
	return []byte("-----BEGIN PGP SIGNATURE-----\n\n" + label + "\n-----END PGP SIGNATURE-----\n")
}

// adopt makes the rig suite adopted — a current snapshot of a clearsigned
// "base" InRelease — so its checks run the current form's conditional GET
// (spec C4 Dispatch 1), whose validators the spec C7 gate sets. The
// verifier's call count restarts at 0.
func (r *retryRig) adopt() {
	r.t.Helper()
	r.up.set(r.release("base", 0))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		r.t.Fatal("rig: base content not adopted")
	}
	r.ver.mu.Lock()
	r.ver.n = 0
	r.ver.mu.Unlock()
}

// adoptDetached is adopt for the detached form: the rig suite's current
// snapshot is a "base" Release + Release.gpg pair (seedDetachedAnchors).
func (r *retryRig) adoptDetached() {
	r.t.Helper()
	seedDetachedAnchors(r.t, r.cache)
	r.up.setAt(rigReleaseURL, r.releaseText("base", 0))
	r.up.setAt(rigReleaseGPGURL, armoredSig("base sig"))
	r.check()
	if r.row().CurrentSnapshotID == nil {
		r.t.Fatal("rig: base pair not adopted")
	}
	r.ver.mu.Lock()
	r.ver.n = 0
	r.ver.mu.Unlock()
}

// check runs one freshness check and waits for any adoption it started.
func (r *retryRig) check() {
	r.checker.Check(context.Background(), "http", rigHost, rigSuite)
	r.checker.WaitForAdoptions()
}

func (r *retryRig) row() cache.SuiteFreshness {
	r.t.Helper()
	s, err := r.cache.GetSuiteFreshness(context.Background(), "http", rigHost, rigSuite)
	if err != nil {
		r.t.Fatal(err)
	}
	return *s
}

// events returns the checker's JSON log records whose msg is msg.
func (r *retryRig) events(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func strOf(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func i64Of(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}
