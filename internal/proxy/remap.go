package proxy

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// builtinRemapRulesSource is the default Remap rule set per SPEC §3.3.
// Stored as raw strings so they compile under the same code path as user
// rules (and surface compile errors loudly during startup tests).
//
// AIDEV-NOTE: order matters. The `^([a-z]{2}\.)?...$` patterns and the
// bare canonical-name patterns are written so the regex matches both
// "foo.archive.ubuntu.com" and "archive.ubuntu.com" with the optional
// geo prefix. Patterns are anchored at both ends to avoid accidental
// substring matches (e.g. evil-archive.ubuntu.com.attacker.tld).
var builtinRemapRulesSource = []struct {
	pattern, canonical string
}{
	{`^([a-z]{2}\.)?archive\.ubuntu\.com$`, "archive.ubuntu.com"},
	{`^([a-z]{2}\.)?security\.ubuntu\.com$`, "security.ubuntu.com"},
	{`^([a-z]{2}\.)?ports\.ubuntu\.com$`, "ports.ubuntu.com"},
	{`^(ftp\.)?[a-z]{2}\.debian\.org$`, "deb.debian.org"},
	{`^deb\.debian\.org$`, "deb.debian.org"},
	{`^security\.debian\.org$`, "security.debian.org"},
}

// builtinRemapRules returns a freshly-compiled copy of the built-in rule
// set. Compilation is cheap and we'd rather not share regex.Regexp values
// across Parser instances in tests.
func builtinRemapRules() []remapRule {
	out := make([]remapRule, 0, len(builtinRemapRulesSource))
	for _, r := range builtinRemapRulesSource {
		// AIDEV-NOTE: the source list is hard-coded and was unit-tested at
		// compile time; MustCompile is appropriate here. A failure means
		// somebody broke the static patterns and we want a fail-fast panic.
		out = append(out, remapRule{
			regex:         regexp.MustCompile(r.pattern),
			canonicalHost: r.canonical,
			source:        "builtin",
		})
	}
	return out
}

// compileRemapRules turns user-supplied rules into compiled regexps.
// Rules are returned in input order so first-match-wins matches the user's
// declared precedence.
func compileRemapRules(rules []config.RemapRule) ([]remapRule, error) {
	out := make([]remapRule, 0, len(rules))
	for i, r := range rules {
		if r.MatchHostRegex == "" {
			return nil, fmt.Errorf("remap[%d]: match_host_regex is empty", i)
		}
		if r.CanonicalHost == "" {
			return nil, fmt.Errorf("remap[%d]: canonical_host is empty", i)
		}
		re, err := regexp.Compile(r.MatchHostRegex)
		if err != nil {
			return nil, fmt.Errorf("remap[%d] regex %q: %w", i, r.MatchHostRegex, err)
		}
		out = append(out, remapRule{
			regex:         re,
			canonicalHost: r.CanonicalHost,
			source:        "user",
		})
	}
	return out, nil
}

// mirrorRoute is a compiled SPEC §2.4 mirror entry: a path prefix that
// resolves to an upstream URL. The upstream URL is split into (scheme,
// host, basePath) at compile time so request-time work is just string
// concatenation.
type mirrorRoute struct {
	prefix   string // e.g. "/ubuntu" — never trailing-slash, simplifies match
	scheme   string // upstream scheme, lowercased
	host     string // upstream host[:port]
	basePath string // upstream path prefix; "/" if upstream URL has no path
}

// compileMirrorRoutes resolves and validates each [[mirror]] entry. Mirror
// upstreams must be plain http(s) origins plus an optional base path —
// userinfo, query strings, and fragments are rejected because the cache
// would silently strip them when constructing per-request fetch URLs.
func compileMirrorRoutes(rules []config.MirrorRule) ([]mirrorRoute, error) {
	out := make([]mirrorRoute, 0, len(rules))
	for i, r := range rules {
		if !strings.HasPrefix(r.Prefix, "/") {
			return nil, fmt.Errorf("mirror[%d].prefix %q must start with /", i, r.Prefix)
		}
		if r.Upstream == "" {
			return nil, fmt.Errorf("mirror[%d].upstream is empty", i)
		}
		u, err := url.Parse(r.Upstream)
		if err != nil {
			return nil, fmt.Errorf("%w: mirror[%d].upstream %q: %v", ErrInvalidMirror, i, r.Upstream, err)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("%w: mirror[%d].upstream scheme %q (must be http or https)", ErrInvalidMirror, i, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%w: mirror[%d].upstream missing host", ErrInvalidMirror, i)
		}
		if u.User != nil {
			return nil, fmt.Errorf("%w: mirror[%d].upstream must not include userinfo", ErrInvalidMirror, i)
		}
		if u.RawQuery != "" {
			return nil, fmt.Errorf("%w: mirror[%d].upstream must not include a query string", ErrInvalidMirror, i)
		}
		if u.Fragment != "" {
			return nil, fmt.Errorf("%w: mirror[%d].upstream must not include a fragment", ErrInvalidMirror, i)
		}
		basePath := u.Path
		if basePath == "" {
			basePath = "/"
		}
		// AIDEV-NOTE: normalize prefix so matching doesn't have to special-case
		// trailing slashes. The on-the-wire prefix `/ubuntu/` and `/ubuntu`
		// must behave identically — both match `/ubuntu/dists/...`.
		prefix := strings.TrimRight(r.Prefix, "/")
		if prefix == "" {
			// User wrote `prefix = "/"`, meaning "everything". Keep as "/" so
			// matchMirror handles it as a special case.
			prefix = "/"
		}
		out = append(out, mirrorRoute{
			prefix:   prefix,
			scheme:   scheme,
			host:     strings.ToLower(u.Host),
			basePath: basePath,
		})
	}
	return out, nil
}

// matchMirror finds the first route whose prefix matches path. Returns the
// route, the request-path remainder after the prefix, and ok=false when no
// route matches. The remainder is either "" (exact prefix match) or a
// string that starts with "/" — joinMirrorPath relies on that invariant.
//
// SPEC §2.4: "first-prefix-match wins". Config validation forbids
// overlapping prefixes (§5.2), so first-match here is unambiguous as
// long as compileMirrorRoutes' output agrees with config validation —
// which it does because both operate on the same (already-validated)
// MirrorRule list.
func (p *Parser) matchMirror(path string) (mirrorRoute, string, bool) {
	for _, r := range p.mirrors {
		if r.prefix == "/" {
			return r, path, true
		}
		if path == r.prefix {
			return r, "", true
		}
		if strings.HasPrefix(path, r.prefix) && path[len(r.prefix)] == '/' {
			return r, path[len(r.prefix):], true
		}
	}
	return mirrorRoute{}, "", false
}

// joinMirrorPath splices the upstream's basePath with the per-request
// remainder. rest is either "" (exact prefix match — caller asked for the
// mirror root) or starts with "/" (a sub-path under the mirror). basePath
// is the upstream's own URL path; we trim its trailing slash so the join
// doesn't produce "//".
func joinMirrorPath(basePath, rest string) string {
	if basePath == "" || basePath == "/" {
		if rest == "" {
			return "/"
		}
		return rest
	}
	base := strings.TrimRight(basePath, "/")
	return base + rest
}
