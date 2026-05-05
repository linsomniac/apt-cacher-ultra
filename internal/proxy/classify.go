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

// SuitePath returns the canonical suite path for p (the longest prefix
// that identifies a single suite for freshness purposes), or "" when p
// is not under any /dists/<suite> hierarchy.
//
// Examples:
//
//	/ubuntu/dists/noble/InRelease       -> /ubuntu/dists/noble
//	/dists/stable/InRelease             -> /dists/stable
//	/ubuntu/pool/main/h/hello/...       -> "" (a blob, not under a suite)
func SuitePath(p string) string {
	m := suiteRegex.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	repo := m[1]
	suite := m[2]
	if repo == "" {
		return "/dists/" + suite
	}
	return "/" + repo + "/dists/" + suite
}
