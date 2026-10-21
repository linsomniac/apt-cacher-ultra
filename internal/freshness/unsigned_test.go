package freshness

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// unsignedRig is a real cache and an adopter with adoption.adopt_unsigned
// on, for suite /u/dists/s on host h; quiet > 0 adds a flip barrier.
type unsignedRig struct {
	t       *testing.T
	c       *cache.Cache
	ad      *Adopter
	gate    *flipgate.Gate
	members *fakeFetcher
	logs    *lockedBuffer
	suite   SuiteRef
	pkgs    []byte
}

const unsignedDeb = "/u/pool/main/f/foo_1.deb"

func newUnsignedRig(t *testing.T, quiet time.Duration, maxDuration time.Duration) *unsignedRig {
	t.Helper()
	c := simtest.OpenCache(t)
	r := &unsignedRig{t: t, c: c, members: newFakeFetcher(), logs: &lockedBuffer{},
		suite: SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/u/dists/s"}}
	var err error
	if quiet > 0 {
		if r.gate, err = flipgate.New(flipgate.Config{Quiet: quiet}); err != nil {
			t.Fatal(err)
		}
	}
	r.ad, err = NewAdopter(AdoptionConfig{Cache: c, Fetcher: r.members, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), MaxConcurrent: 2, MaxDuration: maxDuration, FlipGate: r.gate,
		AdoptUnsigned: true, Logger: slog.New(slog.NewJSONHandler(r.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	trackSuite(t, c, r.suite)
	r.pkgs = fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	r.members.put("http://h/u/dists/s/main/binary-amd64/Packages", r.pkgs)
	return r
}

// release is an unsigned Release text declaring the rig's Packages.
func (r *unsignedRig) release(label string) []byte {
	return hashRelease("Origin: "+label+"\n", true, false, map[string][]byte{"main/binary-amd64/Packages": r.pkgs}, nil)
}

func (r *unsignedRig) current() int64 {
	r.t.Helper()
	sf, err := r.c.GetSuiteFreshness(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	if sf.CurrentSnapshotID == nil {
		return 0
	}
	return *sf.CurrentSnapshotID
}

func (r *unsignedRig) marked() bool {
	r.t.Helper()
	_, ok, err := r.c.SignedSeen(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// cacheAnchor stores body as the suite's url_path row for file
// (InRelease, Release.gpg or Release), as a client's miss would have.
func (r *unsignedRig) cacheAnchor(file string, body []byte) string {
	r.t.Helper()
	return r.cacheAnchorAt(r.suite.SuitePath, file, body)
}

// cacheAnchorAt is cacheAnchor for the suite path dir (the /. twin's).
func (r *unsignedRig) cacheAnchorAt(dir, file string, body []byte) string {
	r.t.Helper()
	h, err := r.ad.writeBlobBytes(context.Background(), body)
	if err != nil {
		r.t.Fatal(err)
	}
	now := int64(1_700_000_000)
	path := dir + "/" + file
	if err := r.c.PutURLPath(context.Background(), cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: path,
		BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now, RequestCount: 1, LastFetchedAt: &now}); err != nil {
		r.t.Fatal(err)
	}
	return h
}

// records returns the adopter's JSON log records whose msg is msg ("" for all).
func (r *unsignedRig) records(msg string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(r.logs.String(), "\n") {
		var m map[string]any
		if line != "" && json.Unmarshal([]byte(line), &m) == nil && (msg == "" || m["msg"] == msg) {
			out = append(out, m)
		}
	}
	return out
}

// Spec C5 / overview "E:" carry-forward: an unsigned adoption stores the
// candidate with Form unsigned (anchor_form 'unsigned'), serves its
// Release from the snapshot, writes no InRelease or Release.gpg member, no
// package_hash row (it vouches no .deb) and no signed-seen marker, and
// CommitAdoption seeds <suite>/Release.
func TestRunUnsigned_AdoptsWithoutVouching(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	ctx := context.Background()
	rel := r.release("unsigned v1")
	if err := r.ad.RunUnsigned(ctx, r.suite, rel, `"e1"`, ""); err != nil {
		t.Fatal(err)
	}
	id := r.current()
	snap, err := r.c.GetSuiteSnapshot(ctx, id)
	if err != nil || snap.Form != cache.FormUnsigned || derefStrP(snap.SingleAnchorHash) != shaOf(rel) || snap.ReleaseHash != nil {
		t.Fatalf("snapshot = %+v (%v); want an unsigned snapshot whose single-anchor slot holds the Release", snap, err)
	}
	paths := snapshotPaths(t, r.c, id)
	if paths["Release"] != shaOf(rel) || paths["main/binary-amd64/Packages"] != shaOf(r.pkgs) {
		t.Fatalf("members %v; want Release and Packages", paths)
	}
	for _, p := range []string{"InRelease", "Release.gpg"} {
		if _, ok := paths[p]; ok {
			t.Errorf("an unsigned snapshot has a %s member", p)
		}
	}
	if rows, err := r.c.DeclaredHashesForPath(ctx, "http", "h", unsignedDeb); err != nil || len(rows) != 0 {
		t.Fatalf("DeclaredHashesForPath = %v, %v; an unsigned snapshot vouches nothing", rows, err)
	}
	if n := packageHashCount(t, r.c, id); n != 0 {
		t.Fatalf("the unsigned snapshot has %d package_hash rows, want none", n)
	}
	if r.marked() {
		t.Fatal("an unsigned adoption wrote a signed-seen marker")
	}
	if row, err := r.c.LookupURL(ctx, "http", "h", r.suite.SuitePath+"/Release"); err != nil || derefStrP(row.BlobHash) != shaOf(rel) {
		t.Fatalf("Release anchor row = %+v, %v", row, err)
	}
	if evs := r.records("adoption_success"); len(evs) != 1 || evs[0]["form"] != "unsigned" {
		t.Fatalf("adoption_success = %v, want one with form unsigned", evs)
	}
}

func derefStrP(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Spec C5: eligibility comes before any download — the knob, a signed
// current snapshot, a marker for the base (the /. twin shares it) under
// either scheme (design review M-7) — and the member cap before any member
// fetch; each refusal is its sentinel and writes nothing.
func TestRunUnsigned_EligibilityBeforeAnyDownload(t *testing.T) {
	big := make(map[string][]byte, unsignedMemberCap+1)
	for i := 0; i <= unsignedMemberCap; i++ {
		big[fmt.Sprintf("main/i18n/Translation-%d", i)] = []byte{byte(i)}
	}
	for _, tc := range []struct {
		name  string
		setup func(r *unsignedRig) []byte
		want  error
	}{
		{"knob off", func(r *unsignedRig) []byte { r.ad.adoptUnsigned = false; return r.release("x") }, errUnsignedDisabled},
		{"marker for the base", func(r *unsignedRig) []byte {
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
				CanonicalHost: "h", Base: r.suite.SuitePath, SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"marker under the other scheme", func(r *unsignedRig) []byte {
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "https",
				CanonicalHost: "h", Base: r.suite.SuitePath, SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"marker for the twin's base", func(r *unsignedRig) []byte {
			r.suite.SuitePath = "/flat/."
			trackSuite(r.t, r.c, r.suite)
			if err := r.c.MarkSignedSeenBatch(context.Background(), []cache.SignedSeenMark{{CanonicalScheme: "http",
				CanonicalHost: "h", Base: "/flat", SeenAt: 1}}); err != nil {
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"signed current snapshot", func(r *unsignedRig) []byte {
			if err := r.ad.Run(context.Background(), r.suite, simtest.ClearsignedFixture(string(r.release("signed"))), "", ""); err != nil {
				r.t.Fatal(err)
			}
			if err := r.c.ClearSignedSeenForTest(context.Background()); err != nil { // the prior form alone refuses
				r.t.Fatal(err)
			}
			return r.release("x")
		}, ErrAdoptionUnsignedDowngradeRefused},
		{"too many members", func(r *unsignedRig) []byte {
			return hashRelease("Origin: big\n", true, false, big, nil)
		}, ErrAdoptionUnsignedTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			rel := tc.setup(r)
			before, fetched := r.current(), len(r.members.requested())
			err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("RunUnsigned = %v, want %v", err, tc.want)
			}
			if r.current() != before {
				t.Fatal("a refused unsigned adoption moved the current snapshot")
			}
			if n := len(r.members.requested()) - fetched; n != 0 {
				t.Fatalf("a refused unsigned adoption fetched %d members, want none", n)
			}
		})
	}
}

// Ruling R-E1 (overview, D carry-forward; DoD): before an unsigned commit
// the adopter re-runs the cached signed-anchor mark. A suite whose marker
// is missing but whose cached InRelease (or Release.gpg) passes the sniff
// — its own, or its /. twin's (design review I-3: the twins share the
// marker) — is marked and the commit is refused; a cached plain-text "InRelease"
// (the §6.1 legacy case) earns no marker and the adoption commits, and so
// does one whose pool file is gone (no evidence).
func TestRunUnsigned_RemarksCachedSignedAnchors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		body    []byte
		gone    bool
		twin    bool // the suite is /flat; the row is its /. twin's, /flat/./<file>
		refused bool
	}{
		{"sniff-passing InRelease", "InRelease", simtest.ClearsignedFixture("Origin: signed\n"), false, false, true},
		{"sniff-passing Release.gpg", "Release.gpg", []byte("-----BEGIN PGP SIGNATURE-----\n\nwsBc\n=AAAA\n-----END PGP SIGNATURE-----\n"), false, false, true},
		{"the twin's sniff-passing InRelease", "InRelease", simtest.ClearsignedFixture("Origin: twin\n"), false, true, true},
		{"plain-text InRelease", "InRelease", []byte("Origin: plain\nSHA256:\n"), false, false, false},
		{"pool file gone", "InRelease", simtest.ClearsignedFixture("Origin: gone\n"), true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			dir := r.suite.SuitePath
			if tc.twin {
				r.suite.SuitePath, dir = "/flat", "/flat/."
				trackSuite(t, r.c, r.suite)
				r.members.put("http://h/flat/main/binary-amd64/Packages", r.pkgs)
			}
			h := r.cacheAnchorAt(dir, tc.file, tc.body)
			if tc.gone {
				if err := os.Remove(r.c.BlobPath(h)); err != nil {
					t.Fatal(err)
				}
			}
			if r.marked() {
				t.Fatal("setup: the suite already has a marker")
			}
			err := r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
			if !tc.refused {
				if err != nil || r.current() == 0 || r.marked() {
					t.Fatalf("RunUnsigned = %v, current %d, marked %v; want adopted, no marker", err, r.current(), r.marked())
				}
				return
			}
			if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || !errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
				t.Fatalf("RunUnsigned = %v, want the commit's never-downgrade refusal", err)
			}
			if r.current() != 0 || !r.marked() {
				t.Fatalf("current %d, marked %v; want no snapshot and the re-mark's marker", r.current(), r.marked())
			}
		})
	}
}

// Ruling R-E1 under both schemes (final fix wave FW-1 (b); design review
// M-7): the commit counts a marker under either scheme as signed history,
// so the re-mark reads the base's and the /. twin's cached anchors under
// http and https and marks each under its own row's scheme. A signed
// InRelease cached under the other scheme and left unmarked (phase D's
// backstop residual, or a suite untracked when it was served) refuses the
// unsigned commit. Both final reviewers' probes (spec I1, tests Critical
// 2) and the concurrency lens's E3.
func TestRunUnsigned_RemarkReadsBothSchemes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		suiteScheme, rowScheme string
		trackOther             bool // the other scheme's suite has a row
		twin                   bool // the suite is /flat; the row is its /. twin's
	}{
		{"https row, untracked, http suite", "http", "https", false, false},
		{"https row of a tracked https suite, http suite", "http", "https", true, false},
		{"http row, https suite", "https", "http", false, false},
		{"https twin row, http suite", "http", "https", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			ctx := context.Background()
			if tc.suiteScheme != "http" || tc.twin {
				r.suite.CanonicalScheme = tc.suiteScheme
				if tc.twin {
					r.suite.SuitePath = "/flat"
				}
				trackSuite(t, r.c, r.suite)
				r.members.put(tc.suiteScheme+"://h"+r.suite.SuitePath+"/main/binary-amd64/Packages", r.pkgs)
			}
			base, dir := r.suite.SuitePath, r.suite.SuitePath
			if tc.twin {
				dir += "/."
			}
			if tc.trackOther {
				trackSuite(t, r.c, SuiteRef{CanonicalScheme: tc.rowScheme, CanonicalHost: "h", SuitePath: r.suite.SuitePath})
			}
			h, err := r.ad.writeBlobBytes(ctx, simtest.ClearsignedFixture("Origin: signed over "+tc.rowScheme+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			now := int64(1_700_000_000)
			path := dir + "/InRelease"
			if err := r.c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: tc.rowScheme, CanonicalHost: "h", Path: path,
				BlobHash: &h, UpstreamURL: tc.rowScheme + "://h" + path, IsMetadata: true, LastRequestedAt: &now,
				RequestCount: 1, LastFetchedAt: &now}); err != nil {
				t.Fatal(err)
			}
			if err := r.c.CheckUnsignedDowngrade(ctx, "h", base); err != nil {
				t.Fatalf("setup: the base already counts as signed: %v", err)
			}
			err = r.ad.RunUnsigned(ctx, r.suite, r.release("unsigned over "+tc.suiteScheme), "", "")
			if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || !errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
				t.Fatalf("RunUnsigned over %s = %v, want the commit's never-downgrade refusal", tc.suiteScheme, err)
			}
			sf, err := r.c.GetSuiteFreshness(ctx, tc.suiteScheme, "h", r.suite.SuitePath)
			if err != nil {
				t.Fatal(err)
			}
			if sf.CurrentSnapshotID != nil {
				t.Fatal("an unsigned snapshot was committed while the other scheme's signed anchor sat cached")
			}
			if seen, ok, err := r.c.SignedSeen(ctx, tc.rowScheme, "h", base); err != nil || !ok || seen != now {
				t.Fatalf("%s marker for %s = %d, %v, %v; want the re-mark's, under the row's scheme, stamped %d",
					tc.rowScheme, base, seen, ok, err, now)
			}
			if _, ok, err := r.c.SignedSeen(ctx, tc.suiteScheme, "h", base); err != nil || ok {
				t.Fatalf("%s marker for %s = %v, %v; want none (no row under the suite's own scheme)", tc.suiteScheme, base, ok, err)
			}
		})
	}
}

// Spec C5 safeguard 4 at its boundary (final fix wave FW-6, final tests
// review X6): at most 1,000 declared members — exactly 1,000 adopts, 1,001
// is unsigned_too_large.
func TestUnsignedCap_Boundary(t *testing.T) {
	if err := unsignedCap(1000); err != nil {
		t.Fatalf("unsignedCap(1000) = %v, want nil: the cap is at most 1,000", err)
	}
	if err := unsignedCap(1001); !errors.Is(err, ErrAdoptionUnsignedTooLarge) {
		t.Fatalf("unsignedCap(1001) = %v, want ErrAdoptionUnsignedTooLarge", err)
	}
}

// Ruling R-E1, fail closed: when signed evidence cannot be read, or the
// re-mark cannot be written, the unsigned adoption is refused, never
// committed — and the fault is logged (design review M-1: the refusal
// alone would read "the suite has signed history"), at ERROR for a pool
// read error and at WARN for a failed mark.
func TestRunUnsigned_FailedRemarkRefuses(t *testing.T) {
	for how, level := range map[string]string{"unreadable pool file": "ERROR", "failed mark write": "WARN"} {
		t.Run(how, func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			h := r.cacheAnchor("InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
			switch how {
			case "unreadable pool file": // a directory where the pool file should be: a read error, not "does not exist"
				if err := os.Remove(r.c.BlobPath(h)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(r.c.BlobPath(h), 0o700); err != nil {
					t.Fatal(err)
				}
			case "failed mark write":
				db, err := sql.Open("sqlite", filepath.Join(r.c.Dir(), "cache.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				if _, err := db.Exec(`CREATE TRIGGER no_marks BEFORE INSERT ON suite_signed_seen
  BEGIN SELECT RAISE(ABORT, 'marker writes fail'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
			if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
				t.Fatalf("RunUnsigned = %v, want the re-mark's refusal (before the commit)", err)
			}
			if r.current() != 0 {
				t.Fatal("committed although the re-mark failed")
			}
			if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != level || !nonEmptyString(recs[0]["err"]) {
				t.Fatalf("re-mark fault records %v, want one at %s with the error", recs, level) // deferred minor E-17
			}
		})
	}
}

// Spec C5 / §7: reconcile builds no package_hash row for an unsigned
// snapshot (buildPackageHashes takes the form).
func TestReconcile_UnsignedSnapshotGetsNoPackageHash(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	ctx := context.Background()
	all := fakePackagesStanzas(map[string]string{"pool/main/f/foo_1.deb": testHex64})
	r.members.put("http://h/u/dists/s/main/binary-all/Packages", all)
	rel := hashRelease("Origin: u\n", true, false, map[string][]byte{"main/binary-all/Packages": all}, nil)
	if err := r.ad.RunUnsigned(ctx, r.suite, rel, "", ""); err != nil {
		t.Fatal(err)
	}
	id := r.current()
	if err := r.c.DeleteSnapshotMembersForTest(ctx, id, "main/binary-all/Packages"); err != nil {
		t.Fatal(err)
	}
	if healed, err := r.ad.ReconcileSnapshot(ctx, r.suite, id, true); err != nil || healed != 1 {
		t.Fatalf("ReconcileSnapshot = %d, %v; want one member healed", healed, err)
	}
	if n := packageHashCount(t, r.c, id); n != 0 {
		t.Fatalf("after reconcile the unsigned snapshot has %d package_hash rows, want none", n)
	}
}

// packageHashCount counts the snapshot's package_hash rows directly: the
// strict-mode reads filter unsigned snapshots out (Task 1), so only a raw
// count shows that none were written.
func packageHashCount(t *testing.T, c *cache.Cache, id int64) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM package_hash WHERE snapshot_id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Spec C8 for the unsigned form: a first adoption whose Release is the
// url_path Release clients were served skips the barrier, like the
// detached form.
func TestRunUnsigned_FirstAdoptionOfTheServedReleaseSkipsTheBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newUnsignedRig(t, 10*time.Second, 0)
		rel := r.release("served")
		r.cacheAnchor("Release", rel)
		tok := r.gate.AnchorStart(context.Background(), flipgate.Key("http", "h", r.suite.SuitePath))
		defer tok.Done()
		start := time.Now()
		if err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", ""); err != nil {
			t.Fatal(err)
		}
		if d := time.Since(start); d != 0 || r.current() == 0 {
			t.Fatalf("waited %v, current %d; want an immediate commit", d, r.current())
		}
		if evs := r.records("adoption_flip_barrier"); len(evs) != 1 || evs[0]["outcome"] != "not_needed" {
			t.Fatalf("adoption_flip_barrier = %v, want one not_needed", evs)
		}
	})
}

// Ruling R-E2 and the phase E DoD's never-downgrade race (spec C5, §7):
// an eligible unsigned adoption is paused before its commit while a
// client's miss stores a signed anchor for the suite with its marker (the
// handler's writer operation); resumed, the commit's in-transaction
// recheck refuses it — no pointer change, no rows — even though its flip
// barrier outlived adoption.max_duration. The refusal is a state: the
// outcome is refused, the record says unsigned_downgrade_refused, the
// barrier event says refused, and nothing is logged at WARN or above.
// The E carry-forward (phase G): the same when the signed anchor and its
// marker arrive through the ./ twin of the flat suite /repo, whose base
// the twin shares.
func TestCheck_UnsignedRefusedInsideTheCommitIsAState(t *testing.T) {
	t.Run("the suite's own anchor", func(t *testing.T) { unsignedRefusedInsideTheCommit(t, "", "") })
	t.Run("the ./ twin's anchor", func(t *testing.T) { unsignedRefusedInsideTheCommit(t, "/repo", "/repo/.") })
}

// unsignedRefusedInsideTheCommit runs TestCheck_UnsignedRefusedInsideTheCommitIsAState
// for the flat suite suite (the rig's dists/ suite when "") with the
// signed anchor served under served (the suite itself when "").
func unsignedRefusedInsideTheCommit(t *testing.T, suite, served string) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		r := newUnsignedRig(t, 10*time.Second, 20*time.Second)
		rel := r.release("unsigned")
		if suite != "" {
			r.suite.SuitePath = suite
			trackSuite(t, r.c, r.suite)
			r.members.put("http://h"+suite+"/Packages", r.pkgs)
			rel = hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": r.pkgs}, nil)
		}
		if served == "" {
			served = r.suite.SuitePath
		} else {
			trackSuite(t, r.c, SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: served})
		}
		ring := observability.NewRing(10)
		checkLogs := &lockedBuffer{}
		chk, err := New(Config{Cache: r.c, Fetcher: &changedUpstream{}, HostLimiter: hostsem.New(8),
			Adopter: r.ad, AdoptionRing: ring, Logger: slog.New(slog.NewJSONHandler(checkLogs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		key := flipgate.Key("http", "h", r.suite.SuitePath)
		tok := r.gate.AnchorStart(ctx, key) // a client reads the old (url_path) anchor: phase A waits
		var once sync.Once
		r.ad.beforeCommit = func() {
			once.Do(func() {
				// The race: a signed InRelease is served on a miss, with its
				// marker, while the adoption is paused before its commit.
				body := simtest.ClearsignedFixture("Origin: now signed\n")
				h, err := r.ad.writeBlobBytes(ctx, body)
				if err != nil {
					t.Error(err)
					return
				}
				now := time.Now().Unix()
				path := served + "/InRelease"
				if _, err := r.c.StoreFetched(ctx, cache.FetchedRow{Row: cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
					Path: path, BlobHash: &h, UpstreamURL: "http://h" + path, IsMetadata: true, LastRequestedAt: &now,
					RequestCount: 1, LastFetchedAt: &now}, Suite: cache.SuiteKey{CanonicalScheme: "http", CanonicalHost: "h",
					SuitePath: served}, MarkSeenAt: now}, cache.SuiteCaps{}, nil); err != nil {
					t.Error(err)
				}
			})
		}
		req := &adoptionRequest{suite: r.suite, form: adoptionFormUnsigned, releaseBytes: rel,
			key: contentKey(adoptionFormUnsigned, nil, rel, nil)}
		mu := &sync.Mutex{}
		mu.Lock()
		before := adoptionTotalFor(t, "h")
		chk.spawnAdoption(mu, req, mustAcquire(t, chk.admission, "h", false))
		time.Sleep(25 * time.Second) // the adoption waits in the barrier; its 20 s deadline passes
		tok.Done()                   // the commit runs 10 s later, after the race
		chk.WaitForAdoptions()
		if !r.marked() || r.current() != 0 {
			t.Fatalf("marked %v, current %d; want the served anchor's marker and no unsigned snapshot", r.marked(), r.current())
		}
		sf, err := r.c.GetSuiteFreshness(ctx, "http", "h", r.suite.SuitePath)
		if err != nil {
			t.Fatal(err)
		}
		if strOf(sf.AdoptionFailReason) != cache.ReasonUnsignedDowngradeRefused || !cache.IsRefusalReason(strOf(sf.AdoptionFailReason)) {
			t.Fatalf("record reason %q, want unsigned_downgrade_refused (a refusal state)", strOf(sf.AdoptionFailReason))
		}
		if evs := ring.Snapshot(); len(evs) != 1 || evs[0].Outcome != "refused" || evs[0].Reason != cache.ReasonUnsignedDowngradeRefused {
			t.Fatalf("admin ring %+v, want one refused/unsigned_downgrade_refused attempt", evs)
		}
		if got := adoptionTotalFor(t, "h") - before; got != 1 {
			t.Fatalf("acu_adoption_total rose by %v, want 1", got)
		}
		if evs := r.records("adoption_flip_barrier"); len(evs) != 1 || evs[0]["outcome"] != "refused" {
			t.Fatalf("adoption_flip_barrier = %v, want one with outcome refused", evs)
		}
		for _, logs := range []*lockedBuffer{r.logs, checkLogs} {
			for _, line := range strings.Split(logs.String(), "\n") {
				var m map[string]any
				if line != "" && json.Unmarshal([]byte(line), &m) == nil && (m["level"] == "WARN" || m["level"] == "ERROR") {
					t.Errorf("a refusal logged %v at %v", m["msg"], m["level"])
				}
			}
		}
		var runFailed int
		for _, line := range strings.Split(checkLogs.String(), "\n") {
			var m map[string]any
			if line != "" && json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "adoption_run_failed" && m["level"] == "INFO" {
				runFailed++
			}
		}
		if runFailed != 1 {
			t.Fatalf("%d INFO adoption_run_failed lines, want 1", runFailed)
		}
	})
}

// Overview "E:" carry-forwards: contentKey keys the unsigned Release
// itself, domain-separated from the inline key of the same bytes (design
// review M-2); classifyAdoptionReason maps every refusal sentinel to its
// own refusal reason, and an unrecognised refusal to no refusal reason.
func TestUnsignedKeysAndReasons(t *testing.T) {
	a, b := []byte("Release a"), []byte("Release b")
	if got, want := contentKey(adoptionFormUnsigned, nil, a, nil), shaOf(append([]byte("unsigned\n"), a...)); got != want {
		t.Fatalf("unsigned key = %s, want sha256(unsigned + Release) %s", got, want)
	}
	if contentKey(adoptionFormUnsigned, nil, a, nil) == contentKey(adoptionFormInline, a, nil, nil) {
		t.Fatal("an unsigned Release and an InRelease with the same bytes share a content key")
	}
	if contentKey(adoptionFormUnsigned, nil, a, nil) == contentKey(adoptionFormUnsigned, nil, b, nil) {
		t.Fatal("two unsigned Releases share a content key")
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrAdoptionUpstreamOlder, cache.ReasonUpstreamOlder},
		{ErrAdoptionRollbackRefused, cache.ReasonRollbackRefused},
		{ErrAdoptionUnsignedDowngradeRefused, cache.ReasonUnsignedDowngradeRefused},
		{ErrAdoptionUnsignedTooLarge, cache.ReasonUnsignedTooLarge},
	} {
		err := fmt.Errorf("adoption: %w", c.err)
		outcome := classifyAdoptionOutcome(err)
		if got := classifyAdoptionReason(err, outcome, nil); outcome != "refused" || got != c.want || !cache.IsRefusalReason(got) {
			t.Errorf("%v: outcome %q reason %q, want refused and %q", c.err, outcome, got, c.want)
		}
	}
	if got := classifyAdoptionReason(errors.New("x"), "refused", nil); cache.IsRefusalReason(got) {
		t.Errorf("an unrecognised refusal got the refusal reason %q", got)
	}
	if formName(adoptionFormUnsigned) != "unsigned" {
		t.Errorf("formName(unsigned) = %q", formName(adoptionFormUnsigned))
	}
	if f, ok := formOfSnapshot(&cache.SuiteSnapshot{Form: cache.FormUnsigned}); !ok || f != adoptionFormUnsigned {
		t.Errorf("formOfSnapshot(unsigned) = %v, %v", f, ok)
	}
}

// Ruling R-E1, fail closed: a url_path read error during the re-mark is
// not "no evidence" — the unsigned adoption is refused before its commit,
// and the fault is logged at WARN (design review M-1). (Proposed by the
// executability review; kills X09.)
func TestRunUnsigned_RemarkReadErrorRefuses(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	db, err := sql.Open("sqlite", filepath.Join(r.c.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`ALTER TABLE url_path RENAME TO url_path_unreadable`); err != nil {
		t.Fatal(err)
	}
	err = r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
	if !errors.Is(err, ErrAdoptionUnsignedDowngradeRefused) || errors.Is(err, cache.ErrUnsignedDowngradeRefused) {
		t.Fatalf("RunUnsigned = %v, want the re-mark's refusal (before the commit)", err)
	}
	if r.current() != 0 {
		t.Fatal("committed although the re-mark could not read the cached anchors")
	}
	if recs := r.records("adoption: signed-anchor re-mark failed"); len(recs) != 1 || recs[0]["level"] != "WARN" || !nonEmptyString(recs[0]["err"]) {
		t.Fatalf("re-mark fault records %v, want one at WARN with the error", recs) // deferred minor E-17
	}
}

// nonEmptyString reports whether v, a decoded JSON log field, is a
// non-empty string (deferred minor E-17: an absent key is no error text).
func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// Ruling R-E1 for the /. twin: the re-mark marks the suite's base
// (aptpath.MarkerBase), which the commit's in-transaction recheck reads —
// not the twin's own path. (Proposed by the executability review; kills
// X10.) Deferred minor E-16 (phase G): it reads the base's own anchors
// too — the twin /flat/. finds the evidence cached at /flat/InRelease.
// The flat suite's Release declares its root Packages (spec C5).
func TestRunUnsigned_RemarkMarksTheTwinsBase(t *testing.T) {
	for _, evidence := range []string{"/flat/.", "/flat"} {
		t.Run("evidence at "+evidence+"/InRelease", func(t *testing.T) {
			r := newUnsignedRig(t, 0, 0)
			r.suite.SuitePath = "/flat/."
			trackSuite(t, r.c, r.suite)
			r.members.put("http://h/flat/./Packages", r.pkgs)
			r.cacheAnchorAt(evidence, "InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
			rel := hashRelease("Origin: unsigned\n", true, false, map[string][]byte{"Packages": r.pkgs}, nil)
			err := r.ad.RunUnsigned(context.Background(), r.suite, rel, "", "")
			if !errors.Is(err, cache.ErrUnsignedDowngradeRefused) || r.current() != 0 {
				t.Fatalf("RunUnsigned = %v, current %d: want the commit's refusal and no snapshot", err, r.current())
			}
			if _, ok, err := r.c.SignedSeen(context.Background(), "http", "h", "/flat"); err != nil || !ok {
				t.Fatalf("no marker for the base /flat (ok %v, err %v)", ok, err)
			}
		})
	}
}

// Ruling R-E1: the re-mark's marker carries the cached anchor's last use,
// as the startup pass and the handler's backstop stamp it (phase F's
// orphan-marker sweep reads the stamp). (Proposed by the executability
// review; kills X11.)
func TestRunUnsigned_RemarkStampsTheAnchorsLastUse(t *testing.T) {
	r := newUnsignedRig(t, 0, 0)
	r.cacheAnchor("InRelease", simtest.ClearsignedFixture("Origin: signed\n"))
	_ = r.ad.RunUnsigned(context.Background(), r.suite, r.release("unsigned"), "", "")
	seen, ok, err := r.c.SignedSeen(context.Background(), "http", "h", r.suite.SuitePath)
	if err != nil || !ok || seen != 1_700_000_000 {
		t.Fatalf("marker = %d, %v, %v; want the cached anchor's last use 1700000000", seen, ok, err)
	}
}
