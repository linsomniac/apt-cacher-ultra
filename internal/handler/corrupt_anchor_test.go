package handler

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// Ruling R-H2 (the maintainer, 2026-10-24; review C-1) through the
// handler: a pre-v7 inline suite whose stored InRelease rotted in the pool
// is quarantined at startup (anchor_corrupt); the audit deletes the
// url_path row that served those bytes and removes the rotted pool file,
// so a client's next request is a miss that fetches a fresh copy from
// upstream, and the one after it a hit on that copy. Both when upstream
// still serves the same InRelease — the usual bit rot: the miss path's
// Finalize keeps an existing pool file of the same hash, so only the
// removal heals it — and when it has moved on.
func TestServeHTTP_ACorruptAnchorQuarantineRefetchesTheAnchor(t *testing.T) {
	old := simtest.ClearsignedFixture("Origin: old\nSuite: c\n")
	for _, tc := range []struct {
		name     string
		upstream []byte
	}{{"upstream unchanged", old}, {"upstream changed", rowInRelease}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSuiteRig(t, true, 0)
			ctx := context.Background()
			hash := writeBlob(t, r.h, old)
			id := commitInlineSnapshot(t, r.h, "http", "127.0.0.1", "/r/dists/c", hash,
				[]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
			p := r.c.BlobPath(hash)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("bit-rotted bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := r.c.ClearSignedSeenForTest(ctx); err != nil { // a pre-v7 cache has no markers
				t.Fatal(err)
			}
			if err := r.c.EnqueueLegacyTrustAudit(ctx, id); err != nil { // as the v7 migration does
				t.Fatal(err)
			}
			if res, err := freshness.RunLegacyTrustAudit(ctx, r.c, silentLogger()); err != nil || res.Quarantined != 1 {
				t.Fatalf("audit = %+v, %v; want the rotted snapshot quarantined", res, err)
			}
			r.serve("/r/dists/c/InRelease", upFile{body: tc.upstream})
			for _, want := range []string{"MISS", "HIT"} {
				rec := r.get("/r/dists/c/InRelease")
				if rec.Code != http.StatusOK || rec.Header().Get("X-Cache") != want || rec.Body.String() != string(tc.upstream) {
					t.Fatalf("status %d, X-Cache %q, body %q; want a %s serving upstream's InRelease",
						rec.Code, rec.Header().Get("X-Cache"), rec.Body.String(), want)
				}
			}
			u, err := r.c.LookupURL(ctx, "http", "127.0.0.1", "/r/dists/c/InRelease")
			if err != nil || u.BlobHash == nil || *u.BlobHash != sha256Hex(tc.upstream) {
				t.Fatalf("anchor row %+v, %v; want the fresh copy stored", u, err)
			}
		})
	}
}
