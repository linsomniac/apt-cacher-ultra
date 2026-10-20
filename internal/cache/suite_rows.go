package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// suiteRowGoneTimeout bounds afterCreate's read of a held candidate's row
// after a failed submit. (Cap eviction's constants are in suite_evict.go.)
const suiteRowGoneTimeout = 5 * time.Second

// Spec C3: suite rows have one creation choke point. createSuiteTx holds
// the only INSERT INTO suite_freshness in production code (pinned by
// TestSuiteFreshnessHasOneInsertSite); every other writer is UPDATE-only.

// ErrSuiteGone is returned by CommitAdoption when the suite row no longer
// exists: the flip is rolled back and nothing is written (spec C3).
//
// AIDEV-NOTE: unreachable while every deleter of a suite row holds the
// suite's mutex (cap eviction through a SuiteGuard; phase F's reaper and
// POST /forget) — an adoption holds it from its check to its commit. It
// stays as the backstop for a guard-less deleter: the commit rolls back
// instead of resurrecting the row.
var ErrSuiteGone = errors.New("cache: suite row is gone")

// SuiteKey names one suite.
type SuiteKey struct {
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string
}

// SuiteSeed is the row CreateSuite inserts (spec C3).
type SuiteSeed struct {
	SuiteKey
	// LastCheckAt, LastSuccessAt and the validators are set only by the
	// adoption-disabled InRelease miss (today's seed: the miss counts as a
	// successful check). Every other creation leaves them NULL, so the
	// suite's first check runs at once and unconditionally.
	LastCheckAt, LastSuccessAt      *int64
	InReleaseETag, InReleaseLastMod *string
	// RequestedAt is the row's client-interest stamp (last_requested_at):
	// the creating request's wall-clock second.
	RequestedAt int64
	// SignedSeenAt, when non-zero, records the signed-seen marker for the
	// suite's base in the creating transaction: the creating request served
	// sniff-passing signed metadata.
	SignedSeenAt int64
	// Cached lists the suite's signed-anchor url_path rows as the creating
	// request read them (ruling PD-5): a created row's base is marked, in
	// the creating transaction, for every one whose row still holds the
	// blob that passed the sniff. A listed row whose blob changed — or that
	// appeared — since the read sets CreateResult.Unproven.
	Cached []CachedAnchor
}

// CachedAnchor is one of a suite's signed-anchor url_path rows
// (<suite>/InRelease or <suite>/Release.gpg) as the creating request read
// it, its blob sniffed outside the writer (ruling PD-5: no pool I/O on the
// writer goroutine). A sniff result is a function of the blob, so
// createSuiteTx needs only to see that the row still holds BlobHash.
type CachedAnchor struct {
	// Path is the url_path path, e.g. "/ubuntu/dists/noble/Release.gpg".
	Path string
	// BlobHash is the blob the row held; "" when there was no row (or no
	// blob), or when the caller could not read the row or its pool file —
	// a row the transaction then finds holding a blob makes the creation
	// unproven, so the caller's backstop retries it.
	BlobHash string
	// Signed: that blob passed the anchor's sniff (spec C3).
	Signed bool
	// SeenAt is the stamp the marker gets: the row's last use (the newer
	// of last_requested_at and last_fetched_at); 0 when unknown, which
	// stamps the creation's RequestedAt.
	SeenAt int64
}

// SuiteCaps bounds row creation (spec C3): Global is freshness.max_suites,
// PerHost is max_suites/10 rows per canonical host. Zero is unbounded.
type SuiteCaps struct {
	Global  int
	PerHost int
}

// CreateOutcome is what CreateSuite did.
type CreateOutcome int

const (
	// SuiteCreated: the row was inserted.
	SuiteCreated CreateOutcome = iota + 1
	// SuiteExists: the row was already there; nothing was written.
	SuiteExists
	// SuiteRejectedGlobal: freshness.max_suites rows exist and no idle
	// suite could be evicted.
	SuiteRejectedGlobal
	// SuiteRejectedHost: the canonical host already has max_suites/10 rows.
	SuiteRejectedHost
)

// String is the outcome's log value.
func (o CreateOutcome) String() string {
	switch o {
	case SuiteCreated:
		return "created"
	case SuiteExists:
		return "exists"
	case SuiteRejectedGlobal:
		return "global_cap"
	case SuiteRejectedHost:
		return "host_cap"
	}
	return "none"
}

// Rejected reports whether the outcome is a cap rejection.
func (o CreateOutcome) Rejected() bool { return o == SuiteRejectedGlobal || o == SuiteRejectedHost }

// CreateResult reports a creation and the suite evicted to make room.
type CreateResult struct {
	Outcome CreateOutcome
	Evicted *SuiteKey
	// Unproven: a row listed in SuiteSeed.Cached changed or appeared
	// between the caller's read and the creating transaction, so its blob
	// was never sniffed; the caller marks it after the commit (the
	// handler's markCachedSignedAnchors backstop).
	Unproven bool
	// CandidatesBusy: a global-cap refusal while every listed eviction
	// candidate was busy (its mutex held by a check, an adoption or
	// another creation) — transient, since an idle suite may well make
	// room within moments (the handler caches it for a minute, not ten).
	CandidatesBusy bool
	// heldGone: the held eviction candidate had no row when the writer ran
	// (a concurrent creation had evicted it, or a reap or POST /forget had
	// deleted it, before this creation held it); heldKept: it no longer
	// qualified (a client asked for it in between). Either way CreateSuite
	// and StoreFetched try once more with a fresh candidate list.
	heldGone, heldKept bool
}

// retry reports whether the attempt should be made once more with a fresh
// candidate list: its held candidate vanished or was kept (design review
// M4; final review, concurrency M2).
func (r CreateResult) retry() bool { return r.heldGone || r.heldKept }

// SuiteGuard lets cap eviction hold a candidate's in-memory suite mutex
// (spec C3: eviction runs under the suite's mutex) without internal/cache
// importing internal/freshness; *freshness.Checker implements it.
// HoldSuite must not block: ok is false when the suite is busy (a check or
// an adoption holds its mutex). release must be called exactly once;
// release(true) — the row was deleted — also drops the suite's in-memory
// state (lock-map entry, flip-gate entry).
type SuiteGuard interface {
	HoldSuite(key SuiteKey) (release func(deleted bool), ok bool)
}

// CreateSuite is spec C3's single creation choke point, for a suite the
// caller found without a row (the handler's url_path-hit onboarding). It
// creates the row only if absent (SuiteExists otherwise), within caps: it
// refuses at the per-host cap; at the global cap it first evicts the
// least-recently-requested suite that has no current snapshot, has been
// idle for suiteEvictIdle and whose mutex guard takes without waiting (a
// nil guard holds no mutex — no in-memory state exists to protect — and
// the candidate is still evicted); above the global cap it evicts one such
// suite and still refuses (ruling PD-8). Count, eviction and insert run in
// one writer transaction, so concurrent creations never exceed a cap. A
// created row's base is marked signed-seen in the same transaction for
// seed.SignedSeenAt and for every sniff-passing anchor in seed.Cached
// whose row still holds the sniffed blob (ruling PD-5).
//
// AIDEV-NOTE: the evictee's metadata url_path rows are left to url_path
// GC: a request never pays for a batched delete (phase F's reaper does
// that work, off the request path). The guard is taken before the writer
// operation is submitted — the writer goroutine never runs foreign code.
func (c *Cache) CreateSuite(ctx context.Context, seed SuiteSeed, caps SuiteCaps, guard SuiteGuard) (CreateResult, error) {
	attempt := func() (CreateResult, error) {
		r, err := c.creationAttempt(ctx, "CreateSuite", &seed, caps, guard,
			func(ctx context.Context, tx *sql.Tx, held *heldSuite) (FetchedResult, error) {
				cr, err := createSuiteTx(ctx, tx, seed, caps, held, nowUnix())
				return FetchedResult{Create: cr}, err
			})
		return r.Create, err
	}
	res, err := attempt()
	if err == nil && res.retry() {
		// The held candidate's row was deleted between the candidate list
		// and the writer (a concurrent creation evicted it, or a reap or
		// POST /forget deleted it before the hold), or kept (a client asked
		// for it meanwhile): one more attempt, with a fresh list (design
		// review M4; final review, concurrency M2).
		res, err = attempt()
	}
	return res, err
}

// creationAttempt is one attempt of a row creation — the lifecycle
// CreateSuite (hits) and StoreFetched (misses) share: when create is set,
// hold an eviction candidate (holdEvictionCandidate); run body in one
// writer transaction with it; then settle the candidate exactly once
// (afterCreate) — after a commit, after a failed submit, or on a panic.
// op names the caller in errors. With create nil nothing is held or
// settled.
//
// AIDEV-NOTE: the release-exactly-once protocol lives here alone (Task 6
// minor 5; final review, concurrency M1). A held candidate's hold is the
// checker's suite mutex: one leaked by a panic would leave that suite
// unchecked, unadopted and unevictable until a restart. So a panic
// between the hold and the settle releases it as not deleted (its
// lock-map and flip-gate entries then stay, one each, as after
// afterCreate's read error) and continues. Never read got after an
// error: on a ctx/close path the op may still be running (see
// InsertCandidateSnapshot).
func (c *Cache) creationAttempt(ctx context.Context, op string, create *SuiteSeed, caps SuiteCaps, guard SuiteGuard,
	body func(ctx context.Context, tx *sql.Tx, held *heldSuite) (FetchedResult, error)) (FetchedResult, error) {
	var held *heldSuite
	busy := false
	if create != nil {
		h, b, err := c.holdEvictionCandidate(ctx, caps, guard)
		if err != nil {
			return FetchedResult{}, fmt.Errorf("%s: %w", op, err)
		}
		held, busy = h, b
	}
	if held != nil {
		// A no-op once afterCreate settled the hold; on a panic it releases
		// it as not deleted, and the panic continues.
		defer held.settle(false)
		if c.heldHook != nil {
			c.heldHook(held.key)
		}
	}
	var got FetchedResult
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("%s: begin: %w", op, err)
		}
		defer func() { _ = tx.Rollback() }()
		r, err := body(ctx, tx, held)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s: commit: %w", op, err)
		}
		if c.committedHook != nil {
			c.committedHook()
		}
		got = r
		return nil
	})
	if create != nil {
		var res CreateResult // unknown after an error: afterCreate reads the row
		if err == nil {
			res = got.Create
		}
		c.afterCreate(create.SuiteKey, res, held)
	}
	if err != nil {
		return FetchedResult{}, err
	}
	if busy && got.Create.Outcome == SuiteRejectedGlobal {
		// Refused for want of a held candidate while candidates exist: they
		// were all busy (createSuiteTx refuses at the global cap with no
		// held candidate).
		got.Create.CandidatesBusy = true
	}
	return got, nil
}

// afterCreate settles a creation attempt after its writer operation. A
// created row counts as touched (its insert stamped last_requested_at),
// and the held eviction candidate is released, telling the guard whether
// its row is gone — deleted by this creation's eviction, or found absent —
// so that release(true), and the dropped touch-coalescing entry, purge the
// suite's in-memory state when this creation saw its row gone. A
// re-creation of the evictee that lands between that commit or read and
// the release keeps its live flip-gate entry (ruling R-F2); its lock-map
// and touch entries come back with its next check and touch (deferred
// minor D-12). Unless the result says so, it reads the row: after a failed
// submit the outcome is unknown, and an unused candidate's row may have
// been deleted before this one held it — evicted by a concurrent creation,
// or reaped or forgotten (the hold then made a fresh lock-map entry for a
// suite without a row).
//
// AIDEV-NOTE: while this creation holds the candidate's mutex no other
// deleter can remove its row (every deleter holds the guard). A failed
// submit can still commit: when the commit raced the context's end in
// submitWrite (an operation run on an ended context rolls back), and when
// submitWrite returned ErrClosed while the writer ran the operation with a
// live context (at shutdown: Close waits for the writer before db.Close,
// so the read races the commit and db.Close — whatever it answers is
// harmless at shutdown) (deferred minor D-12). So the read decides (design
// review M3, M4; TestCreateSuite_AFailedSubmitWhoseEvictionCommittedPurges).
// A read error, or a read that lands before a commit still in flight,
// answers "present": the evictee's lock-map, flip-gate and touch entries
// then stay (one each, bounded), and a check that starts meanwhile meets a
// vanished row, which I-4's re-validation and the UPDATE-only writers make
// harmless (its candidate, if any, was retired: CommitAdoption refuses it).
func (c *Cache) afterCreate(key SuiteKey, res CreateResult, held *heldSuite) {
	if res.Outcome == SuiteCreated {
		c.noteTouch(key)
	}
	if held == nil {
		return // an eviction needs a held candidate: nothing was evicted
	}
	// A kept candidate's row was there in the committed transaction, and no
	// deleter can have removed it since (this creation holds its guard).
	gone := res.Evicted != nil || res.heldGone || (!res.heldKept && c.suiteRowGone(held.key))
	if gone {
		c.forgetTouch(held.key)
	}
	held.settle(gone)
}

// suiteRowGone reports whether key's row is absent, read under a fresh
// context (after a failed submit the caller's may have ended). A read
// error answers false.
func (c *Cache) suiteRowGone(key SuiteKey) bool {
	ctx, cancel := context.WithTimeout(context.Background(), suiteRowGoneTimeout)
	defer cancel()
	exists, err := c.HasSuiteRow(ctx, key)
	return err == nil && !exists
}

// createSuiteTx inserts seed's row unless it exists, enforcing caps inside
// the caller's writer transaction, and marks a created row's base
// signed-seen for the signed metadata its creation knows of (markCreated).
// At or above the global cap it first evicts held (deleteSuiteTx), and
// refuses when held is nil or no longer qualifies (the caller then retries
// once with a fresh list, heldGone/heldKept) — or, above the cap, after
// the eviction (ruling PD-8). It holds the only INSERT INTO
// suite_freshness in production code (spec C3). Its errors say "create
// suite row", which both callers do — onboarding (CreateSuite) and a miss
// (StoreFetched) — never one caller's name (deferred minor D-15).
//
// AIDEV-NOTE: keep this the single INSERT site
// (TestSuiteFreshnessHasOneInsertSite). Everything else that writes
// suite_freshness is UPDATE-only, so no writer can resurrect a row that
// eviction (or phase F's reaper) deleted.
func createSuiteTx(ctx context.Context, tx *sql.Tx, seed SuiteSeed, caps SuiteCaps, held *heldSuite, now int64) (CreateResult, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		seed.CanonicalScheme, seed.CanonicalHost, seed.SuitePath).Scan(&one)
	switch {
	case err == nil:
		return CreateResult{Outcome: SuiteExists}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return CreateResult{}, fmt.Errorf("create suite row: read row: %w", err)
	}
	// AIDEV-NOTE: the per-host refusal must precede the global block,
	// which may evict: a refusal after an eviction commits the eviction
	// (TestCreateSuite_HostCapRefusalEvictsNothing). The global block's own
	// "evict, then refuse" above the cap is deliberate and reported in
	// CreateResult.Evicted, so the evictee's in-memory state is purged.
	if caps.PerHost > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness WHERE canonical_host = ?`,
			seed.CanonicalHost).Scan(&n); err != nil {
			return CreateResult{}, fmt.Errorf("create suite row: count host suites: %w", err)
		}
		if n >= caps.PerHost {
			return CreateResult{Outcome: SuiteRejectedHost}, nil
		}
	}
	var res CreateResult
	if caps.Global > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness`).Scan(&n); err != nil {
			return CreateResult{}, fmt.Errorf("create suite row: count suites: %w", err)
		}
		if n >= caps.Global {
			if held == nil {
				return CreateResult{Outcome: SuiteRejectedGlobal}, nil
			}
			out, err := deleteSuiteTx(ctx, tx, held.key, nil, now-int64(suiteEvictIdle/time.Second), now)
			if err != nil {
				return CreateResult{}, err
			}
			if out != suiteDeleted {
				return CreateResult{Outcome: SuiteRejectedGlobal, heldGone: out == suiteNoRow, heldKept: out == suiteKept}, nil
			}
			evicted := held.key
			res.Evicted = &evicted
			if n-1 >= caps.Global {
				// Above the cap (a lowered max_suites, or an upgraded v6
				// table): the eviction does not make room, but each attempt
				// still evicts one, so the table converges under the 24 h
				// rule instead of waiting for phase F's 30-day reaper.
				res.Outcome = SuiteRejectedGlobal
				return res, nil
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at, inrelease_etag, inrelease_lastmod, last_requested_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		seed.CanonicalScheme, seed.CanonicalHost, seed.SuitePath,
		seed.LastCheckAt, seed.LastSuccessAt, seed.InReleaseETag, seed.InReleaseLastMod, seed.RequestedAt); err != nil {
		return CreateResult{}, fmt.Errorf("create suite row: insert: %w", err)
	}
	unproven, err := markCreated(ctx, tx, seed)
	if err != nil {
		return CreateResult{}, err
	}
	res.Outcome, res.Unproven = SuiteCreated, unproven
	return res, nil
}

// markCreated marks a just-created suite's base signed-seen, in its
// creating transaction, once, with the newest stamp among the signed
// metadata the creation knows of: the body in hand (seed.SignedSeenAt) and
// every seed.Cached anchor that passed the sniff and whose url_path row
// still holds that blob. It reports unproven when a listed row's blob
// changed or appeared since the caller read it: that blob was never
// sniffed, so the caller marks it after the commit.
//
// AIDEV-NOTE: ruling PD-5. The marker is the never-downgrade authority
// (spec C5): a suite must never be tracked while one of its cached signed
// anchors is served without its base marked, so these marks commit with
// the row or not at all (spec C3 "no residual window") — never in a
// follow-up operation that could fail. The blobs were sniffed by the
// caller: no pool I/O runs on the writer goroutine.
func markCreated(ctx context.Context, tx *sql.Tx, seed SuiteSeed) (unproven bool, err error) {
	seen := seed.SignedSeenAt
	for _, a := range seed.Cached {
		var cur sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT blob_hash FROM url_path
 WHERE canonical_scheme = ? AND canonical_host = ? AND path = ?`,
			seed.CanonicalScheme, seed.CanonicalHost, a.Path).Scan(&cur)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("create suite row: read cached anchor %s: %w", a.Path, err)
		}
		switch {
		case cur.String != a.BlobHash:
			// Changed or appeared (unproven), or gone (nothing to mark).
			unproven = unproven || cur.String != ""
		case a.Signed && a.BlobHash != "":
			at := a.SeenAt
			if at <= 0 {
				at = seed.RequestedAt
			}
			seen = max(seen, at)
		}
	}
	if seen == 0 {
		return unproven, nil
	}
	if err := markSignedSeen(ctx, tx, seed.CanonicalScheme, seed.CanonicalHost,
		aptpath.MarkerBase(seed.SuitePath), seen); err != nil {
		return false, fmt.Errorf("create suite row: %w", err)
	}
	return unproven, nil
}

// HasSuiteRow reports whether the suite has a suite_freshness row: the
// handler's cheap tracked-suite test (one indexed read; the flip barrier
// registers only tracked suites' anchor serves), the checker's I-4 row
// gate and afterCreate's decider. (Renamed from SuiteExists, which is also
// a CreateOutcome; deferred minor D-13.)
func (c *Cache) HasSuiteRow(ctx context.Context, key SuiteKey) (bool, error) {
	var one int
	err := c.db.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		key.CanonicalScheme, key.CanonicalHost, key.SuitePath).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("HasSuiteRow: %w", err)
	}
	return true, nil
}

// SuiteCapUsage is the suite table measured against the caps (ruling
// PD-8's startup WARN suite_caps_exceeded).
type SuiteCapUsage struct {
	// Total is the number of suite rows.
	Total int
	// HostsAtCap is the number of canonical hosts with at least the
	// per-host cap's rows.
	HostsAtCap int
	// FullestHost has the most rows (ties: the first by name); "" when
	// there are none.
	FullestHost     string
	FullestHostRows int
}

// SuiteCapUsage counts suite rows in all and per canonical host (across
// schemes, as the per-host cap counts); perHost <= 0 counts no host as
// at its cap.
func (c *Cache) SuiteCapUsage(ctx context.Context, perHost int) (SuiteCapUsage, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT canonical_host, COUNT(*) AS n FROM suite_freshness
 GROUP BY canonical_host
 ORDER BY n DESC, canonical_host`)
	if err != nil {
		return SuiteCapUsage{}, fmt.Errorf("SuiteCapUsage: %w", err)
	}
	var u SuiteCapUsage
	for rows.Next() {
		var host string
		var n int
		if err := rows.Scan(&host, &n); err != nil {
			_ = rows.Close()
			return SuiteCapUsage{}, fmt.Errorf("SuiteCapUsage: scan: %w", err)
		}
		u.Total += n
		if u.FullestHost == "" {
			u.FullestHost, u.FullestHostRows = host, n
		}
		if perHost > 0 && n >= perHost {
			u.HostsAtCap++
		}
	}
	if err := rows.Close(); err != nil {
		return SuiteCapUsage{}, fmt.Errorf("SuiteCapUsage: %w", err)
	}
	if err := rows.Err(); err != nil {
		return SuiteCapUsage{}, fmt.Errorf("SuiteCapUsage: %w", err)
	}
	return u, nil
}

// UpdateSuiteFreshness writes a check's result to an existing suite row:
// last_check_at, last_success_at, the validators, inrelease_change_seen_at
// and check_fail_count. UPDATE-only (spec C3): a suite without a row is
// left alone (0 rows is not an error). It never writes current_snapshot_id
// (CommitAdoption's flip owns it).
//
// AIDEV-NOTE: last_requested_at (v7) is the suite's client-interest
// stamp. This writer never writes it (createSuiteTx sets it, TouchSuite
// updates it), so a scheduler write cannot keep an idle suite alive.
// check_fail_count (spec C4) is written on every call: each check result
// sets or resets the streak in the same statement that bumps
// last_check_at. The adoption failure record (adoption_fail_*) is never
// written here (spec C7). (Carried over from PutSuiteFreshness.)
func (c *Cache) UpdateSuiteFreshness(ctx context.Context, s SuiteFreshness) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if err := updateSuiteFreshness(ctx, conn, s); err != nil {
			return fmt.Errorf("UpdateSuiteFreshness: %w", err)
		}
		return nil
	})
}

// updateSuiteFreshness is UpdateSuiteFreshness's statement (also run by
// the test-only SeedSuiteForTest).
func updateSuiteFreshness(ctx context.Context, ex execer, s SuiteFreshness) error {
	_, err := ex.ExecContext(ctx, `
UPDATE suite_freshness
   SET last_check_at            = ?,
       last_success_at          = ?,
       inrelease_etag           = ?,
       inrelease_lastmod        = ?,
       inrelease_change_seen_at = ?,
       check_fail_count         = ?
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		s.LastCheckAt, s.LastSuccessAt, s.InReleaseETag, s.InReleaseLastMod, s.InReleaseChangeSeenAt,
		s.CheckFailCount, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
	return err
}
