package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// commitUnsigned adopts an unsigned snapshot of suite with complete
// coverage and — as if a bug wrote one — a package_hash row vouching
// debPath: whatever it holds, it must neither vouch nor count.
func commitUnsigned(t *testing.T, h *Handler, scheme, host, suite, debPath string) {
	t.Helper()
	ctx := context.Background()
	rel := writeBlob(t, h, []byte("Origin: unsigned\nSHA256:\n"))
	if err := h.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host,
		SuitePath: suite}); err != nil {
		t.Fatal(err)
	}
	id, _, err := h.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: scheme,
		CanonicalHost: host, SuitePath: suite, InReleaseHash: &rel, Form: cache.FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel,
		DeclaredSHA256: rel}}, nil, []cache.PackageHash{{SnapshotID: id, CanonicalScheme: scheme, CanonicalHost: host,
		Path: debPath, DeclaredSHA256: sha256Hex([]byte("upstream bytes"))}}, nil, true); err != nil {
		t.Fatal(err)
	}
}

// Phase E DoD (spec C5 safeguard 5, C10): an unsigned snapshot never
// changes a host's strict-mode posture — it vouches no .deb on a host
// strict mode protects, and on a host with no signed snapshot it does not
// turn strict mode on.
func TestServeHTTP_UnsignedNeverChangesStrictPosture(t *testing.T) {
	const deb = "/pool/main/u/unsigned_1_amd64.deb"
	t.Run("a protected host stays protected", func(t *testing.T) {
		h, srv, logs := newPhase3StrictHandlerCapturing(t, true, true, nil)
		scheme, host, port := splitURL(t, srv.URL)
		canon := hostKey(host, port)
		adoptCoverageComplete(t, h, scheme, canon, "/dists/signed", true)
		commitUnsigned(t, h, scheme, canon, "/dists/unsigned", deb)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, deb))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status %d, want the strict refusal: an unsigned snapshot vouched the .deb", rec.Code)
		}
		// Deferred minor E-70: exactly one refusal, so the count is read.
		refused := logs.find("unvouched_deb_refused")
		if len(refused) != 1 || refused[0]["current_snapshot_count"] != float64(1) {
			t.Fatalf("refusals %v, want one with current_snapshot_count 1 (the unsigned snapshot does not count)", refused)
		}
	})
	t.Run("an unsigned-only host stays unprotected", func(t *testing.T) {
		h, srv := newPhase3StrictHandler(t, true, true, nil)
		scheme, host, port := splitURL(t, srv.URL)
		commitUnsigned(t, h, scheme, hostKey(host, port), "/dists/unsigned", deb)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxyReq("GET", srv.URL, deb))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, want the passthrough: an unsigned snapshot turned strict mode on", rec.Code)
		}
	})
}
