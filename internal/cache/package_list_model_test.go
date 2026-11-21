package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
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
// it tests exists, so what proves it can fail is mutants, each of which
// it must kill: the eight of the package-list plan's Task 6, and those of
// the final review's M-3 (a heal that does not bump the generation, a
// suffix lookup that reads other suites' lists, a difference not bound to
// its rows, a guard without the list id), and the commit that does not
// bump the generation (the merge review: before the model had a
// difference applied in place between another's read and its commit, that
// one passed it). A new writer of a suite's list belongs in the model's
// operations, and among what it puts between a read and its commit.
//
// What it models: http and https twins of one suite, two hosts that share
// package paths, a flat suite per host (rows with a flat filename, vouched
// through the suffix fallback), rows as old releases left them (no
// version, or no identity: a reconcile heals them). What it does not: the
// hot_package bookkeeping (no client traffic here; the commit and trigger
// tests cover it), and cap eviction and the reaper, which remove a suite
// through the same deleteSuiteTx as the forget it does model.
//
// ACU_MODEL_SEEDS and ACU_MODEL_STEPS make a run longer (the defaults
// keep the package's suite quick; docs/releasing.md has the long form);
// ACU_MODEL_SEED runs one seed.

// modelSuite is what the model knows about one suite.
type modelSuite struct {
	key     SuiteKey
	flat    bool                   // a flat repository: its rows carry a flat filename
	twin    *modelSuite            // the same host and path under the other scheme, if modelled
	exists  bool                   // it has a suite_freshness row
	current int64                  // its current snapshot, 0 for none
	rows    map[string]PackageHash // its current snapshot's package rows; nil: it names no list
	gen     int64                  // the last package_list_generation seen
}

func (s *modelSuite) name() string {
	return s.key.CanonicalScheme + "://" + s.key.CanonicalHost + s.key.SuitePath
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

func modelPackageName(i int) string { return fmt.Sprintf("p%02d", i) }

// modelPath is package i's path in a suite of that kind: under the
// archive's pool, or under a flat repository with a "./" Filename (so a
// client's request for it misses the exact row and takes the suffix
// fallback).
func modelPath(flat bool, i int) string {
	if flat {
		return fmt.Sprintf("/flat/./f%02d.deb", i)
	}
	return fmt.Sprintf("/pool/p%02d.deb", i)
}

// row is a random package row of s for package i. One row in sixteen has
// no identity at all and two have no version: rows as releases before
// schema 3 and schema 6 left them, which a reconcile heals in place.
func (m *listModel) row(s *modelSuite, i int) PackageHash {
	letters := "0123456789abcdef"
	r := PackageHash{CanonicalScheme: s.key.CanonicalScheme, CanonicalHost: s.key.CanonicalHost, Path: modelPath(s.flat, i),
		DeclaredSHA256: hashOf(letters[m.rng.Intn(len(letters))]), PackageName: modelPackageName(i),
		Architecture: []string{"amd64", "arm64"}[m.rng.Intn(2)], Version: strconv.Itoa(1 + m.rng.Intn(3))}
	if s.flat {
		r.FlatFilename = fmt.Sprintf("f%02d.deb", i)
	}
	switch m.rng.Intn(16) {
	case 0:
		r.PackageName, r.Architecture, r.Version = "", "", ""
	case 1, 2:
		r.Version = ""
	}
	return r
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
	switch kind {
	case "identical":
	case "empty":
		return map[string]PackageHash{}
	case "fresh":
		next = map[string]PackageHash{}
		for _, i := range m.rng.Perm(modelPool)[:5+m.rng.Intn(30)] {
			r := m.row(s, i)
			next[r.Path] = r
		}
	case "small":
		for n := m.rng.Intn(6); n > 0; n-- {
			r := m.row(s, m.rng.Intn(modelPool))
			switch _, has := next[r.Path]; {
			case has && m.rng.Intn(2) == 0:
				delete(next, r.Path)
			default:
				next[r.Path] = r // a new row, or a changed one (often only a version or an architecture)
			}
		}
	case "large":
		for _, i := range m.rng.Perm(modelPool)[:modelPool/2+10] {
			r := m.row(s, i)
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

// reconcile adds one to three rows to s's live list, as a recovered index
// does: a row the list lacks is inserted, a row it has identically is left
// alone, and a row it has with empty identity columns is healed (the same
// hash, the empty columns filled). It reports whether a reconcile ran and
// whether it wrote anything.
func (m *listModel) reconcile(s *modelSuite) (ran, wrote bool) {
	if s.rows == nil || s.current == 0 {
		return false, false
	}
	// Half the time, when the list has a row an old release left without
	// its identity, that row is among those reconciled: a heal is rarer
	// than an insert by chance alone, and it is a write like any other.
	picks := m.rng.Perm(modelPool)[:1+m.rng.Intn(3)]
	if m.rng.Intn(2) == 0 {
		var bare []int
		for i := 0; i < modelPool; i++ {
			if have, ok := s.rows[modelPath(s.flat, i)]; ok && (have.PackageName == "" || have.Architecture == "" || have.Version == "") {
				bare = append(bare, i)
			}
		}
		if len(bare) > 0 {
			picks = []int{bare[m.rng.Intn(len(bare))]}
		}
	}
	var add []PackageHash
	healed := false
	for _, i := range picks {
		r := m.row(s, i)
		if have, ok := s.rows[r.Path]; ok {
			r = have
			if r.PackageName == "" {
				r.PackageName, healed = modelPackageName(i), true
			}
			if r.Architecture == "" {
				r.Architecture, healed = "amd64", true
			}
			if r.Version == "" {
				r.Version, healed = "1", true
			}
		} else {
			wrote = true
		}
		add = append(add, r)
	}
	if err := m.c.InsertReconciledMembers(context.Background(), s.current, nil, add); err != nil {
		m.failf("%s: InsertReconciledMembers: %v", s.name(), err)
	}
	for _, r := range add {
		s.rows[r.Path] = r
	}
	m.seen["reconcile"]++
	if healed {
		m.seen["healing reconcile"]++
	}
	m.note("%s: reconcile of %d rows (healed: %v)", s.name(), len(add), healed)
	return true, wrote || healed
}

// forget removes s's suite row, as POST /forget does (cap eviction and the
// reaper delete through the same deleteSuiteTx).
func (m *listModel) forget(s *modelSuite) {
	var want *int64
	if s.current != 0 {
		want = &s.current
	}
	res, err := m.c.ForgetSuite(context.Background(), s.key, want)
	if err != nil || !res.Deleted {
		m.failf("%s: ForgetSuite = %+v, %v", s.name(), res, err)
	}
	if s.rows != nil {
		m.seen["forgot a suite with a list"]++
	}
	s.exists, s.rows, s.current = false, nil, 0
}

func (m *listModel) create(s *modelSuite) {
	seedRow(m.t, m.c, s.key, 1)
	s.exists, s.gen = true, 0
}

func (m *listModel) quarantine(s *modelSuite) {
	if _, err := m.c.ApplyLegacyAuditDecisions(context.Background(), []LegacyAuditDecision{{SnapshotID: s.current, Outcome: LegacyAuditQuarantine}}); err != nil {
		m.failf("%s: quarantine: %v", s.name(), err)
	}
	if s.rows != nil {
		m.seen["quarantined a suite with a list"]++
	}
	s.rows, s.current = nil, 0
}

// publishPlain adopts rows for s without a difference.
func (m *listModel) publishPlain(s *modelSuite, rows map[string]PackageHash) {
	id, members := m.candidate(s, FormInline)
	if err := m.c.CommitAdoption(context.Background(), id, members, nil, m.shuffled(rows), nil, true); err != nil {
		m.failf("%s: CommitAdoption: %v", s.name(), err)
	}
	s.rows, s.current = rows, id
}

// do runs one random operation and updates the model.
func (m *listModel) do() {
	m.t.Helper()
	ctx := context.Background()
	s := m.suites[m.rng.Intn(len(m.suites))]
	name := s.name()
	if !s.exists {
		m.create(s)
		m.note("%s: created", name)
		return
	}
	switch op := m.rng.Intn(100); {
	case op < 37: // a publication, the adopter's way
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
	case op < 51: // a publication whose difference goes stale before the commit
		next := m.nextRows(s, "small")
		rows := m.shuffled(next)
		diff, err := m.c.DiffPackageList(ctx, s.key, rows)
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		// What happens to the suite's list between the read and the commit.
		event := []string{"a reconcile", "another publication", "another publication by difference", "a quarantine",
			"a forget and a new suite", "a forget, a new suite and its first adoption"}[m.rng.Intn(6)]
		wrote := true // whether the event wrote, replaced or un-named the list
		if event == "a reconcile" {
			if ran, w := m.reconcile(s); ran {
				wrote = w
			} else {
				event = "another publication"
			}
		}
		if event == "a quarantine" && s.current == 0 {
			event = "another publication"
		}
		switch event {
		case "another publication":
			m.publishPlain(s, m.nextRows(s, "small"))
		case "another publication by difference":
			// The adopter's way. A small difference is applied in place: the
			// list keeps its id, and the generation alone says it was written.
			other := m.nextRows(s, "small")
			orows := m.shuffled(other)
			oid, omembers := m.candidate(s, FormInline)
			odiff, err := m.c.DiffPackageList(ctx, s.key, orows)
			if err != nil {
				m.failf("%s: DiffPackageList: %v", name, err)
			}
			ow, err := m.c.CommitAdoptionDiff(ctx, oid, omembers, nil, orows, nil, true, odiff)
			if err != nil {
				m.failf("%s: CommitAdoptionDiff of the publication in between: %v", name, err)
			}
			s.rows, s.current = other, oid
			if ow.Mode == PackageListDifference {
				// Rows it did not change are as the stale difference expects
				// them; when it changed none, that difference is still exact.
				wrote = ow.Added+ow.Changed+ow.Removed > 0
				if wrote {
					m.seen["stale after a difference applied in place"]++
				}
			}
		case "a quarantine":
			m.quarantine(s)
		case "a forget and a new suite":
			m.forget(s)
			m.create(s)
		case "a forget, a new suite and its first adoption":
			// The new suite's list is another list at a generation that can
			// equal the old one's: only the list's id tells them apart.
			m.forget(s)
			m.create(s)
			m.publishPlain(s, m.nextRows(s, "fresh"))
		}
		id, members := m.candidate(s, FormInline)
		w, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, rows, nil, true, diff)
		if err != nil {
			m.failf("%s: CommitAdoptionDiff with a difference read before %s: %v", name, event, err)
		}
		if w.Reason == "mismatch" {
			// The tripwires are for a defect in the difference. A list that was
			// written since the read is the guard's to catch (the list's id and
			// the generation every writer bumps): a stale difference must never
			// reach them.
			m.failf("%s: a difference read before %s reached the tripwires: %+v", name, event, w)
		}
		if wrote && w.Mode == PackageListDifference {
			m.failf("%s: a difference read before %s was applied: %+v", name, event, w)
		}
		s.rows, s.current = next, id
		m.seen["stale after "+event]++
		m.note("%s: published after %s: %s %s", name, event, w.Mode, w.Reason)
	case op < 56: // a difference handed over with rows it was not computed for
		next := m.nextRows(s, "small")
		source, forDiff, whose := s, m.nextRows(s, "small"), "another publication's rows"
		if t := s.twin; t != nil && t.exists && t.rows != nil && m.rng.Intn(2) == 0 {
			source, forDiff, whose = t, m.nextRows(t, "small"), "the scheme twin's list and rows"
		}
		diff, err := m.c.DiffPackageList(ctx, source.key, m.shuffled(forDiff))
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		id, members := m.candidate(s, FormInline)
		w, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, m.shuffled(next), nil, true, diff)
		if err != nil {
			m.failf("%s: CommitAdoptionDiff with a difference for %s: %v", name, whose, err)
		}
		if w.Mode == PackageListDifference && (source != s || !maps.Equal(forDiff, next)) {
			m.failf("%s: a difference computed for %s was applied: %+v", name, whose, w)
		}
		s.rows, s.current = next, id
		if w.Reason == "mismatch" {
			m.seen["a difference for other rows refused"]++
		}
		if source != s {
			m.seen["the twin's difference refused"]++
		}
		m.note("%s: published with a difference for %s: %s %s", name, whose, w.Mode, w.Reason)
	case op < 62: // a publication without a difference
		next := m.nextRows(s, []string{"small", "fresh"}[m.rng.Intn(2)])
		m.publishPlain(s, next)
		m.note("%s: published without a difference (%d rows)", name, len(next))
	case op < 67: // an unsigned publication
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
	case op < 73: // a commit that fails after the list step
		next := m.nextRows(s, "small")
		rows := m.shuffled(next)
		id, members := m.candidate(s, FormInline)
		diff, err := m.c.DiffPackageList(ctx, s.key, rows)
		if err != nil {
			m.failf("%s: DiffPackageList: %v", name, err)
		}
		bad := []PrefetchedURLPath{{CanonicalScheme: s.key.CanonicalScheme, CanonicalHost: s.key.CanonicalHost, Path: "/pool/x.deb", BlobHash: "bad", UpstreamURL: "http://u/x"}}
		if _, err := m.c.CommitAdoptionDiff(ctx, id, members, nil, rows, bad, true, diff); !errors.Is(err, ErrInvalidHash) {
			m.failf("%s: a commit with a malformed warmed hash = %v, want ErrInvalidHash", name, err)
		}
		m.seen["failed commit"]++
		m.note("%s: a commit failed", name)
	case op < 81:
		if ran, _ := m.reconcile(s); !ran {
			m.note("%s: nothing to reconcile", name)
		}
	case op < 86: // the suite is forgotten
		m.forget(s)
		m.note("%s: forgotten", name)
	case op < 91: // its current snapshot is quarantined
		if s.current == 0 {
			m.note("%s: no snapshot to quarantine", name)
			return
		}
		m.quarantine(s)
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
	total := 0
	for _, s := range m.suites {
		name := s.name()
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
		total += len(s.rows)
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
	// The admin count reads the lists suites name, and nothing else.
	cov, err := m.c.GetRepoCoverage(ctx)
	if err != nil {
		m.failf("GetRepoCoverage: %v", err)
	}
	if int(cov.PackageHashRowsTotal) != total {
		m.failf("GetRepoCoverage counts %d package rows, the model has %d", cov.PackageHashRowsTotal, total)
	}
	// Vouching answers from the model: each suite of the scheme and host
	// that declares the file, with its hash and its current snapshot;
	// nothing else. Three kinds of request: a pool file by its exact path, a
	// flat repository's file by its stored "./" path, and that file as a
	// client asks for it, which only the suffix fallback answers.
	for n := 0; n < 3; n++ {
		at := m.suites[m.rng.Intn(len(m.suites))]
		scheme, host := at.key.CanonicalScheme, at.key.CanonicalHost
		i := m.rng.Intn(modelPool)
		kind := []string{"pool", "flat stored", "flat requested"}[m.rng.Intn(3)]
		stored := modelPath(kind != "pool", i)
		request := stored
		if kind == "flat requested" {
			request = fmt.Sprintf("/flat/f%02d.deb", i)
		}
		var want []string
		for _, s := range m.suites {
			if r, ok := s.rows[stored]; ok && s.key.CanonicalScheme == scheme && s.key.CanonicalHost == host {
				want = append(want, fmt.Sprintf("%s@%s#%d", r.DeclaredSHA256[:1], s.key.SuitePath, s.current))
			}
		}
		slices.Sort(want)
		rows, err := m.c.DeclaredHashesForPath(ctx, scheme, host, request)
		if err != nil {
			m.failf("DeclaredHashesForPath(%s://%s%s): %v", scheme, host, request, err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, fmt.Sprintf("%s@%s#%d", r.DeclaredSHA256[:1], r.SuitePath, r.SnapshotID))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			m.failf("DeclaredHashesForPath(%s://%s%s) = %v, the model has %v", scheme, host, request, got, want)
		}
		if len(want) > 0 && kind == "flat requested" {
			m.seen["a flat file vouched through the suffix fallback"]++
		}
		if len(want) > 0 && scheme == "https" {
			m.seen["the https twin vouched"]++
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
		add := func(scheme, host, path string, flat bool) *modelSuite {
			s := &modelSuite{key: SuiteKey{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: path}, flat: flat}
			m.suites = append(m.suites, s)
			return s
		}
		// One suite under both schemes, another on the same host, the first
		// again on a second host, and a flat repository on each host.
		a, twin := add("http", "h1", "/a/dists/s", false), add("https", "h1", "/a/dists/s", false)
		a.twin, twin.twin = twin, a
		add("http", "h1", "/b/dists/s", false)
		add("http", "h2", "/a/dists/s", false)
		add("http", "h1", "/flat", true)
		add("http", "h2", "/flat", true)
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
	// un-named and deleted, and every kind of reader the model checks (a
	// model that never took the difference path, or never asked for a flat
	// file, would pass for nothing). A short run cannot promise the rare
	// ones.
	if seeds*steps >= 1500 {
		for _, kind := range []string{
			PackageListDifference + " ", PackageListRewrite + " first", PackageListRewrite + " large",
			"stale after a reconcile", "stale after another publication", "stale after a quarantine",
			"stale after another publication by difference", "stale after a difference applied in place",
			"stale after a forget and a new suite", "stale after a forget, a new suite and its first adoption",
			"a difference for other rows refused", "the twin's difference refused",
			"reconcile", "healing reconcile", "unsigned published", "unsigned refused", "failed commit",
			"forgot a suite with a list", "quarantined a suite with a list", "gc deleted rows",
			"a flat file vouched through the suffix fallback", "the https twin vouched",
		} {
			if seen[kind] == 0 {
				t.Errorf("the model never saw %q in %d seeds of %d steps: %v", kind, seeds, steps, seen)
			}
		}
	}
	t.Logf("what the model saw: %v", seen)
}
