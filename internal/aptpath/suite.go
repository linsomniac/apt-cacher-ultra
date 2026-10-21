// Package aptpath holds the path grammar shared by the proxy, cache,
// freshness and handler packages: suite identity, canonicality, and the
// apt-compatible upstream URL encoding. It depends only on the standard
// library so the storage layer can use it without importing internal/proxy.
package aptpath

import (
	"regexp"
	"strings"
)

// ForbiddenPathBytes are decoded bytes that disqualify a path from suite
// identity. A decoded '#' or '?' means the client sent %23/%3F, '%' means
// it sent %25, and '\' is never part of an apt path (spec C2, S-3).
const ForbiddenPathBytes = "#?%\\"

// SuitePath returns the canonical suite path for decoded request path p,
// or "" when p belongs to no suite (spec C2). Two grammars:
//
// dists/ grammar: /<repo>/dists/<codename>/<member...> with a non-empty
// member; the suite is /<repo>/dists/<codename>. The LAST "dists" segment
// that is followed by a codename and at least one member segment wins
// ("/a/dists/b/dists/c/InRelease" is the suite "/a/dists/b/dists/c").
//
// Flat grammar: a path with no "dists" segment whose tail is one flat
// member (flatMemberSegments: an anchor, a root Packages*/Sources*/Contents-*
// index, a by-hash name or a pdiff Index) belongs to the LITERAL directory
// D before that member. D may end in exactly one "/." (the `deb <uri> ./`
// form: "/repo/./InRelease" is the suite "/repo/."), and is never the
// host root ("/", "/.").
//
// AIDEV-NOTE: canonicality is strict on purpose. Every segment must be
// non-empty and not "." or ".." — the flat grammar's one exception is a
// single final "." segment of D — and ForbiddenPathBytes / control bytes
// are refused. A path that fails gets NO suite (Phase 1 url_path serving
// continues); it never gets a "cleaned" suite, because the handler maps a
// request onto its snapshot by stripping suite+"/" from req.Path, the
// same string SuitePath received (deferred minor A-5): "/repo/./x" must
// stay under "/repo/.", never "/repo". A "dists" segment anywhere makes a
// path dists-only: such a path is never flat (spec C2). ClassifySuiteFile
// (sniff.go) relies on this function as its precondition; widen or narrow
// the grammars together with it and with IsFlatSuitePath.
func SuitePath(p string) string {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) {
		return ""
	}
	segs := strings.Split(p[1:], "/")
	if hasDistsSegment(p) {
		return distsSuitePath(segs)
	}
	return flatSuitePath(segs)
}

// distsSuitePath is the dists/ grammar over p's segments.
func distsSuitePath(segs []string) string {
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

// flatSuitePath is the flat grammar over the segments of a path with no
// "dists" segment: the directory before the path's flat member, or "".
func flatSuitePath(segs []string) string {
	n := flatMemberSegments(segs)
	if n == 0 || n >= len(segs) {
		return "" // no flat member, or the member alone: the host root
	}
	dir := segs[:len(segs)-n]
	twin := dir[len(dir)-1] == "."
	if twin {
		dir = dir[:len(dir)-1]
	}
	if len(dir) == 0 {
		return "" // "/./<member>": the host root again
	}
	for _, s := range dir {
		if s == "" || s == "." || s == ".." {
			return ""
		}
	}
	d := "/" + strings.Join(dir, "/")
	if twin {
		d += "/."
	}
	return d
}

// Spec C2's flat member grammar, one regexp per member shape.
var (
	flatMemberRE   = regexp.MustCompile(`^(?:InRelease|Release|Release\.gpg|(?:Packages|Sources)(?:\.(?:gz|xz|bz2|lzma|lz4|zst))?|Contents-[a-z0-9-]+(?:\.(?:gz|xz|bz2|lzma|lz4|zst))?)$`)
	flatPdiffDirRE = regexp.MustCompile(`^(?:Packages|Sources)\.diff$`)
	hex64RE        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex128RE       = regexp.MustCompile(`^[0-9a-f]{128}$`)
)

// flatIndexExts are the compression extensions of spec C2's flat member
// grammar (flatMemberRE) and of the flat index sniff (flatIndexMagic).
var flatIndexExts = []string{".gz", ".xz", ".bz2", ".lzma", ".lz4", ".zst"}

// FlatIndexNames returns spec C2's root index names of a flat suite —
// Packages and Sources, plain or with one of the six compression
// extensions — 14 names, in a fixed order, as a new slice.
//
// AIDEV-NOTE: the one list (the Task 9 review's Minor 4, the Task 12
// review's Minor 1). The cache probes url_path's key with exactly these
// names (ListSuiteMetadataRows for Dispatch 3 and the anchorless refresh,
// ListUnadoptedFlatIndexRows for the reaper's junk evidence): a name
// missing here is a flat index those reads never see — the reaper would
// then judge an unsigned suite with junk anchors on its anchors alone.
// TestFlatIndexNames keeps it equal to flatMemberRE, ClassifySuiteFile and
// flatIndexMagic.
func FlatIndexNames() []string {
	names := make([]string, 0, 2*(1+len(flatIndexExts)))
	for _, stem := range []string{"Packages", "Sources"} {
		names = append(names, stem)
		for _, ext := range flatIndexExts {
			names = append(names, stem+ext)
		}
	}
	return names
}

// flatMemberSegments returns how many trailing segments of segs form a
// spec C2 flat member — 1 (an anchor or root index), 2 (a pdiff Index) or
// 3 (a by-hash name) — or 0 when the tail is none of them.
func flatMemberSegments(segs []string) int {
	n := len(segs)
	switch {
	case n >= 1 && flatMemberRE.MatchString(segs[n-1]):
		return 1
	case n >= 2 && segs[n-1] == "Index" && flatPdiffDirRE.MatchString(segs[n-2]):
		return 2
	case n >= 3 && segs[n-3] == "by-hash" &&
		((segs[n-2] == "SHA256" && hex64RE.MatchString(segs[n-1])) ||
			(segs[n-2] == "SHA512" && hex128RE.MatchString(segs[n-1]))):
		return 3
	}
	return 0
}

// IsFlatHostRootPath reports whether p is a flat member at the host root
// ("/InRelease", "/./Packages.gz"): spec C2 gives such a path no suite —
// a host-root flat repository (`deb https://h/ /`) is unsupported, and
// the handler logs flat_host_root_unsupported for it.
//
// The cleanBytes and hasDistsSegment guards are defensive: a host-root
// member's segments are "." or member-grammar segments, which never hold a
// forbidden byte or "dists", so no test row can tell them apart from the
// member grammar's own refusal.
func IsFlatHostRootPath(p string) bool {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) || hasDistsSegment(p) {
		return false
	}
	segs := strings.Split(p[1:], "/")
	n := flatMemberSegments(segs)
	return n > 0 && (n == len(segs) || (n == len(segs)-1 && segs[0] == "."))
}

// IsCanonicalSuitePath reports whether s is a suite path SuitePath could
// have produced, in either grammar ("/<repo>/dists/<codename>", a flat
// directory, or its "/." twin). Used to recognise legacy suite rows that
// the grammar can never reach again (phase F reaps them). A codename equal
// to an anchor name ("/foo/dists/InRelease") is canonical: the grammar
// allows it, and idle reaping handles such rows.
func IsCanonicalSuitePath(s string) bool {
	return s != "" && SuitePath(s+"/InRelease") == s
}

// IsFlatSuitePath reports whether s is a canonical flat suite path: one
// SuitePath could have produced from the flat grammar ("/repo",
// "/core:/stable:/v1.34/deb", the twin "/repo/."). A dists suite, a
// non-canonical path and "" are not.
func IsFlatSuitePath(s string) bool {
	return IsCanonicalSuitePath(s) && !hasDistsSegment(s)
}

// MaxFilenameBytes bounds a Packages Filename (spec C5's validateFilename
// refuses a longer one), and so a stored package_hash.flat_filename, its
// DotClean (spec C6): the suffix fallback skips a longer request suffix,
// which no row can match (ruling PG-30).
const MaxFilenameBytes = 4096

// FlatBase returns the directory a flat suite's Packages Filenames are
// relative to for spec C6: the suite path with ONE trailing "/." removed,
// so the twins "/repo" and "/repo/." share it (apt requests
// <uri>/<Filename> for both `deb <uri>/ /` and `deb <uri> ./`). It equals
// MarkerBase for every suite; the two names keep their two meanings
// apart (C6's package base, C3's marker key).
func FlatBase(suite string) string { return strings.TrimSuffix(suite, "/.") }

// DotClean returns p with every "." segment removed ("./cuda.deb" →
// "cuda.deb", "/flat/./amd64/x.deb" → "/flat/amd64/x.deb"); nothing else
// changes — a ".." or an empty segment stays for the caller to refuse
// (spec C6: package_hash.flat_filename and the suffix fallback).
func DotClean(p string) string {
	segs := strings.Split(p, "/")
	out := segs[:0]
	for _, s := range segs {
		if s != "." {
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
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
