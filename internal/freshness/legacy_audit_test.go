package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// realisticRelease is shaped like a real archive InRelease body: several
// hash sections (MD5Sum, SHA1, SHA256, SHA512), Acquire-By-Hash, many
// members. A classifier that misreads any of it would quarantine every
// real snapshot at upgrade, so the clean case must stay clean.
func realisticRelease() string {
	var b strings.Builder
	b.WriteString("Origin: Ubuntu\nLabel: Ubuntu\nSuite: noble-updates\nVersion: 24.04\nCodename: noble\n")
	b.WriteString("Date: Sat, 04 Oct 2026 12:34:56 UTC\nArchitectures: amd64 arm64 i386\nComponents: main restricted universe multiverse\n")
	b.WriteString("Description: Ubuntu Noble Updates\nAcquire-By-Hash: yes\n")
	paths := []string{"main/binary-amd64/Packages", "main/binary-amd64/Packages.gz", "main/binary-amd64/Packages.xz",
		"main/binary-arm64/Packages.xz", "main/i18n/Translation-en.xz", "main/Contents-amd64.gz", "universe/binary-amd64/Packages.xz"}
	for _, sec := range []struct {
		name string
		n    int
	}{{"MD5Sum", 32}, {"SHA1", 40}, {"SHA256", 64}, {"SHA512", 128}} {
		b.WriteString(sec.name + ":\n")
		for i, p := range paths {
			b.WriteString(" " + strings.Repeat(string("0123456789abcdef"[i]), sec.n) + " " + strconv.Itoa(12340+i) + " " + p + "\n")
		}
	}
	return b.String()
}

func TestClassifyLegacyInline(t *testing.T) {
	genuine := realisticRelease()
	signed := clearsignForTest(t, genuine)
	end := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	inject := func(evil string) []byte {
		return append(append(append([]byte{}, signed[:end]...), evil...), signed[end:]...)
	}
	// A whitespace-only line inside the SHA256 block: the clearsign decoder
	// reads it as a blank line; the canonicalised raw parse must too (no
	// false quarantine). go-crypto's encoder never emits one, so splice it in.
	sha := bytes.Index(signed, []byte("SHA256:\n")) + len("SHA256:\n")
	firstEnd := sha + bytes.IndexByte(signed[sha:], '\n') + 1
	wsLine := append(append(append([]byte{}, signed[:firstEnd]...), "  \t\n"...), signed[firstEnd:]...)
	noSHA256 := clearsignForTest(t, "Origin: x\nSuite: y\nMD5Sum:\n "+strings.Repeat("a", 32)+" 1 main/x\n")
	// Second unsigned region: the armor's own header block. A declaration
	// whose path carries a colon survives armor.Decode as a "Key: Value"
	// header and verifies; the pre-fix raw parse ingested it too.
	sigBegin := bytes.Index(signed, []byte("-----BEGIN PGP SIGNATURE-----"))
	sigHdrEnd := sigBegin + bytes.IndexByte(signed[sigBegin:], '\n') + 1
	armorHdr := append(append(append([]byte{}, signed[:sigHdrEnd]...),
		"SHA256:\n "+strings.Repeat("c", 64)+" 10 main/binary-evilarch/Packages:x\n"...), signed[sigHdrEnd:]...)
	crlf := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")) }
	cases := []struct {
		name    string
		blob    []byte
		outcome cache.LegacyAuditOutcome
		reason  string
	}{
		{"realistic signed envelope", signed, cache.LegacyAuditKeep, ""},
		{"leading whitespace", append([]byte("\n \n"), signed...), cache.LegacyAuditKeep, ""},
		{"CRLF line endings", crlf(signed), cache.LegacyAuditKeep, ""},
		{"whitespace-only line in a hash block", wsLine, cache.LegacyAuditKeep, ""},
		{"plain text (old require_signature=false)", []byte(genuine), cache.LegacyAuditQuarantine, "legacy_plaintext"},
		{"marker but no END line", append([]byte{}, signed[:end]...), cache.LegacyAuditQuarantine, "malformed_envelope"},
		{"zero plaintext (I-1)", []byte("-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----\n"), cache.LegacyAuditQuarantine, "malformed_envelope"},
		{"R-1: new member in the armor", inject("=AAAA\nSHA256:\n " + strings.Repeat("e", 64) + " 10 main/binary-evilarch/Packages\n"), cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"R-1: same path, other hash", inject("=AAAA\nSHA256:\n " + strings.Repeat("f", 64) + " 12340 main/binary-amd64/Packages\n"), cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"R-1: same path and hash, other size", inject("=AAAA\nSHA256:\n " + strings.Repeat("0", 64) + " 99 main/binary-amd64/Packages\n"), cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"R-1: unparseable armor declaration", inject("=AAAA\nSHA256:\n not-a-hash x y\n"), cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"R-1: injection inside a CRLF envelope", crlf(inject("=AAAA\nSHA256:\n " + strings.Repeat("e", 64) + " 10 main/binary-evilarch/Packages\n")), cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"R-1: declaration in the armor headers", armorHdr, cache.LegacyAuditQuarantineSigned, "unsigned_declarations"},
		{"signed but no SHA256 block", noSHA256, cache.LegacyAuditQuarantineSigned, "unparseable"},
	}
	for _, c := range cases {
		v := classifyLegacyInline(c.blob)
		if v.outcome != c.outcome || v.reason != c.reason {
			t.Errorf("%s: verdict %+v, want outcome=%d reason=%q", c.name, v, c.outcome, c.reason)
		}
	}
}

// Every routing arm of auditLegacySnapshot, including the detached arm
// (a wrong switch order would quarantine every detached suite at upgrade).
func TestAuditLegacySnapshot_Routes(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	good, err := env.adopter.writeBlobBytes(ctx, clearsignForTest(t, realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	// A pool file whose bytes no longer match its name.
	corrupt := hashOf([]byte("corrupt anchor original"))
	if err := os.MkdirAll(filepath.Dir(c.BlobPath(corrupt)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.BlobPath(corrupt), []byte("different bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	absent := hashOf([]byte("never written"))
	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		item    cache.LegacyAuditItem
		outcome cache.LegacyAuditOutcome
		reason  string
	}{
		{"row gone", cache.LegacyAuditItem{Missing: true}, cache.LegacyAuditDrop, ""},
		{"detached", cache.LegacyAuditItem{Form: cache.FormDetached}, cache.LegacyAuditKeep, ""},
		{"unsigned (never trust-bearing)", cache.LegacyAuditItem{Form: cache.FormUnsigned}, cache.LegacyAuditKeep, ""},
		{"unexpected form", cache.LegacyAuditItem{Form: "bogus"}, cache.LegacyAuditQuarantine, "unexpected_form"},
		{"inline, no hash", cache.LegacyAuditItem{Form: cache.FormInline}, cache.LegacyAuditQuarantine, "anchor_missing"},
		{"inline, file absent", cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: str(absent)}, cache.LegacyAuditQuarantine, "anchor_missing"},
		{"inline, corrupt", cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: str(corrupt)}, cache.LegacyAuditQuarantine, "anchor_corrupt"},
		{"inline, signed", cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: str(good)}, cache.LegacyAuditKeep, ""},
	}
	for _, tc := range cases {
		v, err := auditLegacySnapshot(c, tc.item)
		if err != nil || v.outcome != tc.outcome || v.reason != tc.reason {
			t.Errorf("%s: verdict %+v err=%v, want outcome=%d reason=%q", tc.name, v, err, tc.outcome, tc.reason)
		}
	}
	prevCap := legacyAuditAnchorCap
	legacyAuditAnchorCap = 16
	t.Cleanup(func() { legacyAuditAnchorCap = prevCap })
	if v, err := auditLegacySnapshot(c, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: str(good)}); err != nil ||
		v.outcome != cache.LegacyAuditQuarantine || v.reason != "anchor_oversized" {
		t.Errorf("oversized: verdict %+v err=%v", v, err)
	}
}

// An anchor the daemon cannot read (permissions, I/O) is an error, never a
// quarantine: a fault at one boot must not irreversibly retire snapshots.
func TestAuditLegacySnapshot_UnreadableAnchorIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	env := newAdoptionTestEnv(t)
	h, err := env.adopter.writeBlobBytes(context.Background(), clearsignForTest(t, realisticRelease()))
	if err != nil {
		t.Fatal(err)
	}
	p := env.cache.BlobPath(h)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if _, err := auditLegacySnapshot(env.cache, cache.LegacyAuditItem{Form: cache.FormInline, SingleAnchorHash: &h}); err == nil {
		t.Fatal("unreadable anchor did not return an error")
	}
}

// legacyFixture is a pre-fix cache state on the real cache: one
// R-1-polluted current suite, one plain-text current suite, one clean
// inline suite, one clean detached suite, and a suite whose displaced
// snapshot is polluted.
type legacyFixture struct {
	env                         *adoptionTestEnv
	polluted, plain, clean      int64
	detached                    int64
	displacedBad, displacedGood int64
	evilIndexBlob               string
	evilDeb, goodDeb, plainDeb  string
	detDeb                      string
}

// adoptLegacy commits an inline snapshot for suite whose InRelease blob is
// anchor, with one Packages member and one package_hash row for deb.
func adoptLegacy(t *testing.T, env *adoptionTestEnv, suite string, anchor []byte, deb string) int64 {
	t.Helper()
	ctx := context.Background()
	ah, err := env.adopter.writeBlobBytes(ctx, anchor)
	if err != nil {
		t.Fatal(err)
	}
	ph, err := env.adopter.writeBlobBytes(ctx, []byte("Packages for "+suite))
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	members := []cache.SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}
	hashes := []cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", Path: deb,
		DeclaredSHA256: testHex64, SnapshotID: id, PackageName: "p", Architecture: "amd64", Version: "1"}}
	if err := env.cache.CommitAdoption(ctx, id, members, nil, hashes, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// adoptDetached commits a detached snapshot (Release + Release.gpg) with
// one Packages member and one package_hash row for deb.
func adoptDetached(t *testing.T, env *adoptionTestEnv, suite, deb string) int64 {
	t.Helper()
	ctx := context.Background()
	rel, err := env.adopter.writeBlobBytes(ctx, []byte(realisticRelease()+"Note: detached\n"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := env.adopter.writeBlobBytes(ctx, []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBc\n=abcd\n-----END PGP SIGNATURE-----\n"))
	if err != nil {
		t.Fatal(err)
	}
	ph, err := env.adopter.writeBlobBytes(ctx, []byte("Packages for "+suite))
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	members := []cache.SnapshotMember{
		{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel},
		{SnapshotID: id, Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig},
		{SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: ph, DeclaredSHA256: ph},
	}
	hashes := []cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", Path: deb,
		DeclaredSHA256: testHex64, SnapshotID: id, PackageName: "d", Architecture: "amd64", Version: "1"}}
	if err := env.cache.CommitAdoption(ctx, id, members, nil, hashes, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

func buildLegacyFixture(t *testing.T) *legacyFixture {
	t.Helper()
	ctx := context.Background()
	f := &legacyFixture{env: newAdoptionTestEnv(t),
		evilDeb: "/r/pool/evil_1_amd64.deb", goodDeb: "/r/pool/good_1_amd64.deb",
		plainDeb: "/r/pool/plain_1_amd64.deb", detDeb: "/r/pool/det_1_amd64.deb"}
	genuine := realisticRelease()
	signed := clearsignForTest(t, genuine)
	end := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	evilIndex := []byte("Package: evil\nFilename: pool/evil_1_amd64.deb\n")
	evilSHA := hashOf(evilIndex)
	tampered := append(append(append([]byte{}, signed[:end]...),
		"=AAAA\nSHA256:\n "+evilSHA+" "+strconv.Itoa(len(evilIndex))+" main/binary-evilarch/Packages\n"...), signed[end:]...)

	// Polluted current suite: adopted from the tampered envelope (adoption
	// only used the signed text), then the PRE-FIX reconcile added the
	// forged index, its by-hash alias and a vouching package_hash row.
	f.polluted = adoptLegacy(t, f.env, "/r/dists/polluted", tampered, "/r/pool/legit_1_amd64.deb")
	eh, err := f.env.adopter.writeBlobBytes(ctx, evilIndex)
	if err != nil {
		t.Fatal(err)
	}
	f.evilIndexBlob = eh
	if err := f.env.cache.InsertReconciledMembers(ctx, f.polluted,
		[]cache.SnapshotMember{
			{SnapshotID: f.polluted, Path: "main/binary-evilarch/Packages", BlobHash: eh, DeclaredSHA256: evilSHA},
			{SnapshotID: f.polluted, Path: "main/binary-evilarch/by-hash/SHA256/" + evilSHA, BlobHash: eh, DeclaredSHA256: evilSHA},
		},
		[]cache.PackageHash{{CanonicalScheme: "http", CanonicalHost: "h", Path: f.evilDeb, DeclaredSHA256: testHex64,
			SnapshotID: f.polluted, PackageName: "evil", Architecture: "amd64", Version: "1"}}); err != nil {
		t.Fatal(err)
	}
	// Plain-text current suite (old require_signature=false fallback).
	f.plain = adoptLegacy(t, f.env, "/r/dists/plain", []byte(genuine), f.plainDeb)
	// Clean signed suites, inline and detached.
	f.clean = adoptLegacy(t, f.env, "/r/dists/clean", clearsignForTest(t, genuine), f.goodDeb)
	f.detached = adoptDetached(t, f.env, "/r/dists/det", f.detDeb)
	// Displaced polluted snapshot under a clean current one.
	f.displacedBad = adoptLegacy(t, f.env, "/r/dists/disp", tampered, "/r/pool/d1_amd64.deb")
	f.displacedGood = adoptLegacy(t, f.env, "/r/dists/disp", clearsignForTest(t, genuine+"Note: v2\n"), "/r/pool/d2_amd64.deb")

	// CommitAdoption marked every signed-form commit, as a v7 binary would.
	// A pre-v7 cache has no markers: start from none.
	if err := f.env.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	// As the v7 migration does: every adopted pre-v7 snapshot awaits the audit.
	if err := f.env.cache.EnqueueLegacyTrustAudit(ctx, f.polluted, f.plain, f.clean, f.detached, f.displacedBad, f.displacedGood); err != nil {
		t.Fatal(err)
	}
	return f
}

func discardAuditLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func blobRefcount(t *testing.T, c *cache.Cache, hash string) int64 {
	t.Helper()
	b, err := c.GetBlob(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return b.RefCount
}

func currentOf(t *testing.T, c *cache.Cache, suite string) *int64 {
	t.Helper()
	sf, err := c.GetSuiteFreshness(context.Background(), "http", "h", suite)
	if err != nil {
		t.Fatal(err)
	}
	return sf.CurrentSnapshotID
}

// Spec §6.1 acceptance: a polluted pre-fix cache, upgraded. The forged
// index is not served, forged and plain-text hashes do not vouch, clean
// inline and detached suites keep serving and gain markers, pins are
// released exactly once, and a second run changes nothing.
func TestRunLegacyTrustAudit_PollutedCacheUpgrade(t *testing.T) {
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	evilRefBefore := blobRefcount(t, c, f.evilIndexBlob)

	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil {
		t.Fatal(err)
	}
	if res.Audited != 6 || res.Signed != 3 || res.Quarantined != 3 {
		t.Fatalf("result = %+v, want 6 audited, 3 signed, 3 quarantined", res)
	}
	for suite, want := range map[string]int64{"/r/dists/polluted": 0, "/r/dists/plain": 0,
		"/r/dists/clean": f.clean, "/r/dists/det": f.detached, "/r/dists/disp": f.displacedGood} {
		got := currentOf(t, c, suite)
		if (want == 0) != (got == nil) || (got != nil && *got != want) {
			t.Errorf("%s: current = %v, want %d (0 = none)", suite, got, want)
		}
	}
	if _, err := c.LookupSnapshotMember(ctx, "http", "h", "/r/dists/polluted", "main/binary-evilarch/Packages"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("forged index still served from a snapshot: %v", err)
	}
	for _, deb := range []string{f.evilDeb, f.plainDeb} {
		if got, err := c.DeclaredHashesForPath(ctx, "http", "h", deb); err != nil || len(got) != 0 {
			t.Errorf("%s still vouched: %+v (%v)", deb, got, err)
		}
	}
	for _, deb := range []string{f.goodDeb, f.detDeb} {
		if got, err := c.DeclaredHashesForPath(ctx, "http", "h", deb); err != nil || len(got) != 1 {
			t.Errorf("clean suite's %s lost its vouching: %+v (%v)", deb, got, err)
		}
	}
	if u, err := c.LookupURL(ctx, "http", "h", "/r/dists/polluted/InRelease"); err != nil || u.BlobHash == nil {
		t.Errorf("polluted suite's anchor row gone (Phase 1 serving needs it): %+v (%v)", u, err)
	}
	for _, id := range []int64{f.polluted, f.plain, f.displacedBad} {
		if s, err := c.GetSuiteSnapshot(ctx, id); err != nil || s.RetiredAt == nil {
			t.Errorf("snapshot %d not retired: %+v (%v)", id, s, err)
		}
	}
	// Markers: signed suites (incl. the tampered-but-signed polluted one)
	// yes; plain text no.
	for suite, want := range map[string]bool{"/r/dists/clean": true, "/r/dists/det": true, "/r/dists/disp": true,
		"/r/dists/polluted": true, "/r/dists/plain": false} {
		if _, ok, err := c.SignedSeen(ctx, "http", "h", suite); err != nil || ok != want {
			t.Errorf("%s: marker=%v (%v), want %v", suite, ok, err, want)
		}
	}
	if got := blobRefcount(t, c, f.evilIndexBlob); got != evilRefBefore-1 {
		t.Errorf("forged index refcount = %d, want %d (released exactly once)", got, evilRefBefore-1)
	}
	// Second run: nothing queued, nothing changes.
	res2, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res2.Audited != 0 || res2.Quarantined != 0 {
		t.Fatalf("second run = %+v (%v), want no work", res2, err)
	}
	if got := blobRefcount(t, c, f.evilIndexBlob); got != evilRefBefore-1 {
		t.Errorf("second run changed the refcount to %d", got)
	}
}

// An interrupted audit resumes at the next start without repeating
// effects (spec §6.1): batches of 2, crash after the first.
func TestRunLegacyTrustAudit_ResumesAfterInterruption(t *testing.T) {
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	evilRefBefore := blobRefcount(t, c, f.evilIndexBlob)
	crash := errors.New("simulated crash")
	prevSize, prevAfter := legacyAuditBatchSize, legacyAuditAfterBatch
	t.Cleanup(func() { legacyAuditBatchSize, legacyAuditAfterBatch = prevSize, prevAfter })
	legacyAuditBatchSize = 2
	legacyAuditAfterBatch = func(n int) error {
		if n == 1 {
			return crash
		}
		return nil
	}
	if _, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); !errors.Is(err, crash) {
		t.Fatalf("first run err = %v, want the simulated crash", err)
	}
	legacyAuditAfterBatch = prevAfter
	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Audited != 4 {
		t.Fatalf("resumed run = %+v (%v), want the remaining 4 items", res, err)
	}
	if got := blobRefcount(t, c, f.evilIndexBlob); got != evilRefBefore-1 {
		t.Fatalf("forged index refcount = %d, want %d", got, evilRefBefore-1)
	}
	for _, suite := range []string{"/r/dists/polluted", "/r/dists/plain"} {
		if currentOf(t, c, suite) != nil {
			t.Errorf("%s still current after the resumed run", suite)
		}
	}
	if currentOf(t, c, "/r/dists/det") == nil || currentOf(t, c, "/r/dists/clean") == nil {
		t.Error("a clean suite lost its current snapshot")
	}
}

// A missing, empty or foreign pool must stop startup and change nothing:
// otherwise every anchor looks missing and every snapshot is quarantined.
func TestRunLegacyTrustAudit_PreflightRefusesMissingOrEmptyPool(t *testing.T) {
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	pool := filepath.Join(c.Dir(), "pool")
	moved := pool + ".away"
	if err := os.Rename(pool, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err == nil {
		t.Fatal("audit ran without a pool")
	}
	if err := os.Mkdir(pool, 0o755); err != nil { // an empty (unmounted) pool
		t.Fatal(err)
	}
	if _, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err == nil {
		t.Fatal("audit ran against an empty pool")
	}
	if currentOf(t, c, "/r/dists/clean") == nil || currentOf(t, c, "/r/dists/polluted") == nil {
		t.Fatal("a refused audit changed current pointers")
	}
	if err := os.Remove(pool); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, pool); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.Audited != 6 {
		t.Fatalf("audit after the pool returned = %+v (%v), want 6 audited", res, err)
	}
}

// oneQueuedInline is a pre-v7 cache with exactly one queued snapshot: a
// clean, current inline one.
func oneQueuedInline(t *testing.T) (*cache.Cache, int64) {
	t.Helper()
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	id := adoptLegacy(t, env, "/r/dists/only", clearsignForTest(t, realisticRelease()), "/r/pool/only_1_amd64.deb")
	if err := env.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	return env.cache, id
}

// Blob GC never deletes an adopted snapshot's anchor or member blobs, so an
// empty pool with even one queued inline anchor is a lost or unmounted
// pool, not one lost file: refuse to start and change nothing.
func TestRunLegacyTrustAudit_PreflightRefusesEmptyPoolWithOneInlineAnchor(t *testing.T) {
	c, id := oneQueuedInline(t)
	ctx := context.Background()
	pool := filepath.Join(c.Dir(), "pool")
	moved := pool + ".away"
	if err := os.Rename(pool, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(pool, 0o755); err != nil { // an empty (unmounted) pool
		t.Fatal(err)
	}
	_, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err == nil || !strings.Contains(err.Error(), pool) {
		t.Fatalf("audit err = %v, want a refusal naming %s", err, pool)
	}
	if cur := currentOf(t, c, "/r/dists/only"); cur == nil || *cur != id {
		t.Fatalf("a refused audit changed the current pointer: %v", cur)
	}
	if items, _ := c.LegacyAuditBatch(ctx, 0, 0); len(items) != 1 || items[0].SnapshotID != id {
		t.Fatalf("a refused audit changed the queue: %+v", items)
	}
	if err := os.Remove(pool); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, pool); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.Audited != 1 || res.Signed != 1 {
		t.Fatalf("audit after the pool returned = %+v (%v), want 1 audited and kept", res, err)
	}
}

// One missing anchor in a non-empty pool cannot be told from one lost
// file: it is quarantined, never a refused start.
func TestRunLegacyTrustAudit_LoneMissingAnchorInNonEmptyPoolIsQuarantined(t *testing.T) {
	c, id := oneQueuedInline(t)
	ctx := context.Background()
	s, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(c.BlobPath(*s.SingleAnchorHash)); err != nil {
		t.Fatal(err)
	}
	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Quarantined != 1 {
		t.Fatalf("run = %+v (%v), want the lone missing anchor quarantined", res, err)
	}
	if cur := currentOf(t, c, "/r/dists/only"); cur != nil {
		t.Fatalf("quarantined snapshot still current: %v", *cur)
	}
}

// adoptAnchorOnly commits an inline snapshot for suite whose only member
// is its InRelease blob, anchor: the one pool file it pins.
func adoptAnchorOnly(t *testing.T, env *adoptionTestEnv, suite string, anchor []byte) int64 {
	t.Helper()
	ctx := context.Background()
	ah, err := env.adopter.writeBlobBytes(ctx, anchor)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite, InReleaseHash: &ah})
	if err != nil {
		t.Fatal(err)
	}
	members := []cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: ah, DeclaredSHA256: ah}}
	if err := env.cache.CommitAdoption(ctx, id, members, nil, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	return id
}

// The final review's R2 I-1 and R5 #1: two queued snapshots that share one
// InRelease (Debian's dists/stable and dists/bookworm serve the same
// bytes; an http and an https suite of one repository do too) share one
// content-addressed pool file. That file lost from a pool that is not
// empty is one lost file, as for a lone missing anchor (spec §6.1): both
// snapshots are quarantined anchor_missing and the start proceeds. Also
// when the shared file is the only one the snapshots pin ("anchors only"),
// so no other file of theirs shows the pool is mounted: the pre-flight
// counts distinct files, not snapshots.
func TestRunLegacyTrustAudit_OneLostFileSharedByTwoSnapshotsIsQuarantined(t *testing.T) {
	for _, shape := range []string{"with members", "anchors only"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			env := newAdoptionTestEnv(t)
			c := env.cache
			anchor := clearsignForTest(t, realisticRelease())
			var a, b int64
			if shape == "with members" {
				a = adoptLegacy(t, env, "/r/dists/stable", anchor, "/r/pool/a_1_amd64.deb")
				b = adoptLegacy(t, env, "/r/dists/bookworm", anchor, "/r/pool/b_1_amd64.deb")
			} else {
				a = adoptAnchorOnly(t, env, "/r/dists/stable", anchor)
				b = adoptAnchorOnly(t, env, "/r/dists/bookworm", anchor)
				// A cached file no snapshot pins: the pool is not empty.
				if _, err := env.adopter.writeBlobBytes(ctx, []byte("a cached package")); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.ClearSignedSeenForTest(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.EnqueueLegacyTrustAudit(ctx, a, b); err != nil {
				t.Fatal(err)
			}
			sa, err := c.GetSuiteSnapshot(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			if sb, err := c.GetSuiteSnapshot(ctx, b); err != nil || *sb.SingleAnchorHash != *sa.SingleAnchorHash {
				t.Fatalf("fixture: snapshot %+v (%v), want the same InRelease blob as %d", sb, err, a)
			}
			if err := os.Remove(c.BlobPath(*sa.SingleAnchorHash)); err != nil { // the one lost file
				t.Fatal(err)
			}
			logs := &lockedBuffer{}
			res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(logs, nil)))
			if err != nil || res.Audited != 2 || res.Quarantined != 2 {
				t.Fatalf("run = %+v (%v), want both snapshots quarantined and the start not refused", res, err)
			}
			recs := logRecords(t, logs, "legacy_snapshot_quarantined")
			for _, rec := range recs {
				if rec["reason"] != "anchor_missing" {
					t.Errorf("quarantine %v, want reason anchor_missing", rec)
				}
			}
			if len(recs) != 2 {
				t.Errorf("%d legacy_snapshot_quarantined lines, want 2", len(recs))
			}
			for _, suite := range []string{"/r/dists/stable", "/r/dists/bookworm"} {
				if cur := currentOf(t, c, suite); cur != nil {
					t.Errorf("%s: quarantined snapshot still current: %d", suite, *cur)
				}
			}
		})
	}
}

// The final review's R2 I-1: the deferred residue. A first start defers the
// snapshots whose anchors it cannot read (ERROR legacy_trust_audit_deferred,
// "until the pool is fixed"), and the operator removes the unreadable files
// (by hand, or fsck moves them). The queue then holds only those snapshots,
// every anchor of theirs missing, but the pool is mounted: it holds the
// files the current snapshots pin. The next start quarantines them
// anchor_missing; it does not refuse ("is the pool mounted?") at this start
// and every one after.
func TestRunLegacyTrustAudit_RemovedDeferredResidueIsQuarantined(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ctx := context.Background()
	f := buildLegacyFixture(t)
	c := f.env.cache
	var paths []string
	for _, id := range []int64{f.clean, f.polluted} { // f.displacedBad shares f.polluted's anchor
		s, err := c.GetSuiteSnapshot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		p := c.BlobPath(*s.SingleAnchorHash)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Deferred != 3 || res.Audited != 3 {
		t.Fatalf("first start = %+v (%v), want 3 audited and 3 deferred", res, err)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	res, err = RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Audited != 3 || res.Quarantined != 3 {
		t.Fatalf("second start = %+v (%v), want the removed residue quarantined, not a refused start", res, err)
	}
	if items, err := c.LegacyAuditBatch(ctx, 0, 0); err != nil || len(items) != 0 {
		t.Fatalf("queue = %+v (%v), want it drained", items, err)
	}
	for _, suite := range []string{"/r/dists/clean", "/r/dists/polluted"} {
		if cur := currentOf(t, c, suite); cur != nil {
			t.Errorf("%s: quarantined snapshot still current: %d", suite, *cur)
		}
	}
}

// Spec §6.1's pre-flight still refuses a pool that is not empty but holds
// none of the queued inline anchor files and none of the files the current
// snapshots pin — a foreign pool, or a bare mount point with stray files —
// at the first start and after one that left a deferred residue alike,
// here a residue of one snapshot: the residue's lost files do not make a
// lost mount look like lost files. Nothing changes, and with the pool back
// the audit runs.
func TestRunLegacyTrustAudit_PreflightRefusesAForeignPool(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ctx := context.Background()
	f := buildLegacyFixture(t)
	c := f.env.cache
	pool := filepath.Join(c.Dir(), "pool")
	moved := pool + ".away"
	swapIn := func() {
		t.Helper()
		if err := os.Rename(pool, moved); err != nil {
			t.Fatal(err)
		}
		body := []byte("another cache's blob")
		h := hashOf(body)
		if err := os.MkdirAll(filepath.Join(pool, h[:2]), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pool, h[:2], h), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	swapOut := func() {
		t.Helper()
		if err := os.RemoveAll(pool); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, pool); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(when string, queued int) {
		t.Helper()
		_, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
		if err == nil || !strings.Contains(err.Error(), pool) {
			t.Fatalf("%s: audit err = %v, want a refusal naming %s", when, err, pool)
		}
		if cur := currentOf(t, c, "/r/dists/clean"); cur == nil || *cur != f.clean {
			t.Fatalf("%s: a refused audit changed the current pointer: %v", when, cur)
		}
		if items, _ := c.LegacyAuditBatch(ctx, 0, 0); len(items) != queued {
			t.Fatalf("%s: a refused audit changed the queue: %+v", when, items)
		}
	}
	swapIn()
	refused("first start", 6)
	swapOut()

	// A first start defers the snapshots whose anchors it cannot read.
	s, err := c.GetSuiteSnapshot(ctx, f.clean)
	if err != nil {
		t.Fatal(err)
	}
	p := c.BlobPath(*s.SingleAnchorHash)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.Deferred != 1 {
		t.Fatalf("start with an unreadable anchor = %+v (%v), want 1 deferred", res, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	// A later start with a foreign pool: the residue's one anchor file is
	// missing, but so is every file the current snapshots pin.
	swapIn()
	refused("after a deferred residue", 1)
	swapOut()
	if res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger()); err != nil || res.Audited != 1 || res.Signed != 1 {
		t.Fatalf("audit after the pool returned = %+v (%v), want the deferred snapshot audited and kept", res, err)
	}
}

// A single unreadable anchor defers only that snapshot: the run continues,
// the snapshot stays current and queued, and the next start audits it.
func TestRunLegacyTrustAudit_UnreadableAnchorIsDeferred(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	s, err := c.GetSuiteSnapshot(ctx, f.clean)
	if err != nil {
		t.Fatal(err)
	}
	p := c.BlobPath(*s.SingleAnchorHash)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			_ = os.Chmod(p, 0o644)
		}
	})
	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Deferred != 1 || res.Audited != 5 {
		t.Fatalf("run = %+v (%v), want 5 audited and 1 deferred", res, err)
	}
	if cur := currentOf(t, c, "/r/dists/clean"); cur == nil || *cur != f.clean {
		t.Fatalf("deferred snapshot changed: current = %v", cur)
	}
	if items, _ := c.LegacyAuditBatch(ctx, 0, 0); len(items) != 1 || items[0].SnapshotID != f.clean {
		t.Fatalf("queue = %+v, want only the deferred snapshot", items)
	}
	if currentOf(t, c, "/r/dists/polluted") != nil {
		t.Fatal("the rest of the batch was not applied")
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	restored = true
	res, err = RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Deferred != 0 || res.Signed != 1 {
		t.Fatalf("second run = %+v (%v), want the one deferred snapshot audited and kept", res, err)
	}
}

// A panic while auditing one snapshot is handled like an I/O fault: that
// snapshot is deferred (left current and queued, ERROR with the stack), the
// rest of the batch is applied, and the start is not refused. The queue and
// blob are the same at every start, so an unrecovered panic here would be a
// crash loop with no escape short of editing the database.
// Swaps the package-level legacyClassify seam, so NOT t.Parallel.
func TestRunLegacyTrustAudit_ClassifyPanicIsDeferred(t *testing.T) {
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	s, err := c.GetSuiteSnapshot(ctx, f.clean)
	if err != nil {
		t.Fatal(err)
	}
	cleanAnchor := *s.SingleAnchorHash
	prev := legacyClassify
	t.Cleanup(func() { legacyClassify = prev })
	legacyClassify = func(blob []byte) legacyVerdict {
		if hashOf(blob) == cleanAnchor {
			panic("injected classifier panic")
		}
		return prev(blob)
	}
	var logBuf bytes.Buffer
	res, err := RunLegacyTrustAudit(ctx, c, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if err != nil || res.Deferred != 1 || res.Audited != 5 || res.Signed != 2 || res.Quarantined != 3 {
		t.Fatalf("run = %+v (%v), want no error, 5 audited (2 signed, 3 quarantined) and 1 deferred", res, err)
	}
	if cur := currentOf(t, c, "/r/dists/clean"); cur == nil || *cur != f.clean {
		t.Fatalf("panicked snapshot changed: current = %v", cur)
	}
	if items, _ := c.LegacyAuditBatch(ctx, 0, 0); len(items) != 1 || items[0].SnapshotID != f.clean {
		t.Fatalf("queue = %+v, want only the panicked snapshot", items)
	}
	if currentOf(t, c, "/r/dists/polluted") != nil || currentOf(t, c, "/r/dists/plain") != nil {
		t.Fatal("the rest of the batch was not applied")
	}
	var deferred map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "legacy_trust_audit_deferred" {
			deferred = rec
		}
	}
	switch {
	case deferred == nil:
		t.Fatalf("no legacy_trust_audit_deferred line:\n%s", logBuf.String())
	case deferred["level"] != "ERROR":
		t.Errorf("deferred level = %v, want ERROR", deferred["level"])
	case !strings.Contains(fmt.Sprint(deferred["err"]), "injected classifier panic"):
		t.Errorf("deferred err = %v, want the panic value", deferred["err"])
	case !strings.Contains(fmt.Sprint(deferred["stack"]), "TestRunLegacyTrustAudit_ClassifyPanicIsDeferred"):
		t.Errorf("deferred stack does not reach the panicking frame: %v", deferred["stack"])
	}
	// Next start with a healthy classifier: the deferred snapshot is
	// audited and kept.
	legacyClassify = prev
	res, err = RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Deferred != 0 || res.Signed != 1 {
		t.Fatalf("second run = %+v (%v), want the deferred snapshot audited and kept", res, err)
	}
}

// An anchor that cannot even be stat'ed (its pool subdirectory is not
// searchable) is an I/O fault, not evidence of a lost pool: pre-flight
// must not refuse to start, and the audit defers that snapshot.
func TestRunLegacyTrustAudit_UnstatableAnchorDoesNotRefuseStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	f := buildLegacyFixture(t)
	ctx := context.Background()
	c := f.env.cache
	s, err := c.GetSuiteSnapshot(ctx, f.clean)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(c.BlobPath(*s.SingleAnchorHash))
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
	if err != nil || res.Deferred < 1 {
		t.Fatalf("run = %+v (%v), want no refusal and the unstatable snapshot deferred", res, err)
	}
	if cur := currentOf(t, c, "/r/dists/clean"); cur == nil || *cur != f.clean {
		t.Fatalf("deferred snapshot changed: current = %v", cur)
	}
}

// Validators the honest upstream serves for its InRelease. An R-1 body
// tampered in transit keeps them, so a pre-fix cache stores them on the
// suite next to the tampered bytes.
const (
	upstreamETag    = `"5f2a1c-63c1e0d4b8e00"`
	upstreamLastMod = "Sat, 04 Oct 2026 12:34:56 GMT"
)

// urlConditional answers conditional GETs from a fixed URL → body map.
// Like a real unchanged upstream it honours its own validators: a request
// carrying upstreamETag or upstreamLastMod gets 304 with no body; any
// other request for a known URL gets 200; an unknown URL gets 404.
type urlConditional map[string][]byte

func (u urlConditional) Conditional(_ context.Context, tg *fetch.Target, etag, lastmod string, _ int64) (*fetch.ConditionalResult, error) {
	b, ok := u[tg.URL]
	switch {
	case !ok:
		return &fetch.ConditionalResult{Status: 404}, nil
	case etag == upstreamETag || lastmod == upstreamLastMod:
		return &fetch.ConditionalResult{Status: 304}, nil
	}
	return &fetch.ConditionalResult{Status: 200, Body: b}, nil
}

// Spec §6.1 recovery: an R-1 quarantine heals on the first check, because
// the cached (tampered) anchor never matches a clean upstream, and the
// re-adopted snapshot carries no forged member. Without this, a quarantine
// could leave the suite permanently unadopted.
func TestRunLegacyTrustAudit_QuarantinedR1SuiteReadoptsOnFirstCheck(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	releaseText, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs})
	clean := clearsignForTest(t, string(releaseText))
	end := bytes.LastIndex(clean, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte{}, clean[:end]...),
		"=AAAA\nSHA256:\n "+strings.Repeat("e", 64)+" 10 main/binary-evilarch/Packages\n"...), clean[end:]...)
	// Pre-fix state: adopted from the tampered envelope (adoption used only
	// the signed text). CommitAdoption seeds the suite's url_path anchor
	// with the tampered bytes.
	old := adoptLegacy(t, env, "/r/dists/p", tampered, "/r/pool/p_1_amd64.deb")
	// ... and the upstream's validators, passed through with the tampered
	// body. If the quarantine kept them, the first check would send them,
	// urlConditional would answer 304 and the suite would never re-adopt.
	sf, err := env.cache.GetSuiteFreshness(ctx, "http", "h", "/r/dists/p")
	if err != nil {
		t.Fatal(err)
	}
	etag, lastmod := upstreamETag, upstreamLastMod
	sf.InReleaseETag, sf.InReleaseLastMod = &etag, &lastmod
	if err := env.cache.SeedSuiteForTest(ctx, *sf); err != nil {
		t.Fatal(err)
	}
	if err := env.cache.ClearSignedSeenForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.cache.EnqueueLegacyTrustAudit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if res, err := RunLegacyTrustAudit(ctx, env.cache, discardAuditLogger()); err != nil || res.Quarantined != 1 {
		t.Fatalf("audit = %+v (%v), want 1 quarantined", res, err)
	}
	// A client fetched the anchor (spec C4 Dispatch 3 needs a
	// client-created anchor row).
	if err := env.cache.TouchURLPath(ctx, "http", "h", "/r/dists/p/InRelease"); err != nil {
		t.Fatal(err)
	}
	// A clean upstream: the signed InRelease, and its Packages member for
	// the adopter's fetcher.
	env.fetcher.put("http://h/r/dists/p/main/binary-amd64/Packages", pkgs)
	chk, err := New(Config{Cache: env.cache, Fetcher: urlConditional{"http://h/r/dists/p/InRelease": clean},
		HostLimiter: hostsem.New(8), Adopter: env.adopter, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	chk.Check(ctx, "http", "h", "/r/dists/p")
	chk.WaitForAdoptions()
	cur := currentOf(t, env.cache, "/r/dists/p")
	if cur == nil || *cur == old {
		t.Fatalf("the quarantined R-1 suite did not re-adopt on its first check (current = %v)", cur)
	}
	if _, err := env.cache.LookupSnapshotMember(ctx, "http", "h", "/r/dists/p", "main/binary-evilarch/Packages"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("the re-adopted snapshot carries the forged member: %v", err)
	}
}

// Spec C3: the startup pass marks tracked suites whose cached signed
// anchors pass the sniff, stamped with the anchor's last use — so a
// never-adopted suite that clients were served signed metadata for gains
// history before any check runs. Plain text and untracked suites do not.
func TestRunLegacyTrustAudit_MarksFromCachedSignedAnchors(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	c := env.cache
	signed := clearsignForTest(t, realisticRelease())
	sig := []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQ\n=abcd\n-----END PGP SIGNATURE-----\n")
	at := int64(1_799_990_000)
	seed := func(tracked bool, suite, name string, body []byte) {
		t.Helper()
		if tracked {
			if err := c.SeedSuiteForTest(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}); err != nil {
				t.Fatal(err)
			}
		}
		h, err := env.adopter.writeBlobBytes(ctx, body)
		if err != nil {
			t.Fatal(err)
		}
		p := suite + "/" + name
		if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: p, BlobHash: &h,
			UpstreamURL: "http://h" + p, IsMetadata: true, LastRequestedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	seed(true, "/r/dists/inrel", "InRelease", signed)
	seed(true, "/r/dists/gpg", "Release.gpg", sig)
	seed(true, "/r/dists/plain", "InRelease", []byte(realisticRelease()))
	seed(false, "/r/dists/untracked", "InRelease", signed)
	for run := 0; run < 2; run++ {
		res, err := RunLegacyTrustAudit(ctx, c, discardAuditLogger())
		if err != nil || res.AnchorMarkers != 2 {
			t.Fatalf("run %d: %+v (%v), want 2 anchor markers", run, res, err)
		}
	}
	for suite, want := range map[string]bool{"/r/dists/inrel": true, "/r/dists/gpg": true, "/r/dists/plain": false, "/r/dists/untracked": false} {
		got, ok, err := c.SignedSeen(ctx, "http", "h", suite)
		if err != nil || ok != want || (ok && got != at) {
			t.Errorf("%s: marker=%v at=%d (%v), want %v at %d", suite, ok, got, err, want, at)
		}
	}
}

// Deferred minor A-47: the audit counts an unsigned snapshot it keeps as
// audited, not as signed.
func TestRunLegacyTrustAudit_UnsignedKeepIsNotSigned(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	id := *r.row().CurrentSnapshotID
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if err != nil || res.Audited != 1 || res.Signed != 0 || res.Quarantined != 0 {
		t.Fatalf("audit = %+v (%v), want 1 audited, 0 signed, 0 quarantined", res, err)
	}
}

// Deferred minor C-29: every error RunLegacyTrustAudit returns starts
// "legacy trust audit:" (main's refusal line relies on it). Two sites
// are pinned: the batch hook's, and a later batch's read once the cache is
// closed (the H1 final review's tests M-3); cmd's
// TestServeListeners_LegacyAuditErrorHasOnePrefix pins the preflight's.
func TestRunLegacyTrustAudit_ErrorsCarryThePrefix(t *testing.T) {
	r := newRetryRig(t, 0)
	ctx := context.Background()
	r.adoptUnsigned("u1")
	if err := r.cache.EnqueueLegacyTrustAudit(ctx, *r.row().CurrentSnapshotID); err != nil {
		t.Fatal(err)
	}
	prev := legacyAuditAfterBatch
	t.Cleanup(func() { legacyAuditAfterBatch = prev })
	boom := errors.New("batch hook failed")
	legacyAuditAfterBatch = func(int) error { return boom }
	_, err := RunLegacyTrustAudit(ctx, r.cache, discardAuditLogger())
	if !errors.Is(err, boom) || !strings.HasPrefix(err.Error(), "legacy trust audit: ") {
		t.Fatalf("err = %v, want the batch error with the prefix", err)
	}

	env := newAdoptionTestEnv(t)
	one := adoptLegacy(t, env, "/r/dists/one", clearsignForTest(t, realisticRelease()+"Note: one\n"), "/r/pool/o_1_amd64.deb")
	two := adoptLegacy(t, env, "/r/dists/two", clearsignForTest(t, realisticRelease()+"Note: two\n"), "/r/pool/t_1_amd64.deb")
	if err := env.cache.EnqueueLegacyTrustAudit(ctx, one, two); err != nil {
		t.Fatal(err)
	}
	prevSize := legacyAuditBatchSize
	t.Cleanup(func() { legacyAuditBatchSize = prevSize })
	legacyAuditBatchSize = 1
	legacyAuditAfterBatch = func(int) error { return env.cache.Close() } // the second batch's read fails
	_, err = RunLegacyTrustAudit(ctx, env.cache, discardAuditLogger())
	if err == nil || !strings.HasPrefix(err.Error(), "legacy trust audit: LegacyAuditBatch: ") {
		t.Fatalf("err = %v, want the second batch's read error with the prefix", err)
	}
}
