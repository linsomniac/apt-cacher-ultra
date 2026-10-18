package freshness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// datedRelease is makeRelease's text headed by a Label (so otherwise equal
// bodies differ) and, when date != 0, a Date field in apt's format.
func datedRelease(label string, date int64, members map[string][]byte) []byte {
	rel, _ := makeRelease(members)
	hdr := "Label: " + label + "\n"
	if date != 0 {
		hdr += "Date: " + time.Unix(date, 0).UTC().Format(http.TimeFormat) + "\n"
	}
	return append([]byte(hdr), rel...)
}

// adoptDated runs one adoption of env's suite whose Release is labelled
// label and dated date (0 = no Date field).
func adoptDated(t *testing.T, env *adoptionTestEnv, ad *Adopter, label string, date int64) error {
	t.Helper()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	rel := datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": pkgs})
	return ad.Run(context.Background(), env.suite, rel, "", "")
}

func fixedNowAdopter(t *testing.T, env *adoptionTestEnv, now time.Time) *Adopter {
	t.Helper()
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

var dateNow = time.Unix(1_800_000_000, 0)

func TestAdopter_StoresReleaseDate(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "v1", d); err != nil {
		t.Fatal(err)
	}
	snap, err := env.cache.GetSuiteSnapshot(context.Background(), currentSnapshotID(t, env))
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != d {
		t.Fatalf("release_date = %v (err %v), want %d", snap.ReleaseDate, err, d)
	}
}

// Spec C5: an older Date is refused (upstream_older) after one
// verification and before any member download.
func TestAdopter_OlderDateRefusedBeforeMemberFetch(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d2 := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "v2", d2); err != nil {
		t.Fatal(err)
	}
	cur := currentSnapshotID(t, env)
	calls := env.fetcher.calls.Load()
	err := adoptDated(t, env, ad, "v1", d2-3600)
	if !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
	if got := env.fetcher.calls.Load(); got != calls {
		t.Fatalf("member fetches during a refused adoption = %d, want 0", got-calls)
	}
	if got := currentSnapshotID(t, env); got != cur {
		t.Fatalf("current snapshot moved from %d to %d", cur, got)
	}
}

// A missing or invalid Date is 0, which is older than any dated snapshot
// (apt's rule).
func TestAdopter_MissingDateAfterDatedSnapshotIsOlder(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "v2", dateNow.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "v3", 0); !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
}

// Snapshots adopted before v7 have release_date NULL: no Date rule
// applies to their successor (spec §6 notes).
func TestAdopter_UndatedLegacySnapshotImposesNoRule(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	h := shaOf([]byte("legacy anchor"))
	if err := env.cache.PutBlob(ctx, h, 13); err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{CanonicalScheme: "http",
		CanonicalHost: "archive.ubuntu.com", SuitePath: "/ubuntu/dists/noble", InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cache.CommitAdoption(ctx, id, []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "old-dated", 1_000_000_000); err != nil {
		t.Fatalf("adoption after a legacy (undated) snapshot: %v", err)
	}
}

// apt rejects a Release dated more than 10 s ahead (Max-FutureTime).
func TestAdopter_FutureDateBoundary(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	if err := adoptDated(t, env, ad, "ahead11", dateNow.Unix()+11); !errors.Is(err, ErrAdoptionReleaseDateInFuture) {
		t.Fatalf("Date now+11s: err = %v, want ErrAdoptionReleaseDateInFuture", err)
	}
	if err := adoptDated(t, env, ad, "ahead10", dateNow.Unix()+10); err != nil {
		t.Fatalf("Date now+10s: %v", err)
	}
}

// Spec C5 rollback_refused: equal-Date content equal to an adopted,
// displaced snapshot is refused, so a flapping mirror cannot flip back.
func TestAdopter_RollbackToDisplacedSnapshotRefused(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d := dateNow.Add(-time.Hour).Unix()
	if err := adoptDated(t, env, ad, "A", d); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "B", d); err != nil {
		t.Fatal(err)
	}
	cur := currentSnapshotID(t, env)
	err := adoptDated(t, env, ad, "A", d)
	if !errors.Is(err, ErrAdoptionRollbackRefused) || !errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want ErrAdoptionRollbackRefused wrapping the natural-key collision", err)
	}
	if got := currentSnapshotID(t, env); got != cur {
		t.Fatalf("rolled back: current %d -> %d", cur, got)
	}
}

// The Date rule runs before the candidate insert, so an older rollback
// is upstream_older, not rollback_refused.
func TestAdopter_OlderRollbackIsUpstreamOlder(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ad := fixedNowAdopter(t, env, dateNow)
	d1 := dateNow.Add(-2 * time.Hour).Unix()
	if err := adoptDated(t, env, ad, "A", d1); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "B", d1+3600); err != nil {
		t.Fatal(err)
	}
	if err := adoptDated(t, env, ad, "A", d1); !errors.Is(err, ErrAdoptionUpstreamOlder) {
		t.Fatalf("err = %v, want ErrAdoptionUpstreamOlder", err)
	}
}

func TestClassify_RefusalsAndNewOutcomes(t *testing.T) {
	cases := []struct {
		err             error
		outcome, reason string
	}{
		{fmt.Errorf("%w: x", ErrAdoptionUpstreamOlder), "refused", cache.ReasonUpstreamOlder},
		{fmt.Errorf("%w: %w", ErrAdoptionRollbackRefused, cache.ErrSnapshotNaturalKeyAdopted), "refused", cache.ReasonRollbackRefused},
		{fmt.Errorf("%w: x", ErrAdoptionReleaseDateInFuture), "release_date_in_future", "release_date_in_future"},
		{ErrAdoptionPanicked, "panicked", cache.ReasonPanic},
		{fmt.Errorf("fetch: %w", context.DeadlineExceeded), "timed_out", "timed_out"},
	}
	for _, tc := range cases {
		out := classifyAdoptionOutcome(tc.err)
		if out != tc.outcome {
			t.Errorf("classifyAdoptionOutcome(%v) = %q, want %q", tc.err, out, tc.outcome)
		}
		if r := classifyAdoptionReason(tc.err, out, nil); r != tc.reason {
			t.Errorf("classifyAdoptionReason(%v) = %q, want %q", tc.err, r, tc.reason)
		}
	}
}
