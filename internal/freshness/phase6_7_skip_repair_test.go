package freshness

// SPEC6_7 coverage: recording integrity-class member skips for repair,
// in-adoption retry, and the freshness-tick repair pass. Born from the
// 2026-06-09 incident where a round-robin mirror served the previous
// publication generation for Contents-amd64.gz during adoption; the
// member was skipped and the suite then served authoritative 404s for
// it until upstream re-published InRelease ~17h later.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// currentSnapshotID resolves the suite's current snapshot id, failing
// the test when the suite has not adopted.
func currentSnapshotID(t *testing.T, env *adoptionTestEnv) int64 {
	t.Helper()
	sf, err := env.cache.GetSuiteFreshness(context.Background(),
		env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if err != nil {
		t.Fatalf("GetSuiteFreshness: %v", err)
	}
	if sf.CurrentSnapshotID == nil {
		t.Fatal("suite has no current snapshot")
	}
	return *sf.CurrentSnapshotID
}

// TestAdopter_IntegritySkipRecordedForRepair: an optional member
// skipped over an integrity failure (the stale-mirror signature) must
// land a snapshot_skipped_member row carrying its declaration — the
// trust anchor the repair pass re-fetches against. A 4xx skip on a
// publication artifact must NOT be repairable.
func TestAdopter_IntegritySkipRecordedForRepair(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newTolerantAdopter(t, env)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	declaredContents := []byte(strings.Repeat("X", 1000))
	releaseText, declared := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          declaredContents,
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short")) // served 5 vs declared 1000
	env.fetcher.fail404(base + "Contents-amd64")               // permanent decoy

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption should tolerate the optional failures, got %v", err)
	}
	snapID := currentSnapshotID(t, env)

	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatalf("ListRepairableSkippedMembers: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("repairable rows = %d, want 1 (only the integrity skip): %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Path != "Contents-amd64.gz" {
		t.Errorf("path = %q, want Contents-amd64.gz", got.Path)
	}
	if got.DeclaredSHA256 != declared["Contents-amd64.gz"] {
		t.Errorf("declared_sha256 = %s, want %s", got.DeclaredSHA256, declared["Contents-amd64.gz"])
	}
	if got.Size != 1000 {
		t.Errorf("size = %d, want 1000", got.Size)
	}
	if got.Reason != cache.SkipReasonOptionalMemberIntegrity {
		t.Errorf("reason = %q, want %q", got.Reason, cache.SkipReasonOptionalMemberIntegrity)
	}
	if got.Detail != "served 5 vs declared 1000" {
		t.Errorf("detail = %q, want \"served 5 vs declared 1000\"", got.Detail)
	}
}

// TestAdopter_SuccessLogCarriesIntegritySkipCount: the adoption_success
// line must break the integrity-class skips out of skipped_count so an
// operator (or alert) can see "this snapshot went live degraded"
// without re-deriving it from per-member WARN lines.
func TestAdopter_SuccessLogCarriesIntegritySkipCount(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	var logBuf bytes.Buffer
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        env.fetcher,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		Logger:                         slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("X", 1000)),
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short"))
	env.fetcher.fail404(base + "Contents-amd64")

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := logBuf.String()
	var successLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "msg=adoption_success") {
			successLine = line
		}
	}
	if successLine == "" {
		t.Fatalf("no adoption_success line in log output:\n%s", out)
	}
	if !strings.Contains(successLine, "skipped_integrity_count=1") {
		t.Errorf("adoption_success missing skipped_integrity_count=1: %s", successLine)
	}
	// The aggregate count still includes both skip classes.
	if !strings.Contains(successLine, "skipped_count=2") {
		t.Errorf("adoption_success missing skipped_count=2: %s", successLine)
	}
}

// seqFetcher serves a per-URL SEQUENCE of canned outcomes (body or
// error), consuming one per Fetch call and repeating the final entry
// once the sequence is exhausted. Models a mirror whose answer changes
// between attempts — the mid-sync round-robin pool from the 2026-06-09
// incident.
type seqFetcher struct {
	mu   sync.Mutex
	seq  map[string][]seqStep
	seen []string
}

type seqStep struct {
	body []byte
	err  error
}

func newSeqFetcher() *seqFetcher {
	return &seqFetcher{seq: make(map[string][]seqStep)}
}

func (f *seqFetcher) push(url string, body []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq[url] = append(f.seq[url], seqStep{body: body, err: err})
}

// countFor reports how many Fetch calls targeted url.
func (f *seqFetcher) countFor(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.seen {
		if s == url {
			n++
		}
	}
	return n
}

func (f *seqFetcher) Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	f.mu.Lock()
	f.seen = append(f.seen, target.URL)
	steps, ok := f.seq[target.URL]
	if !ok || len(steps) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("seqFetcher: no canned response for %s", target.URL)
	}
	step := steps[0]
	if len(steps) > 1 {
		f.seq[target.URL] = steps[1:]
	}
	f.mu.Unlock()
	if step.err != nil {
		return nil, step.err
	}
	if _, err := dst.Write(step.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(step.body))}, nil
}

// makeReleaseAcquireByHash mirrors makeRelease but advertises
// Acquire-By-Hash: yes, so adoptMember prefers the content-addressed
// by-hash URL per member.
func makeReleaseAcquireByHash(members map[string][]byte) ([]byte, map[string]string) {
	hashes := make(map[string]string, len(members))
	var sb strings.Builder
	sb.WriteString("Origin: Test\nSuite: noble\nAcquire-By-Hash: yes\nSHA256:\n")
	for p, body := range members {
		h := shaOf(body)
		hashes[p] = h
		fmt.Fprintf(&sb, " %s %d %s\n", h, len(body), p)
	}
	return []byte(sb.String()), hashes
}

// newRetryAdopter builds a tolerant Adopter with member retry enabled
// and an instant, recorded sleep seam.
func newRetryAdopter(t *testing.T, c *cache.Cache, f AdoptionFetcher, retries int, delay time.Duration, sleeps *[]time.Duration) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{
		Cache:                          c,
		Fetcher:                        f,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: true,
		MemberRetryCount:               retries,
		MemberRetryDelay:               delay,
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	ad.retrySleep = func(ctx context.Context, d time.Duration) error {
		*sleeps = append(*sleeps, d)
		return nil
	}
	return ad
}

// TestAdopter_MemberRetryHealsStaleMirrorViaByHash is the 2026-06-09
// incident replay: a round-robin mirror mid-sync 404s the by-hash URL
// for a root-level Contents-amd64.gz while its canonical path serves
// the PREVIOUS publication generation (Content-Length mismatch). One
// retry later the by-hash blob has synced; the member adopts intact —
// no skip, no degraded snapshot, no repair debt.
func TestAdopter_MemberRetryHealsStaleMirrorViaByHash(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	goodContents := []byte(strings.Repeat("C", 100))
	staleContents := []byte(strings.Repeat("c", 90)) // previous generation: wrong size
	releaseText, declared := makeReleaseAcquireByHash(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          goodContents,
	})

	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	sf := newSeqFetcher()
	// Packages by-hash: good from the first attempt.
	sf.push(base+"main/binary-amd64/by-hash/SHA256/"+declared["main/binary-amd64/Packages"], pkgs, nil)
	// Contents by-hash: not yet synced (404), then synced.
	contentsByHash := base + "by-hash/SHA256/" + declared["Contents-amd64.gz"]
	sf.push(contentsByHash, nil, &fetch.StatusError{Code: 404})
	sf.push(contentsByHash, goodContents, nil)
	// Contents canonical: serves the stale generation throughout.
	sf.push(base+"Contents-amd64.gz", staleContents, nil)

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, sf, 2, 25*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "Contents-amd64.gz"); err != nil {
		t.Errorf("Contents-amd64.gz member missing after retry heal: %v", err)
	}
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("repairable rows = %d, want 0 (retry healed in-adoption)", len(rows))
	}
	if len(sleeps) != 1 || sleeps[0] != 25*time.Millisecond {
		t.Errorf("sleeps = %v, want [25ms]", sleeps)
	}
	if got := sf.countFor(contentsByHash); got != 2 {
		t.Errorf("by-hash attempts = %d, want 2", got)
	}
}

// TestAdopter_MemberRetryExhaustedStillSkips: when every retry fails,
// the tolerant skip path runs exactly as before — adoption succeeds,
// and the skip is recorded for the freshness-tick repair pass.
func TestAdopter_MemberRetryExhaustedStillSkips(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("X", 1000)),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte("short")) // wrong size, every attempt

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 10*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	snapID := currentSnapshotID(t, env)
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("repairable rows = %d, want 1", len(rows))
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"Contents-amd64.gz" {
			attempts++
		}
	}
	if attempts != 3 {
		t.Errorf("canonical attempts = %d, want 3 (1 + 2 retries)", attempts)
	}
	if len(sleeps) != 2 {
		t.Errorf("sleeps = %v, want 2 entries", sleeps)
	}
}

// TestAdopter_404SkipNotRetried: a 404/410 on a declared member is the
// permanent "declared but does not serve" publication artifact — it
// must skip immediately, never burn retry delay.
func TestAdopter_404SkipNotRetried(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64":             []byte("uncompressed decoy"),
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.fail404(base + "Contents-amd64")

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 2, 10*time.Millisecond, &sleeps)

	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"Contents-amd64" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Errorf("404 member attempts = %d, want 1 (no retry)", attempts)
	}
	if len(sleeps) != 0 {
		t.Errorf("sleeps = %v, want none", sleeps)
	}
	snapID := currentSnapshotID(t, env)
	rows, err := env.cache.ListRepairableSkippedMembers(ctx, snapID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("4xx skip must not be recorded as repairable: %+v", rows)
	}
}

// TestAdopter_IndexTargetRetriedThenFatal: retries apply to
// IndexTargets too (they benefit most — a deferred adoption costs a
// whole tick), but exhaustion stays FATAL: the REC 1 fatality boundary
// is untouched.
func TestAdopter_IndexTargetRetriedThenFatal(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)

	good := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": good,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", []byte("short, wrong length"))

	var sleeps []time.Duration
	ad := newRetryAdopter(t, env.cache, env.fetcher, 1, 10*time.Millisecond, &sleeps)

	err := ad.Run(ctx, env.suite, releaseText, "", "")
	if !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("want ErrAdoptionMemberFetchFailed after exhausted retries, got %v", err)
	}
	attempts := 0
	for _, u := range env.fetcher.requested() {
		if u == base+"main/binary-amd64/Packages" {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("IndexTarget attempts = %d, want 2 (1 + 1 retry)", attempts)
	}
}

func TestNewAdopter_RejectsNegativeRetryConfig(t *testing.T) {
	env := newAdoptionTestEnv(t)
	if _, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(1), MemberRetryCount: -1,
	}); err == nil {
		t.Error("MemberRetryCount=-1 accepted, want error")
	}
	if _, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(1), MemberRetryDelay: -time.Second,
	}); err == nil {
		t.Error("MemberRetryDelay=-1s accepted, want error")
	}
}
