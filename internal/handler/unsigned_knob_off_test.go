package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling PF-5 (it reverses the F half of phase E ruling PE-12; spec C5
// safeguard 1): with adoption.adopt_unsigned off, an established unsigned
// suite keeps serving its last snapshot — its Release and Packages as one
// coherent pair, with X-Cache-Snapshot — after upstream has moved on, a
// check has refused the new Release (unsigned_disabled, visible) and the
// suite reaper has run. Nothing returns it to the ordinary cache, where
// each index file would be frozen at its own first fetch and every
// upstream publish would bring Hash Sum mismatches that never clear.
func TestServeHTTP_KnobOffKeepsServingTheUnsignedSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := newFlipStack(t, 0) // its adopter's adopt_unsigned is off
		const suite = "/u/dists/s"
		releaseURL := "http://" + flipSimHost + suite + "/Release"
		pkgsURL := "http://" + flipSimHost + suite + "/main/binary-amd64/Packages"
		ref, rel1 := s.publish(suite, 1)
		pkgs1 := s.members.get(pkgsURL)
		s.track(suite)
		on, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: s.c, Fetcher: s.members, Verifier: plainVerifier{},
			HostLimiter: hostsem.New(8), Logger: simtest.DiscardLogger(), AdoptUnsigned: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := on.RunUnsigned(ctx, ref, rel1, "", ""); err != nil { // adopted while the knob was on
			t.Fatal(err)
		}
		snap := s.current(suite)
		if snap == "" {
			t.Fatal("premise: the knob-on adoption committed no snapshot")
		}
		get := func(path string) (snapshot, body string, code int) {
			rec := httptest.NewRecorder()
			s.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+flipSimHost+path, nil))
			return rec.Header().Get(hdrXCacheSnapshot), rec.Body.String(), rec.Code
		}
		_, rel2 := s.publish(suite, 2) // upstream moves on
		pkgs2 := s.members.get(pkgsURL)
		s.h.fetch = staticUpstream{releaseURL: rel2, pkgsURL: pkgs2}
		up := simtest.NewUpstream()
		up.Set(releaseURL, simtest.Serve(rel2))
		chk, err := freshness.New(freshness.Config{Cache: s.c, Fetcher: up, HostLimiter: hostsem.New(8),
			Adopter: s.ad, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		chk.Check(ctx, "http", flipSimHost, suite)
		refused := func(when string) {
			t.Helper()
			if sf, err := s.c.GetSuiteFreshness(ctx, "http", flipSimHost, suite); err != nil || sf.AdoptionFailReason == nil ||
				*sf.AdoptionFailReason != cache.ReasonUnsignedDisabled {
				t.Fatalf("%s: %+v, %v; want the new Release refused, unsigned_disabled", when, sf, err)
			}
		}
		refused("after the check")
		rp, err := freshness.NewReaper(freshness.ReaperConfig{Cache: s.c, Checker: chk, IdleTTL: 30 * 24 * time.Hour,
			Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { rp.Run(runCtx); close(done) }()
		time.Sleep(time.Hour + time.Second) // the uptime floor: the first run
		synctest.Wait()
		cancel()
		<-done
		for path, want := range map[string][]byte{suite + "/Release": rel1, suite + "/main/binary-amd64/Packages": pkgs1} {
			if got, body, code := get(path); got != snap || body != string(want) || code != http.StatusOK {
				t.Fatalf("%s after the run: snapshot %q, status %d, the snapshot's bytes %v; want snapshot %s's, a coherent pair",
					path, got, code, body == string(want), snap)
			}
		}
		if got := s.current(suite); got != snap {
			t.Fatalf("current snapshot %q after the run, want %s kept", got, snap)
		}
		refused("after the reaper's run") // the refusal stays visible on the status page
	})
}
