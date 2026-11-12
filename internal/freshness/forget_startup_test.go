package freshness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

func TestForgetAdoptedAtStartup_RetiresSnapshotsAndPreservesUnadopted(t *testing.T) {
	r := newReaperRig(t)
	r.checker.adopter = nil // The new daemon has adoption disabled.
	r.reaper.idleTTL = 0
	ctx := context.Background()
	r.seed("/r/dists/untouched", reapNow)
	packageHash, err := r.adopter.writeBlobBytes(ctx, []byte("cached package"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/pool/p.deb", BlobHash: &packageHash, UpstreamURL: "http://h/r/pool/p.deb"}); err != nil {
		t.Fatal(err)
	}
	var snapshots []int64
	var anchors []string
	for _, tc := range []struct {
		scheme, suite string
		form          cache.AnchorForm
	}{
		{"http", "/r/dists/x", cache.FormInline},
		{"https", "/r/dists/x", cache.FormDetached},
		{"http", "/flat", cache.FormUnsigned},
		{"http", "/flat/.", cache.FormInline},
	} {
		if err := r.cache.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: tc.scheme,
			CanonicalHost: "h", SuitePath: tc.suite}); err != nil {
			t.Fatal(err)
		}
		for version := range 2 { // Both current and displaced snapshots must retire.
			h, err := r.adopter.writeBlobBytes(ctx, []byte(fmt.Sprintf("%s %s %d", tc.scheme, tc.suite, version)))
			if err != nil {
				t.Fatal(err)
			}
			anchors = append(anchors, h)
			name := "InRelease"
			candidate := cache.SnapshotCandidate{CanonicalScheme: tc.scheme, CanonicalHost: "h", SuitePath: tc.suite, Form: tc.form, InReleaseHash: &h}
			if tc.form != cache.FormInline {
				name = "Release"
			}
			if tc.form == cache.FormDetached {
				candidate.InReleaseHash, candidate.ReleaseHash, candidate.ReleaseGPGHash = nil, &h, &h
			}
			id, _, err := r.cache.InsertCandidateSnapshot(ctx, candidate)
			if err != nil {
				t.Fatal(err)
			}
			members := []cache.SnapshotMember{{SnapshotID: id, Path: name, BlobHash: h, DeclaredSHA256: h}}
			if tc.form == cache.FormDetached {
				members = append(members, cache.SnapshotMember{SnapshotID: id, Path: "Release.gpg", BlobHash: h, DeclaredSHA256: h})
			}
			if err := r.cache.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
				t.Fatal(err)
			}
			snapshots = append(snapshots, id)
			if err := r.cache.PutURLPath(ctx, cache.URLPath{CanonicalScheme: tc.scheme, CanonicalHost: "h",
				Path: tc.suite + "/" + name, BlobHash: &h, IsMetadata: true,
				UpstreamURL: tc.scheme + "://h" + tc.suite + "/" + name}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := r.reaper.ForgetAdoptedAtStartup(ctx); err != nil || n != 4 {
		t.Fatalf("forgot %d suites: %v; want 4", n, err)
	}
	if rows, err := r.cache.ListSuites(ctx); err != nil || len(rows) != 1 || rows[0].SuitePath != "/r/dists/untouched" {
		t.Fatalf("remaining suites: %+v, %v", rows, err)
	}
	for _, path := range []string{"/r/dists/untouched/InRelease", "/r/pool/p.deb"} {
		if _, err := r.cache.LookupURL(ctx, "http", "h", path); err != nil {
			t.Fatalf("preserved %s: %v", path, err)
		}
	}
	for _, id := range snapshots {
		if s, err := r.cache.GetSuiteSnapshot(ctx, id); err != nil || s.RetiredAt == nil {
			t.Fatalf("snapshot %d not retired: %+v %v", id, s, err)
		}
	}
	if n, err := r.reaper.ForgetAdoptedAtStartup(ctx); err != nil || n != 0 {
		t.Fatalf("repeat forgot %d: %v", n, err)
	}
	// Exercise the real GC transactions: retired metadata no longer pins its
	// blobs, while the unadopted anchor and package remain reachable.
	for {
		_, more, err := r.cache.RunRetiredSnapshotGCChunk(ctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	restore := wallclock.SetOffsetForTest(time.Until(time.Unix(reapNow, 0).Add(time.Hour)))
	defer restore()
	if _, err := r.cache.RunBlobGCBatch(ctx, 100, 300); err != nil {
		t.Fatal(err)
	}
	for _, h := range anchors {
		if _, err := r.cache.GetBlob(ctx, h); !errors.Is(err, cache.ErrNotFound) {
			t.Fatalf("forgotten blob %s still retained: %v", h, err)
		}
	}
	if _, err := r.cache.GetBlob(ctx, packageHash); err != nil {
		t.Fatalf("cached package collected: %v", err)
	}
}

func TestForgetAdoptedAtStartup_FailureCanResume(t *testing.T) {
	r := newReaperRig(t)
	r.checker.adopter = nil
	ctx := context.Background()
	r.seed("/r/dists/x", reapNow)
	id := r.adopt("/r/dists/x", "snapshot")
	db, err := sql.Open("sqlite", filepath.Join(r.cache.DBDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TRIGGER fail_startup_forget BEFORE DELETE ON suite_freshness
BEGIN SELECT RAISE(ABORT, 'injected forget failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reaper.ForgetAdoptedAtStartup(ctx); err == nil {
		t.Fatal("startup cleanup swallowed a failure")
	}
	if s, err := r.cache.GetSuiteFreshness(ctx, "http", "h", "/r/dists/x"); err != nil || s.CurrentSnapshotID == nil || *s.CurrentSnapshotID != id {
		t.Fatalf("failed forget must remain selectable on restart: %+v %v", s, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_startup_forget`); err != nil {
		t.Fatal(err)
	}
	if n, err := r.reaper.ForgetAdoptedAtStartup(ctx); err != nil || n != 1 {
		t.Fatalf("retry forgot %d: %v", n, err)
	}
}

func TestForgetAdoptedAtStartup_RefusesEnabledAdoption(t *testing.T) {
	r := newReaperRig(t)
	r.seed("/r/dists/x", reapNow)
	r.adopt("/r/dists/x", "snapshot")
	if _, err := r.reaper.ForgetAdoptedAtStartup(context.Background()); err == nil || !r.exists("/r/dists/x") {
		t.Fatalf("enabled adoption must keep its suite and refuse cleanup: %v", err)
	}
}
