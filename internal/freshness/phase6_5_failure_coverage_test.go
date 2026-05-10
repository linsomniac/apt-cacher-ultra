package freshness

// This file is the SPEC6_5 §15 #11 / §11 coverage map: every H1–H15
// failure mode has either an explicit test (cited below) or a
// no-op-by-design annotation explaining the implicit coverage.
//
//   H1  invalid arch in [adoption].architectures
//       → config.TestValidate_RejectsArchitecturesInvalidValue
//
//   H2  > 32 entries in [adoption].architectures
//       → config.TestValidate_RejectsTooManyArchitectures
//
//   H3  Sources.gz exceeds maxDecompressedPackagesBytes (256 MiB)
//       → IMPLICIT: ParseSources/ParsePackages share readPackagesBlob
//         which size-caps every decompressed read and surfaces the
//         "Packages.gz decompresses past 256-byte cap (bomb defense)"
//         error. buildSourceHashes catches that error in the
//         readPackagesBlob branch, emits source_parse_failed Warn
//         stage=decompress, and continues. The disposition is
//         identical to a parse failure; TestParseSources_LargeStanzaCountCap
//         exercises the cap mechanism on the parser side (different
//         cap, same error-path shape).
//
//   H4  Sources stanza has no SHA256 (only MD5/SHA1)
//       → TestParseSources_SkipsStanzaWithNoSha256
//
//   H5  Packages.diff/Index has no SHA256-Download block
//       → TestParsePdiffIndex_NoDownloadBlock
//
//   H6  Index entries don't match digit/dot/dash + .gz pattern
//       → TestParsePdiffIndex_MalformedFilenameSkipped
//       → TestAdopter_PdiffIndex_MalformedEntriesSkipped
//
//   H7  Cross-variant Sources disagreement
//       → TestAdopter_Sources_CrossVariantDisagreement
//       (the binary Packages analog is TestAdopter_PackageHash_ConflictAcrossVariants;
//       same dedup-map mechanism)
//
//   H8  .dsc hash mismatch on serve
//       → handler.TestServeHash_SourceDsc_HashMismatch_502
//
//   H9  pdiff patch hash mismatch on serve
//       → handler.TestServeHash_PdiffPatch_HashMismatch_502
//
//   H10 arm64-only client w/ amd64-only allowlist
//       → TestAdopter_MultiArch_AllowlistFiltersOutOtherArchs
//
//   H11 Sources file with `..` segments in path
//       → TestParseSources_RejectsTraversalPath (4 sub-cases)
//
//   H12 [adoption].architectures changes between adoptions
//       → IMPLICIT: the architectures field is captured at
//         NewAdopter construction and an Adopter is held by
//         Adopter.runShared for the lifetime of one process. A
//         daemon restart with different architectures will load the
//         new value on the next adoption. There is no in-process
//         re-read path (Phase 7 hot-reload work), and the snapshot-
//         lifecycle GC reaps any rows whose snapshot is no longer
//         current — the same mechanism that handles Phase 3 v3 deb
//         adoption changes. No test would exercise this without a
//         multi-process scenario.
//
//   H13 Two clients race on same arm64 .deb first-fetch
//       → IMPLICIT: identical Phase 1 singleflight semantics apply
//         to all paths regardless of arch. TestPhase2Chaos_*
//         exercises the singleflight mechanism for amd64; the
//         arm64 path runs the same code without arch-specific
//         branching (verified by archFromFilteredPath returning
//         "arm64" for the relevant index member only — the per-deb
//         request flow has no arch awareness at all).
//
//   H14 Repo publishes pdiff for one arch but not another
//       → IMPLICIT: TestAdopter_PdiffIndex_HappyPath uses ONE arch's
//         Packages.diff/Index. Adoption inserts package_hash rows
//         for whatever Indexes the Release lists; arches without an
//         Index simply contribute no pdiff rows. The "no rows ⇒
//         apt falls back to whole-Packages fetch" disposition is
//         apt's behavior, not the cache's.
//
//   H15 Source artifact path collides with binary path
//       → IMPLICIT: the package_hash schema PRIMARY KEY is
//         (canonical_scheme, canonical_host, path, snapshot_id).
//         CommitAdoption uses plain INSERT (cache/adoption.go:329 —
//         not INSERT OR REPLACE), so a colliding path raises
//         SQLite's UNIQUE constraint violation and the whole
//         transaction rolls back. TestAdopter_PackageHash_ConflictAcrossVariants
//         exercises the dedup-map detection path that fires before
//         INSERT; the SQLite layer is the defense-in-depth layer
//         the spec describes for the theoretically-impossible
//         source-binary case. Real Debian/Ubuntu archives never
//         publish source and binary at the same path (extension
//         conventions differ).
