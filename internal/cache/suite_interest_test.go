package cache

import (
	"context"
	"testing"
)

// A suite row's client-interest stamp is set at its creation (phase F
// reaps by it) and kept by the check writer and the adoption commit: only
// the creation and TouchSuite write it (spec C3).
func TestSuiteInterestStamp_SetAtCreationKeptByWriters(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if _, err := c.CreateSuite(ctx, SuiteSeed{SuiteKey: SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite},
		RequestedAt: 1_800_000_000}, SuiteCaps{}, nil); err != nil {
		t.Fatal(err)
	}
	defer stubNow(t, 1_800_009_999)()
	later := int64(1_800_009_999)
	if err := c.UpdateSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite,
		LastCheckAt: &later}); err != nil {
		t.Fatal(err)
	}
	commitCandidate(t, c, SnapshotCandidate{}, "interest stamp")
	if s := getSuite(t, c); derefI64(s.LastRequestedAt) != 1_800_000_000 {
		t.Fatalf("last_requested_at = %d, want 1800000000 (set at creation, kept)", derefI64(s.LastRequestedAt))
	}
}
