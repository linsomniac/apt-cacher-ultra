package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// AIDEV-NOTE: the package-list design's proof (its "Tests" section): a
// model of every operation that writes, names, un-names or deletes a
// suite's package list, run in random order against the real cache. After
// every step each suite's live list must be, row for row, what its
// current snapshot declares, and no reader may answer from a list no
// suite names. The tripwires in the commit are not a proof (a wrong hash
// on a changed row passes them); this is. It cannot fail before the code
// it tests exists, so what proves it can fail is the list of mutants in
// the package-list plan's Task 6, each of which it must kill. A new
// writer of a suite's list belongs in the model's operations.
//
// ACU_MODEL_SEEDS and ACU_MODEL_STEPS make a run longer (the defaults
// keep the package's suite quick); ACU_MODEL_SEED runs one seed.

// modelSuite is what the model knows about one suite.
type modelSuite struct {
	key     SuiteKey
	exists  bool                   // it has a suite_freshness row
	current int64                  // its current snapshot, 0 for none
	rows    map[string]PackageHash // its current snapshot's package rows; nil: it names no list
	gen     int64                  // the last package_list_generation seen
}

type listModel struct {
	t      *testing.T
	c      *Cache
	rng    *rand.Rand
	seed   int64
	step   int
	suites []*modelSuite
	log    []string
	seen   map[string]int // what happened, by kind: the run must have exercised each
}

func (m *listModel) failf(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("seed %d, step %d: %s\nlast operations:\n  %s", m.seed, m.step, fmt.Sprintf(format, args...), strings.Join(m.log, "\n  "))
}

func (m *listModel) note(format string, args ...any) {
	m.log = append(m.log, fmt.Sprintf("%d: ", m.step)+fmt.Sprintf(format, args...))
	if len(m.log) > 14 {
		m.log = m.log[1:]
	}
}

// modelPool is the package paths of a host's archive, which its suites
// share.
const modelPool = 60

func (m *listModel) row(host string, i int) PackageHash {
	letters := "0123456789abcdef"
	return PackageHash{CanonicalScheme: "http", CanonicalHost: host, Path: fmt.Sprintf("/pool/p%02d.deb", i),
		DeclaredSHA256: hashOf(letters[m.rng.Intn(len(letters))]), PackageName: fmt.Sprintf("p%02d", i),
		Architecture: []string{"amd64", "arm64"}[m.rng.Intn(2)], Version: strconv.Itoa(1 + m.rng.Intn(3))}
}

func sortedRows(rows map[string]PackageHash) []PackageHash {
	out := make([]PackageHash, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// nextRows makes a suite's next publication from its current rows: kind
// "small" changes, removes and adds up to five rows; "large" replaces more
// than half; "identical" none; "empty" publishes no row; "fresh" ignores
// what was there.
func (m *listModel) nextRows(s *modelSuite, kind string) map[string]PackageHash {
	next := map[string]PackageHash{}
	for p, r := range s.rows {
		next[p] = r
	}
	host := s.key.CanonicalHost
	switch kind {
	case "identical":
	case "empty":
		return map[string]PackageHash{}
	case "fresh":
		next = map[string]PackageHash{}
		for _, i := range m.rng.Perm(modelPool)[:5+m.rng.Intn(30)] {
			r := m.row(host, i)
			next[r.Path] = r
		}
	case "small":
		for n := m.rng.Intn(6); n > 0; n-- {
			r := m.row(host, m.rng.Intn(modelPool))
			switch _, has := next[r.Path]; {
			case has && m.rng.Intn(2) == 0:
				delete(next, r.Path)
			default:
				next[r.Path] = r // a new row, or a changed one (often only a version or an architecture)
			}
		}
	case "large":
		for _, i := range m.rng.Perm(modelPool)[:modelPool/2+10] {
			r := m.row(host, i)
			if _, has := next[r.Path]; has && m.rng.Intn(3) == 0 {
				delete(next, r.Path)
			} else {
				r.Version = "9"
				next[r.Path] = r
			}
		}
	}
	return next
}

func (m *listModel) candidate(s *modelSuite, form AnchorForm) (int64, []SnapshotMember) {
	m.t.Helper()
	anchor := seedBlob(m.t, m.c, fmt.Sprintf("model publication %d", publicationSeq.Add(1)))
	id, _, err := m.c.InsertCandidateSnapshot(context.Background(), SnapshotCandidate{CanonicalScheme: s.key.CanonicalScheme,
		CanonicalHost: s.key.CanonicalHost, SuitePath: s.key.SuitePath, InReleaseHash: &anchor, Form: form})
	if err != nil {
		m.failf("InsertCandidateSnapshot: %v", err)
	}
	name := "InRelease"
	if form == FormUnsigned {
		name = "Release"
	}
	return id, []SnapshotMember{{SnapshotID: id, Path: name, BlobHash: anchor, DeclaredSHA256: anchor}}
}

// shuffled returns rows in a random order: a caller's order must not
// matter.
func (m *listModel) shuffled(rows map[string]PackageHash) []PackageHash {
	out := sortedRows(rows)
	m.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func (m *listModel) reconcile(s *modelSuite) bool {
	if s.rows == nil || s.current == 0 {
		return false
	}
	var add []PackageHash
	for _, i := range m.rng.Perm(modelPool)[:1+m.rng.Intn(3)] {
		r := m.row(s.key.CanonicalHost, i)
		if have, ok := s.rows[r.Path]; ok {
			r = have // an identical row: nothing to write
		}
		add = append(add, r)
	}
	if err := m.c.InsertReconciledMembers(context.Background(), s.current, nil, add); err != nil {
		m.failf("InsertReconciledMembers: %v", err)
	}
	for _, r := range add {
		s.rows[r.Path] = r
	}
	m.seen["reconcile"]++
	m.note("%s: reconcile added up to %d rows", s.key.SuitePath, len(add))
	return true
}

// do runs one random operation and updates the model.
func (m *listModel) do() {
	m.t.Helper()
	ctx := context.Background()
	s := m.suites[m.rng.Intn(len(m.suites))]
	name := s.key.CanonicalHost + s.key.SuitePath
	if !s.exists {
		seedRow(m.t, m.c, s.key, 1)
		s.exists, s.gen = true, 0
		m.note("%s: created", name)
		return
	}
	switch op := m.rng.Intn(100); {
	case op < 40: // a publication, the adopter's way
		kind := []string{"small", "small", "small", "large", "identical", "empty", "fresh"}[m.rng.Intn(7)]
		next := m.nextRows(s, kind)
		rows := m.shuffled(next)
		id, members := m.candidate(s, FormInline)
		diff, err := m.c.DiffPackageList(ctx, s.key, rows)
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		w, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, diff)
		if err != nil {
			m.failf("%s: CommitAdoptionDiff (%s): %v", name, kind, err)
		}
		if w.Reason == "mismatch" {
			m.failf("%s: a real difference was reported as a mismatch: %+v", name, w)
		}
		if (s.rows == nil) != (w.Reason == "first") {
			m.failf("%s: write %+v for a suite whose model has a list: %v", name, w, s.rows != nil)
		}
		s.rows, s.current = next, id
		m.seen[w.Mode+" "+w.Reason]++
		m.note("%s: published %s (%d rows): %s %s +%d ~%d -%d", name, kind, len(next), w.Mode, w.Reason, w.Added, w.Changed, w.Removed)
	case op < 52: // a publication whose difference goes stale before the commit
		next := m.nextRows(s, "small")
		rows := m.shuffled(next)
		id, members := m.candidate(s, FormInline)
		diff, err := m.c.DiffPackageList(ctx, s.key, rows)
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		how := "a reconcile"
		if m.rng.Intn(2) == 0 || !m.reconcile(s) {
			how = "another publication"
			other := m.nextRows(s, "small")
			oid, omembers := m.candidate(s, FormInline)
			if err := m.c.CommitAdoption(ctx, oid, omembers, nil, m.shuffled(other), nil, true); err != nil {
				m.failf("%s: the publication in between: %v", name, err)
			}
			s.rows, s.current = other, oid
		}
		w, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, diff)
		if err != nil {
			m.failf("%s: CommitAdoptionDiff with a stale difference: %v", name, err)
		}
		if w.Reason == "mismatch" {
			// The tripwires are for a defect in the difference. A list that was
			// written since the read is the guard's to catch (the generation
			// every writer bumps): a stale difference must never reach them.
			m.failf("%s: a difference read before %s reached the tripwires: %+v", name, how, w)
		}
		if diff != nil && !diff.TooLarge && w.Mode == PackageListDifference {
			// A stale difference may only be applied when nothing was written
			// in between: a reconcile of identical rows.
			if how != "a reconcile" {
				m.failf("%s: a difference read before %s was applied: %+v", name, how, w)
			}
		}
		s.rows, s.current = next, id
		m.seen["stale: "+w.Mode+" "+w.Reason]++
		m.note("%s: published after %s: %s %s", name, how, w.Mode, w.Reason)
	case op < 60: // a publication without a difference
		next := m.nextRows(s, []string{"small", "fresh"}[m.rng.Intn(2)])
		id, members := m.candidate(s, FormInline)
		if err := m.c.CommitAdoption(ctx, id, members, nil, m.shuffled(next), nil, true); err != nil {
			m.failf("%s: CommitAdoption: %v", name, err)
		}
		s.rows, s.current = next, id
		m.note("%s: published without a difference (%d rows)", name, len(next))
	case op < 65: // an unsigned publication
		id, members := m.candidate(s, FormUnsigned)
		w, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, m.shuffled(m.nextRows(s, "fresh")), nil, false, nil)
		switch {
		case errors.Is(err, ErrUnsignedDowngradeRefused):
			m.seen["unsigned refused"]++
			m.note("%s: an unsigned publication was refused", name)
		case err != nil:
			m.failf("%s: an unsigned commit: %v", name, err)
		case s.rows != nil:
			m.failf("%s: an unsigned commit displaced a signed snapshot", name)
		default:
			if w.Mode != PackageListNone {
				m.failf("%s: an unsigned commit wrote %+v", name, w)
			}
			s.rows, s.current = nil, id
			m.seen["unsigned published"]++
			m.note("%s: published unsigned", name)
		}
	case op < 72: // a commit that fails after the list step
		next := m.nextRows(s, "small")
		rows := m.shuffled(next)
		id, members := m.candidate(s, FormInline)
		diff, err := m.c.DiffPackageList(ctx, s.key, rows)
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		bad := []PrefetchedURLPath{{CanonicalScheme: "http", CanonicalHost: s.key.CanonicalHost, Path: "/pool/x.deb", BlobHash: "bad", UpstreamURL: "http://u/x"}}
		if _, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, rows, bad, true, diff); !errors.Is(err, ErrInvalidHash) {
			m.failf("%s: a commit with a malformed warmed hash = %v, want ErrInvalidHash", name, err)
		}
		m.seen["failed commit"]++
		m.note("%s: a commit failed", name)
	case op < 80:
		if !m.reconcile(s) {
			m.note("%s: nothing to reconcile", name)
		}
	case op < 85: // the suite is forgotten
		var want *int64
		if s.current != 0 {
			want = &s.current
		}
		res, err := m.c.ForgetSuite(ctx, s.key, want)
		if err != nil || !res.Deleted {
			m.failf("%s: ForgetSuite = %+v, %v", name, res, err)
		}
		if s.rows != nil {
			m.seen["forgot a suite with a list"]++
		}
		s.exists, s.rows, s.current = false, nil, 0
		m.note("%s: forgotten", name)
	case op < 90: // its current snapshot is quarantined
		if s.current == 0 {
			m.note("%s: no snapshot to quarantine", name)
			return
		}
		if _, err := m.c.ApplyLegacyAuditDecisions(ctx, []LegacyAuditDecision{{SnapshotID: s.current, Outcome: LegacyAuditQuarantine}}); err != nil {
			m.failf("%s: quarantine: %v", name, err)
		}
		if s.rows != nil {
			m.seen["quarantined a suite with a list"]++
		}
		s.rows, s.current = nil, 0
		m.note("%s: quarantined", name)
	default: // GC deletes a chunk of an unnamed list
		n, more, err := m.c.RunPackageListGCChunk(ctx, 1+m.rng.Intn(50))
		if err != nil {
			m.failf("RunPackageListGCChunk: %v", err)
		}
		if n > 0 {
			m.seen["gc deleted rows"]++
		}
		m.note("gc: %d rows, more %v", n, more)
	}
}

// check compares the cache with the model.
func (m *listModel) check() {
	m.t.Helper()
	ctx := context.Background()
	for _, s := range m.suites {
		name := s.key.CanonicalHost + s.key.SuitePath
		var (
			list, current sql.NullInt64
			gen           int64
		)
		err := m.c.db.QueryRow(`SELECT package_list_id, package_list_generation, current_snapshot_id FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			s.key.CanonicalScheme, s.key.CanonicalHost, s.key.SuitePath).Scan(&list, &gen, &current)
		if !s.exists {
			if !errors.Is(err, sql.ErrNoRows) {
				m.failf("%s: the model has no suite, the cache has one (%v)", name, err)
			}
			continue
		}
		if err != nil {
			m.failf("%s: read the suite: %v", name, err)
		}
		if current.Int64 != s.current {
			m.failf("%s: current snapshot %d, the model has %d", name, current.Int64, s.current)
		}
		if list.Valid != (s.rows != nil) {
			m.failf("%s: names a list: %v; the model has rows: %v", name, list.Valid, s.rows != nil)
		}
		if gen < s.gen {
			m.failf("%s: the generation went from %d back to %d", name, s.gen, gen)
		}
		s.gen = gen
		if s.rows == nil {
			continue
		}
		got, err := m.c.PackageRowsOfSnapshotForTest(ctx, s.current)
		if err != nil {
			m.failf("%s: read the live list: %v", name, err)
		}
		want := sortedRows(s.rows)
		if len(got) != len(want) {
			m.failf("%s: the live list holds %d rows, the model %d", name, len(got), len(want))
		}
		for i := range got {
			g := got[i]
			g.SnapshotID = 0
			if g != want[i] {
				m.failf("%s: live row %d is %+v, the model has %+v", name, i, g, want[i])
			}
		}
	}
	// Vouching answers from the model: each suite of the host that declares
	// the path, with its hash and its current snapshot; nothing else.
	for n := 0; n < 3; n++ {
		host := m.suites[m.rng.Intn(len(m.suites))].key.CanonicalHost
		path := fmt.Sprintf("/pool/p%02d.deb", m.rng.Intn(modelPool))
		var want []string
		for _, s := range m.suites {
			if r, ok := s.rows[path]; ok && s.key.CanonicalHost == host {
				want = append(want, fmt.Sprintf("%s@%s#%d", r.DeclaredSHA256[:1], s.key.SuitePath, s.current))
			}
		}
		slices.Sort(want)
		rows, err := m.c.DeclaredHashesForPath(ctx, "http", host, path)
		if err != nil {
			m.failf("DeclaredHashesForPath(%s%s): %v", host, path, err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, fmt.Sprintf("%s@%s#%d", r.DeclaredSHA256[:1], r.SuitePath, r.SnapshotID))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			m.failf("DeclaredHashesForPath(%s%s) = %v, the model has %v", host, path, got, want)
		}
	}
}

func envIntOr(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func TestPackageLists_AModelOfTheSwitch(t *testing.T) {
	seeds, steps := envIntOr("ACU_MODEL_SEEDS", 12), envIntOr("ACU_MODEL_STEPS", 150)
	if testing.Short() {
		seeds = 3
	}
	first := int64(1)
	if one := envIntOr("ACU_MODEL_SEED", 0); one != 0 {
		first, seeds = int64(one), 1
	}
	seen := map[string]int{}
	for seed := first; seed < first+int64(seeds); seed++ {
		c := openCache(t)
		m := &listModel{t: t, c: c, rng: rand.New(rand.NewSource(seed)), seed: seed, seen: seen}
		for _, host := range []string{"h1", "h2"} {
			for _, suite := range []string{"/a/dists/s", "/b/dists/s"} {
				m.suites = append(m.suites, &modelSuite{key: sk(host, suite)})
			}
		}
		for m.step = 1; m.step <= steps; m.step++ {
			m.do()
			m.check()
		}
		// GC runs dry: no unnamed list is left, and the table holds exactly
		// what the suites' current snapshots declare.
		m.step = steps + 1
		runPackageListGC(t, c)
		m.check()
		if got := unnamedLists(t, c); len(got) != 0 {
			m.failf("unnamed lists after GC ran dry: %v", got)
		}
		var entries, lists, want, wantLists int
		if err := c.db.QueryRow(`SELECT (SELECT count(*) FROM package_list_entry), (SELECT count(*) FROM package_list)`).Scan(&entries, &lists); err != nil {
			t.Fatal(err)
		}
		for _, s := range m.suites {
			want += len(s.rows)
			if s.rows != nil {
				wantLists++
			}
		}
		if entries != want || lists != wantLists {
			m.failf("after GC the cache holds %d package rows in %d lists; the model has %d in %d", entries, lists, want, wantLists)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// The premise: the run exercised every way a list is written, named,
	// un-named and deleted (a model that never took the difference path
	// would pass for nothing).
	if seeds >= 3 {
		for _, kind := range []string{
			PackageListDifference + " ", PackageListRewrite + " first", PackageListRewrite + " large",
			"stale: " + PackageListRewrite + " list_changed", "unsigned published", "unsigned refused",
			"failed commit", "reconcile", "forgot a suite with a list", "quarantined a suite with a list", "gc deleted rows",
		} {
			if seen[kind] == 0 {
				t.Errorf("the model never saw %q in %d seeds of %d steps: %v", kind, seeds, steps, seen)
			}
		}
	}
	t.Logf("what the model saw: %v", seen)
}
