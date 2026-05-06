package freshness

import (
	"compress/gzip"
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

// maxDecompressedPackagesBytes caps how much we'll inflate a single
// Packages.gz blob. Real-world Packages files for Ubuntu noble main
// amd64 are ~50 MiB uncompressed; 256 MiB is comfortable headroom and
// bounds the memory cost of a gzip-bomb signed by an otherwise-valid
// adoption (the bytes were signed and hash-verified, but a hostile
// upstream could still ship pathological content).
const maxDecompressedPackagesBytes = 256 << 20

// SuiteRef identifies a suite for adoption — the canonical scheme/host
// (post-Remap) plus the suite path. The freshness checker passes this
// in already-canonicalized; the Adopter does no further normalization.
type SuiteRef struct {
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string // e.g. "/ubuntu/dists/noble"
}

// Verifier returns the verified Release-style plaintext for one of
// the two SPEC2 §7.6.3 signature forms:
//
//   - VerifyInline accepts a clearsigned InRelease blob (the body
//     between BEGIN PGP SIGNED MESSAGE / END PGP SIGNATURE markers).
//     Returns the cleartext between the markers.
//   - VerifyDetached accepts a Release file plus its detached
//     Release.gpg signature. Returns releaseBytes verbatim — the
//     Release file IS the verified plaintext, so there is nothing to
//     "extract."
//
// The production implementation lives in internal/gpg and uses
// github.com/ProtonMail/go-crypto/openpgp; tests inject a
// pass-through stub.
type Verifier interface {
	VerifyInline(ctx context.Context, suite SuiteRef, inRelease []byte) ([]byte, error)
	VerifyDetached(ctx context.Context, suite SuiteRef, releaseBytes, sigBytes []byte) ([]byte, error)
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

// adoptionForm distinguishes the two SPEC2 §7.6.3 signature forms.
type adoptionForm int

const (
	adoptionFormInline   adoptionForm = iota // clearsigned InRelease
	adoptionFormDetached                     // Release + Release.gpg
)

// adoptionPayload carries the form-specific inputs to runShared.
// Exactly one of (inlineBytes) or (releaseBytes + sigBytes) is set on
// entry; runShared populates the *Hash fields once step 2 has stored
// the verified blobs in pool/.
type adoptionPayload struct {
	form adoptionForm

	// Inline mode (form == adoptionFormInline).
	inlineBytes []byte // clearsigned InRelease
	inlineHash  string // sha256 of inlineBytes (set in step 2)

	// Detached mode (form == adoptionFormDetached).
	releaseBytes    []byte
	sigBytes        []byte
	releaseHash     string // sha256 of releaseBytes (set in step 2)
	releaseGPGHash  string // sha256 of sigBytes (set in step 2)
}

// Run executes the §7.5 adoption flow for an inline (clearsigned
// InRelease) suite. inRelease is the freshness-fetched body;
// etag/lastmod are validators from the same response. Returns nil on
// a successful flip, or one of the Err* sentinels (wrapped with
// context) for each failure category.
//
// Run is synchronous: callers (typically the freshness Checker)
// invoke it from a goroutine. The per-suite mutex held by the caller
// serializes overlapping adoptions on the same suite; the global
// concurrency cap held inside Run bounds total parallel adoptions.
func (a *Adopter) Run(ctx context.Context, suite SuiteRef, inRelease []byte, etag, lastmod string) error {
	return a.runShared(ctx, suite, &adoptionPayload{
		form:        adoptionFormInline,
		inlineBytes: inRelease,
	}, etag, lastmod)
}

// RunDetached executes the §7.5 adoption flow for a detached-form
// suite. releaseBytes is the Release file body, sigBytes is the
// Release.gpg body (armored or binary). etag/lastmod are validators
// from the Release fetch response; subsequent freshness checks
// conditional-GET Release with these.
//
// SPEC2 §7.6.3 calls for both forms; this is the "Release +
// Release.gpg" branch invoked when an upstream returns 404 on
// InRelease (or never serves an inline InRelease).
func (a *Adopter) RunDetached(ctx context.Context, suite SuiteRef, releaseBytes, sigBytes []byte, etag, lastmod string) error {
	return a.runShared(ctx, suite, &adoptionPayload{
		form:         adoptionFormDetached,
		releaseBytes: releaseBytes,
		sigBytes:     sigBytes,
	}, etag, lastmod)
}

// runShared is the SPEC2 §7.5 adoption pipeline shared across both
// signature forms. Form-specific dispatch happens at three points:
//   - Step 1: VerifyInline vs VerifyDetached.
//   - Step 2: persist 1 blob (InRelease) vs 2 blobs (Release, Release.gpg).
//   - Step 6: metadata-self rows — 1 (InRelease) vs 2 (Release, Release.gpg).
//
// Steps 3–5 and 7–10 are identical: parse, candidate insert, member
// prefetch, by-hash, package_hash, atomic flip, success log.
func (a *Adopter) runShared(ctx context.Context, suite SuiteRef, p *adoptionPayload, etag, lastmod string) error {
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
	// style plaintext (the cleartext between BEGIN/END markers in a
	// clearsigned InRelease, or releaseBytes verbatim in detached
	// mode). Both paths converge on "verified Release-equivalent text"
	// for the §7.5 step 3 parse.
	var (
		releaseText []byte
		verifyErr   error
	)
	switch p.form {
	case adoptionFormInline:
		releaseText, verifyErr = a.verifier.VerifyInline(ctx, suite, p.inlineBytes)
	case adoptionFormDetached:
		releaseText, verifyErr = a.verifier.VerifyDetached(ctx, suite, p.releaseBytes, p.sigBytes)
	}
	if verifyErr != nil {
		a.logger.Info("adoption_gpg_failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"err", verifyErr,
		)
		return fmt.Errorf("%w: %v", ErrAdoptionGPGFailed, verifyErr)
	}

	// Step 2: persist the verified metadata blob(s) into pool/ BEFORE
	// the candidate row references them. writeBlobBytes is idempotent
	// and rehashes-on-reuse — a pre-existing pool blob with the same
	// hash is re-verified before adoption claims it. Detached mode
	// stores both the Release body and the Release.gpg signature so
	// snapshot_member rows can FK-reference them later.
	switch p.form {
	case adoptionFormInline:
		h, err := a.writeBlobBytes(ctx, p.inlineBytes)
		if err != nil {
			a.logger.Warn("adoption: persist InRelease failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
			return fmt.Errorf("%w: persist InRelease: %v", ErrAdoptionDBFailed, err)
		}
		p.inlineHash = h
	case adoptionFormDetached:
		rh, err := a.writeBlobBytes(ctx, p.releaseBytes)
		if err != nil {
			a.logger.Warn("adoption: persist Release failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
			return fmt.Errorf("%w: persist Release: %v", ErrAdoptionDBFailed, err)
		}
		gh, err := a.writeBlobBytes(ctx, p.sigBytes)
		if err != nil {
			a.logger.Warn("adoption: persist Release.gpg failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
			return fmt.Errorf("%w: persist Release.gpg: %v", ErrAdoptionDBFailed, err)
		}
		p.releaseHash = rh
		p.releaseGPGHash = gh
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

	// Step 4: insert the candidate suite_snapshot row. The schema
	// CHECK constraint enforces XOR between inrelease_hash and
	// (release_hash + release_gpg_hash); we set whichever pair this
	// adoption form uses. The validators (etag/lastmod) are stored
	// alongside whichever metadata file the freshness checker
	// conditional-GETs next time, so the inrelease_etag /
	// inrelease_lastmod columns end up holding Release's validators
	// in detached mode despite their column names.
	cand := cache.SnapshotCandidate{
		CanonicalScheme: suite.CanonicalScheme,
		CanonicalHost:   suite.CanonicalHost,
		SuitePath:       suite.SuitePath,
	}
	switch p.form {
	case adoptionFormInline:
		cand.InReleaseHash = &p.inlineHash
	case adoptionFormDetached:
		cand.ReleaseHash = &p.releaseHash
		cand.ReleaseGPGHash = &p.releaseGPGHash
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

	// Step 6: metadata-self snapshot_member row(s). Without these
	// the §6.1 snapshot-scoped lookup would 404 on the very URLs
	// apt fetches first. Inline mode contributes one row (InRelease);
	// detached mode contributes two (Release, Release.gpg).
	switch p.form {
	case adoptionFormInline:
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           "InRelease",
			BlobHash:       p.inlineHash,
			DeclaredSHA256: p.inlineHash,
		})
	case adoptionFormDetached:
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           "Release",
			BlobHash:       p.releaseHash,
			DeclaredSHA256: p.releaseHash,
		})
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           "Release.gpg",
			BlobHash:       p.releaseGPGHash,
			DeclaredSHA256: p.releaseGPGHash,
		})
	}

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

	// Step 8: parse every Packages member to populate package_hash
	// rows. Deduped by .deb url-path within the adoption — multiple
	// Packages variants (Packages, Packages.gz) declare the same
	// content, and the resulting rows would otherwise collide on the
	// package_hash primary key.
	packageHashes, err := a.buildPackageHashes(suite, snapshotID, members)
	if err != nil {
		return err // already wrapped with category
	}

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
		"form", formName(p.form),
		"member_count", len(members),
		"alias_count", len(aliasSeen),
		"package_hash_count", len(packageHashes),
	)
	return nil
}

// formName renders an adoptionForm as a stable string suitable for
// structured logs. Operators pivot adoption_success on this to track
// per-suite form drift over time.
func formName(f adoptionForm) string {
	switch f {
	case adoptionFormInline:
		return "inline"
	case adoptionFormDetached:
		return "detached"
	default:
		return "unknown"
	}
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

// buildPackageHashes walks every Packages-shaped Release member, parses
// it, and returns deduplicated cache.PackageHash rows for the snapshot.
// Returns (nil, nil) for suites whose layout doesn't follow apt's
// "/dists/<codename>" convention — those still adopt successfully but
// with empty package_hash coverage, falling back to trust-upstream for
// .deb hash validation.
func (a *Adopter) buildPackageHashes(suite SuiteRef, snapshotID int64,
	members []ReleaseMember) ([]cache.PackageHash, error) {
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if !ok {
		a.logger.Info("adoption: skipping package_hash population (non-/dists/ suite layout)",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
		)
		return nil, nil
	}

	// debPath -> declared SHA256. Multiple Packages variants in the
	// same Release declare identical content, so deduplication is
	// load-bearing for the package_hash primary key.
	dedup := make(map[string]string)
	for _, m := range members {
		if !isPackagesMember(m.Path) {
			continue
		}
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
		if err != nil {
			return nil, fmt.Errorf("%w: read %q: %v", ErrAdoptionParseFailed, m.Path, err)
		}
		refs, err := ParsePackages(body)
		if err != nil {
			return nil, fmt.Errorf("%w: parse %q: %v", ErrAdoptionParseFailed, m.Path, err)
		}
		for _, ref := range refs {
			debPath := repoRoot + ref.Filename
			if existing, dup := dedup[debPath]; dup && existing != ref.SHA256 {
				// Self-inconsistent Release: two variants declare
				// different SHA256 for the same .deb. Adoption
				// fails closed — apt would also reject this.
				return nil, fmt.Errorf("%w: %q declared %s vs %s across Packages variants",
					ErrAdoptionParseFailed, debPath, existing, ref.SHA256)
			}
			dedup[debPath] = ref.SHA256
		}
	}

	rows := make([]cache.PackageHash, 0, len(dedup))
	for debPath, declared := range dedup {
		rows = append(rows, cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            debPath,
			DeclaredSHA256:  declared,
			SnapshotID:      snapshotID,
		})
	}
	return rows, nil
}

// isPackagesMember reports whether m's relative path is a Packages
// file we can parse. Phase 2 step 2 supports plain Packages and
// Packages.gz; Packages.xz is intentionally skipped (would require an
// xz dependency — defer until needed).
func isPackagesMember(p string) bool {
	base := path.Base(p)
	return base == "Packages" || base == "Packages.gz"
}

// repoRootFromSuitePath returns the apt repository root path for a
// "<repo>/dists/<codename>" suite path — that is, everything up to
// and including the last "/" before "dists/". Returns (path, true) on
// success, ("", false) for non-conforming layouts.
//
// Examples:
//
//	"/ubuntu/dists/noble"           -> "/ubuntu/", true
//	"/debian/dists/bookworm-updates" -> "/debian/", true
//	"/dists/foo"                    -> "/", true
//	"/some/non/standard"            -> "", false
func repoRootFromSuitePath(suitePath string) (string, bool) {
	idx := strings.Index(suitePath, "/dists/")
	if idx < 0 {
		return "", false
	}
	return suitePath[:idx+1], true
}

// readPackagesBlob opens the pool blob for a Packages member and
// returns its decompressed bytes (or raw bytes for plain Packages).
// Reads are size-capped against gzip-bomb amplification.
func (a *Adopter) readPackagesBlob(memberPath, blobHash string) ([]byte, error) {
	f, err := os.Open(a.cache.BlobPath(blobHash))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if strings.HasSuffix(memberPath, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gr.Close()
		// io.LimitReader caps at exactly the limit; if the actual
		// content reaches the cap, treat that as a bomb and abort.
		// Add 1 byte of slack so we can distinguish "exactly cap"
		// from "would have exceeded cap".
		limited := io.LimitReader(gr, maxDecompressedPackagesBytes+1)
		body, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		if int64(len(body)) > maxDecompressedPackagesBytes {
			return nil, fmt.Errorf("Packages.gz decompresses past %d-byte cap (bomb defense)",
				maxDecompressedPackagesBytes)
		}
		return body, nil
	}
	// Plain Packages — also size-cap the read to bound a hostile-
	// upstream signed-but-huge file (matches the gzip path's posture).
	limited := io.LimitReader(f, maxDecompressedPackagesBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxDecompressedPackagesBytes {
		return nil, fmt.Errorf("Packages exceeds %d-byte cap", maxDecompressedPackagesBytes)
	}
	return body, nil
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
