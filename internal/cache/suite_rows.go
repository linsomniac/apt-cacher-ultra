package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

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
	// blob).
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
}

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
// refuses at the per-host cap, and at the global cap until cap eviction
// (for which guard holds the evicted suite's mutex) is wired. Count and
// insert run in one writer transaction, so concurrent creations never
// exceed a cap. A created row's base is marked signed-seen in the same
// transaction for seed.SignedSeenAt and for every sniff-passing anchor in
// seed.Cached whose row still holds the sniffed blob (ruling PD-5).
func (c *Cache) CreateSuite(ctx context.Context, seed SuiteSeed, caps SuiteCaps, guard SuiteGuard) (CreateResult, error) {
	_ = guard // unused until cap eviction (Task 5)
	var got CreateResult
	err := c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("CreateSuite: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		r, err := createSuiteTx(ctx, tx, seed, caps)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("CreateSuite: commit: %w", err)
		}
		got = r
		return nil
	})
	if err != nil {
		// Never read got after an error: on a ctx/close path the op may
		// still be running (see InsertCandidateSnapshot).
		return CreateResult{}, err
	}
	return got, nil
}

// createSuiteTx inserts seed's row unless it exists, enforcing caps inside
// the caller's writer transaction, and marks a created row's base
// signed-seen for the signed metadata its creation knows of (markCreated).
// It holds the only INSERT INTO suite_freshness in production code (spec
// C3).
//
// AIDEV-NOTE: keep this the single INSERT site
// (TestSuiteFreshnessHasOneInsertSite). Everything else that writes
// suite_freshness is UPDATE-only, so no writer can resurrect a row that
// eviction (or phase F's reaper) deleted.
func createSuiteTx(ctx context.Context, tx *sql.Tx, seed SuiteSeed, caps SuiteCaps) (CreateResult, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
		seed.CanonicalScheme, seed.CanonicalHost, seed.SuitePath).Scan(&one)
	switch {
	case err == nil:
		return CreateResult{Outcome: SuiteExists}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return CreateResult{}, fmt.Errorf("CreateSuite: read row: %w", err)
	}
	if caps.PerHost > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness WHERE canonical_host = ?`,
			seed.CanonicalHost).Scan(&n); err != nil {
			return CreateResult{}, fmt.Errorf("CreateSuite: count host suites: %w", err)
		}
		if n >= caps.PerHost {
			return CreateResult{Outcome: SuiteRejectedHost}, nil
		}
	}
	if caps.Global > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suite_freshness`).Scan(&n); err != nil {
			return CreateResult{}, fmt.Errorf("CreateSuite: count suites: %w", err)
		}
		if n >= caps.Global {
			return CreateResult{Outcome: SuiteRejectedGlobal}, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path,
   last_check_at, last_success_at, inrelease_etag, inrelease_lastmod, last_requested_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		seed.CanonicalScheme, seed.CanonicalHost, seed.SuitePath,
		seed.LastCheckAt, seed.LastSuccessAt, seed.InReleaseETag, seed.InReleaseLastMod, seed.RequestedAt); err != nil {
		return CreateResult{}, fmt.Errorf("CreateSuite: insert: %w", err)
	}
	unproven, err := markCreated(ctx, tx, seed)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Outcome: SuiteCreated, Unproven: unproven}, nil
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
			return false, fmt.Errorf("CreateSuite: read cached anchor %s: %w", a.Path, err)
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
		return false, fmt.Errorf("CreateSuite: %w", err)
	}
	return unproven, nil
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
