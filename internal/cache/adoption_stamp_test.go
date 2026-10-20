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
// retired snapshot's stamp, never below, so a client's If-Modified-Since
// from that snapshot's anchor never gets a 304 for the new one (a
// quarantine clears the pointer with the same effect).
func TestAdoptionStamp_FloorsOnEverySnapshotOfTheSuite(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	const t0 = int64(1_800_000_000)
	restore := stubNow(t, t0)
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
