package cache

// URLPath mirrors the url_path table (SPEC §4.3). Pointer fields hold
// SQL NULLs; non-nullable columns are plain values.
type URLPath struct {
	CanonicalScheme string  // "http" | "https"
	CanonicalHost   string  // post-Remap canonical hostname
	Path            string  // request path including leading "/"
	BlobHash        *string // sha256 hex of cached blob, nil if not yet cached
	UpstreamURL     string  // the real URL we fetch from
	IsMetadata      bool    // index/Release/InRelease/etc. vs. immutable blob
	LastRequestedAt *int64  // unix epoch seconds
	RequestCount    int64
	LastFetchedAt   *int64
	UpstreamETag    *string // validator captured for resumable If-Range
	UpstreamLastMod *string // validator fallback when ETag absent
}

// Blob mirrors the blob table.
type Blob struct {
	Hash      string // sha256 hex (lowercase, 64 chars)
	Size      int64
	CreatedAt int64 // unix epoch seconds
	RefCount  int64 // populated for Phase 4 GC
}

// SuiteFreshness mirrors the suite_freshness table. Identifies an apt
// suite (e.g. noble) on a canonical (scheme, host) and tracks when we last
// checked InRelease, what validators we have, and whether upstream has
// rolled forward. Phase 1 records change detection; Phase 2 adopts via
// CurrentSnapshotID.
type SuiteFreshness struct {
	CanonicalScheme       string
	CanonicalHost         string
	SuitePath             string // e.g. "/ubuntu/dists/noble"
	LastCheckAt           *int64 // unix epoch seconds
	LastSuccessAt         *int64
	InReleaseETag         *string
	InReleaseLastMod      *string
	InReleaseChangeSeenAt *int64 // diagnostic; non-nil = upstream is ahead
	CurrentSnapshotID     *int64 // SPEC2 §4.3.1: NULL until first adoption
}

// SnapshotCandidate is the input to InsertCandidateSnapshot — a fresh
// suite_snapshot row with adopted_at = NULL. Exactly one of (InReleaseHash)
// or (ReleaseHash + ReleaseGPGHash) must be set; the schema CHECK enforces
// this and rejects all-NULL or both-mode rows. SPEC2 §4.3.1, §7.5.
//
// PackageCoverageComplete (SPEC3 §7.5.4) is computed by the adopter
// from the Release-listed Packages* members and persisted on the
// candidate row. CommitAdoption does not flip it; the candidate's
// recorded coverage is what strict mode reads on hit-path checks.
type SnapshotCandidate struct {
	CanonicalScheme         string
	CanonicalHost           string
	SuitePath               string
	InReleaseHash           *string // sha256 hex of clearsigned InRelease bytes
	InReleaseETag           *string
	InReleaseLastMod        *string
	ReleaseHash             *string // sha256 hex of detached Release bytes
	ReleaseGPGHash          *string // sha256 hex of Release.gpg
	PackageCoverageComplete bool
}

// SnapshotMember mirrors the snapshot_member table — one (snapshot_id, path)
// row pointing at the blob the snapshot vouches for under that suite-relative
// path. SPEC2 §4.3.1, §7.5 step 5/6/7.
type SnapshotMember struct {
	SnapshotID     int64
	Path           string // suite-relative, e.g. "main/binary-amd64/Packages"
	BlobHash       string // sha256 hex of the blob in pool/
	DeclaredSHA256 string // the sha256 the Release file declared for this path
}

// PackageHash mirrors the package_hash table — one (host, .deb path,
// snapshot_id) row asserting that the .deb at the URL must hash to
// DeclaredSHA256 under this snapshot. SPEC2 §4.3.1, §7.5 step 8.
//
// Phase 3 (SPEC3 §4.3.1) adds PackageName and Architecture: the binary
// `Package:` and `Architecture:` from the Packages stanza. The hot-set
// matching across snapshot transitions (SPEC3 §7.5.3) keys on these
// fields. Stanzas that didn't declare both still produce a row (with
// the missing field as ""), so hash validation is unaffected; the
// hot-set query excludes empty values explicitly.
type PackageHash struct {
	CanonicalScheme string
	CanonicalHost   string
	Path            string // .deb path matching url_path.path, e.g. "/ubuntu/pool/main/f/foo.deb"
	DeclaredSHA256  string
	SnapshotID      int64
	PackageName     string
	Architecture    string
}

// SuiteSnapshot mirrors the suite_snapshot table. Read-only model used
// by request-path lookups and adoption diagnostics. SPEC2 §4.3.1,
// SPEC3 §4.3.1 (PackageCoverageComplete).
type SuiteSnapshot struct {
	SnapshotID       int64
	CanonicalScheme  string
	CanonicalHost    string
	SuitePath        string
	InReleaseHash    *string
	InReleaseETag    *string
	InReleaseLastMod *string
	ReleaseHash      *string
	ReleaseGPGHash   *string
	CreatedAt        int64
	AdoptedAt        *int64 // NULL while candidate; set on flip

	// PackageCoverageComplete is the SPEC3 §7.5.4 per-snapshot proof
	// that strict mode (§6.1) keys on. True iff every Release-listed
	// directory containing a Packages* member contributed at least one
	// parseable variant to package_hash. Pre-v3 rows default to false.
	PackageCoverageComplete bool
}

// SnapshotCoverage is a compact lookup result for the SPEC3 §6.1
// strict-mode predicate: per current snapshot on a given (scheme, host),
// the snapshot id and its package_coverage_complete bit. The handler
// uses this to decide whether to refuse unvouched .debs (every snapshot
// must be coverage_complete = 1) or fall through to trust-upstream and
// log unvouched_deb_passthrough_no_coverage.
type SnapshotCoverage struct {
	SnapshotID              int64
	PackageCoverageComplete bool
}

// CacheStats is the SPEC5 §10.5 cache.* numeric block: blob count
// + total bytes + url_path count + zero-refcount backlog. Sourced
// by both the §9.7.3 status page and the §9.7.6 refresher
// goroutine (which uses the same numbers to populate the
// acu_blobs_db_count / acu_blobs_db_total_bytes /
// acu_url_paths_tracked / acu_blobs_zero_refcount_backlog gauges).
//
// The two numeric fields that source from /proc-style filesystem
// data — pool_disk_bytes — live elsewhere; this struct is purely
// the database-derivable fields.
type CacheStats struct {
	BlobCount           int64
	TotalBytes          int64
	URLPathCount        int64
	ZeroRefcountBacklog int64
}

// RepoCoverage is the SPEC6_5 §2.4 repo_coverage status-page payload:
// counts and architecture observations across every current snapshot's
// package_hash + snapshot_member rows.
//
// ArchitecturesSeen is the union of distinct architecture column
// values across current snapshots' package_hash rows (excluding
// empty-string arch). Includes the pseudo-arch "source" when source
// adoption is active.
//
// SnapshotsWithSources counts current snapshots having ≥ 1
// package_hash row with architecture = "source".
//
// SnapshotsWithPdiff counts current snapshots whose snapshot_member
// table contains ≥ 1 row whose path ends in "Packages.diff/Index" or
// "Sources.diff/Index" — populated regardless of whether any individual
// patch file was ever fetched (the Index member's adoption is the
// proof of pdiff coverage, per SPEC6_5 §2.4).
//
// PackageHashRowsBy{Binary,Source,Pdiff,Total} are the per-kind row
// counts: source rows are arch="source" with non-pdiff paths; pdiff
// rows live under Packages.diff/ or Sources.diff/ (compressed patch
// files); binary rows are everything else with a non-empty arch.
type RepoCoverage struct {
	ArchitecturesSeen        []string
	SnapshotsWithSources     int64
	SnapshotsWithPdiff       int64
	PackageHashRowsBinary    int64
	PackageHashRowsSource    int64
	PackageHashRowsPdiff     int64
	PackageHashRowsTotal     int64
}

// SuiteStats is the SPEC5 §9.7.6 suite/snapshot count block: the
// three counters the refresher goroutine derives the
// acu_suites_tracked / acu_snapshots_current / acu_snapshots_displaced
// gauges from. AdoptedTotal is "suite_snapshot rows whose
// adopted_at IS NOT NULL"; the displaced gauge is then
// AdoptedTotal - WithCurrentSnapshot. Sourced via GetSuiteStats.
type SuiteStats struct {
	Tracked             int64 // suite_freshness rows
	WithCurrentSnapshot int64 // suite_freshness rows with current_snapshot_id IS NOT NULL
	AdoptedTotal        int64 // suite_snapshot rows with adopted_at IS NOT NULL
}

// HotURLPath is one row of the SPEC5 §10.5 hot_url_paths status-page
// section: a url_path row that has been recently requested. Sorted
// by (request_count DESC, last_requested_at DESC).
type HotURLPath struct {
	Host                string
	Path                string
	IsMetadata          bool
	RequestCount        int64
	LastRequestedAtUnix int64 // last_requested_at; never NULL in this slice
}

// SuiteWithAdoption embeds SuiteFreshness and adds the
// suite_snapshot.adopted_at corresponding to current_snapshot_id.
// CurrentAdoptedAt is non-nil exactly when the LEFT JOIN found a
// matching suite_snapshot row whose adopted_at is non-NULL — i.e.
// the suite has a live current adoption. Returned by
// ListSuitesWithAdoption (SPEC5 §9.7.8) for the status-page render.
type SuiteWithAdoption struct {
	SuiteFreshness
	CurrentAdoptedAt *int64 // unix seconds; nil when current_snapshot_id is NULL or no matching suite_snapshot row exists
}

// HotSetEntry is one (.deb path, declared sha256) tuple that the SPEC3
// §7.5.3 hot-set computation identified as worth proactively warming
// before the candidate snapshot's atomic flip. Path is the canonical
// .deb URL path (leading "/"); DeclaredSHA256 is the value the
// candidate snapshot's package_hash row asserts. The freshness adopter
// composes the upstream URL textually from the suite's canonical
// scheme/host plus Path.
type HotSetEntry struct {
	Path           string
	DeclaredSHA256 string
}
