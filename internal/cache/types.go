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
// rolled forward. Phase 1 records change detection; Phase 2 will adopt.
type SuiteFreshness struct {
	CanonicalScheme       string
	CanonicalHost         string
	SuitePath             string  // e.g. "/ubuntu/dists/noble"
	LastCheckAt           *int64  // unix epoch seconds
	LastSuccessAt         *int64
	InReleaseETag         *string
	InReleaseLastMod      *string
	InReleaseChangeSeenAt *int64 // diagnostic; non-nil = upstream is ahead
}
