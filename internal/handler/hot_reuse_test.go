package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

func TestHotPrefetchUnchangedPackagesServeOfflineAfterFlip(t *testing.T) {
	specs := chaos3DebSpecs()
	snap := makeChaos3Snapshot("A", specs)
	var current atomic.Pointer[chaos3Snapshot]
	current.Store(&snap)
	stalls := newChaos3StallSet()
	srv := chaos3Server(t, &current, stalls)
	upstream, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	stack := newPhase3ChaosStack(t, upstream, 300*24*time.Hour, time.Second, time.Second)
	t.Cleanup(stack.handler.Close)
	suite := chaos3SuiteRef(upstream)
	prior := chaos3PrimeAndAdoptA(t, stack, srv, &snap, suite)
	// A metadata-only publication: every package hash is unchanged.
	changed := snap
	changed.inRelease = simtest.ClearsignedFixture("Origin: second publication\n" + string(chaos2BuildRelease(map[string][]byte{chaos3PackagesGzPath: snap.packagesGz})))
	current.Store(&changed)
	// Any package request would hang and consume the prefetch budget.
	for _, spec := range specs {
		stalls.Stall(debCanonicalPath(spec))
	}
	if err := stack.adopter.Run(t.Context(), suite, changed.inRelease, `"B"`, ""); err != nil {
		t.Fatal(err)
	}
	fresh, err := stack.handler.cache.GetSuiteFreshness(t.Context(), suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil || fresh.CurrentSnapshotID == nil || *fresh.CurrentSnapshotID == prior {
		t.Fatalf("flip: %+v %v", fresh, err)
	}
	rec, count := stack.logBuf.findForSnapshot("adoption_hot_prefetch_complete", *fresh.CurrentSnapshotID)
	if count != 1 || rec["reused"] != float64(len(specs)) || rec["fetched"] != float64(0) {
		t.Fatalf("prefetch summary: %+v count=%d", rec, count)
	}
	srv.Close()
	for _, spec := range specs {
		w := httptest.NewRecorder()
		stack.handler.ServeHTTP(w, proxyReq("GET", srv.URL, debCanonicalPath(spec)))
		if w.Code != http.StatusOK || w.Header().Get(hdrXCache) != cacheHit || !bytes.Equal(w.Body.Bytes(), snap.debBodies[spec.rel]) {
			t.Fatalf("offline %s: status=%d cache=%s body=%q", spec.rel, w.Code, w.Header().Get(hdrXCache), w.Body.Bytes())
		}
	}
}
