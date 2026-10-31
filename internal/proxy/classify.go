package proxy

import (
	"path"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// metadataPrefixes lists filename prefixes that classify a path as
// suite/index metadata. SPEC §4.5: Packages*, Sources*, Contents-*,
// Translation-*, Components-*, icons-*.
var metadataPrefixes = []string{
	"Packages",
	"Sources",
	"Contents-",
	"Translation-",
	"Components-",
	"icons-",
}

// poolArtifactSuffixes are the extensions of Debian pool artifacts: a
// basename ending in one is never metadata (isPoolArtifact, ruling PG-2).
var poolArtifactSuffixes = []string{".deb", ".udeb", ".ddeb", ".dsc", ".buildinfo", ".changes", ".diff.gz"}

// isPoolArtifact reports whether base names a Debian pool artifact: one of
// poolArtifactSuffixes, or a source tarball — a "_" and a ".tar." component
// (foo_1.0.orig.tar.xz, foo_1.0-1.debian.tar.xz). icons-48x48.tar.gz (no
// "_") and Translation-pt_BR.xz (no ".tar.") stay metadata.
func isPoolArtifact(base string) bool {
	for _, s := range poolArtifactSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return strings.Contains(base, "_") && strings.Contains(base, ".tar.")
}

// metadataExact lists exact filenames that classify a path as metadata.
var metadataExact = map[string]struct{}{
	"InRelease":   {},
	"Release":     {},
	"Release.gpg": {},
}

// IsMetadata reports whether p is an apt repository metadata path. The
// classification drives policy: metadata is subject to freshness checks
// (SPEC §7); anything else is an immutable blob whose content-addressed
// store entry never needs revalidation.
//
// Special cases beyond simple basename matching:
//   - any path containing "/by-hash/" — content-addressed metadata
//   - "*.diff/Index" — pdiff index living inside a .diff/ directory
func IsMetadata(p string) bool {
	if p == "" {
		return false
	}
	// AIDEV-NOTE: by-hash matches anywhere in the path because by-hash
	// directories appear under each component, e.g.
	// /ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/abc...
	if strings.Contains(p, "/by-hash/") {
		return true
	}

	base := path.Base(p)
	if _, ok := metadataExact[base]; ok {
		return true
	}
	if base == "Index" {
		// .diff/Index lives inside a directory whose name ends in ".diff".
		dir := path.Base(path.Dir(p))
		if strings.HasSuffix(dir, ".diff") {
			return true
		}
	}
	// AIDEV-NOTE: ruling PG-2 (phase G). The prefixes below collide with
	// real package names — a package icons-foo puts
	// pool/…/icons-foo_1.0_all.deb under "icons-", and a flat repository
	// may publish Packages_1.0_all.deb — and a pool artifact classified as
	// metadata is freshness-revalidated, never hash-checked and never
	// refused by strict mode. So an artifact's basename is never metadata,
	// whatever its prefix; by-hash and .diff/Index keep their rules above.
	// Residual: a url_path row an earlier binary stored for such a .deb
	// keeps is_metadata = 1 until it is refetched; serving and strict mode
	// read the request's classification (this function), so only the
	// admin page's hot paths (their Kind column and JSON is_metadata)
	// show the stale flag.
	if isPoolArtifact(base) {
		return false
	}
	for _, prefix := range metadataPrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// SuitePath returns the canonical suite path for p (see
// aptpath.SuitePath, which owns the grammar). Kept as a proxy-level name
// because callers throughout the package use it.
func SuitePath(p string) string { return aptpath.SuitePath(p) }
