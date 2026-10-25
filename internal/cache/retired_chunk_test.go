package cache

import "testing"

// Deferred minor F-2 (its mutant C22): a sub-job C chunk asked for 0 rows
// or fewer — what the snapshot pass asks for — deletes retiredChunkRows
// (10,000) rows at most in its transaction, and any other request its own
// number. One transaction of a whole 600,000-row snapshot would hold the
// writer about 5 s.
func TestEffectiveChunkRows(t *testing.T) {
	for _, tc := range []struct{ asked, want int }{{0, 10_000}, {-1, 10_000}, {1, 1}, {500, 500}} {
		if got := effectiveChunkRows(tc.asked); got != tc.want {
			t.Errorf("effectiveChunkRows(%d) = %d, want %d", tc.asked, got, tc.want)
		}
	}
}
