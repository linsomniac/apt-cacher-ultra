// Package aptpath holds the path grammar shared by the proxy, cache,
// freshness and handler packages: suite identity, canonicality, and the
// apt-compatible upstream URL encoding. It depends only on the standard
// library so the storage layer can use it without importing internal/proxy.
package aptpath

import "strings"

// ForbiddenPathBytes are decoded bytes that disqualify a path from suite
// identity. A decoded '#' or '?' means the client sent %23/%3F, '%' means
// it sent %25, and '\' is never part of an apt path (spec C2, S-3).
const ForbiddenPathBytes = "#?%\\"

// SuitePath returns the canonical suite path for decoded request path p,
// or "" when p belongs to no suite.
//
// dists/ grammar (spec C2): /<repo>/dists/<codename>/<member...> with a
// non-empty member; the suite is /<repo>/dists/<codename>. The LAST
// "dists" segment that is followed by a codename and at least one member
// segment wins, matching the old greedy regex.
//
// AIDEV-NOTE: canonicality is strict on purpose. Every segment must be
// non-empty and not "." or "..", and ForbiddenPathBytes / control bytes
// are refused. A path that fails gets NO suite (Phase 1 url_path serving
// continues); it never gets a "cleaned" suite, because the handler maps a
// request onto its snapshot by stripping suite+"/" from the raw path.
// Phase G adds the flat grammar here (one trailing "/." allowed there).
func SuitePath(p string) string {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) {
		return ""
	}
	segs := strings.Split(p[1:], "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return ""
		}
	}
	for i := len(segs) - 3; i >= 0; i-- {
		if segs[i] == "dists" {
			return "/" + strings.Join(segs[:i+2], "/")
		}
	}
	return ""
}

// IsCanonicalSuitePath reports whether s is a suite path SuitePath could
// have produced. Used to recognise legacy suite rows that the tightened
// grammar can never reach again (phase F reaps them). A codename equal to
// an anchor name ("/foo/dists/InRelease") is canonical: the grammar allows
// it, and idle reaping handles such rows.
func IsCanonicalSuitePath(s string) bool {
	return s != "" && SuitePath(s+"/InRelease") == s
}

func cleanBytes(p string) bool {
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(ForbiddenPathBytes, c) >= 0 {
			return false
		}
	}
	return true
}
