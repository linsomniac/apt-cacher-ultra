package aptpath

import (
	"net/url"
	"strings"
)

const hexLower = "0123456789abcdef"

// AptEncodePath percent-encodes decoded path p the way apt's
// QuoteString(part, "+~ ") does (plus '%', controls and non-ASCII, in
// lowercase hex), extended with every byte Go's net/url refuses inside a
// RawPath (" < > \ ^ ` { | } # ?). The upstream therefore receives what
// apt would send it directly (spec C1).
//
// AIDEV-NOTE: the extension set is what makes url.URL.EscapedPath() use
// this encoding verbatim; TestBuildUpstreamURL_AllBytesRoundTrip proves
// it for every byte. Do not remove bytes from needsEscape.
func AptEncodePath(p string) string {
	n := 0
	for i := 0; i < len(p); i++ {
		if needsEscape(p[i]) {
			n++
		}
	}
	if n == 0 {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 2*n)
	for i := 0; i < len(p); i++ {
		c := p[i]
		if needsEscape(c) {
			b.WriteByte('%')
			b.WriteByte(hexLower[c>>4])
			b.WriteByte(hexLower[c&0x0f])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func needsEscape(c byte) bool {
	if c <= 0x20 || c >= 0x7f {
		return true
	}
	switch c {
	case '+', '~', '%', '"', '<', '>', '\\', '^', '`', '{', '|', '}', '#', '?':
		return true
	}
	return false
}

// BuildUpstreamURL composes scheme://authority + path with path encoded
// by AptEncodePath. It is the only way upstream URLs are built from a
// decoded canonical path (spec C1.3).
func BuildUpstreamURL(scheme, authority, path string) string {
	u := url.URL{Scheme: scheme, Host: authority, Path: path, RawPath: AptEncodePath(path)}
	if u.EscapedPath() != u.RawPath {
		// Unreachable: escaping is byte-local and the all-bytes test
		// covers every byte on every toolchain CI builds with, so a Go
		// change to net/url fails that test before it ships. Fall back to
		// Go's own (still correct, not apt-identical) escaping rather than
		// panic. No counter (plan deviation 2): aptpath is stdlib-only.
		u.RawPath = ""
	}
	return u.String()
}
