package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// chaosCount runs a COUNT query on its own connection to c.
func chaosCount(t *testing.T, c *cache.Cache, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countMsgs counts logs' JSON records by msg.
func countMsgs(logs *lockedBuffer) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil {
			if msg, ok := m["msg"].(string); ok {
				out[msg]++
			}
		}
	}
	return out
}

// Review Focus 5, spec §7 C9's reap-vs-adoption chaos: for two virtual
// days, clients come and go on four suites (creating, touching and
// checking them) under a suite cap of three (design review M-6: a fourth
// suite's creation is refused, or evicts an idle one, while a suite is
// reaped or forgotten), the upstream publishes new content that their
// checks adopt behind a flip barrier, an operator forgets suites, and the
// reaper runs hourly with a three-hour TTL, all at once. Nothing
// deadlocks or panics; afterwards every current snapshot is adopted and
// not retired, every snapshot of a suite without a row is retired, every
// current snapshot's member blobs are there, the table holds at most
// three rows, and the lock map, the flip gate and the touch map hold no
// more entries than there are suite rows; once everything has stopped the
// goroutines are back at the count before the run. The run reaped, forgot,
// refused at the cap and re-adopted suites (lower bounds).
func TestDoD_ReapRacesAdoptionsAndClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := simtest.OpenCache(t)
		gate, err := flipgate.New(flipgate.Config{Quiet: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		logs := &lockedBuffer{}
		logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		members := newFakeFetcher()
		up := &rigUpstream{bodies: map[string][]byte{}, sent: map[string][]string{}}
		ad, err := NewAdopter(AdoptionConfig{Cache: c, Fetcher: members, Verifier: passThroughVerifier{},
			HostLimiter: hostsem.New(8), MaxConcurrent: 2, Logger: logger, FlipGate: gate})
		if err != nil {
			t.Fatal(err)
		}
		life, stop := context.WithCancel(ctx)
		ck, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
			Refresh: 15 * time.Minute, Adopter: ad, LifetimeCtx: life, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		rp, err := NewReaper(ReaperConfig{Cache: c, Checker: ck, IdleTTL: 3 * time.Hour, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		suites := []string{"/r/dists/s0", "/r/dists/s1", "/r/dists/s2", "/r/dists/s3"}
		caps := cache.SuiteCaps{Global: 3}
		var refused atomic.Int64
		var pubMu sync.Mutex
		publish := func(s string, gen int) {
			pubMu.Lock()
			defer pubMu.Unlock()
			pkgs := fakePackagesStanzas(map[string]string{fmt.Sprintf("pool/main/f/foo_%d.deb", gen): testHex64})
			members.put("http://h"+s+"/main/binary-amd64/Packages", pkgs)
			up.setAt("http://h"+s+"/InRelease", simtest.ClearsignedFixture(string(hashRelease(
				fmt.Sprintf("Origin: %s generation %d\n", s, gen), true, false,
				map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil))))
		}
		for _, s := range suites {
			publish(s, 0)
		}
		var wg sync.WaitGroup
		spawn := func(f func()) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				f()
			}()
		}
		sleep := func(d time.Duration) bool {
			select {
			case <-life.Done():
				return false
			case <-time.After(d):
				return true
			}
		}
		base := baseGoroutines()
		spawn(func() { rp.Run(life) })
		for i, s := range suites { // the clients: a suite's client returns every 20 minutes to 5 hours
			rng := rand.New(rand.NewPCG(uint64(i), 1))
			spawn(func() {
				key := cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: s}
				for sleep(time.Duration(20+rng.IntN(280)) * time.Minute) {
					ok, err := c.HasSuiteRow(ctx, key)
					if err != nil {
						t.Error(err)
						return
					}
					if ok {
						_ = c.TouchSuite(ctx, key)
					} else {
						res, err := c.CreateSuite(ctx, cache.SuiteSeed{SuiteKey: key, RequestedAt: time.Now().Unix()}, caps, ck)
						if err != nil {
							t.Error(err)
							return
						}
						if res.Outcome == cache.SuiteRejectedGlobal {
							refused.Add(1) // served untracked: no row, so no anchor row to seed or suite to check
							continue
						}
						seedStaleAnchorAt(t, c, "h", s)
					}
					ck.Check(ctx, "http", "h", s) // its T1 check
				}
			})
		}
		spawn(func() { // the upstream publishes a new generation of a suite every five hours
			rng := rand.New(rand.NewPCG(7, 7))
			for gen := 1; sleep(5 * time.Hour); gen++ {
				publish(suites[rng.IntN(len(suites))], gen)
			}
		})
		spawn(func() { // an operator forgets a suite every seven hours
			rng := rand.New(rand.NewPCG(9, 9))
			for sleep(7 * time.Hour) {
				if _, err := rp.Forget(ctx, "http", "h", suites[rng.IntN(len(suites))]); err != nil {
					t.Error(err)
				}
			}
		})
		time.Sleep(48 * time.Hour)
		stop()
		wg.Wait()
		ck.WaitForAdoptions()
		time.Sleep(time.Minute) // the barrier's quiet periods end; idle gate entries are sweepable
		for _, q := range []struct{ what, sql string }{
			{"current snapshots retired or never adopted", `SELECT COUNT(*) FROM suite_freshness sf JOIN suite_snapshot ss
  ON ss.snapshot_id = sf.current_snapshot_id WHERE ss.retired_at IS NOT NULL OR ss.adopted_at IS NULL`},
			{"current pointers to no snapshot", `SELECT COUNT(*) FROM suite_freshness WHERE current_snapshot_id IS NOT NULL
  AND current_snapshot_id NOT IN (SELECT snapshot_id FROM suite_snapshot)`},
			{"live snapshots of suites without a row", `SELECT COUNT(*) FROM suite_snapshot ss WHERE ss.retired_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM suite_freshness sf WHERE sf.canonical_scheme = ss.canonical_scheme
  AND sf.canonical_host = ss.canonical_host AND sf.suite_path = ss.suite_path)`},
		} {
			if n := chaosCount(t, c, q.sql); n != 0 {
				t.Errorf("%d %s", n, q.what)
			}
		}
		rows, err := c.ListSuitesWithAdoption(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range rows {
			if s.CurrentSnapshotID == nil {
				continue
			}
			ms, err := c.ListSnapshotMembers(ctx, *s.CurrentSnapshotID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if ok, err := c.BlobExists(m.BlobHash); err != nil || !ok {
					t.Errorf("%s: member %s's blob is gone (%v, %v)", s.SuitePath, m.Path, ok, err)
				}
			}
		}
		if n := len(rows); n > caps.Global || ck.LockCount() > n || gate.Len() > n || c.TouchedSuites() > n {
			t.Errorf("%d rows (cap %d); lock map %d, flip gate %d, touch map %d entries", n, caps.Global, ck.LockCount(),
				gate.Len(), c.TouchedSuites())
		}
		if n := sampleGoroutines(base); n > base {
			t.Errorf("%d goroutines after the run, %d before it", n, base)
		}
		got := countMsgs(logs)
		if got["suite_reaped"] == 0 || got["suite_forgotten"] == 0 || refused.Load() == 0 || got["adoption_success"] <= len(suites) {
			t.Errorf("reaps %d, forgets %d, refusals at the cap %d, adoptions %d; want reaps, forgets, refusals and re-adoptions",
				got["suite_reaped"], got["suite_forgotten"], refused.Load(), got["adoption_success"])
		}
	})
}
