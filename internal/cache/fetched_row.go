package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// FetchedRow is what a metadata miss stores in one writer transaction,
// before its body is served (spec C3): the url_path row and, for a suite
// file, the suite row and the signed-seen marker.
type FetchedRow struct {
	Row URLPath
	// Suite is the suite the path belongs to; zero when it has none.
	Suite SuiteKey
	// Create, when set, creates Suite's row (Create.SuiteKey == Suite) if
	// it has none, under the caps, with cap eviction.
	Create *SuiteSeed
	// MarkSeenAt, when non-zero, marks the suite's base signed-seen if the
	// suite has a row once Create has run: the body is a sniff-passing
	// InRelease or Release.gpg.
	MarkSeenAt int64
	// Restamp, when set (adoption disabled; a sniff-passing InRelease),
	// re-stamps an existing row without a current snapshot with Restamp's
	// check stamps and validators — today's Phase-1 refresh miss (ruling
	// PD-6) — UPDATE-only, in the same transaction; a row Create just made
	// already carries them.
	Restamp *SuiteSeed
}

// FetchedResult reports what StoreFetched did for the suite.
type FetchedResult struct {
	// Create is the creation's result (zero Outcome when Create was nil).
	Create CreateResult
	// Tracked: the suite has a row after the transaction.
	Tracked bool
	// Marked: the signed-seen marker was written.
	Marked bool
}

// StoreFetched stores a metadata miss's url_path row, creates the suite row,
// re-stamps it and writes the signed-seen marker as m asks, all in ONE
// writer transaction (spec C3): a signed anchor body is never served before
// its base has a marker, and a failure stores nothing — the caller then
// fails the miss (502, no body) as it does today. A cap rejection is not a
// failure: the url_path row is stored and the suite stays untracked (no
// marker). Like CreateSuite, it tries once more when its held eviction
// candidate vanished or was kept (requested) before the writer ran.
//
// AIDEV-NOTE: the writer is a FIFO queue drained by one goroutine
// (submitWrite), so an unsigned CommitAdoption enqueued after this
// operation runs after it, and its in-transaction marker re-read (phase E)
// sees the marker (TestStoreFetched_MarkerPrecedesALaterWriterOperation).
func (c *Cache) StoreFetched(ctx context.Context, m FetchedRow, caps SuiteCaps, guard SuiteGuard) (FetchedResult, error) {
	attempt := func() (FetchedResult, error) {
		return c.creationAttempt(ctx, "StoreFetched", m.Create, caps, guard,
			func(ctx context.Context, tx *sql.Tx, held *heldSuite) (FetchedResult, error) {
				return storeFetchedTx(ctx, tx, m, caps, held)
			})
	}
	res, err := attempt()
	if err == nil && res.Create.retry() {
		// The held candidate was evicted by a concurrent creation first, or
		// kept (a client asked for it meanwhile): one more attempt with a
		// fresh list (the url_path upsert repeats harmlessly).
		res, err = attempt()
	}
	return res, err
}

// storeFetchedTx is one StoreFetched attempt's transaction body
// (creationAttempt runs it on the writer, with the held candidate).
func storeFetchedTx(ctx context.Context, tx *sql.Tx, m FetchedRow, caps SuiteCaps, held *heldSuite) (FetchedResult, error) {
	isMD := int64(0)
	if m.Row.IsMetadata {
		isMD = 1
	}
	u := m.Row
	if _, err := tx.ExecContext(ctx, putURLPathSQL,
		u.CanonicalScheme, u.CanonicalHost, u.Path, u.BlobHash, u.UpstreamURL,
		isMD, u.LastRequestedAt, u.RequestCount, u.LastFetchedAt,
		u.UpstreamETag, u.UpstreamLastMod); err != nil {
		return FetchedResult{}, fmt.Errorf("StoreFetched: url_path: %w", err)
	}
	var r FetchedResult
	if m.Create != nil {
		cr, err := createSuiteTx(ctx, tx, *m.Create, caps, held, nowUnix())
		if err != nil {
			return FetchedResult{}, err
		}
		r.Create = cr
	}
	if m.Restamp != nil && r.Create.Outcome != SuiteCreated {
		if err := restampSuiteTx(ctx, tx, *m.Restamp); err != nil {
			return FetchedResult{}, err
		}
	}
	if m.Suite != (SuiteKey{}) {
		var one int
		err := tx.QueryRowContext(ctx, `
SELECT 1 FROM suite_freshness
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?`,
			m.Suite.CanonicalScheme, m.Suite.CanonicalHost, m.Suite.SuitePath).Scan(&one)
		switch {
		case err == nil:
			r.Tracked = true
		case !errors.Is(err, sql.ErrNoRows):
			return FetchedResult{}, fmt.Errorf("StoreFetched: read suite row: %w", err)
		}
	}
	if m.MarkSeenAt != 0 && r.Tracked {
		if err := markSignedSeen(ctx, tx, m.Suite.CanonicalScheme, m.Suite.CanonicalHost,
			aptpath.MarkerBase(m.Suite.SuitePath), m.MarkSeenAt); err != nil {
			return FetchedResult{}, fmt.Errorf("StoreFetched: %w", err)
		}
		r.Marked = true
	}
	return r, nil
}

// restampSuiteTx is the adoption-disabled refresh miss's re-stamp (ruling
// PD-6): UPDATE-only, an existing row without a current snapshot takes
// the miss's check stamps and validators as today's Phase-1 seed upsert
// wrote them — the miss counts as a successful check, and the pending
// change and the failure streak clear. last_requested_at and the adoption
// failure record stay, as they did.
func restampSuiteTx(ctx context.Context, tx *sql.Tx, s SuiteSeed) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_freshness
   SET last_check_at            = ?,
       last_success_at          = ?,
       inrelease_etag           = ?,
       inrelease_lastmod        = ?,
       inrelease_change_seen_at = NULL,
       check_fail_count         = 0
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ? AND current_snapshot_id IS NULL`,
		s.LastCheckAt, s.LastSuccessAt, s.InReleaseETag, s.InReleaseLastMod,
		s.CanonicalScheme, s.CanonicalHost, s.SuitePath); err != nil {
		return fmt.Errorf("StoreFetched: re-stamp: %w", err)
	}
	return nil
}
