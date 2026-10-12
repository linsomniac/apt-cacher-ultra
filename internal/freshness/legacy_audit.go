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
)

var errAnchorTooLarge = errors.New("anchor exceeds the audit read cap")

// LegacyAuditResult summarises one RunLegacyTrustAudit call.
type LegacyAuditResult struct {
	Audited, Signed, Quarantined, Deferred, AnchorMarkers int
}

type legacyVerdict struct {
	outcome cache.LegacyAuditOutcome
	reason  string // set for quarantines
}

// RunLegacyTrustAudit drains legacy_trust_audit, then records signed-seen
// markers from tracked suites' cached signed anchors (spec §6.1, C3). main
// runs it after cache.Open and before GC, the checker or any listener
// serves. An error means the database failed or the pool is missing: the
// caller refuses to start, nothing in the failed batch was applied, and the
// audit resumes on the next start. A single unreadable anchor is not an
// error: that snapshot is deferred (left queued, unchanged, ERROR logged).
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
			v, err := auditLegacySnapshot(c, it)
			if err != nil {
				// An I/O fault is never evidence: leave the row queued,
				// decide nothing for it, retry at the next start.
				res.Deferred++
				logger.Error("legacy_trust_audit_deferred",
					"snapshot_id", it.SnapshotID,
					"canonical_host", it.CanonicalHost,
					"suite_path", it.SuitePath,
					"err", err,
				)
				continue
			}
			decisions = append(decisions, cache.LegacyAuditDecision{SnapshotID: it.SnapshotID, Outcome: v.outcome})
			decided = append(decided, it)
			reasons = append(reasons, v.reason)
		}
		if len(decisions) > 0 {
			wasCurrent, err := c.ApplyLegacyAuditDecisions(ctx, decisions)
			if err != nil {
				return res, fmt.Errorf("legacy trust audit: %w", err)
			}
			for i, d := range decisions {
				res.Audited++
				switch d.Outcome {
				case cache.LegacyAuditKeep:
					res.Signed++
				case cache.LegacyAuditQuarantine, cache.LegacyAuditQuarantineSigned:
					res.Quarantined++
					legacyQuarantinedTotal.Inc()
					// Logged after the batch commits: a crash can lose the
					// line, never the effect.
					logger.Warn("legacy_snapshot_quarantined",
						"reason", reasons[i],
						"snapshot_id", d.SnapshotID,
						"canonical_host", decided[i].CanonicalHost,
						"suite_path", decided[i].SuitePath,
						"anchor_form", string(decided[i].Form),
						"was_current", wasCurrent[d.SnapshotID],
					)
				}
			}
		}
		logger.Info("legacy_trust_audit_progress",
			"audited", res.Audited, "quarantined", res.Quarantined, "deferred", res.Deferred)
		if err := legacyAuditAfterBatch(batch); err != nil {
			return res, err
		}
	}
	n, err := markFromCachedSignedAnchors(ctx, c)
	if err != nil {
		return res, err
	}
	res.AnchorMarkers = n
	logger.Info("legacy_trust_audit_completed",
		"audited", res.Audited, "signed", res.Signed, "quarantined", res.Quarantined,
		"deferred", res.Deferred, "anchor_markers", res.AnchorMarkers, "duration", time.Since(start))
	return res, nil
}

// preflightLegacyAudit refuses to audit against a pool that is not there.
// An unmounted, empty or foreign pool makes every anchor look missing,
// which would quarantine every snapshot (spec §6.1). Nothing is changed.
// It loads the whole queue (ids and anchor hashes only; fine for 10^5
// rows) because the decision needs every queued inline anchor.
func preflightLegacyAudit(ctx context.Context, c *cache.Cache) error {
	items, err := c.LegacyAuditBatch(ctx, 0, 0)
	if err != nil {
		return fmt.Errorf("legacy trust audit: %w", err)
	}
	var anchors []string
	for _, it := range items {
		if it.Missing || it.Form != cache.FormInline || it.SingleAnchorHash == nil {
			continue
		}
		anchors = append(anchors, c.BlobPath(*it.SingleAnchorHash))
	}
	if len(anchors) == 0 {
		// Spec §6.1: detached-only queues read no pool files.
		return nil
	}
	pool := filepath.Join(c.Dir(), "pool")
	f, err := os.Open(pool)
	if err != nil {
		return fmt.Errorf("legacy trust audit: pool %s is not readable: %w (fix it and restart; nothing was changed)", pool, err)
	}
	_, rerr := f.Readdirnames(1)
	_ = f.Close()
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return fmt.Errorf("legacy trust audit: pool %s is not readable: %w (fix it and restart; nothing was changed)", pool, rerr)
	}
	inline, missing := len(anchors), 0
	for _, p := range anchors {
		// AIDEV-NOTE: only "does not exist" counts as missing. Any other
		// stat error is an I/O fault, not evidence of a lost pool: it never
		// refuses the start here, and the audit defers that one snapshot.
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			missing++
		}
	}
	if inline >= 2 && missing == inline {
		return fmt.Errorf("legacy trust audit: none of the %d queued inline anchors exist under %s — is the pool mounted? Mount or restore it and restart; to start without the pool, wipe the cache directory. Nothing was changed", inline, pool)
	}
	return nil
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
		return quarantine("anchor_corrupt")
	}
	return classifyLegacyInline(blob), nil
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
func markFromCachedSignedAnchors(ctx context.Context, c *cache.Cache) (int, error) {
	anchors, err := c.ListTrackedSuiteAnchors(ctx)
	if err != nil {
		return 0, fmt.Errorf("legacy trust audit: list anchors: %w", err)
	}
	var marks []cache.SignedSeenMark
	for _, a := range anchors {
		head, err := readCapped(c.BlobPath(a.BlobHash), aptpath.SniffHeadBytes)
		if err != nil && !errors.Is(err, errAnchorTooLarge) {
			continue // pool file gone or unreadable: no evidence either way
		}
		ok := false
		switch {
		case strings.HasSuffix(a.Path, "/InRelease"):
			ok = aptpath.SniffInRelease(head)
		case strings.HasSuffix(a.Path, "/Release.gpg"):
			ok = aptpath.SniffReleaseGPG(head)
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
