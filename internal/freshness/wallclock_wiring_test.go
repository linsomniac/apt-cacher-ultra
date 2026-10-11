package freshness

import (
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

func TestNew_DefaultNowFollowsWallclock(t *testing.T) {
	defer wallclock.SetOffsetForTest(-48 * time.Hour)()
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8)})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(c.now()); d < 48*time.Hour-time.Minute || d > 48*time.Hour+time.Minute {
		t.Fatalf("Checker.now is %v behind real time, want ~48h", d)
	}
}
