package proxy

import (
	"path"
	"regexp"
	"strings"
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
	for _, prefix := range metadataPrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

// suiteRegex matches the SPEC §4.4 suite identification pattern:
//
//	^/(?:(.+)/)?dists/([^/]+)(?:/.*)?$
//
// Captures the optional repo path before "dists/" and the suite codename
// after. repo_path may be empty for upstreams that serve /dists/ off the
// host root (apt.corretto.aws, repo.charm.sh).
var suiteRegex = regexp.MustCompile(`^/(?:(.+)/)?dists/([^/]+)(?:/.*)?$`)

// flatSuiteFileRegex matches the basenames that sit at the root of a flat
// repository (`deb <uri>/ /`, no dists/ hierarchy): its signed Release
// files and its Packages/Sources indexes, in any compression codec.
// Deliberately narrower than IsMetadata — Translation-*, Contents-* and
// friends are not given a suite identity when they appear outside dists/,
// and a file that merely starts with "Packages" (a .deb named
// Packages_1.0_all.deb) does not match.
var flatSuiteFileRegex = regexp.MustCompile(
	`^(?:InRelease|Release|Release\.gpg|(?:Packages|Sources)(?:\.[a-z0-9]+)?)$`)

// SuitePath returns the canonical suite path for p (the longest prefix
// that identifies a single suite for freshness purposes), or "" when p
// belongs to no suite.
//
// A suite is either a /dists/<suite> hierarchy, or the directory of a
// flat repository: a Release/InRelease/Packages/Sources file outside
// dists/ identifies the directory it sits in. Flat repositories have no
// codename, so their InRelease lives at <dir>/InRelease and every
// Release member is relative to <dir> — the same shape the freshness
// check and adopter already use for <repo>/dists/<suite>. Without this,
// a flat repository's metadata was served from the Phase 1 url_path cache
// forever: no freshness check ever ran, so a changed upstream InRelease
// was never observed.
//
// A flat repository at the host root (/InRelease) is not given a suite:
// the suite path would be "/", which cannot be joined with the
// "/InRelease" suffix the freshness check appends.
//
// Examples:
//
//	/ubuntu/dists/noble/InRelease       -> /ubuntu/dists/noble
//	/dists/stable/InRelease             -> /dists/stable
//	/core:/stable:/v1.34/deb/InRelease  -> /core:/stable:/v1.34/deb (flat)
//	/core:/stable:/v1.34/deb/Packages.gz -> /core:/stable:/v1.34/deb (flat)
//	/ubuntu/pool/main/h/hello/...       -> "" (a blob, not under a suite)
func SuitePath(p string) string {
	m := suiteRegex.FindStringSubmatch(p)
	if m == nil {
		return flatSuitePath(p)
	}
	repo := m[1]
	suite := m[2]
	if repo == "" {
		return "/dists/" + suite
	}
	return "/" + repo + "/dists/" + suite
}

// flatSuitePath returns the flat-repository suite path for p (its
// directory) when p's basename is a flat repository root file, else "".
// Only reached for paths outside any dists/ hierarchy.
//
// AIDEV-NOTE: the suite is the LITERAL directory, never a cleaned one.
// Request paths are not normalised, and the handler maps a request onto
// its snapshot by stripping suitePath+"/" from req.Path (suiteRelativePath)
// — so a suite derived with path.Dir would give "/repo/./Packages.gz" the
// suite "/repo" and an authoritative "not in snapshot" 404 once "/repo" is
// adopted. apt sends exactly that shape: a `deb <uri> ./` source requests
// <uri>/./InRelease, and a `deb <uri>/ /` source falls back to it. So a
// single trailing "/." is kept as its own suite ("/repo/."), consistent
// with every path that client sends; any other non-canonical directory
// (//, /./ elsewhere, .., percent-encoding) gets no suite, as before flat
// suites existed.
func flatSuitePath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	i := strings.LastIndexByte(p, '/')
	if !flatSuiteFileRegex.MatchString(p[i+1:]) {
		return ""
	}
	dir := p[:i]
	canonical := strings.TrimSuffix(dir, "/.")
	if canonical == "" || strings.ContainsRune(dir, '%') || path.Clean(canonical) != canonical {
		return ""
	}
	return dir
}

// FlatSuiteDir reports whether suitePath is a flat repository's suite (as
// returned by SuitePath for a path outside dists/) and, if so, the
// directory its packages are addressed under: the suite path itself, or
// its parent for a `deb <uri> ./` suite ("/repo/." -> "/repo"). A
// /dists/<suite> path returns ("", false).
//
// apt fetches a flat repository's .debs at <uri>/<Filename>, and the
// Filename fields of every flat repository checked (pkgs.k8s.io, OBS) are
// relative paths such as amd64/kubelet_….deb, so they land under this
// directory. Strict mode uses it to scope a flat snapshot to its own
// .debs; keep it in step with flatSuitePath.
func FlatSuiteDir(suitePath string) (string, bool) {
	if suitePath == "" || suiteRegex.MatchString(suitePath) {
		return "", false
	}
	return strings.TrimSuffix(suitePath, "/."), true
}
