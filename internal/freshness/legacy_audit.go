package freshness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Test seams (spec C11 constants: anchor read cap 8 MiB, batch 100).
var (
	legacyAuditAnchorCap  int64 = 8 << 20
	legacyAuditBatchSize        = 100
	legacyAuditAfterBatch       = func(batch int) error { return nil }
	// legacyClassify is auditLegacySnapshot's classifier; tests swap it to
	// inject a panic.
	legacyClassify = classifyLegacyInline
	// legacyDiscardAnchor removes a corrupt anchor's pool file (ruling
	// R-H2); tests swap it to inject a failure.
	legacyDiscardAnchor = func(c *cache.Cache, hash string) error { return c.DiscardFinalizedBlob(hash) }
)

var errAnchorTooLarge = errors.New("anchor exceeds the audit read cap")

// LegacyAuditResult summarises one RunLegacyTrustAudit call.
type LegacyAuditResult struct {
	Audited, Signed, Quarantined, Deferred, AnchorMarkers int
}

type legacyVerdict struct {
	outcome cache.LegacyAuditOutcome
	reason  string // set for quarantines
	// dropAnchorRows: the quarantine also deletes the url_path rows of the
	// stored InRelease blob (anchor_corrupt only; ruling R-H2).
	dropAnchorRows bool
}

// RunLegacyTrustAudit drains legacy_trust_audit, then records signed-seen
// markers from tracked suites' cached signed anchors (spec §6.1, C3). main
// runs it after cache.Open and before GC, the checker or any listener
// serves. An error means the database failed or the pool is missing,
// empty or holds none of the files checked (preflightLegacyAudit; deferred
// minor A-27): the caller refuses to start, nothing in the failed batch was
// applied, and the audit resumes on the next start. A single unreadable anchor, or a panic
// while auditing one snapshot, is not an error: that snapshot is deferred
// (left queued, unchanged, ERROR logged). Every error it returns starts
// "legacy trust audit:" — main's refusal line relies on it (deferred minors
// A-48, C-29) — so a new error path must keep the prefix.
//
// AIDEV-NOTE: plan review 2026-10-05 findings 1–3 and its follow-ups.
// Quarantine is the remedy for anything that cannot be proved signed AND
// clean; never "repair" a polluted snapshot in place; never write a marker
// for a plain-text, undecodable, missing or corrupt anchor (it would block
// the suite's correct unsigned adoption in phase E); and never turn an I/O
// fault into a quarantine — only "file does not exist" is evidence.
func RunLegacyTrustAudit(ctx context.Context, c *cache.Cache, logger *slog.Logger) (LegacyAuditResult, error) {
	start := time.Now()
	var res LegacyAuditResult
	if err := preflightLegacyAudit(ctx, c); err != nil {
		return res, err
	}
	afterID := int64(0)
	for batch := 1; ; batch++ {
		items, err := c.LegacyAuditBatch(ctx, afterID, legacyAuditBatchSize)
		if err != nil {
			return res, fmt.Errorf("legacy trust audit: %w", err)
		}
		if len(items) == 0 {
			break
		}
		afterID = items[len(items)-1].SnapshotID
		var (
			decisions []cache.LegacyAuditDecision
			decided   []cache.LegacyAuditItem
			reasons   []string
		)
		for _, it := range items {
			v, stack, err := auditLegacySnapshotRecovered(c, it)
			if err != nil {
				// An I/O fault (or a recovered panic) is never evidence:
				// leave the row queued, decide nothing for it, retry at
				// the next start.
				res.Deferred++
				attrs := []any{
					"snapshot_id", it.SnapshotID,
					"canonical_host", it.CanonicalHost,
					"suite_path", it.SuitePath,
					"err", err,
				}
				if stack != nil {
					attrs = append(attrs, "stack", string(stack))
				}
				logger.Error("legacy_trust_audit_deferred", attrs...)
				continue
			}
			decisions = append(decisions, cache.LegacyAuditDecision{SnapshotID: it.SnapshotID, Outcome: v.outcome,
				DropAnchorRows: v.dropAnchorRows})
			decided = append(decided, it)
			reasons = append(reasons, v.reason)
		}
		if len(decisions) > 0 {
			effects, err := c.ApplyLegacyAuditDecisions(ctx, decisions)
			if err != nil {
				return res, fmt.Errorf("legacy trust audit: %w", err)
			}
			for i, d := range decisions {
				res.Audited++
				switch d.Outcome {
				case cache.LegacyAuditKeep:
					// An unsigned snapshot is kept but proves nothing
					// signed (deferred minor A-47).
					if decided[i].Form != cache.FormUnsigned {
						res.Signed++
					}
				case cache.LegacyAuditQuarantine, cache.LegacyAuditQuarantineSigned:
					res.Quarantined++
					legacyQuarantinedTotal.Inc()
					// Logged after the batch commits: a crash can lose the
					// line, never the effect. Before the pool-file removal
					// below, so its ERROR follows the line it qualifies (the H1
					// final review's ops M-6).
					logger.Warn("legacy_snapshot_quarantined",
						"reason", reasons[i],
						"snapshot_id", d.SnapshotID,
						"canonical_host", decided[i].CanonicalHost,
						"suite_path", decided[i].SuitePath,
						"anchor_form", string(decided[i].Form),
						"was_current", effects[d.SnapshotID].WasCurrent,
						"anchor_rows_deleted", effects[d.SnapshotID].AnchorRowsDeleted,
					)
					if d.DropAnchorRows {
						// AIDEV-NOTE: ruling R-H2 as amended by review C-1 and
						// re-review N-1 (phase H1). The rotted pool file goes too,
						// not only its url_path rows: the miss path's Finalize
						// keeps an existing pool/<hash>, so refetching an upstream
						// InRelease that has not changed (the usual bit rot) would
						// store onto, and serve, the rotted file. It runs after the
						// batch commits, on main's goroutine before anything
						// serves, never on the writer (PD-5), as the integrity
						// scanner's DiscardFinalizedBlob does. Never before the
						// commit: a lost commit would leave the snapshots queued
						// with their anchors missing, judged anchor_missing at the
						// next start, or — when the pool holds none of the other
						// files the preflight checks either — a refused start ("is
						// the pool mounted?"; the final review's R2 I-1 narrowed
						// that refusal to a pool that is not there). For the same
						// reason it waits while another snapshot still queued
						// names the file (an http and an https suite of one
						// repository share it; the H1 final review's prod minor
						// 3): removed now, a lost commit of that snapshot's later
						// batch would leave it queued with its anchor missing.
						// Its own audit then finds the file corrupt, quarantines
						// it and removes the file; a queue read that fails keeps
						// the file and logs the failure. A crash between the
						// commit and this removal leaves the file, as a failed
						// removal does: the rows are gone, so a changed upstream
						// heals, and an unchanged one is healed by the eager
						// re-adoption's rehash (writeBlobBytes) once a client
						// asks with adoption enabled; the audit does not revisit
						// it.
						hash := *decided[i].SingleAnchorHash
						// A shutdown signal (ctx) must not keep a dequeued snapshot's rotted file.
						queued, err := c.LegacyAuditQueueNamesAnchor(context.WithoutCancel(ctx), hash)
						if err == nil && !queued {
							err = legacyDiscardAnchor(c, hash)
						}
						if err != nil {
							logger.Error("legacy_anchor_discard_failed",
								"snapshot_id", d.SnapshotID,
								"canonical_host", decided[i].CanonicalHost,
								"suite_path", decided[i].SuitePath,
								"blob_hash", hash,
								"err", err,
							)
						}
					}
				}
			}
		}
		logger.Info("legacy_trust_audit_progress",
			"audited", res.Audited, "quarantined", res.Quarantined, "deferred", res.Deferred)
		if err := legacyAuditAfterBatch(batch); err != nil {
			return res, fmt.Errorf("legacy trust audit: %w", err)
		}
	}
	n, err := markFromCachedSignedAnchors(ctx, c, logger)
	if err != nil {
		return res, err
	}
	res.AnchorMarkers = n
	logger.Info("legacy_trust_audit_completed",
		"audited", res.Audited, "signed", res.Signed, "quarantined", res.Quarantined,
		"deferred", res.Deferred, "anchor_markers", res.AnchorMarkers, "duration", time.Since(start))
	return res, nil
}

// legacyAuditRefusalTail ends every pre-flight refusal.
//
// AIDEV-NOTE: the final review's R5 #2 / R7 #2. main runs the audit after
// cache.Open, which has already committed the v7 migration, so a refusal
// changes nothing of the audit's own but the database is no longer one
// 1.0.x opens ("schema version 7 is newer than this binary supports"): an
// operator who rolls the package back needs the cache.db copy the release
// notes tell them to take before upgrading.
const legacyAuditRefusalTail = "The audit changed nothing, but the database is already migrated to schema 7, which 1.0.x refuses: to go back to 1.0.x, restore the cache.db copied before the upgrade."

// legacyAuditPoolSample is how many pinned pool files outside the queue the
// pre-flight checks once every queued inline anchor file is missing.
const legacyAuditPoolSample = 16

// preflightLegacyAudit refuses to audit against a pool that is not there.
// An unmounted, empty or foreign pool makes every anchor look missing,
// which would quarantine every snapshot (spec §6.1). The audit changes
// nothing, though the migration has (legacyAuditRefusalTail).
// It loads the whole queue (ids and anchor hashes only; fine for 10^5
// rows) because the decision needs every queued inline anchor.
//
// AIDEV-NOTE: the final review's R2 I-1 and R5 #1. The evidence is pool
// FILES, not snapshots: queued snapshots that share an InRelease (an http
// and an https suite of one repository, Debian's stable and its codename)
// share its content-addressed file, so the anchors are deduplicated by
// hash. And once every queued anchor file is missing, the files decide
// with a sample of the pinned files outside the queue
// (PinnedBlobsOutsideAuditQueue: blob GC never deletes them), which the
// queue cannot exhaust: after the first start the queue holds only the
// deferred residue, whose unreadable files an operator may remove. One of
// those files present means a mounted pool that lost the anchors: they are
// quarantined anchor_missing. Two or more files checked, none present,
// means a lost or foreign pool: refuse. A single file checked (one anchor
// file and nothing else pinned) cannot tell one lost file from a lost
// mount, and refusing would leave no recovery short of wiping the cache:
// it is quarantined.
func preflightLegacyAudit(ctx context.Context, c *cache.Cache) error {
	items, err := c.LegacyAuditBatch(ctx, 0, 0)
	if err != nil {
		return fmt.Errorf("legacy trust audit: %w", err)
	}
	queued := 0
	seen := make(map[string]bool)
	var anchors []string
	for _, it := range items {
		if it.Missing || it.Form != cache.FormInline || it.SingleAnchorHash == nil {
			continue
		}
		queued++
		if !seen[*it.SingleAnchorHash] {
			seen[*it.SingleAnchorHash] = true
			anchors = append(anchors, c.BlobPath(*it.SingleAnchorHash))
		}
	}
	if len(anchors) == 0 {
		// Spec §6.1: detached-only queues read no pool files.
		return nil
	}
	pool := filepath.Join(c.Dir(), "pool")
	f, err := os.Open(pool)
	if err != nil {
		return fmt.Errorf("legacy trust audit: pool %s is not readable: %w. Fix it and restart. %s", pool, err, legacyAuditRefusalTail)
	}
	_, rerr := f.Readdirnames(1)
	_ = f.Close()
	// AIDEV-NOTE: blob GC never deletes an adopted snapshot's anchor or
	// member blobs, so an empty pool with any queued inline anchor is a
	// lost or unmounted pool, never one lost file: refuse even for a
	// single anchor. A lone missing anchor in a NON-empty pool is still
	// quarantined by the audit (spec §6.1).
	if errors.Is(rerr, io.EOF) {
		return fmt.Errorf("legacy trust audit: pool %s is empty but the audit queue holds inline snapshots (%d) — is the pool mounted? Mount or restore it and restart; to start without the pool, wipe the cache directory. %s", pool, queued, legacyAuditRefusalTail)
	}
	if rerr != nil {
		return fmt.Errorf("legacy trust audit: pool %s is not readable: %w. Fix it and restart. %s", pool, rerr, legacyAuditRefusalTail)
	}
	if anyPoolFileExists(anchors) {
		return nil
	}
	pinned, err := c.PinnedBlobsOutsideAuditQueue(ctx, legacyAuditPoolSample)
	if err != nil {
		return fmt.Errorf("legacy trust audit: %w", err)
	}
	others := make([]string, len(pinned))
	for i, h := range pinned {
		others[i] = c.BlobPath(h)
	}
	if len(anchors)+len(others) >= 2 && !anyPoolFileExists(others) {
		return fmt.Errorf("legacy trust audit: none of the %d files checked exists under %s (%d queued inline anchors, %d other files current snapshots use) — is the pool mounted? Mount or restore it and restart; to start without the pool, wipe the cache directory. %s", len(anchors)+len(others), pool, len(anchors), len(others), legacyAuditRefusalTail)
	}
	return nil
}

// anyPoolFileExists reports whether any of paths is not missing.
//
// AIDEV-NOTE: only "does not exist" counts as missing. Any other stat error
// is an I/O fault, not evidence of a lost pool: it never refuses the start,
// and the audit defers a snapshot whose anchor it cannot read.
func anyPoolFileExists(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	return false
}

// auditLegacySnapshotRecovered runs auditLegacySnapshot and turns a panic
// into an error carrying the panic value, plus the stack, so the caller
// defers that one snapshot exactly as it defers an I/O fault.
//
// AIDEV-NOTE: the audit runs synchronously on main's goroutine before
// anything serves, and the queue and pool blob are identical at the next
// start, so an unrecovered panic here would recur on every start (a crash
// loop whose only escape is editing legacy_trust_audit by hand). A panic
// is not evidence: it never quarantines and never marks; the row stays
// queued and untouched, and the rest of the batch proceeds.
func auditLegacySnapshotRecovered(c *cache.Cache, it cache.LegacyAuditItem) (v legacyVerdict, stack []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, stack, err = legacyVerdict{}, debug.Stack(), fmt.Errorf("audit panicked: %v", r)
		}
	}()
	v, err = auditLegacySnapshot(c, it)
	return v, nil, err
}

func auditLegacySnapshot(c *cache.Cache, it cache.LegacyAuditItem) (legacyVerdict, error) {
	quarantine := func(reason string) (legacyVerdict, error) {
		return legacyVerdict{outcome: cache.LegacyAuditQuarantine, reason: reason}, nil
	}
	switch {
	case it.Missing:
		return legacyVerdict{outcome: cache.LegacyAuditDrop}, nil
	case it.Form == cache.FormDetached:
		// Detached adoption always verified Release.gpg (or, under
		// accept_any_signer, checked its structure): signed. R-1 cannot
		// apply — the whole Release file is the signed text.
		return legacyVerdict{outcome: cache.LegacyAuditKeep}, nil
	case it.Form == cache.FormUnsigned:
		// Never trust-bearing (spec C5): nothing to prove. Keep writes no
		// marker for it (markSnapshotBaseTx skips unsigned rows).
		return legacyVerdict{outcome: cache.LegacyAuditKeep}, nil
	case it.Form != cache.FormInline:
		return quarantine("unexpected_form")
	case it.SingleAnchorHash == nil:
		return quarantine("anchor_missing")
	}
	blob, err := readCapped(c.BlobPath(*it.SingleAnchorHash), legacyAuditAnchorCap)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return quarantine("anchor_missing")
	case errors.Is(err, errAnchorTooLarge):
		return quarantine("anchor_oversized")
	case err != nil:
		return legacyVerdict{}, err // an I/O fault is never evidence
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != *it.SingleAnchorHash {
		// AIDEV-NOTE: ruling R-H2 (the maintainer, 2026-10-24; review C-1).
		// The bytes were read and hashed, so this is evidence: the
		// quarantine also deletes the url_path rows that serve this pool
		// file, and RunLegacyTrustAudit removes the file itself, so the next
		// client request fetches a fresh copy instead of bytes apt rejects.
		// anchor_oversized (not read in full) and anchor_missing (a missing
		// pool file already falls through to the miss path) keep theirs.
		return legacyVerdict{outcome: cache.LegacyAuditQuarantine, reason: "anchor_corrupt", dropAnchorRows: true}, nil
	}
	return legacyClassify(blob), nil
}

// classifyLegacyInline decides whether a stored InRelease proves a signed,
// unpolluted inline snapshot (spec §6.1). Pure; table-tested.
func classifyLegacyInline(blob []byte) legacyVerdict {
	if !bytes.Contains(blob, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		return legacyVerdict{outcome: cache.LegacyAuditQuarantine, reason: "legacy_plaintext"}
	}
	text, err := inReleasePlaintext(blob)
	if err != nil {
		return legacyVerdict{outcome: cache.LegacyAuditQuarantine, reason: "malformed_envelope"}
	}
	signedOnly := func(reason string) legacyVerdict {
		return legacyVerdict{outcome: cache.LegacyAuditQuarantineSigned, reason: reason}
	}
	signed, err := ParseRelease(text)
	if err != nil {
		return signedOnly("unparseable")
	}
	// What the pre-fix reconcile saw: the whole envelope, canonicalised the
	// way the decoder canonicalises the plaintext (CRLF → LF, trailing
	// whitespace trimmed) so only declarations, never line endings, can
	// differ. Anything it declares that the signed text does not — or a
	// parse error the signed text does not have — means declarations were
	// placed outside the signature (R-1: after a forged "=CRC" line, or as
	// colon-bearing armor headers).
	raw, err := ParseRelease(clearsignCanonical(blob))
	if err != nil {
		return signedOnly("unsigned_declarations")
	}
	type key struct {
		path, sha256 string
		size         int64
	}
	have := make(map[key]struct{}, len(signed))
	for _, m := range signed {
		have[key{m.Path, m.SHA256, m.Size}] = struct{}{}
	}
	for _, m := range raw {
		if _, ok := have[key{m.Path, m.SHA256, m.Size}]; !ok {
			return signedOnly("unsigned_declarations")
		}
	}
	return legacyVerdict{outcome: cache.LegacyAuditKeep}
}

// clearsignCanonical returns b with every line canonicalised exactly as
// go-crypto's clearsign.Decode canonicalises Block.Plaintext: CRLF and LF
// both end a line, trailing spaces and tabs are removed, lines are rejoined
// with LF (a whitespace-only line becomes blank). Dash-escapes ("- "
// prefixes) are not undone: no Release line begins with "-", and the
// armor's "-----" lines are not dash-escaped.
func clearsignCanonical(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, l := range lines {
		lines[i] = bytes.TrimRight(l, " \t\r")
	}
	return bytes.Join(lines, []byte("\n"))
}

// markFromCachedSignedAnchors is the C3 startup marker pass: every
// tracked suite whose cached InRelease or Release.gpg passes the sniff
// gets a marker stamped with that row's last use. Idempotent (markers
// keep the newest stamp), so it runs at every start.
func markFromCachedSignedAnchors(ctx context.Context, c *cache.Cache, logger *slog.Logger) (int, error) {
	anchors, err := c.ListTrackedSuiteAnchors(ctx)
	if err != nil {
		return 0, fmt.Errorf("legacy trust audit: list anchors: %w", err)
	}
	var marks []cache.SignedSeenMark
	for _, a := range anchors {
		ok, err := sniffCachedAnchor(c, a.Path, a.BlobHash)
		if err != nil {
			// Pool file gone or unreadable: no evidence either way
			// (deferred minor A-49: said at DEBUG, not silently).
			logger.Debug("legacy_trust_audit_cached_anchor_skipped",
				"canonical_host", a.CanonicalHost, "path", a.Path, "err", err)
			continue
		}
		if ok {
			marks = append(marks, cache.SignedSeenMark{CanonicalScheme: a.CanonicalScheme,
				CanonicalHost: a.CanonicalHost, Base: aptpath.MarkerBase(a.SuitePath), SeenAt: a.SeenAt})
		}
	}
	if err := c.MarkSignedSeenBatch(ctx, marks); err != nil {
		return 0, fmt.Errorf("legacy trust audit: mark anchors: %w", err)
	}
	return len(marks), nil
}

// sniffCachedAnchor reports whether the pool file of a cached signed
// anchor (path is <suite>/InRelease or <suite>/Release.gpg) passes its
// sniff (spec C3: the rule the handler marks by). The startup marker pass
// and the adopter's re-mark before an unsigned commit (ruling R-E1) share
// it, so they agree on what counts as signed. A pool file that does not
// exist is an error wrapping fs.ErrNotExist; any other path is never
// signed.
func sniffCachedAnchor(c *cache.Cache, path, blobHash string) (bool, error) {
	var file aptpath.SuiteFile
	switch {
	case strings.HasSuffix(path, "/InRelease"):
		file = aptpath.FileInRelease
	case strings.HasSuffix(path, "/Release.gpg"):
		file = aptpath.FileReleaseGPG
	default:
		return false, nil
	}
	head, err := readCapped(c.BlobPath(blobHash), int64(file.HeadBytes()))
	if err != nil && !errors.Is(err, errAnchorTooLarge) {
		return false, err
	}
	return file.Sniff(path, head), nil
}

// readCapped reads at most limit bytes of path. A longer file returns its
// first limit bytes AND an error wrapping errAnchorTooLarge, so the sniffs
// can use the head while the full-body reader rejects.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return b[:limit], fmt.Errorf("%s: %w (%d bytes)", path, errAnchorTooLarge, limit)
	}
	return b, nil
}
