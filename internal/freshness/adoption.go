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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/integrity"
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

	// ErrAdoptionUnpinnedSuite is the freshness-level sentinel for
	// the SPEC5 §10.4.3 `unpinned_suite` outcome — emitted by the
	// gpg verifier when integrity.require_pinned_signer is set and
	// no [[trusted_signer]] block matches the suite's host. The
	// gpg package wraps its own ErrUnpinnedSuite alongside this
	// sentinel so errors.Is(err, ErrAdoptionUnpinnedSuite) at the
	// freshness layer produces the right metric label without
	// pulling internal/gpg into freshness's import graph (gpg
	// already imports freshness, so a freshness->gpg edge would
	// cycle).
	ErrAdoptionUnpinnedSuite = errors.New("adoption_unpinned_suite")
)

// AdoptionMemberError carries the failing Release member's suite-
// relative path and a short human detail alongside one of the
// category sentinels above. It is the carrier that lets the admin
// status surface answer "which member failed and why" (SPEC5 §10.5
// recent_adoptions[].member_path / .detail) without string-parsing
// the error text.
//
// The wrapped err is built with fmt.Errorf("%w: ...", sentinel, ...),
// so errors.Is(err, ErrAdoptionMemberFetchFailed) (and
// ErrAdoptionMemberMismatch) still see the category through Unwrap —
// classifyAdoptionOutcome keeps working unchanged. errors.As(err,
// &*AdoptionMemberError) recovers Path/Detail at the freshness layer.
type AdoptionMemberError struct {
	Path   string // suite-relative member path, e.g. "Contents-amd64"
	Detail string // short reason, e.g. "served 114572 vs declared 1664594"
	err    error  // wraps the category sentinel via %w
}

func (e *AdoptionMemberError) Error() string { return e.err.Error() }

func (e *AdoptionMemberError) Unwrap() error { return e.err }

// newMemberErr wraps a category sentinel with the failing member's
// path + detail. The resulting error satisfies both errors.Is(…,
// sentinel) and errors.As(…, **AdoptionMemberError).
func newMemberErr(sentinel error, path, detail string) error {
	return &AdoptionMemberError{
		Path:   path,
		Detail: detail,
		err:    fmt.Errorf("%w: %s: %s", sentinel, path, detail),
	}
}

// memberErrorFields recovers the failing member path + detail from an
// adoption error, seeing through arbitrary wrapping via errors.As.
// Returns ("","") for nil or any error not carrying an
// *AdoptionMemberError — the freshness ring writes those as empty so
// success rows and non-member failures keep the pre-existing shape.
func memberErrorFields(err error) (path, detail string) {
	var me *AdoptionMemberError
	if errors.As(err, &me) {
		return me.Path, me.Detail
	}
	return "", ""
}

// archFilterBinaryRE matches the binary-arch index file shapes that the
// SPEC6_5 §7.2 architecture filter scopes to: Packages files (and their
// compressed variants) plus the Packages.diff/Index pdiff manifest. Per-
// component-arch Release files (binary-<arch>/Release) are deliberately
// NOT covered — they pass through the filter.
var archFilterBinaryRE = regexp.MustCompile(`(?:^|/)binary-([a-z][a-z0-9]*)/(?:Packages(?:\.(?:gz|xz|bz2))?|Packages\.diff/Index)$`)

// archFilterSourceRE matches the source-component index files (Sources
// and Sources.diff/Index) — the §7.2 filter treats these under the
// pseudo-arch "source".
var archFilterSourceRE = regexp.MustCompile(`(?:^|/)source/(?:Sources(?:\.(?:gz|xz|bz2))?|Sources\.diff/Index)$`)

// archFilterOptionalRES are the SPEC6_7 §7 filter extension: per-arch
// OPTIONAL member shapes — Contents (plain and udeb flavor, component
// or suite-root level), cnf Commands, and dep11 Components. Capture 1
// is the arch in each. Foreign-arch instances of these are the bulk of
// the ~160 guaranteed-404 fetch attempts per Ubuntu suite (the main
// archive declares them but only ports.ubuntu.com serves them);
// pre-skipping them under the allowlist saves the traffic and makes
// the adoption_member_skipped log/metric high-signal. Arch-independent
// optional members (dep11 icons, i18n Translations) stay unfiltered.
var archFilterOptionalRES = []*regexp.Regexp{
	regexp.MustCompile(`(?:^|/)Contents-(?:udeb-)?([a-z][a-z0-9]*)(?:\.(?:gz|xz|bz2))?$`),
	regexp.MustCompile(`(?:^|/)cnf/Commands-([a-z][a-z0-9]*)(?:\.(?:gz|xz|bz2))?$`),
	regexp.MustCompile(`(?:^|/)dep11/Components-([a-z][a-z0-9]*)\.yml(?:\.(?:gz|xz|bz2))?$`),
}

// archFromFilteredPath inspects a Release member's suite-relative path
// and reports the architecture tag the SPEC6_5 §7.2 / SPEC6_7 §7
// filter would key on. Returns ("amd64", true) for
// "main/binary-amd64/Packages.gz" or "Contents-amd64.gz", ("source",
// true) for "main/source/Sources.xz", and ("", false) for any path the
// filter does not scope to (Release.gpg, i18n translations, dep11
// icons, per-component-arch Release files, etc.).
//
// The pseudo-arch "all" is exempt by construction: arch:all content
// (e.g. Debian's Contents-all) serves clients of EVERY architecture,
// so no allowlist may filter it — it reports ("", false) like an
// unfiltered path.
func archFromFilteredPath(p string) (arch string, filtered bool) {
	if m := archFilterBinaryRE.FindStringSubmatch(p); m != nil {
		return m[1], true
	}
	if archFilterSourceRE.MatchString(p) {
		return "source", true
	}
	for _, re := range archFilterOptionalRES {
		if m := re.FindStringSubmatch(p); m != nil {
			if m[1] == "all" {
				return "", false
			}
			return m[1], true
		}
	}
	return "", false
}

// indexTargetRE matches any apt "IndexTarget" member: a per-arch
// Packages or per-component Sources file, with ANY compression suffix
// (.gz/.xz/.bz2/.zst/.lzma/.lz4/…) or none, plus the Packages.diff /
// Sources.diff pdiff Index manifests.
//
// AIDEV-NOTE: this is deliberately BROADER than the SPEC6_5 §7.2
// arch-filter regexes (archFilterBinaryRE/archFilterSourceRE), which
// only list the codecs adoption can currently parse. isIndexTarget is
// the REC 1 fatality boundary, not a parse-capability gate: a
// Packages.zst we cannot yet parse must still be FATAL-on-failure, not
// silently skipped as "optional," or a fetch failure on the only codec
// a suite publishes would adopt an empty index. The `[a-z0-9]+` suffix
// class matches future/unknown codecs by construction. Never narrow
// this without a security review — it is the surface apt installs from.
var indexTargetRE = regexp.MustCompile(
	`(?:^|/)(?:binary-[a-z][a-z0-9]*/Packages|source/Sources)(?:\.diff/Index|\.[a-z0-9]+)?$`)

// isIndexTarget reports whether a Release member's suite-relative path
// is an apt IndexTarget — a per-arch Packages* / per-component Sources*
// (any codec) or their pdiff Index, i.e. the files apt actually fetches
// for `apt update` + `apt install`. Everything else (Contents-*, dep11
// Components/icons, i18n Translations, …) is an optional member apt does
// not require.
//
// REC 1 keys on this: when [adoption].tolerate_optional_member_failures
// is set, an integrity/availability failure on a non-IndexTarget member
// is skipped instead of aborting the whole suite; IndexTargets stay
// fatal regardless.
func isIndexTarget(p string) bool {
	return indexTargetRE.MatchString(p)
}

// indexTargetGroupRE matches the BASE IndexTarget shapes — a per-arch
// Packages or per-component Sources file with any (or no) compression
// codec — but NOT the .diff/Index pdiff manifests. Capture 1 is the
// path tail through the basename ("binary-<arch>/Packages" or
// "source/Sources"); capture 2 is the arch (empty for source).
var indexTargetGroupRE = regexp.MustCompile(
	`(?:^|/)(binary-([a-z][a-z0-9]*)/Packages|source/Sources)(?:\.[a-z0-9]+)?$`)

// indexTargetGroup classifies a Release member path into its SPEC6_7
// §6 index-target GROUP: all compression variants of one logical index
// collapse to a single key (the path minus codec suffix), because an
// archive that serves Packages.xz but declares an uncompressed
// Packages it never publishes is healthy — the group is "served" iff
// at least one variant fetched. pdiff Index manifests deliberately
// return ok=false: a present patch manifest with no actual index must
// not count as the index being served.
//
// Returns (group key, arch, true) for base IndexTarget shapes — arch
// is the pseudo-arch "source" for Sources groups — and ("", "", false)
// for everything else.
func indexTargetGroup(p string) (group, arch string, ok bool) {
	m := indexTargetGroupRE.FindStringSubmatchIndex(p)
	if m == nil {
		return "", "", false
	}
	// m[2]:m[3] bounds capture 1; the group key runs from the path
	// start through capture 1's end (i.e. the codec suffix stripped).
	group = p[:m[3]]
	if m[4] >= 0 {
		arch = p[m[4]:m[5]]
	} else {
		arch = "source"
	}
	return group, arch, true
}

// errAdoptionMemberSkipped is the in-package signal from adoptMember
// that the upstream returned 4xx for a declared Release member. Step 5
// of runShared catches this with errors.Is, increments skippedCount,
// and continues — the snapshot is committed without the member.
//
// Unexported because it is not an outcome category: adoptions that
// skipped members emit outcome=success (or run_failed via the
// all-skipped guard if zero members fetched). Operators see the skip
// via the per-member adoption_member_skipped WARN line and the
// skipped_count field on adoption_success.
//
// SPEC2 §7.5.2 (Phase 2 clarification): a member 4xx is treated as
// "upstream declared but does not serve" — apt itself only fetches
// IndexTargets, so an entry the Release advertises but the archive
// 404s on is a publication artifact, not a contract violation. The
// canonical case is Ubuntu's Release file declaring an uncompressed
// Contents-amd64 the archive only ships as Contents-amd64.gz.
var errAdoptionMemberSkipped = errors.New("adoption_member_skipped")

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

	// HotPackagesWindow is the SPEC3 §5.2 hot_packages.window. 0
	// disables hot-package proactive refresh entirely.
	HotPackagesWindow time.Duration

	// HotPrefetchBudget is the SPEC3 §5.2 adoption.hot_prefetch_budget
	// wall-clock cap on the entire hot-prefetch loop. 0 disables the
	// wall-clock guard (per-deb upstream.total_timeout × max_retries
	// still applies). Per SPEC3 §10.2 a startup warning is emitted
	// when 0; the loud-config check lives at the cmd/main level.
	HotPrefetchBudget time.Duration

	// HeartbeatInterval is the period of the SPEC4 §7.5.2 site 6
	// per-adoption heartbeat ticker. The ticker runs as a sidecar
	// goroutine for the lifetime of runShared and bounds the gap
	// between heartbeats during phases the five event-driven sites
	// don't cover (Packages-parse, hot-set computation, writer-queue
	// waits). 0 disables the ticker entirely; callers (cmd/main)
	// pass gc.heartbeat_interval, validated to > 0 by config.
	HeartbeatInterval time.Duration

	// Architectures is the SPEC6_5 §5.1 [adoption].architectures
	// allowlist. Empty preserves Phase 6 behavior (every Release-listed
	// per-arch / per-source index is adopted). Non-empty restricts
	// adoption to the listed binary-<arch>/ and (optionally) source/
	// indices per §7.2. Validated by the config layer; this field
	// receives only well-formed values.
	Architectures []string

	// TolerateOptionalMemberFailures is the REC 1
	// [adoption].tolerate_optional_member_failures switch (default true).
	// When true, an integrity/availability failure (size/content-length
	// mismatch, hash mismatch, transport error, non-404 status) on a
	// non-IndexTarget member (Contents-*, dep11 Components/icons, i18n
	// Translations) is logged as adoption_member_skipped and skipped,
	// instead of aborting the whole suite adoption. IndexTarget members
	// (per-arch Packages*, per-component Sources*, their pdiff Indexes)
	// remain fatal-on-failure regardless — the surface apt installs from
	// is never weakened. When false, every member failure is fatal
	// (Phase-6 strict all-or-nothing).
	TolerateOptionalMemberFailures bool

	// MemberRetryCount / MemberRetryDelay are the SPEC6_7 §1
	// [adoption].member_retry_count / member_retry_delay knobs: how many
	// EXTRA attempts a failing member fetch gets, and the wait between
	// attempts. Retries fire only on integrity/availability failures
	// (ErrAdoptionMemberFetchFailed / ErrAdoptionMemberMismatch) — the
	// stale-mirror class where a round-robin pool serves the previous
	// publication generation mid-sync; each attempt re-probes the
	// by-hash URL first, which is immune to the revision race and heals
	// as soon as the backend syncs. 404/410 skips (permanent publication
	// artifacts) and DB faults are never retried. 0 retries = single
	// attempt (the pre-SPEC6_7 behavior; the zero value).
	MemberRetryCount int
	MemberRetryDelay time.Duration

	// RepairSkippedMembers is the SPEC6_7 §3
	// [adoption].repair_skipped_members switch (default true in config;
	// zero-value false here so direct constructions opt in explicitly).
	// When true, a freshness check that finds an adopted suite
	// unchanged at upstream re-attempts the snapshot's recorded
	// integrity-class skips (RepairSkippedMembers method) — bounding
	// recovery from a stale-mirror adoption to mirror-sync time instead
	// of the next InRelease publication (up to ~17h on devel suites).
	RepairSkippedMembers bool

	// RequiredArchitectures is the SPEC6_7 §6
	// [adoption].required_architectures guard: arches (or the
	// pseudo-arch "source") whose IndexTarget groups MUST be served for
	// a snapshot to commit. When a required arch's group has declared
	// variants but ZERO fetched (e.g. a transient mirror 404 across all
	// its Packages* variants), the adoption FAILS — deferring to the
	// next freshness tick while the previous coherent snapshot keeps
	// serving — instead of committing a snapshot that would hard-fail
	// `apt update` for that arch until the next InRelease publication.
	// Empty (default) preserves prior behavior: upstreams legitimately
	// declare arches they never serve (Ubuntu's main archive declares
	// armhf/ppc64el/… served only by ports.ubuntu.com), so only the
	// operator can say which arches their fleet needs. Config validates
	// it is a subset of Architectures when that allowlist is set.
	RequiredArchitectures []string

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

	// hotPackagesWindow + hotPrefetchBudget are the SPEC3 §5.2 keys
	// the hot-prefetch loop reads. Stored on the Adopter so each
	// adoption invocation observes a stable snapshot of the policy at
	// startup — operators flipping config mid-run only see the new
	// values on the next process restart, the same way Phase 2
	// require_signature is captured.
	hotPackagesWindow time.Duration
	hotPrefetchBudget time.Duration

	// heartbeatInterval drives the SPEC4 §7.5.2 site 6 ticker. 0
	// disables the ticker (used by tests; production main always
	// passes a positive value validated by the [gc] config block).
	heartbeatInterval time.Duration

	// architectureAllowlist is the precomputed SPEC6_5 §7.2 lookup set.
	// nil = filter inert (preserves Phase 6 behavior); non-nil = arch
	// must be a key in the map for binary-<arch>/ and source/ index
	// members to be adopted.
	architectureAllowlist map[string]struct{}

	// tolerateOptionalMembers is the REC 1 switch (see AdoptionConfig.
	// TolerateOptionalMemberFailures). When true, non-IndexTarget member
	// failures are skipped rather than fatal.
	tolerateOptionalMembers bool

	// memberRetryCount / memberRetryDelay: SPEC6_7 §1 in-adoption retry
	// policy (see AdoptionConfig). retrySleep is the ctx-aware wait
	// between attempts — a test seam; production uses ctxSleep.
	memberRetryCount int
	memberRetryDelay time.Duration
	retrySleep       func(ctx context.Context, d time.Duration) error

	// repairSkippedMembers gates the SPEC6_7 §3 freshness-tick repair
	// pass (see AdoptionConfig.RepairSkippedMembers). The Checker reads
	// this before spawning a repair goroutine.
	repairSkippedMembers bool

	// requiredArchitectures is the precomputed SPEC6_7 §6 lookup set
	// (see AdoptionConfig.RequiredArchitectures). nil = guard inert.
	requiredArchitectures map[string]struct{}

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
	if cfg.HotPackagesWindow < 0 {
		return nil, fmt.Errorf("freshness: hot_packages.window must be >= 0, got %s", cfg.HotPackagesWindow)
	}
	if cfg.HotPrefetchBudget < 0 {
		return nil, fmt.Errorf("freshness: adoption.hot_prefetch_budget must be >= 0, got %s", cfg.HotPrefetchBudget)
	}
	if cfg.HeartbeatInterval < 0 {
		return nil, fmt.Errorf("freshness: heartbeat_interval must be >= 0, got %s", cfg.HeartbeatInterval)
	}
	if cfg.MemberRetryCount < 0 {
		return nil, fmt.Errorf("freshness: member_retry_count must be >= 0, got %d", cfg.MemberRetryCount)
	}
	if cfg.MemberRetryDelay < 0 {
		return nil, fmt.Errorf("freshness: member_retry_delay must be >= 0, got %s", cfg.MemberRetryDelay)
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
	var allowlist map[string]struct{}
	if len(cfg.Architectures) > 0 {
		allowlist = make(map[string]struct{}, len(cfg.Architectures))
		for _, arch := range cfg.Architectures {
			allowlist[arch] = struct{}{}
		}
	}
	var required map[string]struct{}
	if len(cfg.RequiredArchitectures) > 0 {
		required = make(map[string]struct{}, len(cfg.RequiredArchitectures))
		for _, arch := range cfg.RequiredArchitectures {
			required[arch] = struct{}{}
		}
	}
	return &Adopter{
		cache:                   cfg.Cache,
		fetcher:                 cfg.Fetcher,
		verifier:                cfg.Verifier,
		hostSem:                 cfg.HostLimiter,
		concurrencySem:          sem,
		hotPackagesWindow:       cfg.HotPackagesWindow,
		hotPrefetchBudget:       cfg.HotPrefetchBudget,
		heartbeatInterval:       cfg.HeartbeatInterval,
		architectureAllowlist:   allowlist,
		tolerateOptionalMembers: cfg.TolerateOptionalMemberFailures,
		memberRetryCount:        cfg.MemberRetryCount,
		memberRetryDelay:        cfg.MemberRetryDelay,
		retrySleep:              ctxSleep,
		repairSkippedMembers:    cfg.RepairSkippedMembers,
		requiredArchitectures:   required,
		logger:                  logger,
		now:                     now,
	}, nil
}

// ctxSleep waits d, returning early with ctx.Err() on cancellation.
// The production retrySleep — shutdown must never block on a member
// retry backoff.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// blobHeartbeatTracker is the per-adoption mutable list of member-blob
// hashes that runShared has PutBlob'd but whose snapshot_member rows
// have not yet been inserted (CommitAdoption Step 4 is the inserter).
// Each §7.5.2 heartbeat site reads the snapshot of this list and
// passes it to cache.HeartbeatBlobs to refresh refcount_zeroed_at on
// the in-flight blobs — defending against the race where adoption
// duration > gc.blob_grace causes a still-needed member blob to be
// reaped before CommitAdoption can claim it via Rule 2.
//
// The mutex protects against the concurrent reads from the periodic
// heartbeat ticker (site 6) racing the appends in the member-fetch
// loop (site 2).
type blobHeartbeatTracker struct {
	mu     sync.Mutex
	hashes []string
}

// Add appends hash if it is non-empty. Duplicates are accepted — the
// HeartbeatBlobs IN-list collapses them at the SQL level and a
// duplicate UPDATE is a no-op.
func (t *blobHeartbeatTracker) Add(hash string) {
	if hash == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hashes = append(t.hashes, hash)
}

// Snapshot returns a copy of the current hash list. Returns nil on an
// empty tracker so callers can branch cheaply on the zero case.
func (t *blobHeartbeatTracker) Snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.hashes) == 0 {
		return nil
	}
	out := make([]string, len(t.hashes))
	copy(out, t.hashes)
	return out
}

// heartbeat is the shared site-2/3/4/5 + ticker call. SPEC4 §7.5.2:
// the orphan-candidate reap predicate keys on
// suite_snapshot.heartbeat_at, not on created_at. Each event-driven
// site refreshes the row's clock after a phase that could otherwise
// leave the gap unbounded under adversarial conditions (slow members,
// large Packages parse, deep writer queue).
//
// When tracker is non-nil and has accumulated hashes, this also
// refreshes refcount_zeroed_at on the in-flight member blobs via
// cache.HeartbeatBlobs — closing the SPEC4 §7.5.1 Rule 1 race window
// where a long adoption ages PutBlob'd member blobs past
// gc.blob_grace before CommitAdoption inserts their snapshot_member
// rows.
//
// Heartbeat-write failures are non-fatal: log at
// adoption_heartbeat_failed Warn and continue. The next heartbeat (or
// the successful CommitAdoption that flips adopted_at) restores
// liveness; an adoption whose every heartbeat silently fails is what
// the periodic ticker (site 6) defends against, and even that
// defence has the §9.6.3 grace floor as the safety bound.
//
// Context cancellation (parent shutdown) is suppressed — that's the
// expected exit path during graceful shutdown, not an operator-visible
// failure mode.
func (a *Adopter) heartbeat(ctx context.Context, host string, snapshotID int64, tracker *blobHeartbeatTracker) {
	if err := a.cache.HeartbeatSnapshot(ctx, snapshotID); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			a.logger.Warn("adoption_heartbeat_failed",
				"snapshot_id", snapshotID,
				"err", err,
			)
			// SPEC5 §10.4.3: count only operator-actionable failures
			// — ctx cancel/deadline are the expected shutdown path
			// and stay suppressed (consistent with the Warn gate).
			adoptionHeartbeatFailuresTotal.Inc(host)
		}
	}
	if tracker == nil {
		return
	}
	hashes := tracker.Snapshot()
	if len(hashes) == 0 {
		return
	}
	if err := a.cache.HeartbeatBlobs(ctx, hashes); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			a.logger.Warn("adoption_heartbeat_blobs_failed",
				"snapshot_id", snapshotID,
				"hash_count", len(hashes),
				"err", err,
			)
			// AIDEV-NOTE: do NOT increment
			// acu_adoption_heartbeat_failures_total here. SPEC5
			// §10.4.3 specifies the counter mirrors the
			// adoption_heartbeat_failed Warn (snapshot heartbeat
			// only); adoption_heartbeat_blobs_failed is a
			// distinct event. Folding both into one counter
			// would double-count when both writes fail in the
			// same heartbeat pass.
		}
	}
}

// runHeartbeatTicker is SPEC4 §7.5.2 site 6: a per-adoption sidecar
// goroutine that wakes every heartbeatInterval and submits a
// HeartbeatSnapshot write (and a HeartbeatBlobs write when the
// tracker has accumulated member hashes). Cancels via ctx — the
// caller (runShared) derives a child ctx and cancels it at function
// exit, then waits on the WaitGroup so no goroutine outlives
// runShared.
//
// This site bounds the heartbeat-gap independently of which phase
// runShared is in (Packages-parse, hot-set computation, between-fetch
// gaps, the gap from runHotPrefetch returning to CommitAdoption
// running). Sites 1–5 are latency-fresh event-driven heartbeats; the
// ticker is the floor under them, not a replacement.
func (a *Adopter) runHeartbeatTicker(ctx context.Context, host string, snapshotID int64, tracker *blobHeartbeatTracker) {
	if a.heartbeatInterval <= 0 {
		return
	}
	t := time.NewTicker(a.heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.heartbeat(ctx, host, snapshotID, tracker)
		}
	}
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
	releaseBytes   []byte
	sigBytes       []byte
	releaseHash    string // sha256 of releaseBytes (set in step 2)
	releaseGPGHash string // sha256 of sigBytes (set in step 2)
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

	// Step 0a: capture the prior adoption form. After CommitAdoption
	// the prior snapshot is no longer current, so the lookup must
	// happen before any mutation. Step 10 compares this against p.form
	// and emits adoption_form_drift on a transition.
	priorForm, hadPrior := a.priorAdoptionForm(ctx, suite)

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
		// %w: %w preserves verifyErr in the chain so
		// classifyAdoptionOutcome can distinguish unpinned_suite
		// (gpg.ErrUnpinnedSuite chain-wrapped via
		// ErrAdoptionUnpinnedSuite) from generic gpg_failed.
		return fmt.Errorf("%w: %w", ErrAdoptionGPGFailed, verifyErr)
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
	// REC 2: detect Acquire-By-Hash once for the whole suite; adoptMember
	// uses it to prefer the content-addressed by-hash URL per member.
	acquireByHash := parseAcquireByHash(releaseText)
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
	// AIDEV-NOTE: Steps 5-9 are content-only against snapshot_id;
	// CommitAdoption is its own transaction guarded by adopted_at IS NULL,
	// so retrying a reused candidate snapshot_id is safe. reused == true
	// surfaces the "we recovered an orphaned candidate from a prior
	// failed adoption attempt" case as a one-shot INFO so operators can
	// see the fix is active without having to grep for absence of WARNs.
	snapshotID, reused, err := a.cache.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		if errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
			a.logger.Warn("adoption: natural key already adopted",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
		} else {
			a.logger.Warn("adoption: insert candidate failed",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"err", err,
			)
		}
		return fmt.Errorf("%w: insert candidate: %v", ErrAdoptionDBFailed, err)
	}
	if reused {
		a.logger.Info("adoption: reusing orphaned candidate",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
		)
	}

	// SPEC4 §7.5.1 Rule 1 race-window extension: track the in-flight
	// member-blob hashes so each heartbeat (sites 2-6) can also
	// refresh refcount_zeroed_at on them. Without this, a member
	// blob fetched in the first minute of a 6-minute adoption ages
	// past gc.blob_grace before CommitAdoption can insert its
	// snapshot_member row, and the FK INSERT then fails. Seed with
	// the metadata blob hashes already PutBlob'd in step 2 — those
	// are FK-protected by the candidate suite_snapshot row's hash
	// columns once InsertCandidateSnapshot has run, but seeding
	// covers the brief window between writeBlobBytes and
	// InsertCandidateSnapshot landing.
	tracker := &blobHeartbeatTracker{}
	if p.inlineHash != "" {
		tracker.Add(p.inlineHash)
	}
	if p.releaseHash != "" {
		tracker.Add(p.releaseHash)
	}
	if p.releaseGPGHash != "" {
		tracker.Add(p.releaseGPGHash)
	}

	// SPEC4 §7.5.2 site 6: launch the periodic heartbeat ticker.
	// The ticker runs until tickerCancel fires at runShared exit;
	// the deferred close-then-wait ensures the cancel happens
	// before the wait (a single deferred func avoids the LIFO
	// ordering trap of two separate defers, which would Wait first
	// and deadlock waiting on a goroutine whose ctx hasn't been
	// cancelled yet).
	tickerCtx, tickerCancel := context.WithCancel(ctx)
	var tickerWG sync.WaitGroup
	if a.heartbeatInterval > 0 {
		tickerWG.Add(1)
		go func() {
			defer tickerWG.Done()
			a.runHeartbeatTicker(tickerCtx, suite.CanonicalHost, snapshotID, tracker)
		}()
	}
	defer func() {
		tickerCancel()
		tickerWG.Wait()
	}()

	// Step 5: prefetch declared members sequentially. Each member's
	// declared_sha256 is the trust anchor — bytes that arrive from
	// upstream are accepted only if their fresh hash matches.
	//
	// SPEC4 §7.5.2 site 2: heartbeat after each adoptMember return.
	// Member fetches against degraded upstreams can take minutes;
	// without this site the gap from row creation to the next
	// in-runShared heartbeat (site 3 after Packages parsing)
	// could exceed grace under a slow-member cascade. The tracker
	// gets the freshly-fetched blob hash so the same heartbeat
	// also refreshes refcount_zeroed_at on it via HeartbeatBlobs.
	memberRows := make([]cache.SnapshotMember, 0, len(members)+3)
	// fetchedMembers parallels memberRows but holds the original
	// ReleaseMember (with Size). Step 7 (by-hash alias) and Step 8
	// (buildPackageHashes) iterate this slice instead of `members` so
	// that 4xx-skipped paths don't get phantom alias rows pointing at
	// a non-existent blob (FK violation on snapshot_member.blob_hash)
	// or a Packages-blob read against an empty pool entry.
	fetchedMembers := make([]ReleaseMember, 0, len(members))
	skippedCount := 0
	// integritySkipped collects the SPEC6_7 §2 repair records: members
	// skipped over an integrity failure keep their signed declaration
	// (sha256 + size) in snapshot_skipped_member so the freshness-tick
	// repair pass can re-fetch them once the mirror finishes syncing.
	// 4xx skips are NOT recorded for repair — those are near-always
	// publication artifacts (declared but never served) and re-fetching
	// them every tick would be guaranteed-failure upstream traffic.
	var integritySkipped []cache.SkippedMember
	// SPEC6_7 §6: per-group declared/fetched bookkeeping for the
	// required-architectures guard. Tracked only for required arches;
	// nil maps when the guard is inert.
	var (
		requiredGroupDeclared map[string]int
		requiredGroupFetched  map[string]bool
	)
	if len(a.requiredArchitectures) > 0 {
		requiredGroupDeclared = make(map[string]int)
		requiredGroupFetched = make(map[string]bool)
	}
	for _, m := range members {
		// SPEC6_5 §7.2: per-arch / per-source index filter. Inert when
		// the allowlist is empty (Phase 6 default). Skipped members never
		// reach upstream — saves bandwidth and pool disk on caches whose
		// clients only fetch a subset of the upstream's published arches.
		if a.architectureAllowlist != nil {
			if arch, filtered := archFromFilteredPath(m.Path); filtered {
				if _, ok := a.architectureAllowlist[arch]; !ok {
					a.logger.Warn("adoption_member_skipped",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"path", m.Path,
						"declared_sha256", m.SHA256,
						"reason", "arch_not_in_allowlist",
						"architecture", arch,
					)
					adoptionMembersSkippedTotal.Inc("arch_not_in_allowlist")
					skippedCount++
					continue
				}
			}
		}

		// SPEC6_7 §6: count declared variants per required-arch index
		// group. Placed after the allowlist filter — config validation
		// guarantees required ⊆ allowlist when both are set, so a
		// required arch's members are never filtered out above.
		var requiredGroup string
		if requiredGroupDeclared != nil {
			if group, arch, ok := indexTargetGroup(m.Path); ok {
				if _, want := a.requiredArchitectures[arch]; want {
					requiredGroup = group
					requiredGroupDeclared[group]++
				}
			}
		}

		blobHash, err := a.adoptMemberWithRetry(ctx, suite, m, acquireByHash)
		if err != nil {
			if errors.Is(err, errAdoptionMemberSkipped) {
				skippedCount++
				continue
			}
			// REC 1: a non-IndexTarget member (Contents-*, dep11
			// Components/icons, i18n Translations) whose fetch or
			// integrity check failed must not abort the whole suite when
			// tolerance is enabled. These are files apt does not fetch
			// for `apt update`/`apt install`; failing the suite on them
			// is what causes the bulk of observed adoption_run_failed
			// events (e.g. an index republished mid-fetch, or a mirror
			// that serves the .gz at the uncompressed URL). The member
			// is skipped: no snapshot_member / by-hash / package_hash row
			// is created for it, so a tampered optional file can never be
			// served — apt 404s it, exactly as on the 404-skip path.
			// IndexTarget members stay fatal regardless (isIndexTarget),
			// and a local DB fault (ErrAdoptionDBFailed) is never
			// tolerated — only upstream member fetch/mismatch failures.
			if a.tolerateOptionalMembers && !isIndexTarget(m.Path) &&
				(errors.Is(err, ErrAdoptionMemberFetchFailed) || errors.Is(err, ErrAdoptionMemberMismatch)) {
				detail := ""
				var me *AdoptionMemberError
				if errors.As(err, &me) {
					detail = me.Detail
				}
				a.logger.Warn("adoption_member_skipped",
					"canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath,
					"path", m.Path,
					"declared_sha256", m.SHA256,
					"reason", "optional_member_integrity",
					"detail", detail,
				)
				adoptionMembersSkippedTotal.Inc("optional_member_integrity")
				integritySkipped = append(integritySkipped, cache.SkippedMember{
					SnapshotID:     snapshotID,
					Path:           m.Path,
					DeclaredSHA256: m.SHA256,
					Size:           m.Size,
					Reason:         cache.SkipReasonOptionalMemberIntegrity,
					Detail:         detail,
				})
				skippedCount++
				continue
			}
			return err // already wrapped with category
		}
		memberRows = append(memberRows, cache.SnapshotMember{
			SnapshotID:     snapshotID,
			Path:           m.Path,
			BlobHash:       blobHash,
			DeclaredSHA256: m.SHA256,
		})
		fetchedMembers = append(fetchedMembers, m)
		if requiredGroup != "" {
			requiredGroupFetched[requiredGroup] = true
		}
		tracker.Add(blobHash)
		a.heartbeat(ctx, suite.CanonicalHost, snapshotID, tracker)
	}
	fetchedCount := len(fetchedMembers)

	// SPEC6_7 §6: a required arch's index group with declared variants
	// but ZERO fetched would commit a snapshot that hard-fails
	// `apt update` for that arch until the next InRelease publication.
	// Fail the adoption instead — the previous coherent snapshot keeps
	// serving and the next freshness tick re-attempts (upstream still
	// differs, so the check observes "changed" again). Groups are
	// checked in sorted order so the error is deterministic when more
	// than one is missing.
	if len(requiredGroupDeclared) > 0 {
		groups := make([]string, 0, len(requiredGroupDeclared))
		for g := range requiredGroupDeclared {
			groups = append(groups, g)
		}
		sort.Strings(groups)
		for _, g := range groups {
			if requiredGroupFetched[g] {
				continue
			}
			a.logger.Warn("adoption_required_index_target_missing",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"group", g,
				"declared_variants", requiredGroupDeclared[g],
			)
			return newMemberErr(ErrAdoptionMemberFetchFailed, g,
				fmt.Sprintf("required-architecture index target: all %d declared variants missing",
					requiredGroupDeclared[g]))
		}
	}

	// SPEC2 §7.5.2 (Phase 2 clarification): an adoption where zero
	// declared members were successfully fetched is still a failure —
	// the resulting snapshot would have only metadata-self rows and
	// serve nothing useful, while creating the false appearance of an
	// adopted suite that fails strict-mode .deb requests. Realistic
	// trigger: misconfigured suite_path that points at a directory
	// whose Release lists members the archive serves under a different
	// prefix.
	if fetchedCount == 0 && len(members) > 0 {
		return fmt.Errorf("%w: all %d declared members returned 4xx",
			ErrAdoptionMemberFetchFailed, skippedCount)
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
	//
	// Iterates fetchedMembers, not members: a 4xx-skipped member has
	// no blob in the pool, so an alias row pointing at its declared
	// SHA256 would violate the snapshot_member.blob_hash → blob.hash
	// foreign key.
	aliasSeen := make(map[string]bool, len(fetchedMembers))
	for _, m := range fetchedMembers {
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
	// Packages variants (Packages, Packages.gz, Packages.xz) declare
	// the same content, and the resulting rows would otherwise collide
	// on the package_hash primary key.
	//
	// SPEC3 §7.5.4: buildPackageHashes also returns the per-snapshot
	// package_coverage_complete bit for the strict-mode predicate
	// (§6.1) to key on. The bit is folded into CommitAdoption so it
	// becomes visible to readers atomically with the suite_freshness
	// flip — strict mode only reads current snapshots, but pinning
	// the timing prevents any "candidate has coverage = 1 but is not
	// yet current" mid-state from leaking into the §7.5 flow.
	// Pass both slices: allMembers drives the SPEC3 §7.5.4 coverage
	// denominator (a 4xx-skipped Packages directory must drop
	// coverage to false, not disappear from the count); fetchedMembers
	// drives the parse loop (only those have blobs in the pool).
	pkgHashRes, err := a.buildPackageHashes(suite, snapshotID, members, fetchedMembers)
	if err != nil {
		return err // already wrapped with category
	}
	packageHashes := pkgHashRes.rows

	// SPEC6_5 §7.1: source-package adoption. Walks the same
	// fetchedMembers slice for Sources-shaped index files and folds
	// the resulting rows (Architecture="source") into the same
	// package_hash insert that runs in CommitAdoption Step 3. Per-
	// row dedup is keyed by the suite-relative artifact path; cross-
	// variant disagreement (e.g. Sources.gz vs Sources.xz declaring
	// different SHA256 for one .dsc) surfaces as ErrAdoptionParseFailed
	// alongside the existing Packages-variant disagreement check
	// (SPEC6_5 §11 H7).
	sourceRows, _, err := a.buildSourceHashes(suite, snapshotID, fetchedMembers)
	if err != nil {
		return err
	}
	packageHashes = append(packageHashes, sourceRows...)

	// SPEC6_5 §7.3: pdiff Index adoption. Parses each
	// Packages.diff/Index and Sources.diff/Index, populates
	// package_hash rows for each listed patch file (filename
	// validated against the digit/dot/dash + .gz shape). The
	// architecture column derives from the Index path's
	// binary-<arch>/ or source/ segment so the §10.4 status surface
	// can present per-arch counts uniformly across binary, source,
	// and pdiff rows. Per-Index parse failures are tolerated; cross-
	// Index disagreement on a patch path surfaces as
	// ErrAdoptionParseFailed (§11 H7-equivalent).
	pdiffRows, _, err := a.buildPdiffHashes(suite, snapshotID, fetchedMembers)
	if err != nil {
		return err
	}
	packageHashes = append(packageHashes, pdiffRows...)

	// SPEC4 §7.5.2 site 3: heartbeat after Packages parsing returns.
	// debian-main at multiple architectures can be tens of MiB of
	// compressed input; on degraded CPU/storage the parse takes
	// minutes. Without this site the gap from the last member-fetch
	// heartbeat through Packages parsing to the next heartbeat
	// would be unbounded by any fetch timeout.
	a.heartbeat(ctx, suite.CanonicalHost, snapshotID, tracker)

	// Steps 9 + 10 (SPEC3 §7.5): hot-set computation + hot-deb prefetch
	// loop. The result list (prefetchedURLPaths) feeds CommitAdoption
	// so its url_path inserts happen inside the same transaction that
	// flips current_snapshot_id — readers never observe a warmed deb's
	// url_path while the prior snapshot is still current. The
	// candidate's package_hash rows are passed in memory because the
	// flip transaction below is what inserts them — Stage 2 of the
	// hot-set computation cannot rely on them being DB-visible yet.
	prefetchedURLPaths, hotStats := a.runHotPrefetch(ctx, suite, snapshotID, packageHashes, tracker)

	// SPEC4 §7.5.2 site 5: heartbeat right before CommitAdoption.
	// Resets the grace clock at the latest possible moment before
	// the adopted_at flip becomes the source of truth, defending
	// against writer-queue depth between runHotPrefetch returning
	// and CommitAdoption actually committing.
	a.heartbeat(ctx, suite.CanonicalHost, snapshotID, tracker)

	// Step 11: atomic flip transaction. Pass adoptionCtx as ctx so the
	// budget-cancelled prefetch loop above never causes CommitAdoption
	// to fail — the contract is "cancel hot fetches, then flip", not
	// "cancel hot fetches, then also cancel the flip". coverageComplete
	// is the SPEC3 §7.5.4 per-snapshot proof for strict mode.
	if err := a.cache.CommitAdoption(ctx, snapshotID, memberRows, integritySkipped, packageHashes,
		prefetchedURLPaths, pkgHashRes.coverageComplete); err != nil {
		a.logger.Warn("adoption: commit failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"err", err,
		)
		return fmt.Errorf("%w: commit: %v", ErrAdoptionDBFailed, err)
	}

	// Step 12 (SPEC3 §10.2): aggregate adoption_hot_prefetch_complete.
	// Always emitted — even when hot_count = 0 — so an operator
	// scanning the journal can confirm the loop ran. The four
	// sum-bucket fields plus zero must equal hot_count by construction
	// (every entry lands in exactly one bucket).
	a.logger.Info("adoption_hot_prefetch_complete",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"hot_count", hotStats.hotCount,
		"fetched", hotStats.fetched,
		"failed", hotStats.failed,
		"mismatched", hotStats.mismatched,
		"unattempted", hotStats.unattempted,
	)
	hotPrefetchTotal.Inc("complete", suite.CanonicalHost)

	// Step 10: success log + form-drift signal.
	//
	// adoption_form_drift fires when a suite's signature form has
	// changed between the prior current snapshot and the one just
	// committed (inline → detached or vice versa). Operators monitoring
	// fleet-wide signing-policy changes use this as a one-time signal:
	// in steady state the form is stable, so a drift line in the log
	// surfaces an upstream archive switching its publication form
	// (e.g. dropping clearsigned InRelease in favor of detached
	// Release.gpg). Suites that have just gone from no-prior-snapshot
	// to first adoption don't drift.
	if hadPrior && priorForm != p.form {
		a.logger.Warn("adoption_form_drift",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"prior_form", formName(priorForm),
			"new_form", formName(p.form),
			"snapshot_id", snapshotID,
		)
		// SPEC5 §10.4.3: form_drift is its own counter (NOT an
		// outcome under acu_adoption_total). The drifting adoption
		// also lands in adoption_total{outcome=success} — these two
		// counters are independent.
		adoptionFormDriftTotal.Inc(formName(priorForm), formName(p.form), suite.CanonicalHost)
	}

	a.logger.Info("adoption_success",
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"snapshot_id", snapshotID,
		"form", formName(p.form),
		"member_count", len(members),
		"fetched_count", fetchedCount,
		"skipped_count", skippedCount,
		"skipped_integrity_count", len(integritySkipped),
		"alias_count", len(aliasSeen),
		"package_hash_count", len(packageHashes),
	)
	return nil
}

// priorAdoptionForm returns the adoption form of the suite's current
// snapshot, derived from its hash columns. Returns (form, true) when
// a current snapshot exists with one of the known fingerprints; (0,
// false) when there is no current snapshot, the lookup fails, or the
// snapshot has neither hash set (the latter shouldn't happen given
// the suite_snapshot CHECK constraint, but treat defensively).
//
// First-ever adoption produces (0, false), which the caller treats as
// "no prior" — first adoption is not drift.
func (a *Adopter) priorAdoptionForm(ctx context.Context, suite SuiteRef) (adoptionForm, bool) {
	fresh, err := a.cache.GetSuiteFreshness(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil || fresh == nil || fresh.CurrentSnapshotID == nil {
		return 0, false
	}
	snap, err := a.cache.GetSuiteSnapshot(ctx, *fresh.CurrentSnapshotID)
	if err != nil || snap == nil {
		return 0, false
	}
	switch {
	case snap.InReleaseHash != nil:
		return adoptionFormInline, true
	case snap.ReleaseHash != nil:
		return adoptionFormDetached, true
	}
	return 0, false
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

// adoptMemberWithRetry wraps adoptMember in the SPEC6_7 §1 retry loop.
// Only integrity/availability failures (ErrAdoptionMemberFetchFailed /
// ErrAdoptionMemberMismatch) are retried — the signature of a
// round-robin mirror serving the previous publication generation
// mid-sync, which heals within the retry window. Each fresh attempt
// re-runs the full adoptMember sequence: pool-reuse check, by-hash
// probe (the URL immune to the revision race — this is the expected
// heal path), then the canonical fetch.
//
// Never retried:
//   - errAdoptionMemberSkipped (404/410): "declared but does not
//     serve" is a permanent publication artifact; retrying would burn
//     the delay budget on guaranteed failures (~160 such members per
//     Ubuntu suite).
//   - ErrAdoptionDBFailed: local fault, not an upstream race.
//   - any error once ctx is cancelled: shutdown path.
//
// The sleep happens with NO host-semaphore slot held — adoptMember
// acquires and releases per attempt — so a member in backoff never
// starves other fetches to the same host.
func (a *Adopter) adoptMemberWithRetry(ctx context.Context, suite SuiteRef, m ReleaseMember, acquireByHash bool) (string, error) {
	blobHash, err := a.adoptMember(ctx, suite, m, acquireByHash)
	if err == nil || a.memberRetryCount == 0 {
		return blobHash, err
	}
	retried := false
	for attempt := 1; attempt <= a.memberRetryCount; attempt++ {
		if ctx.Err() != nil {
			break
		}
		if !errors.Is(err, ErrAdoptionMemberFetchFailed) && !errors.Is(err, ErrAdoptionMemberMismatch) {
			break
		}
		a.logger.Info("adoption_member_retry",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"path", m.Path,
			"attempt", attempt,
			"max_retries", a.memberRetryCount,
			"delay", a.memberRetryDelay.String(),
			"err", err,
		)
		if serr := a.retrySleep(ctx, a.memberRetryDelay); serr != nil {
			break
		}
		retried = true
		blobHash, err = a.adoptMember(ctx, suite, m, acquireByHash)
		if err == nil {
			adoptionMemberRetriesTotal.Inc("success")
			return blobHash, nil
		}
	}
	if retried {
		adoptionMemberRetriesTotal.Inc("exhausted")
	}
	return blobHash, err
}

// adoptMember handles step 5 for one declared member: try pool reuse
// (with rehash defense), else fetch from upstream. Returns the blob
// hash on success — guaranteed to equal m.SHA256, since adoption
// rejects any byte stream whose hash differs from the declaration.
func (a *Adopter) adoptMember(ctx context.Context, suite SuiteRef, m ReleaseMember, acquireByHash bool) (string, error) {
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
		integrity.IncPoolCorruptionDuringAdoption()
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

	// REC 2: prefer Acquire-By-Hash when the upstream advertises it and
	// the member lives under a directory. A by-hash URL embeds the
	// declared sha256 in its path, so it is immune to the publication
	// race where the mutable path serves a newer-but-valid revision than
	// our InRelease declared (the dominant intermittent failure on the
	// Ubuntu archives). The probe is best-effort and side-effect-free:
	// tryByHashFetch never logs adoption_member_skipped / increments the
	// skip metric and never lets mismatched bytes reach pool/ (it uses
	// FinalizeExpectingHash). On ANY by-hash failure we fall back to the
	// authoritative plain-path fetch below, which keeps today's exact
	// semantics (404/410 → skip; size/hash mismatch → typed-fatal).
	if acquireByHash {
		if bh := byHashAliasPath(m.Path, m.SHA256); bh != "" {
			if hash, ferr := a.tryByHashFetch(ctx, suite, m, bh); ferr == nil {
				return hash, nil
			} else {
				a.logger.Debug("adoption_byhash_fallback",
					"canonical_host", suite.CanonicalHost,
					"suite_path", suite.SuitePath,
					"path", m.Path,
					"err", ferr,
				)
			}
		}
	}
	return a.fetchMemberURL(ctx, suite, m, m.Path)
}

// tryByHashFetch attempts to fetch a member from its content-addressed
// by-hash URL (byHashRelPath). It is a SIDE-EFFECT-FREE probe: unlike
// fetchMemberURL it emits no adoption_member_skipped WARN, increments no
// skip metric, and — by finalizing through FinalizeExpectingHash — never
// promotes bytes whose hash differs from the declaration into pool/ (a
// by-hash URL that serves wrong bytes is rejected with the temp removed,
// leaving no orphan). On success the verified blob is PutBlob'd and its
// hash (== m.SHA256) returned. Any error means "by-hash unavailable or
// wrong"; the caller falls back to the authoritative plain-path fetch.
func (a *Adopter) tryByHashFetch(ctx context.Context, suite SuiteRef, m ReleaseMember, byHashRelPath string) (string, error) {
	target := &fetch.Target{
		CanonicalHost: suite.CanonicalHost,
		URL:           buildMemberURL(suite, byHashRelPath),
	}
	w, err := a.cache.NewTempBlob()
	if err != nil {
		return "", err
	}
	defer func() { _ = w.Abort() }() // no-op once a Finalize* wins
	if _, err := a.fetcher.Fetch(ctx, target, w); err != nil {
		return "", err // by-hash 404 (pruned), transport, etc. — silent fallback
	}
	// FinalizeExpectingHash gates on the declared sha256 (and size): on
	// any mismatch the temp is removed and pool/ is never touched.
	hashHex, err := w.FinalizeExpectingHash(m.SHA256, m.Size)
	if err != nil {
		return "", err
	}
	if err := a.cache.PutBlob(ctx, hashHex, w.Written()); err != nil {
		return "", err
	}
	return hashHex, nil
}

// fetchMemberURL fetches one Release member from a specific suite-
// relative URL (either the plain declared path or its by-hash alias),
// verifies the bytes against the member's declared Size + SHA256, and
// PutBlobs them. The host slot is held by the caller (adoptMember).
//
// Error semantics are the authoritative member-fetch rules: a 404/410
// returns errAdoptionMemberSkipped (SPEC2 §7.5.2 "declared but does not
// serve"); a size/content-length mismatch, finalize error, transport
// error, or other status returns a typed *AdoptionMemberError wrapping
// ErrAdoptionMemberFetchFailed; a hash mismatch wraps
// ErrAdoptionMemberMismatch. Verification always uses m.Size / m.SHA256
// (the signed-Release declarations), independent of which URL served
// the bytes.
func (a *Adopter) fetchMemberURL(ctx context.Context, suite SuiteRef, m ReleaseMember, relPath string) (string, error) {
	upstreamURL := buildMemberURL(suite, relPath)
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
		// SPEC2 §7.5.2 (Phase 2 clarification): only the explicit
		// "resource not present" 4xx codes — 404 Not Found and 410
		// Gone — are treated as "upstream declared but does not
		// serve" and skipped. The canonical case is Ubuntu's Release
		// file declaring an uncompressed Contents-amd64 the archive
		// only ships as Contents-amd64.gz (404). Other 4xx codes
		// stay fatal:
		//   - 401/403: auth or policy. The project assumes no
		//     upstream needs auth, but if one starts returning these
		//     we want loud failure, not silent partial snapshots.
		//   - 408/425/429: transient (timeout / too-early / rate
		//     limit). Adoption should retry on the next tick rather
		//     than persist a degraded snapshot.
		//   - All other 4xx: unknown semantics; fail closed.
		// 5xx and transport errors stay fatal too — those are
		// "upstream is broken right now", not "upstream never serves
		// this thing".
		var se *fetch.StatusError
		if errors.As(err, &se) && (se.Code == 404 || se.Code == 410) {
			// SPEC6_7 §6: a 404-skipped IndexTarget gets its own reason
			// value. Skipping it is unchanged (Ubuntu's main archive
			// declares foreign-arch Packages* that only ports.ubuntu.com
			// serves), but the signal is categorically worse than an
			// optional-member artifact — for an arch clients actually
			// use it means `apt update` hard-fails for the snapshot's
			// lifetime — so operators need to alert on it separately
			// (and opt into the required_architectures guard).
			reason := "4xx"
			if isIndexTarget(m.Path) {
				reason = "4xx_index_target"
			}
			a.logger.Warn("adoption_member_skipped",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", m.Path,
				"declared_sha256", m.SHA256,
				"upstream_status", se.Code,
				"reason", reason,
			)
			adoptionMembersSkippedTotal.Inc(reason)
			return "", errAdoptionMemberSkipped
		}
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("fetch: %v", err))
	}
	// Size sanity: declared Size in the Release file should match
	// what the fetch actually wrote. fetch.Client already honors
	// Content-Length internally, so the case where they diverge is
	// genuinely abnormal — surface it as a member-fetch error rather
	// than a hash mismatch (the bytes might still hash correctly,
	// but a length mismatch is its own integrity violation).
	if m.Size > 0 && res.ContentLength > 0 && res.ContentLength != m.Size {
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("served %d vs declared %d", res.ContentLength, m.Size))
	}

	hashHex, err := w.Finalize(res.ContentLength)
	if err != nil {
		return "", newMemberErr(ErrAdoptionMemberFetchFailed, m.Path,
			fmt.Sprintf("finalize: %v", err))
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
		return "", newMemberErr(ErrAdoptionMemberMismatch, m.Path,
			fmt.Sprintf("declared %s, got %s", shortHash(m.SHA256), shortHash(hashHex)))
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
		integrity.IncPoolCorruptionDuringAdoption()
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
// shortHash truncates a hex digest to its first 12 characters for
// compact display in the admin reason chip. The full digests stay in
// the adoption_member_mismatch Warn log (declared_sha256 /
// actual_sha256), so operators can still recover the complete values.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// byHashAliasPath returns the by-hash alias path for a member, or ""
// for an empty declared path. The alias is constructed by stripping
// the filename component from the declared path and appending
// "by-hash/SHA256/<sha256>".
//
// For "main/binary-amd64/Packages.gz" + sha "abc...": returns
// "main/binary-amd64/by-hash/SHA256/abc...".
//
// For a member at the suite root (no slash in path) the alias is
// "by-hash/SHA256/<hash>" — the suite root's own by-hash directory.
// AIDEV-NOTE: an earlier revision returned "" here on the assumption
// that apt never fetches suite-root files via by-hash. The 2026-06-09
// incident logs disproved that: apt requests
// dists/<suite>/by-hash/SHA256/<h> for root-level Contents-* (and
// Ubuntu serves it). The "" special case denied root members both the
// adoption-time by-hash probe (the URL immune to the mirror-sync
// revision race) and the served alias row — do not reintroduce it.
// SPEC6_7 §4.
func byHashAliasPath(declaredPath, sha256hex string) string {
	if declaredPath == "" {
		return ""
	}
	dir := path.Dir(declaredPath)
	if dir == "." || dir == "" {
		return "by-hash/SHA256/" + sha256hex
	}
	return dir + "/by-hash/SHA256/" + sha256hex
}

// packageHashBuildResult bundles buildPackageHashes' outputs. SPEC3
// §7.5.4: coverage_complete is *only* true when the suite layout is
// /dists/-shaped, the Release lists at least one Packages* member, and
// every directory containing such a member contributed at least one
// parseable variant.
type packageHashBuildResult struct {
	rows             []cache.PackageHash
	coverageComplete bool
}

// debPathDecl is the per-(deb path) running record buildPackageHashes
// keeps so it can detect conflicts across Packages variants. SHA256 is
// the SPEC2 conflict key; SPEC3 (§7.5.2) extends conflict detection to
// (Package, Architecture) so a Packages.xz that disagrees with
// Packages.gz on identity surfaces as adoption_parse_failed rather
// than silently overwriting in the dedup map.
type debPathDecl struct {
	sha256       string
	packageName  string
	architecture string
}

// buildPackageHashes walks every Packages-shaped Release member, parses
// it, and returns the dedup'd cache.PackageHash rows + the SPEC3 §7.5.4
// coverage_complete bit for the snapshot. Returns coverage = false for
// non-/dists/ layouts (with rows = nil); strict mode (§6.1) treats
// such hosts as fail-through, never fail-closed.
//
// allMembers is the full Release-declared set (used to compute the
// coverage denominator — every declared Packages-basename directory
// needs at least one parseable variant for coverage_complete). fetchedMembers
// is the subset whose blob is actually in the pool and therefore
// parseable; SPEC2 §7.5.2 4xx-skipped members are present in
// allMembers but absent from fetchedMembers. Computing pkgDirs from
// fetchedMembers would silently drop a 4xx-skipped Packages directory
// from the denominator and let coverage_complete = true vacuously hold
// for an incomplete snapshot.
func (a *Adopter) buildPackageHashes(suite SuiteRef, snapshotID int64,
	allMembers, fetchedMembers []ReleaseMember) (packageHashBuildResult, error) {
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if !ok {
		a.logger.Info("package_coverage_incomplete",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"reason", "non_dists_layout",
		)
		return packageHashBuildResult{rows: nil, coverageComplete: false}, nil
	}

	// SPEC3 §7.5.4: walk the *full* Release-declared set to identify
	// the directories that contain at least one Packages* member by
	// basename match (regardless of whether it's a variant we can
	// parse, and regardless of whether the upstream actually served
	// it). Each such directory needs at least one parseable variant
	// for coverage_complete to hold — a 4xx-skipped Packages
	// directory therefore drops coverage to false rather than
	// disappearing from the denominator.
	pkgDirs := make(map[string]bool)
	hasParseable := make(map[string]bool)
	for _, m := range allMembers {
		if !isPackagesBasename(path.Base(m.Path)) {
			continue
		}
		dir := path.Dir(m.Path)
		pkgDirs[dir] = true
		if isPackagesMember(m.Path) {
			// Existence in the index alone — the actual parse below
			// will populate hasParseable iff the body decodes cleanly.
			_ = dir
		}
	}

	// debPath -> running decl. Multiple Packages variants in the
	// same Release declare identical content, so deduplication is
	// load-bearing for the package_hash primary key.
	//
	// Iterates fetchedMembers only: a 4xx-skipped Packages member
	// has no blob in the pool, so readPackagesBlob would miss.
	dedup := make(map[string]debPathDecl)
	for _, m := range fetchedMembers {
		if !isPackagesMember(m.Path) {
			continue
		}
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
		if err != nil {
			return packageHashBuildResult{}, fmt.Errorf("%w: read %q: %v", ErrAdoptionParseFailed, m.Path, err)
		}
		refs, err := ParsePackages(body)
		if err != nil {
			return packageHashBuildResult{}, fmt.Errorf("%w: parse %q: %v", ErrAdoptionParseFailed, m.Path, err)
		}
		hasParseable[path.Dir(m.Path)] = true
		for _, ref := range refs {
			debPath := repoRoot + ref.Filename
			if existing, dup := dedup[debPath]; dup {
				if existing.sha256 != ref.SHA256 {
					return packageHashBuildResult{}, fmt.Errorf("%w: %q declared %s vs %s across Packages variants",
						ErrAdoptionParseFailed, debPath, existing.sha256, ref.SHA256)
				}
				// SPEC3 §7.5.2: detect identity conflicts
				// (e.g. Packages.gz says Architecture: amd64 but
				// Packages.xz says arm64 for the same Filename).
				// "" on either side is the absence-of-stanza
				// case from SPEC3 §7.5.2 — non-conflicting,
				// fill the gap.
				if existing.packageName != "" && ref.Package != "" && existing.packageName != ref.Package {
					return packageHashBuildResult{}, fmt.Errorf("%w: %q declared Package %q vs %q across Packages variants",
						ErrAdoptionParseFailed, debPath, existing.packageName, ref.Package)
				}
				if existing.architecture != "" && ref.Architecture != "" && existing.architecture != ref.Architecture {
					return packageHashBuildResult{}, fmt.Errorf("%w: %q declared Architecture %q vs %q across Packages variants",
						ErrAdoptionParseFailed, debPath, existing.architecture, ref.Architecture)
				}
				if existing.packageName == "" {
					existing.packageName = ref.Package
				}
				if existing.architecture == "" {
					existing.architecture = ref.Architecture
				}
				dedup[debPath] = existing
			} else {
				dedup[debPath] = debPathDecl{
					sha256:       ref.SHA256,
					packageName:  ref.Package,
					architecture: ref.Architecture,
				}
			}
			// SPEC3 §7.5.3 Stage 2 returns *every* matching path for a
			// hot (Package, Arch) pair — multiple debPaths sharing one
			// (Package, Arch) within a single snapshot is allowed.
			// They all flow through the hot-set list and get prefetched.
		}
	}

	rows := make([]cache.PackageHash, 0, len(dedup))
	for debPath, decl := range dedup {
		rows = append(rows, cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            debPath,
			DeclaredSHA256:  decl.sha256,
			SnapshotID:      snapshotID,
			PackageName:     decl.packageName,
			Architecture:    decl.architecture,
		})
	}

	// SPEC3 §7.5.4: coverage_complete classification.
	if len(pkgDirs) == 0 {
		// Release lists no Packages-basename members at all — a
		// source-only suite or other corner case. Set 0 rather than
		// vacuously 1.
		a.logger.Info("package_coverage_incomplete",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"reason", "no_packages_members",
		)
		return packageHashBuildResult{rows: rows, coverageComplete: false}, nil
	}
	missingDirs := make([]string, 0)
	for dir := range pkgDirs {
		if !hasParseable[dir] {
			missingDirs = append(missingDirs, dir)
		}
	}
	if len(missingDirs) > 0 {
		a.logger.Info("package_coverage_incomplete",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"reason", "unsupported_variants",
			"directories", missingDirs,
		)
		return packageHashBuildResult{rows: rows, coverageComplete: false}, nil
	}
	return packageHashBuildResult{rows: rows, coverageComplete: true}, nil
}

// sourcePathDecl is the per-(source artifact path) running record
// buildSourceHashes keeps so it can detect SPEC6_5 §11 H7 cross-variant
// disagreement (Sources.gz declaring SHA256 X for pkg.dsc while
// Sources.xz declares SHA256 Y for the same path).
type sourcePathDecl struct {
	sha256      string
	packageName string
}

// buildSourceHashes walks every Sources-shaped Release member, parses
// it via ParseSources, and returns dedup'd cache.PackageHash rows for
// the declared source artifacts (.dsc, source tarballs, debian patches).
// Each row carries Architecture="source" — the Debian convention for
// source-package rows that lets the SPEC6_5 §10.4 status surface
// surface them under their own pseudo-arch.
//
// The second return value is the count of Sources members successfully
// parsed (drives the SPEC6_5 §10.3 acu_adoption_sources_parsed_total{outcome=ok}
// counter; the parse_failed counter is incremented at the call site
// when err is non-nil).
//
// Cross-declaration disagreement (two stanzas/variants claiming
// different SHA256 for one source path) is NOT fatal here — unlike the
// Phase 3 buildPackageHashes posture for binary Packages. Source
// coverage is best-effort, so a conflict logs source_hash_conflict and
// DROPS the conflicting artifact (never guesses a hash; it falls back
// to trust-on-fetch serving) while the suite still adopts. The error
// return is retained for the signature but is always nil today.
// SPEC6_5 §11 H3/H4/H11 per-stanza skips happen inside ParseSources
// silently — operators see the per-Sources-file granularity at the
// source_parsed Debug log; per-stanza visibility is a future phase.
//
// Non-/dists/ layouts (where repoRootFromSuitePath returns false)
// return (nil, 0, nil): source rows are simply not populated. Phase 1
// trust-upstream still serves on hit; the cache just doesn't validate
// the bytes.
func (a *Adopter) buildSourceHashes(suite SuiteRef, snapshotID int64,
	fetchedMembers []ReleaseMember) ([]cache.PackageHash, int, error) {
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if !ok {
		return nil, 0, nil
	}

	dedup := make(map[string]sourcePathDecl)
	// conflicted holds source paths whose declarations disagreed on
	// SHA256 across stanzas/variants. Such artifacts are dropped from
	// the output rows (we never guess which hash is right), but the
	// suite still adopts — see the disagreement handling below.
	conflicted := make(map[string]bool)
	parsedCount := 0
	for _, m := range fetchedMembers {
		if !isSourcesMember(m.Path) {
			continue
		}
		// AIDEV-NOTE: SPEC6_5 §10.2 / §11 H3 H4 treat per-Sources-
		// file failures as non-fatal: emit source_parse_failed Warn
		// and skip the member's rows. Adoption proceeds with binary-
		// only hash coverage. This is intentionally LESS strict than
		// the Phase 3 Packages-parse posture (which fails closed) —
		// source-package coverage is opt-in / best-effort, while
		// binary coverage is on the strict-mode predicate's path. The
		// same best-effort posture extends to cross-declaration hash
		// disagreement below (log + drop, never fail the suite).
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
		if err != nil {
			a.logger.Warn("source_parse_failed",
				"suite_path", suite.SuitePath,
				"member_path", m.Path,
				"stage", "decompress",
				"error", err.Error(),
			)
			adoptionSourcesParsedTotal.Inc("parse_failed")
			continue
		}
		refs, stats, err := ParseSources(body)
		if err != nil {
			a.logger.Warn("source_parse_failed",
				"suite_path", suite.SuitePath,
				"member_path", m.Path,
				"stage", "parse",
				"error", err.Error(),
			)
			adoptionSourcesParsedTotal.Inc("parse_failed")
			continue
		}
		adoptionSourcesParsedTotal.Inc("ok")
		parsedCount++
		a.logger.Debug("source_parsed",
			"canonical_scheme", suite.CanonicalScheme,
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"member_path", m.Path,
			"stanza_count", stats.StanzaCount,
			"package_hash_rows", len(refs),
		)
		for _, ref := range refs {
			fullPath := repoRoot + ref.Path
			if existing, dup := dedup[fullPath]; dup {
				if existing.sha256 != ref.SHA256 {
					// Two declarations claim different bytes for one
					// source path — a corrupt/inconsistent index (e.g.
					// an upstream that published two different orig
					// tarballs under one filename, as packages.icinga.com
					// does for icinga-director_1.11.1.orig.tar.gz). Source
					// coverage is best-effort: log it, mark the path
					// conflicted so it is dropped from the output (we
					// never guess which hash is right), and keep adopting
					// — do NOT fail the whole suite. Binary Packages
					// disagreement stays fatal (buildPackageHashes), since
					// that feeds the strict .deb serving path.
					a.logger.Warn("source_hash_conflict",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"path", fullPath,
						"declared_a", existing.sha256,
						"declared_b", ref.SHA256,
					)
					conflicted[fullPath] = true
					continue
				}
				if existing.packageName != "" && ref.PackageName != "" && existing.packageName != ref.PackageName {
					// Same bytes (SHA256 agreed) but two package names
					// claim the path. Benign for serving (keyed on
					// path + hash), so keep the first-seen declaration;
					// just log the inconsistency for operators.
					a.logger.Warn("source_hash_conflict",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"path", fullPath,
						"package_a", existing.packageName,
						"package_b", ref.PackageName,
						"note", "package_name_only",
					)
					continue
				}
				if existing.packageName == "" {
					existing.packageName = ref.PackageName
					dedup[fullPath] = existing
				}
			} else {
				dedup[fullPath] = sourcePathDecl{
					sha256:      ref.SHA256,
					packageName: ref.PackageName,
				}
			}
		}
	}

	rows := make([]cache.PackageHash, 0, len(dedup))
	for fullPath, decl := range dedup {
		if conflicted[fullPath] {
			// SHA256 disagreement: dropped, not served with a guessed hash.
			continue
		}
		rows = append(rows, cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            fullPath,
			DeclaredSHA256:  decl.sha256,
			SnapshotID:      snapshotID,
			PackageName:     decl.packageName,
			Architecture:    "source",
		})
	}
	return rows, parsedCount, nil
}

// pdiffPathDecl is the per-(patch-file path) running record
// buildPdiffHashes keeps for cross-Index dedup. pdiff Index files
// rarely overlap (each binary-<arch>/Packages.diff/Index covers its
// own arch) but the dedup is cheap and defends against publication
// quirks where two Indexes might list the same patch path.
type pdiffPathDecl struct {
	sha256       string
	architecture string
}

// buildPdiffHashes walks every Packages.diff/Index and
// Sources.diff/Index member, parses it via ParsePdiffIndex, and
// returns dedup'd cache.PackageHash rows for the listed patch files.
// Each row's Architecture is derived from the Index path's
// `binary-<arch>/` segment (or "source" for source/Sources.diff/);
// PackageName is empty (pdiff patches have no package identity).
//
// Per-Index parse failures are tolerated SPEC6_5-style (Warn + skip
// member, adoption proceeds) — the same posture as buildSourceHashes.
// Cross-Index disagreement on a patch path's hash surfaces as
// ErrAdoptionParseFailed.
//
// Returns the rows, the count of Indexes successfully parsed (drives
// the SPEC6_5 §10.3 acu_adoption_pdiff_indexes_parsed_total metric),
// and an error.
//
// Non-/dists/ layouts return (nil, 0, nil) like buildSourceHashes.
func (a *Adopter) buildPdiffHashes(suite SuiteRef, snapshotID int64,
	fetchedMembers []ReleaseMember) ([]cache.PackageHash, int, error) {
	repoRoot, ok := repoRootFromSuitePath(suite.SuitePath)
	if !ok {
		return nil, 0, nil
	}

	dedup := make(map[string]pdiffPathDecl)
	parsedCount := 0
	for _, m := range fetchedMembers {
		if !isPdiffIndexMember(m.Path) {
			continue
		}
		// archFromPdiffIndexPath returns "" for paths that don't
		// match the binary-<arch>/ or source/ shape. Such Index
		// files (a hypothetical archive layout outside the Phase
		// 6.5 scope) have no arch label to assign — skip with a
		// Warn so operators see the unhandled shape, but don't
		// fail the adoption.
		arch, archOK := archFromPdiffIndexPath(m.Path)
		if !archOK {
			a.logger.Warn("pdiff_index_parse_failed",
				"suite_path", suite.SuitePath,
				"member_path", m.Path,
				"stage", "arch_extract",
				"error", "Index path does not contain a binary-<arch>/ or source/ segment",
			)
			adoptionPdiffIndexesParsedTotal.Inc("parse_failed")
			continue
		}
		body, err := a.readPackagesBlob(m.Path, m.SHA256)
		if err != nil {
			a.logger.Warn("pdiff_index_parse_failed",
				"suite_path", suite.SuitePath,
				"member_path", m.Path,
				"stage", "decompress",
				"error", err.Error(),
			)
			adoptionPdiffIndexesParsedTotal.Inc("parse_failed")
			continue
		}
		refs, err := ParsePdiffIndex(body)
		if err != nil {
			a.logger.Warn("pdiff_index_parse_failed",
				"suite_path", suite.SuitePath,
				"member_path", m.Path,
				"stage", "parse",
				"error", err.Error(),
			)
			adoptionPdiffIndexesParsedTotal.Inc("parse_failed")
			continue
		}
		adoptionPdiffIndexesParsedTotal.Inc("ok")
		parsedCount++
		a.logger.Debug("pdiff_index_parsed",
			"canonical_scheme", suite.CanonicalScheme,
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", snapshotID,
			"index_path", m.Path,
			"patch_count", len(refs),
		)
		// dirname-of-Index ends without a trailing slash; append "/"
		// before the patch filename. e.g. Index path
		// "main/binary-amd64/Packages.diff/Index" → dirname
		// "main/binary-amd64/Packages.diff" → patch path
		// "<repoRoot>main/binary-amd64/Packages.diff/<filename>".
		indexDir := path.Dir(m.Path)
		for _, ref := range refs {
			fullPath := repoRoot + indexDir + "/" + ref.Filename
			if existing, dup := dedup[fullPath]; dup {
				if existing.sha256 != ref.SHA256 {
					return nil, parsedCount, fmt.Errorf("%w: %q declared %s vs %s across pdiff Indexes",
						ErrAdoptionParseFailed, fullPath, existing.sha256, ref.SHA256)
				}
				continue
			}
			dedup[fullPath] = pdiffPathDecl{
				sha256:       ref.SHA256,
				architecture: arch,
			}
		}
	}

	rows := make([]cache.PackageHash, 0, len(dedup))
	for fullPath, decl := range dedup {
		rows = append(rows, cache.PackageHash{
			CanonicalScheme: suite.CanonicalScheme,
			CanonicalHost:   suite.CanonicalHost,
			Path:            fullPath,
			DeclaredSHA256:  decl.sha256,
			SnapshotID:      snapshotID,
			PackageName:     "",
			Architecture:    decl.architecture,
		})
	}
	return rows, parsedCount, nil
}

// isPackagesBasename reports whether base is *any* `Packages` variant
// (including unsupported compressions). Used by SPEC3 §7.5.4 coverage
// detection to identify which directories are *expected* to contribute
// a parseable variant; isPackagesMember would miss a Packages.bz2
// directory and let coverage_complete vacuously stay true.
func isPackagesBasename(base string) bool {
	if base == "Packages" {
		return true
	}
	const prefix = "Packages."
	return strings.HasPrefix(base, prefix) && len(base) > len(prefix)
}

// isPackagesMember reports whether m's relative path is a Packages
// file we can parse. Phase 3 adds Packages.xz alongside Phase 2's
// plain Packages and Packages.gz; Packages.bz2 / .lz4 / .zst remain
// unsupported and surface as `package_coverage_incomplete` (SPEC3
// §7.5.4) when they're the only variant in a directory.
func isPackagesMember(p string) bool {
	base := path.Base(p)
	switch base {
	case "Packages", "Packages.gz", "Packages.xz":
		return true
	}
	return false
}

// isSourcesMember reports whether p is a Sources index member shape
// SPEC6_5 §7.1 dispatches to ParseSources for. The same compression
// matrix as isPackagesMember (plain / .gz / .xz) — Sources.bz2 etc.
// remain unsupported (and almost never appear in real Debian repos).
func isSourcesMember(p string) bool {
	base := path.Base(p)
	switch base {
	case "Sources", "Sources.gz", "Sources.xz":
		return true
	}
	return false
}

// isPdiffIndexMember reports whether p is a Packages.diff/Index or
// Sources.diff/Index member shape SPEC6_5 §7.3 dispatches to
// ParsePdiffIndex for. Index files are uncompressed by convention —
// no .gz / .xz variant exists in real Debian/Ubuntu archives.
func isPdiffIndexMember(p string) bool {
	return strings.HasSuffix(p, "/Packages.diff/Index") ||
		strings.HasSuffix(p, "/Sources.diff/Index")
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
	defer func() { _ = f.Close() }()

	if strings.HasSuffix(memberPath, ".gz") {
		gr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = gr.Close() }()
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
			return nil, fmt.Errorf("decompressed Packages.gz size exceeds %d-byte cap (bomb defense)",
				maxDecompressedPackagesBytes)
		}
		return body, nil
	}
	if strings.HasSuffix(memberPath, ".xz") {
		// Phase 3: ulikunitz/xz pure-Go reader. Same size-cap posture
		// as the gzip path — a hostile signed-but-huge upstream cannot
		// inflate past maxDecompressedPackagesBytes.
		xr, err := xz.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("xz: %w", err)
		}
		limited := io.LimitReader(xr, maxDecompressedPackagesBytes+1)
		body, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("decompress: %w", err)
		}
		if int64(len(body)) > maxDecompressedPackagesBytes {
			return nil, fmt.Errorf("decompressed Packages.xz size exceeds %d-byte cap (bomb defense)",
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
		return nil, fmt.Errorf("plain Packages exceeds %d-byte cap", maxDecompressedPackagesBytes)
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
