package freshness

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

func sha512Of(b []byte) string {
	s := sha512.Sum512(b)
	return hex.EncodeToString(s[:])
}

// hashRelease builds Release text for members (path → content) with a
// SHA256 block, a SHA512 block, or both (spec C5), in path order, after
// the given header lines. forge maps a path to a block ("SHA256" or
// "SHA512") whose entry declares another content's hash.
func hashRelease(header string, with256, with512 bool, members map[string][]byte, forge map[string]string) []byte {
	paths := make([]string, 0, len(members))
	for p := range members {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(header)
	block := func(name string, sum func([]byte) string) {
		b.WriteString(name + ":\n")
		for _, p := range paths {
			body := members[p]
			if forge[p] == name {
				body = []byte("forged " + p)
			}
			fmt.Fprintf(&b, " %s %d %s\n", sum(body), len(members[p]), p)
		}
	}
	if with256 {
		block("SHA256", shaOf)
	}
	if with512 {
		block("SHA512", sha512Of)
	}
	return []byte(b.String())
}

// Spec C5 "Hash blocks": ParseRelease merges the SHA256 and SHA512 blocks
// per member path, as apt does; the size comes from the SHA512 entry when
// there is one; members declared only in MD5Sum/SHA1 are not returned.
func TestParseRelease_MergesHashBlocks(t *testing.T) {
	h256, h512 := sha(1), strings.Repeat("a", 128)
	o512 := strings.Repeat("b", 128)
	body := "Origin: x\n" +
		"MD5Sum:\n 0123456789abcdef0123456789abcdef 9 md5/only\n" +
		"SHA512:\n" +
		" " + h512 + " 20 both/Packages\n" +
		" " + o512 + " 30 only512/Packages\n" +
		"SHA256:\n" +
		" " + h256 + " 10 both/Packages\n" +
		" " + sha(2) + " 40 only256/Packages\n"
	got, err := ParseRelease([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseMember{
		{Path: "both/Packages", SHA256: h256, SHA512: h512, Size: 20},
		{Path: "only512/Packages", SHA512: o512, Size: 30},
		{Path: "only256/Packages", SHA256: sha(2), Size: 40},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("members = %+v\nwant %+v", got, want)
	}
	// A SHA512 block alone is a complete Release (libnvidia-container).
	got, err = ParseRelease([]byte("SHA512:\n " + o512 + " 30 main/binary-amd64/Packages\n"))
	if err != nil || len(got) != 1 || got[0].SHA512 != o512 || got[0].SHA256 != "" {
		t.Fatalf("SHA512-only Release = %+v, %v", got, err)
	}
}

// A malformed SHA512 entry fails the parse like a malformed SHA256 one,
// and so does a member declared twice in one block with different values;
// a repeated identical entry is one member.
func TestParseRelease_SHA512BlockErrors(t *testing.T) {
	good := strings.Repeat("c", 128)
	for _, c := range []struct {
		name, body, want string
	}{
		{"short hash", "SHA512:\n " + strings.Repeat("c", 64) + " 1 p\n", "invalid sha512"},
		{"upper case", "SHA512:\n " + strings.ToUpper(good) + " 1 p\n", "invalid sha512"},
		{"conflicting duplicate", "SHA512:\n " + good + " 1 p\n " + good + " 2 p\n", "declared twice"},
		{"conflicting SHA256 duplicate", "SHA256:\n " + sha(1) + " 1 p\n " + sha(2) + " 1 p\n", "declared twice"},
	} {
		if _, err := ParseRelease([]byte(c.body)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	got, err := ParseRelease([]byte("SHA512:\n " + good + " 1 p\n " + good + " 1 p\n"))
	if err != nil || len(got) != 1 {
		t.Fatalf("identical duplicate = %+v, %v; want one member", got, err)
	}
}

// The shared verifier (spec C5): every declared hash must match; a
// SHA512-only member yields the SHA-256 of its bytes; mismatched bytes
// never reach pool/.
func TestVerifyMember(t *testing.T) {
	body := []byte("Package: p\n")
	other := []byte("other bytes")
	for _, c := range []struct {
		name     string
		m        ReleaseMember
		mismatch string // the algorithm that fails, "" for none
		noHash   bool
	}{
		{"sha256 only", ReleaseMember{Path: "p", SHA256: shaOf(body)}, "", false},
		{"sha512 only", ReleaseMember{Path: "p", SHA512: sha512Of(body)}, "", false},
		{"both", ReleaseMember{Path: "p", SHA256: shaOf(body), SHA512: sha512Of(body)}, "", false},
		{"sha256 wrong", ReleaseMember{Path: "p", SHA256: shaOf(other), SHA512: sha512Of(body)}, "SHA256", false},
		{"sha512 wrong", ReleaseMember{Path: "p", SHA256: shaOf(body), SHA512: sha512Of(other)}, "SHA512", false},
		{"sha512-only wrong", ReleaseMember{Path: "p", SHA512: sha512Of(other)}, "SHA512", false},
		{"no hash", ReleaseMember{Path: "p"}, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			w, err := newMemberWriter(env.cache)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
			got, err := verifyMember(w, c.m, int64(len(body)))
			exists, xerr := env.cache.BlobExists(shaOf(body))
			if xerr != nil {
				t.Fatal(xerr)
			}
			var mm *memberHashMismatch
			switch {
			case c.noHash:
				if err == nil || exists {
					t.Fatalf("no declared hash: err %v, in pool %v; want an error and nothing in the pool", err, exists)
				}
			case c.mismatch != "":
				if !errors.As(err, &mm) || mm.algo != c.mismatch || exists {
					t.Fatalf("err %v (in pool %v); want a %s mismatch and nothing in the pool", err, exists, c.mismatch)
				}
			default:
				if err != nil || got != shaOf(body) || !exists {
					t.Fatalf("got %q, %v (in pool %v); want the bytes' SHA-256 and the blob in the pool", got, err, exists)
				}
			}
		})
	}
}

// Truncate restarts the SHA-512 too (a resumed fetch that starts over).
func TestMemberWriter_TruncateRestartsBothHashes(t *testing.T) {
	env := newAdoptionTestEnv(t)
	w, err := newMemberWriter(env.cache)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("the real bytes")
	if _, err := w.Write([]byte("a partial, stale ")); err != nil {
		t.Fatal(err)
	}
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if got, err := verifyMember(w, ReleaseMember{Path: "p", SHA512: sha512Of(body)}, int64(len(body))); err != nil || got != shaOf(body) {
		t.Fatalf("after Truncate: %q, %v; want the real bytes verified", got, err)
	}
}

func TestByHashAliasesAndProbe(t *testing.T) {
	h256, h512 := sha(1), strings.Repeat("d", 128)
	m := ReleaseMember{Path: "main/binary-amd64/Packages.xz", SHA256: h256, SHA512: h512}
	want := []string{"main/binary-amd64/by-hash/SHA256/" + h256, "main/binary-amd64/by-hash/SHA512/" + h512}
	if got := byHashAliases(m); !slices.Equal(got, want) {
		t.Errorf("byHashAliases = %q, want %q", got, want)
	}
	if got := byHashProbePath(m); got != want[1] {
		t.Errorf("probe = %q, want the strongest hash's %q", got, want[1])
	}
	root := ReleaseMember{Path: "Contents-amd64", SHA512: h512}
	if got := byHashAliases(root); !slices.Equal(got, []string{"by-hash/SHA512/" + h512}) {
		t.Errorf("a root member's aliases = %q", got)
	}
	if got := byHashProbePath(ReleaseMember{Path: "a/b", SHA256: h256}); got != "a/by-hash/SHA256/"+h256 {
		t.Errorf("SHA256-only probe = %q", got)
	}
}

const noble = "http://archive.ubuntu.com/ubuntu/dists/noble/"

// snapshotPaths returns the snapshot's member paths mapped to their blobs.
func snapshotPaths(t *testing.T, c *cache.Cache, id int64) map[string]string {
	t.Helper()
	rows, err := c.ListSnapshotMembers(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.DeclaredSHA256 != r.BlobHash {
			t.Errorf("member %s declares %s for blob %s", r.Path, r.DeclaredSHA256, r.BlobHash)
		}
		out[r.Path] = r.BlobHash
	}
	return out
}

func currentID(t *testing.T, c *cache.Cache, s SuiteRef) int64 {
	t.Helper()
	sf, err := c.GetSuiteFreshness(context.Background(), s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	if err != nil || sf.CurrentSnapshotID == nil {
		t.Fatalf("no current snapshot: %+v, %v", sf, err)
	}
	return *sf.CurrentSnapshotID
}

// Phase E DoD (spec C5, §7): an nvidia-shaped suite — InRelease only, a
// Release text with only a SHA512 block, a Date frozen in 2018, a Packages
// index with SHA256 Filenames — adopts: its Packages member is bound to
// the SHA-256 derived from the verified bytes, it is aliased by SHA512
// only, and its .debs are vouched. (Its flat layout and "./" Filenames are
// phase G's.)
func TestAdopter_SHA512OnlyReleaseAdopts(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	deb := shaOf([]byte("a .deb"))
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/libnvidia-container1_1.17.0-1_amd64.deb": deb})
	env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
	rel := hashRelease("Origin: https://nvidia.github.io/libnvidia-container/stable/deb/amd64\nDate: Thu, 05 Apr 2018 16:50:03 +0000\n",
		false, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
	if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	paths := snapshotPaths(t, env.cache, id)
	derived := shaOf(pkgs)
	if paths["main/binary-amd64/Packages"] != derived {
		t.Fatalf("Packages member = %q, want the derived SHA-256 %s; members %v", paths["main/binary-amd64/Packages"], derived, paths)
	}
	if paths["main/binary-amd64/by-hash/SHA512/"+sha512Of(pkgs)] != derived {
		t.Fatalf("no SHA512 alias bound to the Packages blob; members %v", paths)
	}
	if _, ok := paths["main/binary-amd64/by-hash/SHA256/"+derived]; ok {
		t.Fatalf("a SHA256 alias for a member the Release declares only by SHA512; members %v", paths)
	}
	rows, err := env.cache.DeclaredHashesForPath(ctx, "http", "archive.ubuntu.com", "/ubuntu/pool/main/libnvidia-container1_1.17.0-1_amd64.deb")
	if err != nil || len(rows) != 1 || rows[0].DeclaredSHA256 != deb {
		t.Fatalf("the .deb is not vouched: %v, %v", rows, err)
	}
	snap, err := env.cache.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.ReleaseDate == nil || *snap.ReleaseDate != 1522947003 {
		t.Fatalf("release_date = %v (%v), want the frozen 2018 Date", snap.ReleaseDate, err)
	}
}

// Spec C5: the fetched bytes are verified against every declared hash: a
// dual-hash Release whose SHA512 (or SHA256) entry is forged fails the
// member — an IndexTarget, so the adoption — and the bytes never reach
// the pool; when both match, the member is aliased under both algorithms.
func TestAdopter_VerifiesEveryDeclaredHash(t *testing.T) {
	for _, forged := range []string{"SHA512", "SHA256", ""} {
		t.Run("forged "+forged, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
			env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
			var forge map[string]string
			if forged != "" {
				forge = map[string]string{"main/binary-amd64/Packages": forged}
			}
			rel := hashRelease("Origin: x\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, forge)
			err := env.adopter.Run(context.Background(), env.suite, simtest.ClearsignedFixture(string(rel)), "", "")
			if forged == "" {
				if err != nil {
					t.Fatal(err)
				}
				paths := snapshotPaths(t, env.cache, currentID(t, env.cache, env.suite))
				for _, alias := range []string{"by-hash/SHA256/" + shaOf(pkgs), "by-hash/SHA512/" + sha512Of(pkgs)} {
					if paths["main/binary-amd64/"+alias] != shaOf(pkgs) {
						t.Errorf("alias %s missing; members %v", alias, paths)
					}
				}
				return
			}
			if !errors.Is(err, ErrAdoptionMemberMismatch) {
				t.Fatalf("Run = %v, want a member mismatch", err)
			}
			if !strings.Contains(err.Error(), "main/binary-amd64/Packages") {
				t.Fatalf("Run = %v, want the member named", err)
			}
			if ok, _ := env.cache.BlobExists(shaOf(pkgs)); ok {
				t.Fatal("bytes that failed a declared hash reached the pool")
			}
		})
	}
}

// Spec §3/C5: with Acquire-By-Hash the adopter probes by-hash with the
// strongest declared hash, as apt requests it.
func TestAdopter_ByHashProbeUsesTheStrongestHash(t *testing.T) {
	env := newAdoptionTestEnv(t)
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	byHash := noble + "main/binary-amd64/by-hash/SHA512/" + sha512Of(pkgs)
	env.fetcher.put(byHash, pkgs)
	rel := hashRelease("Origin: x\nAcquire-By-Hash: yes\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs}, nil)
	if err := env.adopter.Run(context.Background(), env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := env.fetcher.requested(); len(got) != 1 || got[0] != byHash {
		t.Fatalf("fetched %q, want only the SHA512 by-hash URL", got)
	}
}

// Spec C5 / §7: reconcile heals a SHA512-only IndexTarget into the
// snapshot with its SHA512 alias, through the same verifier.
func TestReconcile_HealsASHA512OnlyMember(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	env.fetcher.put(noble+"main/binary-all/Packages", pkgs)
	rel := hashRelease("Origin: x\n", false, true, map[string][]byte{"main/binary-all/Packages": pkgs}, nil)
	if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	alias := "main/binary-all/by-hash/SHA512/" + sha512Of(pkgs)
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, id, "main/binary-all/Packages", alias); err != nil {
		t.Fatal(err)
	}
	healed, err := env.adopter.ReconcileSnapshot(ctx, env.suite, id, true)
	if err != nil || healed != 1 {
		t.Fatalf("ReconcileSnapshot = %d, %v; want one member healed", healed, err)
	}
	paths := snapshotPaths(t, env.cache, id)
	if paths["main/binary-all/Packages"] != shaOf(pkgs) || paths[alias] != shaOf(pkgs) {
		t.Fatalf("after reconcile: members %v; want the Packages member and its SHA512 alias", paths)
	}
}

// Spec C5: an integrity-skipped member of a dual-hash suite is repaired
// with an alias for both declared algorithms (the record holds only the
// SHA-256; the snapshot's Release adds the SHA512); a SHA512-only member
// that fails integrity is skipped without a repair record (documented
// limitation).
func TestRepair_UsesEveryDeclaredHash(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true,
		Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	contents := []byte("Contents of the archive\n")
	translation := []byte("Package: foo\nDescription-en: foo\n")
	env.fetcher.put(noble+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(noble+"main/Contents-amd64", []byte("a mirror mid-sync")) // fails both hashes
	env.fetcher.put(noble+"main/i18n/Translation-en", []byte("stale"))
	dual := hashRelease("Origin: x\n", true, true, map[string][]byte{
		"main/binary-amd64/Packages": pkgs, "main/Contents-amd64": contents}, nil)
	// The SHA512-only member is declared in a second SHA512 block.
	rel := append(dual, []byte("SHA512:\n "+sha512Of(translation)+fmt.Sprintf(" %d main/i18n/Translation-en\n", len(translation)))...)
	if err := ad.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	skips, err := env.cache.ListRepairableSkippedMembers(ctx, id)
	if err != nil || len(skips) != 1 || skips[0].Path != "main/Contents-amd64" {
		t.Fatalf("repair records = %+v, %v; want Contents-amd64 only (no record for the SHA512-only member)", skips, err)
	}
	env.fetcher.put(noble+"main/Contents-amd64", contents) // the mirror synced
	ad.RepairSkippedMembers(ctx, env.suite, id)
	paths := snapshotPaths(t, env.cache, id)
	for _, p := range []string{"main/Contents-amd64", "main/by-hash/SHA256/" + shaOf(contents), "main/by-hash/SHA512/" + sha512Of(contents)} {
		if paths[p] != shaOf(contents) {
			t.Errorf("after repair %s = %q; members %v", p, paths[p], paths)
		}
	}
}

// A pooled blob whose bytes match the declared SHA-256 but not the
// declared SHA-512 — a Release whose two declarations disagree — fails
// the member and is kept in the pool (it is the SHA-256's true content).
func TestAdopter_PoolReuseChecksTheSHA512(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	if _, err := env.adopter.writeBlobBytes(ctx, pkgs); err != nil {
		t.Fatal(err)
	}
	rel := hashRelease("Origin: x\n", true, true, map[string][]byte{"main/binary-amd64/Packages": pkgs},
		map[string]string{"main/binary-amd64/Packages": "SHA512"})
	err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(rel)), "", "")
	if !errors.Is(err, ErrAdoptionMemberMismatch) || !strings.Contains(err.Error(), "SHA512") {
		t.Fatalf("Run = %v, want a SHA512 member mismatch", err)
	}
	if ok, _ := env.cache.BlobExists(shaOf(pkgs)); !ok {
		t.Fatal("the pooled blob was evicted; it is the declared SHA-256's true content")
	}
	if n := len(env.fetcher.requested()); n != 0 {
		t.Fatalf("fetched %d times, want none (a fetch brings the same bytes)", n)
	}
}
