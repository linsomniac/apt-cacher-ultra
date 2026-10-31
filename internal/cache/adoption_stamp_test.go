package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Deferred minor C-11 (spec C8): the adoption stamp is above every
// snapshot of the suite, not only its current one. A suite reaped while
// its current snapshot's stamp led the clock — it was adopted before a
// backward step — and then re-created stamps its first adoption above the
// retired snapshot's stamp while that snapshot is still there (sub-job C
// deletes it at a periodic GC tick: adoptionStampTx's residual), so a
// client's If-Modified-Since from that snapshot's anchor gets no 304 for
// the new one (a quarantine clears the pointer with the same effect). A
// suite with a current snapshot floors on its displaced snapshots too
// (the Task 5 review's minor 2): a displaced stamp above the current
// one's — data written before C-11, such as a first adoption the old rule
// stamped below a quarantined snapshot — still puts the next flip above
// it.
func TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const t0 = int64(1_800_000_000)
	restore := stubNow(t, t0)
	defer restore() // a Fatal before the explicit restore must not leave nowUnix stubbed (calling it twice is harmless)
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, t0)
	s1, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v1"})
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`, t0+1000, s1); err != nil {
		t.Fatal(err)
	}
	if del, _, err := c.ReapIdleSuite(ctx, key, &s1, t0); err != nil || !del {
		t.Fatalf("ReapIdleSuite = %v, %v", del, err)
	}
	restore()
	defer stubNow(t, t0+10)()
	seedRow(t, c, key, t0+10) // the suite's next request re-creates it
	s2, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v2"})
	snap, err := c.GetSuiteSnapshot(ctx, s2)
	if err != nil || derefI64(snap.AdoptedAt) != t0+1001 {
		t.Fatalf("the next adoption stamped %d (%v), want %d: above the retired snapshot's stamp", derefI64(snap.AdoptedAt), err, t0+1001)
	}

	// The current-snapshot branch: s2 is current; s3 displaces it, and s2's
	// stamp is then raised above s3's. The next flip stamps above s2's.
	s3, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v3"})
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = ? WHERE snapshot_id = ?`, t0+5000, s2); err != nil {
		t.Fatal(err)
	}
	s4, _ := adoptWith(t, c, key, map[string]string{"InRelease": "v4"})
	snap3, err3 := c.GetSuiteSnapshot(ctx, s3)
	snap4, err4 := c.GetSuiteSnapshot(ctx, s4)
	if err3 != nil || err4 != nil || derefI64(snap3.AdoptedAt) >= t0+5000 || derefI64(snap4.AdoptedAt) != t0+5001 {
		t.Fatalf("current %d stamped %d, next flip stamped %d (%v, %v); want the flip at %d: above the displaced snapshot's stamp",
			s3, derefI64(snap3.AdoptedAt), derefI64(snap4.AdoptedAt), err3, err4, t0+5001)
	}
}

// Deferred minor E-4: an error of CommitAdoption's in-transaction
// never-downgrade recheck says CommitAdoption, so a log line tells it from
// the early exits' reads of the same rule; the refusal still matches its
// sentinel.
func TestCommitAdoption_RecheckErrorsSayCommitAdoption(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	key := sk("h", "/u/dists/s")
	seedRow(t, c, key, 1)
	rel := seedBlob(t, c, "unsigned Release")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h",
		SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	commit := func() error {
		return c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
			nil, nil, nil, false)
	}
	if err := c.MarkSignedSeenBatch(ctx, []SignedSeenMark{{CanonicalScheme: "http", CanonicalHost: "h", Base: key.SuitePath, SeenAt: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := commit(); !errors.Is(err, ErrUnsignedDowngradeRefused) || !strings.HasPrefix(err.Error(), "CommitAdoption: ") {
		t.Fatalf("refusal = %v; want ErrUnsignedDowngradeRefused prefixed \"CommitAdoption: \"", err)
	}
	if _, err := c.db.Exec(`DROP TABLE suite_signed_seen`); err != nil { // the recheck's read fails
		t.Fatal(err)
	}
	if err := commit(); err == nil || errors.Is(err, ErrUnsignedDowngradeRefused) ||
		!strings.HasPrefix(err.Error(), "CommitAdoption: unsigned downgrade check: ") {
		t.Fatalf("read failure = %v; want an error prefixed \"CommitAdoption: unsigned downgrade check: \"", err)
	}
}
