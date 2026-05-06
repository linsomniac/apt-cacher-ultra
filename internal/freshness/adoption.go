package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// SuiteRef identifies a suite for adoption — the canonical scheme/host
// (post-Remap) plus the suite path. The freshness checker passes this
// in already-canonicalized; the Adopter does no further normalization.
type SuiteRef struct {
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string // e.g. "/ubuntu/dists/noble"
}

// Verifier returns the verified Release-style plaintext for an inline
// InRelease (clearsigned) blob. Phase 2 step 3 implements this with
// `github.com/ProtonMail/go-crypto/openpgp`; step 2's tests inject a
// stub that returns the input verbatim.
//
// AIDEV-NOTE: VerifyInline ONLY validates a clearsigned InRelease.
// Detached (Release + Release.gpg) verification is added when needed.
// Phase 2 step 2 only exercises the inline path because that is what
// the freshness checker fetches (§7).
type Verifier interface {
	VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error)
}

// AdoptionFetcher is the subset of *fetch.Client the Adopter uses to
// pull declared members from upstream. The Fetch method writes the
// member bytes into the cache.BlobWriter that the caller provides;
// the BlobWriter's hasher is the only ground truth — verification
// happens at Finalize when adoption checks the hash against the
// declared sha256 from the Release file.
type AdoptionFetcher interface {
	Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error)
}

// Adoption category sentinels — Run() wraps the underlying error in
// one of these so callers (and structured logs per SPEC2 §10.2) can
// pivot on the failure category without string-matching error text.
var (
	ErrAdoptionGPGFailed         = errors.New("adoption_gpg_failed")
	ErrAdoptionParseFailed       = errors.New("adoption_parse_failed")
	ErrAdoptionMemberFetchFailed = errors.New("adoption_member_fetch_failed")
	ErrAdoptionMemberMismatch    = errors.New("adoption_member_mismatch")
	ErrAdoptionDBFailed          = errors.New("adoption_db_failed")
)

// AdoptionConfig bundles Adopter dependencies. Required: Cache, Fetcher,
// Verifier, HostLimiter. Optional: MaxConcurrent (0 = unlimited),
// MemberFetchTimeout (per-member upstream budget — adopters can run
// long; we want each member call bounded), Logger.
type AdoptionConfig struct {
	Cache       *cache.Cache
	Fetcher     AdoptionFetcher
	Verifier    Verifier
	HostLimiter *hostsem.Sem

	// MaxConcurrent caps how many adoption goroutines may run at once
	// across the whole cache (SPEC2 §9.3.1). Zero means unlimited.
	MaxConcurrent int

	Logger *slog.Logger

	// now is a test seam; production uses time.Now.
	now func() time.Time
}

// Adopter executes the SPEC2 §7.5 adoption flow when invoked by the
// freshness checker on a changed InRelease. One Adopter instance is
// shared across all suites; per-suite serialization is the freshness
// Checker's job (the same in-memory mutex map that gates §7.3).
type Adopter struct {
	cache    *cache.Cache
	fetcher  AdoptionFetcher
	verifier Verifier
	hostSem  *hostsem.Sem

	// concurrencySem bounds the total in-flight adoptions across all
	// suites. nil channel means "no cap" (MaxConcurrent = 0). Acquired
	// once at the top of Run, released after success or error.
	concurrencySem chan struct{}

	logger *slog.Logger
	now    func() time.Time
}

// NewAdopter validates dependencies and constructs an Adopter. Returns
// an error if any required dependency is missing.
func NewAdopter(cfg AdoptionConfig) (*Adopter, error) {
	if cfg.Cache == nil {
		return nil, errors.New("freshness: nil Cache")
	}
	if cfg.Fetcher == nil {
		return nil, errors.New("freshness: nil Fetcher")
	}
	if cfg.Verifier == nil {
		return nil, errors.New("freshness: nil Verifier")
	}
	if cfg.HostLimiter == nil {
		return nil, errors.New("freshness: nil HostLimiter")
	}
	if cfg.MaxConcurrent < 0 {
		return nil, fmt.Errorf("freshness: max_concurrent_adoptions must be >= 0, got %d", cfg.MaxConcurrent)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	var sem chan struct{}
	if cfg.MaxConcurrent > 0 {
		sem = make(chan struct{}, cfg.MaxConcurrent)
	}
	return &Adopter{
		cache:          cfg.Cache,
		fetcher:        cfg.Fetcher,
		verifier:       cfg.Verifier,
		hostSem:        cfg.HostLimiter,
		concurrencySem: sem,
		logger:         logger,
		now:            now,
	}, nil
}

// Run executes the §7.5 adoption flow for a single suite. inRelease is
// the freshness-fetched body; etag/lastmod are validators from the
// same response. Returns nil on a successful flip, or one of the
// Err* sentinels (wrapped with context) for each failure category.
//
// Run is synchronous: callers (typically the freshness Checker)
// invoke it from a goroutine. The per-suite mutex held by the caller
// serializes overlapping adoptions on the same suite; the global
// concurrency cap held inside Run bounds total parallel adoptions.
func (a *Adopter) Run(ctx context.Context, suite SuiteRef, inRelease []byte, etag, lastmod string) error {
	// Step 0: global concurrency cap. nil channel skips the gate.
	if a.concurrencySem != nil {
		select {
		case a.concurrencySem <- struct{}{}:
			defer func() { <-a.concurrencySem }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Step 1: GPG verify. The Verifier returns the verified Release-
	// style plaintext; for step 2 the production wiring is not yet in
	// place (a real Verifier comes in step 3) — tests inject a
	// pass-through.
	releaseText, err := a.verifier.VerifyInline(ctx, suite, inRelease)
	if err != nil {
		a.logger.Info("adoption_gpg_failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"err", err,
		)
		return fmt.Errorf("%w: %v", ErrAdoptionGPGFailed, err)
	}

	// Step 2: persist the verified InRelease blob into pool/ BEFORE
	// the candidate row references it. writeBlobBytes is idempotent
	// and rehashes-on-reuse — a pre-existing pool blob with the same
	// hash is re-verified before adoption claims it.
	inReleaseHash, err := a.writeBlobBytes(ctx, inRelease)
	if err != nil {
		a.logger.Warn("adoption: persist InRelease failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"err", err,
		)
		return fmt.Errorf("%w: persist InRelease: %v", ErrAdoptionDBFailed, err)
	}

	// Step 3: parse the verified Release-style plaintext.
	members, err := ParseRelease(releaseText)
	if err != nil {
		a.logger.Info("adoption_parse_failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"err", err,
		)
		return fmt.Errorf("%w: %v", ErrAdoptionParseFailed, err)
	}

	// Step 4: insert the candidate suite_snapshot row. FK constraints
	// resolve because step 2 stored inReleaseHash.
	cand := cache.SnapshotCandidate{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		SuitePath:       suite.SuitePath,
		InReleaseHash:   &inReleaseHash,
	}
	if etag != "" {
		v := etag
		cand.InReleaseETag = &v
	}
	if lastmod != "" {
		v := lastmod
		cand.InReleaseLastMod = &v
	}
	snapshotID, err := a.cache.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		a.logger.Warn("adoption: insert candidate failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"err", err,
		)
		return fmt.Errorf("%w: insert candidate: %v", ErrAdoptionDBFailed, err)
	}

	// Step 5: prefetch declared members sequentially. Each member's
	// declared_sha256 is the trust anchor — bytes that arrive from
	// upstream are accepted only if their fresh hash matches.
	memberRows := make([]cache.SnapshotMember, 0, len(members)+3)
	for _, m := range members {
		blobHash, err := a.adoptMember(ctx, suite, m)
		if err != nil {
			return err // already wrapped with category
		}
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           m.Path,
			BlobHash:       blobHash,
			DeclaredSHA256: m.SHA256,
		})
	}

	// Step 6: metadata-self snapshot_member row for InRelease itself.
	// Without this the §6.1 snapshot-scoped lookup would 404 on the
	// very URL apt fetches first. Detached mode (when added) inserts
	// metadata-self rows for both Release and Release.gpg here.
	memberRows = append(memberRows, cache.SnapshotMember{
		SnapshotID:     snapshotID,
		Path:           "InRelease",
		BlobHash:       inReleaseHash,
		DeclaredSHA256: inReleaseHash,
	})

	// Step 7: by-hash alias rows. apt's Acquire-By-Hash clients fetch
	// from <suite>/<component>/by-hash/SHA256/<declared_sha256>; a
	// snapshot_member row at that alias path lets §6.1 resolve those
	// requests through the same blob. We dedupe — multiple members
	// with the same content (e.g. "Sources" and "Sources.bz2") would
	// produce the same alias path and trigger a unique violation.
	aliasSeen := make(map[string]bool, len(members))
	for _, m := range members {
		alias := byHashAliasPath(m.Path, m.SHA256)
		if alias == "" || aliasSeen[alias] {
			continue
		}
		aliasSeen[alias] = true
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           alias,
			BlobHash:       m.SHA256,
			DeclaredSHA256: m.SHA256,
		})
	}

	// AIDEV-TODO: Step 8 — parse Packages members and populate
	// package_hash rows. Deferred to Phase 2 step 2d-ii. Without it,
	// the §6.5 .deb hash validation degrades to "no current snapshot"
	// = "trust upstream" (Phase 1 behavior) until a follow-up commit
	// wires the Packages parser into adoption.
	var packageHashes []cache.PackageHash

	// Step 9: atomic flip transaction.
	if err := a.cache.CommitAdoption(ctx, snapshotID, memberRows, packageHashes); err != nil {
		a.logger.Warn("adoption: commit failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return fmt.Errorf("%w: commit: %v", ErrAdoptionDBFailed, err)
	}

	// Step 10: success log.
	a.logger.Info("adoption_success",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"member_count", len(members),
		"alias_count", len(aliasSeen),
		"package_hash_count", len(packageHashes),
	)
	return nil
}

// adoptMember handles step 5 for one declared member: try pool reuse
// (with rehash defense), else fetch from upstream. Returns the blob
// hash on success — guaranteed to equal m.SHA256, since adoption
// rejects any byte stream whose hash differs from the declaration.
func (a *Adopter) adoptMember(ctx context.Context, suite SuiteRef, m ReleaseMember) (string, error) {
	exists, err := a.cache.BlobExists(m.SHA256)
	if err != nil {
		return "", fmt.Errorf("%w: BlobExists %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
	}
	if exists {
		// "Rehash on reuse" — SPEC2 §7.5 step 5. Pool blobs predating
		// Phase 2 were inserted under the trust-upstream model; their
		// on-disk content was not verified against a declared hash at
		// the time. Re-hashing bounds the verified set to bytes we
		// have *just* confirmed.
		actual, err := hashFile(a.cache.BlobPath(m.SHA256))
		if err != nil {
			return "", fmt.Errorf("%w: rehash %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
		}
		if actual == m.SHA256 {
			// Confirmed reuse. Make sure the row exists (may not, e.g.
			// a stray pool file from a partial migration); PutBlob is
			// idempotent.
			if err := a.cache.PutBlob(ctx, m.SHA256, m.Size); err != nil {
				return "", fmt.Errorf("%w: PutBlob reuse %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
			}
			return m.SHA256, nil
		}
		// Pool blob is corrupted at rest. SPEC2 §7.5 step 5: log and
		// evict; fall through to upstream fetch.
		a.logger.Warn("pool_corruption_during_adoption",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"declared_sha256", m.SHA256,
			"actual_sha256", actual,
		)
		if err := os.Remove(a.cache.BlobPath(m.SHA256)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: evict corrupted %s: %v", ErrAdoptionDBFailed, m.SHA256, err)
		}
		// Don't try to delete the blob row — Phase 4 GC handles
		// refcount=0 cleanup. The next refetch will Put-Blob through.
	}

	// Acquire host slot for the member fetch (SPEC §9.3 / §9.3.1).
	// Adoption uses the same hostsem as the request path, so a
	// member fetch contends with cache-miss fetches for the same
	// per-host budget.
	release, err := a.hostSem.Acquire(ctx, suite.CanonicalHost)
	if err != nil {
		return "", fmt.Errorf("%w: hostsem acquire %s: %v",
			ErrAdoptionMemberFetchFailed, suite.CanonicalHost, err)
	}
	defer release()

	// Build the upstream URL: suite_path + "/" + relative path.
	upstreamURL := buildMemberURL(suite, m.Path)
	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           upstreamURL,
	}

	w, err := a.cache.NewTempBlob()
	if err != nil {
		return "", fmt.Errorf("%w: NewTempBlob %s: %v", ErrAdoptionDBFailed, m.Path, err)
	}
	defer func() { _ = w.Abort() }() // no-op once Finalize wins

	res, err := a.fetcher.Fetch(ctx, target, w)
	if err != nil {
		return "", fmt.Errorf("%w: fetch %s: %v",
			ErrAdoptionMemberFetchFailed, m.Path, err)
	}
	// Size sanity: declared Size in the Release file should match
	// what the fetch actually wrote. fetch.Client already honors
	// Content-Length internally, so the case where they diverge is
	// genuinely abnormal — surface it as a member-fetch error rather
	// than a hash mismatch (the bytes might still hash correctly,
	// but a length mismatch is its own integrity violation).
	if m.Size > 0 && res.ContentLength > 0 && res.ContentLength != m.Size {
		return "", fmt.Errorf("%w: %s: content-length %d vs declared %d",
			ErrAdoptionMemberFetchFailed, m.Path, res.ContentLength, m.Size)
	}

	hashHex, err := w.Finalize(res.ContentLength)
	if err != nil {
		return "", fmt.Errorf("%w: finalize %s: %v",
			ErrAdoptionMemberFetchFailed, m.Path, err)
	}
	if hashHex != m.SHA256 {
		// Mismatch. The blob is now in pool/ under hashHex (its
		// actual hash, not the declared). Adoption rejects.
		// Don't promote a divergent blob — but also don't evict it,
		// since a different snapshot might legitimately reference
		// hashHex elsewhere. Log loudly.
		a.logger.Warn("adoption_member_mismatch",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"declared_sha256", m.SHA256,
			"actual_sha256", hashHex,
		)
		return "", fmt.Errorf("%w: %s: declared %s, got %s",
			ErrAdoptionMemberMismatch, m.Path, m.SHA256, hashHex)
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		return "", fmt.Errorf("%w: PutBlob %s: %v", ErrAdoptionDBFailed, hashHex, err)
	}
	return hashHex, nil
}

// writeBlobBytes persists in-memory bytes as a blob, returning the
// sha256 hex hash. Implements SPEC2 §7.5 step 2's "rehash on reuse"
// semantics: if pool/<hash> already exists, its on-disk content is
// re-hashed against the in-memory expectation and either confirmed
// (PutBlob is idempotent) or evicted and rewritten.
func (a *Adopter) writeBlobBytes(ctx context.Context, content []byte) (string, error) {
	sum := sha256.Sum256(content)
	hashHex := hex.EncodeToString(sum[:])

	exists, err := a.cache.BlobExists(hashHex)
	if err != nil {
		return "", fmt.Errorf("BlobExists %s: %w", hashHex, err)
	}
	if exists {
		actual, err := hashFile(a.cache.BlobPath(hashHex))
		if err != nil {
			return "", fmt.Errorf("rehash %s: %w", hashHex, err)
		}
		if actual == hashHex {
			if err := a.cache.PutBlob(ctx, hashHex, int64(len(content))); err != nil {
				return "", fmt.Errorf("PutBlob reuse %s: %w", hashHex, err)
			}
			return hashHex, nil
		}
		a.logger.Warn("pool_corruption_during_adoption",
			"hash", hashHex,
			"actual_sha256", actual,
			"context", "writeBlobBytes",
		)
		if err := os.Remove(a.cache.BlobPath(hashHex)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("evict corrupted %s: %w", hashHex, err)
		}
	}

	w, err := a.cache.NewTempBlob()
	if err != nil {
		return "", fmt.Errorf("NewTempBlob: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Abort()
		return "", fmt.Errorf("write blob: %w", err)
	}
	final, err := w.Finalize(int64(len(content)))
	if err != nil {
		return "", fmt.Errorf("finalize blob: %w", err)
	}
	if final != hashHex {
		// Sanity: sha256 of the same bytes must match. If it doesn't,
		// either crypto/sha256 is broken or BlobWriter's hasher is.
		return "", fmt.Errorf("hash mismatch sanity: in-memory %s vs Finalize %s",
			hashHex, final)
	}
	if err := a.cache.PutBlob(ctx, hashHex, int64(len(content))); err != nil {
		return "", fmt.Errorf("PutBlob %s: %w", hashHex, err)
	}
	return hashHex, nil
}

// hashFile streams a file through sha256 and returns the lowercase hex
// digest. Used by writeBlobBytes and adoptMember for the "rehash on
// reuse" defense — content sitting in pool/ from a prior fetch is
// re-verified before being trusted as a snapshot member.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// byHashAliasPath returns the by-hash alias path for a member, or ""
// if the member's declared path lacks a directory component the
// alias would be relative to. The alias is constructed by stripping
// the filename component from the declared path and appending
// "by-hash/SHA256/<sha256>".
//
// For "main/binary-amd64/Packages.gz" + sha "abc...": returns
// "main/binary-amd64/by-hash/SHA256/abc...".
//
// For a member at the suite root (no slash in path), the alias would
// degenerate to "by-hash/SHA256/<hash>", which is technically valid
// but apt clients don't fetch suite-root files via by-hash. Return
// "" to skip those entries — they wouldn't be requested through the
// alias anyway.
func byHashAliasPath(declaredPath, sha256hex string) string {
	dir := path.Dir(declaredPath)
	if dir == "." || dir == "" {
		return ""
	}
	return dir + "/by-hash/SHA256/" + sha256hex
}

// buildMemberURL constructs the upstream URL for a suite-relative
// member path. The freshness checker fetches InRelease at
// "<scheme>://<host><suite_path>/InRelease"; member URLs follow the
// same pattern with the relative path appended.
//
// AIDEV-NOTE: this composes URL strings textually rather than going
// through net/url, because the inputs are already canonicalized: the
// suite_path is opaque from canonicalization, and the relative
// member path was validated by ParseRelease (no NUL, no absolute
// prefix, no dotdot). A conservative approach is to ensure exactly
// one "/" between suite and member.
func buildMemberURL(suite SuiteRef, relPath string) string {
	base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suite.SuitePath
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + relPath
}
