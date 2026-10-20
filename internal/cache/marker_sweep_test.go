package cache

import (
	"context"
	"fmt"
	"testing"
)

const (
	sweepNow  = int64(1_800_000_000)
	sweepYear = int64(365 * 86400)
)

// putMarks writes markers, each with its own stamp.
func putMarks(t *testing.T, c *Cache, marks []SignedSeenMark) {
	t.Helper()
	if err := c.MarkSignedSeenBatch(context.Background(), marks); err != nil {
		t.Fatal(err)
	}
}

// markerStamp returns m's marker stamp, or -1 when it has none.
func markerStamp(t *testing.T, c *Cache, m SignedSeenMark) int64 {
	t.Helper()
	at, ok, err := c.SignedSeen(context.Background(), m.CanonicalScheme, m.CanonicalHost, m.Base)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		return -1
	}
	return at
}

// Deferred minor E-18: URLPath.LastUse is the one Go copy of a marker
// stamp's source — the newer of last_requested_at and last_fetched_at, 0
// when neither is set.
func TestURLPath_LastUse(t *testing.T) {
	at := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		requested, fetched *int64
		want               int64
	}{
		{nil, nil, 0}, {at(5), nil, 5}, {nil, at(7), 7}, {at(5), at(7), 7}, {at(9), at(7), 9},
	} {
		if got := (&URLPath{LastRequestedAt: tc.requested, LastFetchedAt: tc.fetched}).LastUse(); got != tc.want {
			t.Errorf("LastUse(requested %v, fetched %v) = %d, want %d", tc.requested != nil, tc.fetched != nil, got, tc.want)
		}
	}
}

// Spec C9 (ruling R-F5): the sweep deletes a marker last seen before the
// cutoff whose base no suite row of its host has — not the base, not its
// "/." twin, under neither scheme — and keeps every other: a younger
// orphan, one at the cutoff, and the markers a suite row of the same host
// keeps under either scheme, through the base or its twin.
func TestSweepOrphanMarkers_SweepsOnlyYearOldOrphans(t *testing.T) {
	c := openCache(t)
	cutoff := sweepNow - sweepYear
	seedRow(t, c, sk("h", "/kept/dists/s"), sweepNow)
	seedRow(t, c, SuiteKey{CanonicalScheme: "https", CanonicalHost: "h", SuitePath: "/other/dists/s"}, sweepNow)
	seedRow(t, c, sk("h", "/flat/."), sweepNow)
	swept := []SignedSeenMark{
		{"http", "h", "/gone/dists/s", cutoff - 1},
		{"https", "h", "/gone/dists/s", cutoff - 1},
		{"http", "h2", "/kept/dists/s", cutoff - 1}, // another host's suite does not keep it
	}
	kept := []SignedSeenMark{
		{"http", "h", "/young/dists/s", cutoff + 1},
		{"http", "h", "/edge/dists/s", cutoff},
		{"http", "h", "/kept/dists/s", cutoff - 1},
		{"http", "h", "/other/dists/s", cutoff - 1}, // its suite's row is under https
		{"http", "h", "/flat", cutoff - 1},          // its /. twin has a row
	}
	putMarks(t, c, append(append([]SignedSeenMark{}, swept...), kept...))
	res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, cutoff)
	if err != nil || res.Swept != len(swept) || res.Furthest != nil {
		t.Fatalf("SweepOrphanMarkers = %+v, %v; want %d swept, no future stamp", res, err, len(swept))
	}
	for _, m := range swept {
		if markerStamp(t, c, m) != -1 {
			t.Errorf("%+v was not swept", m)
		}
	}
	for _, m := range kept {
		if markerStamp(t, c, m) != m.SeenAt {
			t.Errorf("%+v was swept or changed", m)
		}
	}
}

// Ruling PF-1 (it overrides R-F5's "healed"): the sweep never rewrites a
// stamp. A marker stamped more than 2 s past now reads as now — never an
// orphan, its stamp left as it is — and the furthest is reported; one
// within the 2 s slack is no clock step. A future stamp ages out only
// once the clock has passed it by the TTL.
func TestSweepOrphanMarkers_NeverRewritesAFutureStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	slack := SignedSeenMark{"http", "h", "/slack/dists/s", sweepNow + 2}
	putMarks(t, c, []SignedSeenMark{slack})
	if res, err := c.SweepOrphanMarkers(ctx, sweepNow, sweepNow-sweepYear); err != nil || res.Swept != 0 || res.Furthest != nil {
		t.Fatalf("a stamp within the slack: %+v, %v; want nothing swept, no future stamp", res, err)
	}
	far := SignedSeenMark{"http", "h", "/far/dists/s", sweepNow + 5*sweepYear}
	near := SignedSeenMark{"http", "h", "/near/dists/s", sweepNow + 10}
	putMarks(t, c, []SignedSeenMark{far, near})
	res, err := c.SweepOrphanMarkers(ctx, sweepNow, sweepNow-sweepYear)
	if err != nil || res.Swept != 0 || res.Furthest == nil || *res.Furthest != far {
		t.Fatalf("SweepOrphanMarkers = %+v (furthest %+v), %v; want nothing swept, the furthest %+v", res, res.Furthest, err, far)
	}
	for _, m := range []SignedSeenMark{far, near, slack} {
		if got := markerStamp(t, c, m); got != m.SeenAt {
			t.Fatalf("%s's stamp is %d, want %d unchanged", m.Base, got, m.SeenAt)
		}
	}
	later := near.SeenAt + sweepYear + 1 // a year after near: near and slack are year-old orphans
	if res, err := c.SweepOrphanMarkers(ctx, later, later-sweepYear); err != nil || res.Swept != 2 || res.Furthest == nil ||
		*res.Furthest != far {
		t.Fatalf("a year later: %+v, %v; want near and slack swept, far kept and reported", res, err)
	}
	last := far.SeenAt + sweepYear + 1
	if res, err := c.SweepOrphanMarkers(ctx, last, last-sweepYear); err != nil || res.Swept != 1 || res.Furthest != nil {
		t.Fatalf("a year after its stamp: %+v, %v; want far swept", res, err)
	}
}

// Design review I-1, the hazard ruling PF-1 closes: a reaped suite's base
// marker, an orphan last seen a month ago, survives a two-year backward
// clock step and its correction. Read as now during the step (never
// rewritten), it is a month old again once the clock is right.
func TestSweepOrphanMarkers_BackwardStepThenCorrectionSweepsNothing(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	orphan := SignedSeenMark{"http", "h", "/reaped/dists/s", sweepNow - 31*86400}
	putMarks(t, c, []SignedSeenMark{orphan})
	stepped := sweepNow - 2*sweepYear // the clock set back two years
	if res, err := c.SweepOrphanMarkers(ctx, stepped, stepped-sweepYear); err != nil || res.Swept != 0 ||
		res.Furthest == nil || *res.Furthest != orphan {
		t.Fatalf("during the step: %+v, %v; want nothing swept, the marker reported", res, err)
	}
	corrected := sweepNow + 86400
	if res, err := c.SweepOrphanMarkers(ctx, corrected, corrected-sweepYear); err != nil || res.Swept != 0 {
		t.Fatalf("after the correction: %+v, %v; want nothing swept", res, err)
	}
	if got := markerStamp(t, c, orphan); got != orphan.SeenAt {
		t.Fatalf("the marker's stamp is %d, want %d: never-downgrade protection lost", got, orphan.SeenAt)
	}
}

// Ruling R-F5: the delete re-checks in its own writer transaction both
// that a marker is still an orphan and that it is still a year old. A
// marker whose suite came back, or that was sighted again, between the
// sweep's listing and its delete is kept — the never-downgrade authority
// is not stripped from a suite that is tracked or seen signed again.
func TestDeleteOrphanMarkers_RechecksInTheWriter(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	cutoff := sweepNow - sweepYear
	back := SignedSeenMark{"http", "h", "/back/dists/s", cutoff - 1}
	seen := SignedSeenMark{"http", "h", "/seen/dists/s", cutoff - 1}
	putMarks(t, c, []SignedSeenMark{back, seen})
	keys, err := c.orphanMarkers(ctx, cutoff)
	if err != nil || len(keys) != 2 {
		t.Fatalf("orphanMarkers = %v, %v; want both", keys, err)
	}
	seedRow(t, c, sk("h", "/back/dists/s"), sweepNow)                          // the suite came back
	putMarks(t, c, []SignedSeenMark{{"http", "h", "/seen/dists/s", sweepNow}}) // sighted again
	if n, err := c.deleteOrphanMarkers(ctx, keys, cutoff); err != nil || n != 0 {
		t.Fatalf("deleteOrphanMarkers = %d, %v; want nothing deleted", n, err)
	}
	if markerStamp(t, c, back) != back.SeenAt || markerStamp(t, c, seen) != sweepNow {
		t.Fatal("a re-checked marker was deleted or changed")
	}
}

// Spec C9's bounded writer transactions: the sweep deletes at most 1,000
// markers per writer transaction. A failure in the second batch leaves the
// first deleted and reports it; the next sweep deletes the rest.
func TestSweepOrphanMarkers_DeletesInBatchesOfAThousand(t *testing.T) {
	c := openCache(t)
	var marks []SignedSeenMark
	for i := range 1200 {
		marks = append(marks, SignedSeenMark{"http", "h", fmt.Sprintf("/m%04d/dists/s", i), 1})
	}
	putMarks(t, c, marks)
	if _, err := c.db.Exec(`CREATE TRIGGER fail_m1100 BEFORE DELETE ON suite_signed_seen
WHEN OLD.base = '/m1100/dists/s' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, sweepNow-sweepYear)
	if err == nil || res.Swept != 1000 {
		t.Fatalf("first sweep = %+v, %v; want 1000 swept, then the second batch's error", res, err)
	}
	if markerStamp(t, c, marks[999]) != -1 || markerStamp(t, c, marks[1000]) != 1 {
		t.Fatal("the first batch is not the first 1,000 markers in key order")
	}
	if _, err := c.db.Exec(`DROP TRIGGER fail_m1100`); err != nil {
		t.Fatal(err)
	}
	if res, err := c.SweepOrphanMarkers(context.Background(), sweepNow, sweepNow-sweepYear); err != nil || res.Swept != 200 {
		t.Fatalf("second sweep = %+v, %v; want the remaining 200", res, err)
	}
}
