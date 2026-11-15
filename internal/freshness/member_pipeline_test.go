package freshness

// The member pipeline (Step 5): members are fetched MemberFetchWorkers at a
// time, a member the suite's current snapshot does not serve is asked for
// at its declared path first (one request for a file upstream never
// serves, not two), and members whose failure may heal share one retry
// wait per round. The 2026-11-14 publication-delay review.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

const pipelineBase = "http://archive.ubuntu.com/ubuntu/dists/noble/"

// byHashURL is the by-hash URL adoption probes for a member.
func byHashURL(path, sha256hex string) string {
	return pipelineBase + path[:strings.LastIndex(path, "/")+1] + "by-hash/SHA256/" + sha256hex
}

// countURL reports how many of urls equal want.
func countURL(urls []string, want string) int {
	n := 0
	for _, u := range urls {
		if u == want {
			n++
		}
	}
	return n
}

// pipelineGeneration publishes one generation of a by-hash suite on
// env.fetcher: the amd64 Packages index, served by hash, and absent
// members that answer 404 at both their by-hash and declared URLs unless
// the test serves them afterwards. Every generation has other bytes, so
// every declared hash is new.
func pipelineGeneration(env *adoptionTestEnv, gen int, absent []string) (release []byte, declared map[string]string, bodies map[string][]byte) {
	pkgs := fakePackagesStanzas(map[string]string{
		fmt.Sprintf("pool/main/f/foo/foo_%d.deb", gen): strings.Repeat("a", 64),
	})
	bodies = map[string][]byte{"main/binary-amd64/Packages": pkgs}
	for _, p := range absent {
		bodies[p] = []byte(fmt.Sprintf("generation %d of %s", gen, p))
	}
	release, declared = makeReleaseAcquireByHash(bodies)
	// Clearsigned, as an adopted suite's anchor is: the next adoption reads
	// what this one declared from the stored anchor.
	release = asInRelease(append(release, []byte(fmt.Sprintf("Generation: %d\n", gen))...))
	env.fetcher.put(byHashURL("main/binary-amd64/Packages", declared["main/binary-amd64/Packages"]), pkgs)
	for _, p := range absent {
		env.fetcher.fail404(byHashURL(p, declared[p]))
		env.fetcher.fail404(pipelineBase + p)
	}
	return release, declared, bodies
}

// A file the Release declares and upstream never serves (the uncompressed
// variants on Debian, the foreign architectures on Ubuntu's main archive)
// costs two requests at a suite's first adoption — by hash, then by path —
// and one, by path, at every later one: the suite's current snapshot shows
// it was not served.
func TestAdopter_NeverServedMemberIsAskedOnceAfterTheFirstAdoption(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	absent := []string{
		"main/binary-armhf/Packages.gz",
		"main/binary-arm64/Packages.xz",
		"universe/binary-amd64/Packages",
		"main/i18n/Translation-en",
		"Contents-armhf",
	}

	rel1, decl1, _ := pipelineGeneration(env, 1, absent)
	if err := env.adopter.Run(ctx, env.suite, rel1, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	first := env.fetcher.requested()
	for _, p := range absent {
		if n := countURL(first, byHashURL(p, decl1[p])); n != 1 {
			t.Errorf("first adoption: %d by-hash requests for %s, want 1", n, p)
		}
		if n := countURL(first, pipelineBase+p); n != 1 {
			t.Errorf("first adoption: %d declared-path requests for %s, want 1", n, p)
		}
	}

	rel2, decl2, _ := pipelineGeneration(env, 2, absent)
	if err := env.adopter.Run(ctx, env.suite, rel2, "", ""); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	second := env.fetcher.requested()[len(first):]
	for _, p := range absent {
		if n := countURL(second, byHashURL(p, decl2[p])); n != 0 {
			t.Errorf("second adoption: %d by-hash requests for %s, want 0: its path answers 404", n, p)
		}
		if n := countURL(second, pipelineBase+p); n != 1 {
			t.Errorf("second adoption: %d declared-path requests for %s, want 1", n, p)
		}
	}
	if want := 1 + len(absent); len(second) != want {
		t.Errorf("second adoption made %d upstream requests, want %d (the index, and one per never-served member): %v",
			len(second), want, second)
	}
}

// A member that was not served at the last adoption and is served now is
// adopted from its declared path.
func TestAdopter_PreviouslyAbsentMemberThatAppearsIsAdopted(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	absent := []string{"main/i18n/Translation-de"}

	rel1, _, _ := pipelineGeneration(env, 1, absent)
	if err := env.adopter.Run(ctx, env.suite, rel1, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	rel2, decl2, bodies2 := pipelineGeneration(env, 2, absent)
	env.fetcher.put(pipelineBase+absent[0], bodies2[absent[0]])
	delete(env.fetcher.errFor, pipelineBase+absent[0])
	if err := env.adopter.Run(ctx, env.suite, rel2, "", ""); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	mem, err := env.cache.GetSnapshotMember(ctx, currentSnapshotID(t, env), absent[0])
	if err != nil {
		t.Fatalf("the member that appeared is not in the snapshot: %v", err)
	}
	if mem.BlobHash != decl2[absent[0]] {
		t.Errorf("blob = %s, want the declared %s", mem.BlobHash, decl2[absent[0]])
	}
}

// Asking the declared path first must not lose the by-hash heal: when that
// path exists but serves another generation's bytes (a mirror mid-sync),
// the by-hash URL is tried before the member fails.
func TestAdopter_PreviouslyAbsentMemberStaleAtItsPathHealsByHash(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	absent := []string{"main/i18n/Translation-de"}

	rel1, _, _ := pipelineGeneration(env, 1, absent)
	if err := env.adopter.Run(ctx, env.suite, rel1, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	before := len(env.fetcher.requested())
	rel2, decl2, bodies2 := pipelineGeneration(env, 2, absent)
	p := absent[0]
	env.fetcher.put(pipelineBase+p, []byte("the previous generation's bytes"))
	delete(env.fetcher.errFor, pipelineBase+p)
	env.fetcher.put(byHashURL(p, decl2[p]), bodies2[p])
	delete(env.fetcher.errFor, byHashURL(p, decl2[p]))
	if err := env.adopter.Run(ctx, env.suite, rel2, "", ""); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	if _, err := env.cache.GetSnapshotMember(ctx, currentSnapshotID(t, env), p); err != nil {
		t.Fatalf("the member is not in the snapshot: %v", err)
	}
	second := env.fetcher.requested()[before:]
	path, hash := -1, -1
	for i, u := range second {
		switch u {
		case pipelineBase + p:
			path = i
		case byHashURL(p, decl2[p]):
			hash = i
		}
	}
	if path < 0 || hash < 0 || path > hash {
		t.Errorf("requests = %v: want the declared path, then the by-hash URL", second)
	}
}

// An index apt needs — binary-all, which the serve-contract guard requires
// of every snapshot — keeps the full sequence, by hash first, whether or
// not the last adoption found it: nothing is saved on the files that fail
// `apt update` when they are missing.
func TestAdopter_RequiredIndexKeepsTheByHashProbe(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	// binary-all/Packages.xz is absent; the group is served by its plain
	// variant, so the guard lets the snapshot commit.
	absent := []string{"main/binary-all/Packages.xz"}
	publish := func(gen int) ([]byte, map[string]string) {
		_, _, bodies := pipelineGeneration(env, gen, absent)
		all := fakePackagesStanzas(map[string]string{
			fmt.Sprintf("pool/main/d/doc/doc_%d_all.deb", gen): strings.Repeat("b", 64),
		})
		bodies["main/binary-all/Packages"] = all
		rel, decl := makeReleaseAcquireByHash(bodies)
		rel = asInRelease(append(rel, []byte(fmt.Sprintf("Generation: %d\n", gen))...))
		env.fetcher.put(byHashURL("main/binary-all/Packages", decl["main/binary-all/Packages"]), all)
		env.fetcher.put(byHashURL("main/binary-amd64/Packages", decl["main/binary-amd64/Packages"]), bodies["main/binary-amd64/Packages"])
		for _, p := range absent {
			env.fetcher.fail404(byHashURL(p, decl[p]))
		}
		return rel, decl
	}
	rel1, _ := publish(1)
	if err := env.adopter.Run(ctx, env.suite, rel1, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	before := len(env.fetcher.requested())
	rel2, decl2 := publish(2)
	if err := env.adopter.Run(ctx, env.suite, rel2, "", ""); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	second := env.fetcher.requested()[before:]
	if n := countURL(second, byHashURL(absent[0], decl2[absent[0]])); n != 1 {
		t.Errorf("by-hash requests for the required index = %d, want 1: %v", n, second)
	}
}

// Members caught mid-sync wait together: the retry delay passes once per
// round for all of them, not once per member. Six stale files cost two
// waits, not twelve.
func TestAdopter_StaleMembersShareOneRetryWait(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	const stale = 6

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	members := map[string][]byte{"main/binary-amd64/Packages": pkgs}
	for i := 0; i < stale; i++ {
		members[fmt.Sprintf("Contents-arch%d.gz", i)] = []byte(strings.Repeat("C", 100+i))
	}
	releaseText, _ := makeRelease(members)
	env.fetcher.put(pipelineBase+"main/binary-amd64/Packages", pkgs)
	for i := 0; i < stale; i++ {
		env.fetcher.put(pipelineBase+fmt.Sprintf("Contents-arch%d.gz", i), []byte("previous generation")) // wrong size
	}

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 30*time.Second, &sleeps)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sleeps) != 2 || sleeps[0] != 30*time.Second || sleeps[1] != 30*time.Second {
		t.Errorf("retry waits = %v, want [30s 30s]: one per round, whatever the number of stale members", sleeps)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, currentSnapshotID(t, env))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != stale {
		t.Errorf("repairable skip records = %d, want %d", len(rows), stale)
	}
	for i := 0; i < stale; i++ {
		if n := countURL(env.fetcher.requested(), pipelineBase+fmt.Sprintf("Contents-arch%d.gz", i)); n != 3 {
			t.Errorf("Contents-arch%d.gz was requested %d times, want 3 (the attempt and two retries)", i, n)
		}
	}
}

// A retry round asks again only for the members that still fail, and the
// rounds end as soon as none does.
func TestAdopter_RetryRoundAsksOnlyForTheMembersStillFailing(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	good := map[string][]byte{
		"Contents-a.gz": []byte(strings.Repeat("A", 101)),
		"Contents-b.gz": []byte(strings.Repeat("B", 102)),
		"Contents-c.gz": []byte(strings.Repeat("C", 103)),
	}
	members := map[string][]byte{"main/binary-amd64/Packages": pkgs}
	for p, b := range good {
		members[p] = b
	}
	releaseText, _ := makeRelease(members)

	sf := newSeqFetcher()
	sf.push(pipelineBase+"main/binary-amd64/Packages", pkgs, nil)
	stale := []byte("previous generation")
	// a and b heal after one wait, c after two.
	for _, p := range []string{"Contents-a.gz", "Contents-b.gz"} {
		sf.push(pipelineBase+p, stale, nil)
		sf.push(pipelineBase+p, good[p], nil)
	}
	sf.push(pipelineBase+"Contents-c.gz", stale, nil)
	sf.push(pipelineBase+"Contents-c.gz", stale, nil)
	sf.push(pipelineBase+"Contents-c.gz", good["Contents-c.gz"], nil)

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, sf, 2, 30*time.Second, &sleeps)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sleeps) != 2 {
		t.Errorf("retry waits = %v, want two", sleeps)
	}
	snap := currentSnapshotID(t, env)
	for p := range good {
		if _, err := env.cache.GetSnapshotMember(ctx, snap, p); err != nil {
			t.Errorf("%s is not in the snapshot: %v", p, err)
		}
	}
	for p, want := range map[string]int{"Contents-a.gz": 2, "Contents-b.gz": 2, "Contents-c.gz": 3, "main/binary-amd64/Packages": 1} {
		if n := sf.countFor(pipelineBase + p); n != want {
			t.Errorf("%s was requested %d times, want %d", p, n, want)
		}
	}
	if rows, err := env.cache.ListRepairableSkippedMembers(ctx, snap); err != nil || len(rows) != 0 {
		t.Errorf("repairable skip records = %d (err %v), want none: every member healed in the adoption", len(rows), err)
	}
}

// gateFetcher holds every Fetch until want of them are in flight at once,
// then lets all pass; a pipeline that never reaches want fails its fetches
// when the wait times out. It records the most Fetches in flight at once.
type gateFetcher struct {
	inner   AdoptionFetcher
	want    int32
	open    chan struct{}
	once    sync.Once
	flying  atomic.Int32
	highest atomic.Int32
}

func (g *gateFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	n := g.flying.Add(1)
	defer g.flying.Add(-1)
	for {
		h := g.highest.Load()
		if n <= h || g.highest.CompareAndSwap(h, n) {
			break
		}
	}
	if n >= g.want {
		g.once.Do(func() { close(g.open) })
	}
	select {
	case <-g.open:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("gateFetcher: %d fetches never ran at once", g.want)
	}
	return g.inner.Fetch(ctx, target, dst)
}

// Members are fetched MemberFetchWorkers at a time, never more: the
// per-host limit they share with client downloads keeps its headroom.
func TestAdopter_MembersAreFetchedConcurrently(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	members := map[string][]byte{"main/binary-amd64/Packages": pkgs}
	for i := 0; i < 11; i++ {
		members[fmt.Sprintf("Contents-arch%02d.gz", i)] = []byte(fmt.Sprintf("contents of architecture %d", i))
	}
	releaseText, _ := makeRelease(members)
	for p, b := range members {
		env.fetcher.put(pipelineBase+p, b)
	}
	gate := &gateFetcher{inner: env.fetcher, want: MemberFetchWorkers, open: make(chan struct{})}
	env.adopter.fetcher = gate

	if err := env.adopter.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := gate.highest.Load(); got != MemberFetchWorkers {
		t.Errorf("most member fetches in flight at once = %d, want %d", got, MemberFetchWorkers)
	}
	members2, err := env.cache.ListSnapshotMembers(ctx, currentSnapshotID(t, env))
	if err != nil {
		t.Fatal(err)
	}
	// 12 members, the InRelease self row, and by-hash aliases.
	if len(members2) < 13 {
		t.Errorf("snapshot has %d member rows, want at least 13", len(members2))
	}
}

// Members with the same bytes under several paths (the empty indexes of a
// quiet component) are downloaded once, as when members were fetched one
// after another: the others reuse the pool blob.
func TestAdopter_IdenticalMembersAreDownloadedOnce(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	same := []byte("the same bytes under every path")
	paths := []string{"contrib/i18n/Translation-en", "non-free/i18n/Translation-en", "extra/i18n/Translation-en", "more/i18n/Translation-en"}
	members := map[string][]byte{"main/binary-amd64/Packages": pkgs}
	for _, p := range paths {
		members[p] = same
	}
	releaseText, _ := makeRelease(members)
	for p, b := range members {
		env.fetcher.put(pipelineBase+p, b)
	}
	if err := env.adopter.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	downloads := 0
	for _, p := range paths {
		downloads += countURL(env.fetcher.requested(), pipelineBase+p)
	}
	if downloads != 1 {
		t.Errorf("identical members were downloaded %d times, want once", downloads)
	}
	snap := currentSnapshotID(t, env)
	for _, p := range paths {
		if _, err := env.cache.GetSnapshotMember(ctx, snap, p); err != nil {
			t.Errorf("%s is not in the snapshot: %v", p, err)
		}
	}
}

// A panic in a member worker must not end the daemon: the adoption
// goroutine's recover cannot see another goroutine's panic, so the pool
// carries it there. It is then an adoption panic like any other — logged
// (with the worker's stack), recorded, and the suite's lock released.
func TestCheck_MemberWorkerPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	c := simtest.OpenCache(t)
	seedStaleAnchorAt(t, c, rigHost, rigSuite)
	ad, err := NewAdopter(AdoptionConfig{
		Cache: c, Fetcher: panickingAdoptionFetcher{}, Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8),
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	sb.WriteString("Origin: Test\nSuite: x\nSHA256:\n")
	for i := 0; i < 8; i++ {
		body := []byte(fmt.Sprintf("contents %d", i))
		fmt.Fprintf(&sb, " %s %d Contents-arch%d.gz\n", shaOf(body), len(body), i)
	}
	up := &changedUpstream{body: simtest.ClearsignedFixture(sb.String())}
	logs := &lockedBuffer{}
	chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad,
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	chk.Check(ctx, "http", rigHost, rigSuite)
	chk.WaitForAdoptions()
	out := logs.String()
	if !strings.Contains(out, "adoption_panicked") || !strings.Contains(out, "member fetch bug") {
		t.Fatalf("no adoption_panicked line with the worker's panic; logs:\n%s", out)
	}
	if !strings.Contains(out, "worker stack") {
		t.Errorf("the panic line lacks the worker's stack; logs:\n%s", out)
	}
	sf, err := c.GetSuiteFreshness(ctx, "http", rigHost, rigSuite)
	if err != nil {
		t.Fatal(err)
	}
	if sf.AdoptionFailCount != 1 || sf.AdoptionFailReason == nil || *sf.AdoptionFailReason != cache.ReasonPanic {
		t.Errorf("failure record = count %d, reason %v; want one panic", sf.AdoptionFailCount, sf.AdoptionFailReason)
	}
	// The suite's lock was released: nothing holds it now.
	if n := chk.LockCount(); n != 1 {
		t.Fatalf("lock map entries = %d, want the suite's one", n)
	}
	if mu, st := chk.lockSuite(suiteKey("http", rigHost, rigSuite)); st != lockOK {
		t.Fatalf("the suite's lock is still held after the panic (%v)", st)
	} else {
		mu.Unlock()
	}
}

// The same pool warms hot packages: a worker's panic is raised again on
// the adoption goroutine, once every worker has returned.
func TestPrefetchHotSet_WorkerPanicReachesTheCaller(t *testing.T) {
	f := newHotPrefetchFixture(t, time.Minute)
	entries := hotEntries(f, 6)
	f.adopter.fetcher = panickingAdoptionFetcher{}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("prefetchHotSet returned: the worker's panic was swallowed")
		}
		if s := fmt.Sprint(r); !strings.Contains(s, "member fetch bug") || !strings.Contains(s, "worker stack") {
			t.Errorf("panic value = %q, want the worker's panic and stack", s)
		}
	}()
	f.adopter.prefetchHotSet(context.Background(), f.suite, f.candidate, entries, nil)
}

// A member the Release declares for the first time is not one upstream
// "never serves": nothing is known about it. It keeps the full sequence,
// by hash first — a mirror mid-sync can have a new file by hash before it
// appears at its path, and asking the path first would lose it for the
// snapshot's lifetime. Only a member the current snapshot's own Release
// declared, and that was not served then, is asked for at its path first.
func TestAdopter_MemberNewInThisReleaseKeepsTheByHashProbe(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	rel1, _, _ := pipelineGeneration(env, 1, nil)
	if err := env.adopter.Run(ctx, env.suite, rel1, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}

	// The second release adds a member: by hash it is there, at its path
	// not yet.
	const added = "main/i18n/Translation-fr"
	rel2, decl2, bodies2 := pipelineGeneration(env, 2, []string{added})
	env.fetcher.put(byHashURL(added, decl2[added]), bodies2[added])
	delete(env.fetcher.errFor, byHashURL(added, decl2[added]))
	if err := env.adopter.Run(ctx, env.suite, rel2, "", ""); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	if _, err := env.cache.GetSnapshotMember(ctx, currentSnapshotID(t, env), added); err != nil {
		t.Fatalf("the member new in this release was not adopted: %v", err)
	}
}

// errSilent is what a fetch returns when it ended at a silent upstream.
var errSilent = fmt.Errorf("%w: %w", fetch.ErrUpstreamUnavailable, fetch.ErrIdleTimeout)

// A member whose upstream went silent is not tried again in the retry
// rounds: each attempt would wait out the idle timeout again. An optional
// one is skipped (and left to repair) so the release publishes, instead of
// being held for the rounds' further idle timeouts.
func TestAdopter_SilentUpstreamMemberIsNotRetriedInRounds(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("C", 100)),
	})
	env.fetcher.put(pipelineBase+"main/binary-amd64/Packages", pkgs)
	env.fetcher.failWith(pipelineBase+"Contents-amd64.gz", errSilent)

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 30*time.Second, &sleeps)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sleeps) != 0 {
		t.Errorf("retry waits = %v, want none: a silent upstream is not retried in rounds", sleeps)
	}
	if n := countURL(env.fetcher.requested(), pipelineBase+"Contents-amd64.gz"); n != 1 {
		t.Errorf("the silent member was fetched %d times, want once", n)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, currentSnapshotID(t, env))
	if err != nil || len(rows) != 1 || rows[0].Path != "Contents-amd64.gz" {
		t.Errorf("repairable skip records = %+v (err %v), want the silent member", rows, err)
	}
}

// When the by-hash probe goes silent the member's declared path is not
// asked as well: upstream is not answering for this file, and the second
// URL would wait out the idle timeout all over again.
func TestAdopter_SilentByHashProbeEndsTheMember(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	contents := []byte(strings.Repeat("C", 100))
	releaseText, decl := makeReleaseAcquireByHash(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/Contents-amd64.gz":     contents,
	})
	env.fetcher.put(byHashURL("main/binary-amd64/Packages", decl["main/binary-amd64/Packages"]), pkgs)
	env.fetcher.failWith(byHashURL("main/Contents-amd64.gz", decl["main/Contents-amd64.gz"]), errSilent)
	env.fetcher.put(pipelineBase+"main/Contents-amd64.gz", contents)

	ad := newTolerantAdopter(t, env)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := countURL(env.fetcher.requested(), pipelineBase+"main/Contents-amd64.gz"); n != 0 {
		t.Errorf("the declared path was asked %d times after the by-hash probe went silent, want none", n)
	}
}

// A member the snapshot cannot do without is another matter: if its
// by-hash URL goes silent and its declared path answers, the suite is
// adopted. Giving up on it would fail the attempt, and every retry would
// ask the silent URL first again — a suite kept unadopted by one URL.
// Robustness comes before the two idle timeouts this can cost.
func TestAdopter_RequiredIndexSilentByHashStillAsksItsPath(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, decl := makeReleaseAcquireByHash(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
	})
	env.fetcher.failWith(byHashURL("main/binary-amd64/Packages", decl["main/binary-amd64/Packages"]), errSilent)
	env.fetcher.put(pipelineBase+"main/binary-amd64/Packages", pkgs)

	ad := newTolerantAdopter(t, env)
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v: the declared path had the index", err)
	}
	if _, err := env.cache.GetSnapshotMember(ctx, currentSnapshotID(t, env), "main/binary-amd64/Packages"); err != nil {
		t.Fatalf("the index was not adopted from its declared path: %v", err)
	}
}

// With optional-member tolerance off every member's failure fails the
// attempt, so every member keeps its second URL.
func TestAdopter_WithoutToleranceASilentByHashStillAsksThePath(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	contents := []byte(strings.Repeat("C", 100))
	releaseText, decl := makeReleaseAcquireByHash(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"main/Contents-amd64.gz":     contents,
	})
	env.fetcher.put(byHashURL("main/binary-amd64/Packages", decl["main/binary-amd64/Packages"]), pkgs)
	env.fetcher.failWith(byHashURL("main/Contents-amd64.gz", decl["main/Contents-amd64.gz"]), errSilent)
	env.fetcher.put(pipelineBase+"main/Contents-amd64.gz", contents)

	if err := env.adopter.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v: the declared path had the member", err)
	}
	if _, err := env.cache.GetSnapshotMember(ctx, currentSnapshotID(t, env), "main/Contents-amd64.gz"); err != nil {
		t.Fatalf("the member was not adopted from its declared path: %v", err)
	}
}

// noIdleRetryByURL maps each fetched member URL to the NoIdleRetry its
// target carried.
func noIdleRetryByURL(f *fakeFetcher) map[string]bool {
	out := map[string]bool{}
	for _, tg := range f.fetchTargets() {
		out[tg.URL] = tg.NoIdleRetry
	}
	return out
}

// A member the snapshot can do without is fetched without the second
// chance after a silent upstream (fetch.Target.NoIdleRetry): one idle
// timeout, then it is skipped and left to repair. An index keeps the
// retry, and so does every member when optional-member tolerance is off —
// there a member's failure fails the attempt.
func TestAdopter_OnlyMembersTheSnapshotCanDoWithoutGiveUpAtTheFirstSilence(t *testing.T) {
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	contents := []byte(strings.Repeat("C", 100))
	const pkgsPath, contentsPath = "main/binary-amd64/Packages", "main/Contents-amd64.gz"
	for _, tc := range []struct {
		name     string
		tolerant bool
		want     map[string]bool // member path → NoIdleRetry
	}{
		{"tolerance on", true, map[string]bool{pkgsPath: false, contentsPath: true}},
		{"tolerance off", false, map[string]bool{pkgsPath: false, contentsPath: false}},
	} {
		env := newAdoptionTestEnv(t)
		releaseText, decl := makeReleaseAcquireByHash(map[string][]byte{pkgsPath: pkgs, contentsPath: contents})
		env.fetcher.put(byHashURL(pkgsPath, decl[pkgsPath]), pkgs)
		env.fetcher.put(byHashURL(contentsPath, decl[contentsPath]), contents)
		ad := env.adopter
		if tc.tolerant {
			ad = newTolerantAdopter(t, env)
		}
		if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
			t.Fatalf("%s: Run: %v", tc.name, err)
		}
		got := noIdleRetryByURL(env.fetcher)
		for path, want := range tc.want {
			u := byHashURL(path, decl[path])
			if v, ok := got[u]; !ok || v != want {
				t.Errorf("%s: %s fetched with NoIdleRetry=%v (fetched: %v), want %v", tc.name, path, v, ok, want)
			}
		}
	}
}
