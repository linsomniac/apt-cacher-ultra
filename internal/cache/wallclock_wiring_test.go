package cache

import (
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Persisted cache stamps follow the shared wall clock, so simulations
// can step it.
func TestNowUnix_FollowsWallclock(t *testing.T) {
	defer wallclock.SetOffsetForTest(-48 * time.Hour)()
	if d := time.Now().Unix() - nowUnix(); d < 48*3600-5 || d > 48*3600+5 {
		t.Fatalf("nowUnix is %ds behind real time, want ~172800", d)
	}
}
