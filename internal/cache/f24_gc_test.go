package cache

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

// commitUnsignedCandidate tries to commit an unsigned candidate of key and
// returns CommitAdoption's error (spec C5: the never-downgrade authority).
func commitUnsignedCandidate(t *testing.T, c *Cache, key SuiteKey) error {
	t.Helper()
	ctx := context.Background()
	rel := seedBlob(t, c, "an unsigned Release for "+key.SuitePath)
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{CanonicalScheme: key.CanonicalScheme,
		CanonicalHost: key.CanonicalHost, SuitePath: key.SuitePath, InReleaseHash: &rel, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	return c.CommitAdoption(ctx, id, []SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}},
		nil, nil, nil, false)
}

// Deferred minor F-24, ruling R-H1 (the phase F final re-review's probe):
// with gc.keep_displaced = 0, sub-job B deleted a displaced, adopted,
// audit-deferred signed snapshot — the base's only signed history, since
// the audit left it unmarked — while the suite row stayed, and never-
// downgrade then admitted an unsigned candidate. A snapshot still queued
// in legacy_trust_audit is never in B's displaced class: it is kept, and
// the unsigned candidate is refused, early and inside the commit.
func TestRunSnapshotGCBatch_KeepsAuditDeferredSignedHistory(t *testing.T) {
	for _, form := range []AnchorForm{FormInline, FormDetached} {
		t.Run(string(form), func(t *testing.T) {
			c := openCache(t)
			ctx := context.Background()
			defer stubNow(t, evictNow)()
			key := sk("h", "/r/dists/x")
			id := seedSignedHistory(t, c, key, form, false, evictNow)
			if hostMarker(t, c, "http", "/r/dists/x") >= 0 {
				t.Fatal("premise: the base has a marker")
			}
			if err := c.CheckUnsignedDowngrade(ctx, "h", "/r/dists/x"); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("premise: CheckUnsignedDowngrade = %v, want refused by the displaced snapshot", err)
			}
			res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0)
			if err != nil || res.DisplacedReaped != 0 || res.OrphanReaped != 0 {
				t.Fatalf("RunSnapshotGCBatch(keep_displaced 0) = %+v, %v; want nothing reaped", res, err)
			}
			if !snapshotExists(t, c, id) || !rowExists(t, c, key) {
				t.Fatalf("snapshot %d kept %v, row kept %v; want both", id, snapshotExists(t, c, id), rowExists(t, c, key))
			}
			if err := c.CheckUnsignedDowngrade(ctx, "h", "/r/dists/x"); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("after sub-job B: CheckUnsignedDowngrade = %v, want refused", err)
			}
			if err := commitUnsignedCandidate(t, c, key); !errors.Is(err, ErrUnsignedDowngradeRefused) {
				t.Fatalf("after sub-job B: unsigned commit = %v, want ErrUnsignedDowngradeRefused", err)
			}
		})
	}
}

// Ruling R-H1, Review Focus 2: kept, the deferred snapshot waits for the
// audit's verdict, and sub-job B (or C) deletes it only afterwards. Keep:
// its base is marked, so once B deletes it the unsigned candidate is
// still refused (by the marker). A quarantine (plain text, no marker by
// §6.1's design): it is retired and sub-job C deletes it, and no marker
// was invented, so the suite may then adopt unsigned.
func TestRunSnapshotGCBatch_KeepsTheAuditsVerdict(t *testing.T) {
	t.Run("keep", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		defer stubNow(t, evictNow)()
		key := sk("h", "/r/dists/x")
		id := seedSignedHistory(t, c, key, FormInline, false, evictNow)
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditKeep}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 1 || snapshotExists(t, c, id) {
			t.Fatalf("after Keep: RunSnapshotGCBatch = %+v, %v, snapshot kept %v; want it reaped", res, err, snapshotExists(t, c, id))
		}
		if hostMarker(t, c, "http", "/r/dists/x") < 0 {
			t.Fatal("Keep wrote no marker")
		}
		if err := commitUnsignedCandidate(t, c, key); !errors.Is(err, ErrUnsignedDowngradeRefused) {
			t.Fatalf("after Keep and sub-job B: unsigned commit = %v, want refused by the marker", err)
		}
	})
	t.Run("quarantine", func(t *testing.T) {
		c := openCache(t)
		ctx := context.Background()
		defer stubNow(t, evictNow)()
		key := sk("h", "/r/dists/x")
		id := seedSignedHistory(t, c, key, FormInline, false, evictNow)
		if _, err := c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: id, Outcome: LegacyAuditQuarantine}}); err != nil {
			t.Fatal(err)
		}
		if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 0 {
			t.Fatalf("a retired snapshot: RunSnapshotGCBatch = %+v, %v; want nothing (sub-job C's)", res, err)
		}
		if n, more, err := c.RunRetiredSnapshotGCChunk(ctx, 0); err != nil || n != 1 || !more || snapshotExists(t, c, id) {
			t.Fatalf("RunRetiredSnapshotGCChunk = %d, %v, %v; want the quarantined snapshot deleted", n, more, err)
		}
		if hostMarker(t, c, "http", "/r/dists/x") >= 0 {
			t.Fatal("a marker for a quarantined plain-text snapshot")
		}
		if err := commitUnsignedCandidate(t, c, key); err != nil {
			t.Fatalf("after the quarantine: unsigned commit = %v, want admitted (no signed history)", err)
		}
	})
}

// seedDisplacedAt gives key's row one more adopted, displaced inline
// snapshot adopted at adoptedAt, queued for the audit when queued, and
// returns its id. The row must exist (seedRow).
func seedDisplacedAt(t *testing.T, c *Cache, key SuiteKey, adoptedAt int64, queued bool) int64 {
	t.Helper()
	h := seedBlob(t, c, "displaced anchor of "+key.SuitePath+" at "+strconv.FormatInt(adoptedAt, 10))
	res, err := c.db.Exec(`INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path, inrelease_hash, anchor_form, created_at, adopted_at, heartbeat_at)
  VALUES (?, ?, ?, ?, 'inline', ?, ?, ?)`, key.CanonicalScheme, key.CanonicalHost, key.SuitePath, h, adoptedAt, adoptedAt, adoptedAt)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if queued {
		if err := c.EnqueueLegacyTrustAudit(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Design note 1: a queued snapshot leaves B's displaced class before the
// ROW_NUMBER ranking, as retired rows do, so it takes no keep_displaced
// slot: with keep_displaced = 1 the newest unqueued displaced snapshot
// keeps the slot, the older one goes, and the queued one stays. And an
// unqueued displaced snapshot is still reaped at keep_displaced = 0.
func TestRunSnapshotGCBatch_AQueuedSnapshotTakesNoKeepSlot(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, evictNow)()
	key := sk("h", "/r/dists/x")
	seedRow(t, c, key, evictNow)
	oldest := seedDisplacedAt(t, c, key, evictNow-3000, false)
	older := seedDisplacedAt(t, c, key, evictNow-2000, false)
	queued := seedDisplacedAt(t, c, key, evictNow-1000, true)
	if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 1); err != nil || res.DisplacedReaped != 1 {
		t.Fatalf("RunSnapshotGCBatch(keep_displaced 1) = %+v, %v; want one reaped", res, err)
	}
	if snapshotExists(t, c, oldest) || !snapshotExists(t, c, older) || !snapshotExists(t, c, queued) {
		t.Fatalf("kept: oldest %v, older %v, queued %v; want older and queued (the queued one takes no slot)",
			snapshotExists(t, c, oldest), snapshotExists(t, c, older), snapshotExists(t, c, queued))
	}
	if res, err := c.RunSnapshotGCBatch(ctx, 100, 0, 0); err != nil || res.DisplacedReaped != 1 || snapshotExists(t, c, older) {
		t.Fatalf("RunSnapshotGCBatch(keep_displaced 0) = %+v, %v; want the unqueued one reaped", res, err)
	}
	if !snapshotExists(t, c, queued) {
		t.Fatal("the queued snapshot was reaped at keep_displaced 0")
	}
}
