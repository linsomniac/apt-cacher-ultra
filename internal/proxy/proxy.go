// Package proxy translates an inbound apt request (proxy-mode absolute URI,
// mirror-mode relative URI, or the http://HTTPS/// HTTPS-tunnel convention)
// into the canonical (scheme, host, path) tuple that is the cache key
// everywhere downstream — SQLite primary keys, singleflight, freshness.
//
// The package is pure: zero I/O, no time, no goroutines. It only owns the
// URL/Remap rule state. See SPEC.md §2.1-§2.4 (wire), §3 (Remap), §4.4
// (suite identification), §4.5 (metadata classification).
package proxy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// Mode identifies which inbound URL form we matched.
type Mode int

const (
	// ModeProxy is the apt-proxy form, an absolute-URI request line:
	//   GET http://archive.ubuntu.com/ubuntu/dists/noble/InRelease
	ModeProxy Mode = iota
	// ModeMirror is the mirror-mode form, a relative request line plus a
	// matching [[mirror]] config entry:
	//   GET /ubuntu/dists/noble/InRelease
	ModeMirror
)

func (m Mode) String() string {
	switch m {
	case ModeProxy:
		return "proxy"
	case ModeMirror:
		return "mirror"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Request is the canonical decomposition of an inbound apt request.
//
// CanonicalScheme/CanonicalHost/Path together form the cache key; they are
// what gets stored in url_path, what singleflight keys on, and what
// freshness state attaches to (via SuitePath).
//
// UpstreamURL is the URL the fetcher should hit. After Remap it is built
// from the canonical tuple, so geo-mirror traffic transparently funnels
// through whichever single upstream the user (or built-in defaults) points
// at. There is no value in remembering which geo-mirror DNS the client
// happened to ask for.
type Request struct {
	Mode Mode

	CanonicalScheme string
	CanonicalHost   string
	Path            string

	UpstreamURL string

	IsMetadata bool
	SuitePath  string
}

// Errors returned by Parse.
var (
	ErrEmptyURI          = errors.New("proxy: empty request URI")
	ErrInvalidURI        = errors.New("proxy: invalid request URI")
	ErrUnsupportedScheme = errors.New("proxy: unsupported URL scheme")
	ErrEmptyHost         = errors.New("proxy: empty host")
	ErrInvalidPath       = errors.New("proxy: invalid request path")
	ErrNoMirrorMatch     = errors.New("proxy: no mirror prefix matches request path")
	ErrInvalidMirror     = errors.New("proxy: invalid mirror upstream URL")
	ErrInvalidHTTPSMagic = errors.New("proxy: malformed HTTPS/// magic URL")
)

// Parser bundles compiled remap rules and resolved mirror routes.
//
// Construction is one-shot at startup: regex compilation and upstream URL
// parsing all happen in New. Parse() is allocation-light and reentrant.
type Parser struct {
	remap   []remapRule
	mirrors []mirrorRoute
}

// New compiles user remap rules and mirror routes from config. Built-in
// remap rules (SPEC §3.3) are appended after the user rules so user rules
// always have precedence. Returns an error if any rule fails to compile.
func New(remap []config.RemapRule, mirror []config.MirrorRule) (*Parser, error) {
	rules, err := compileRemapRules(remap)
	if err != nil {
		return nil, err
	}
	rules = append(rules, builtinRemapRules()...)

	routes, err := compileMirrorRoutes(mirror)
	if err != nil {
		return nil, err
	}

	return &Parser{
		remap:   rules,
		mirrors: routes,
	}, nil
}

// Parse converts a wire-form request URI plus its Host header into a
// canonical Request. requestURI is the literal value from the HTTP request
// line (absolute-URI for proxy clients, abs_path for mirror clients).
// hostHeader is currently unused — apt sends consistent values in proxy
// mode and arbitrary cache-side hostnames in mirror mode — but kept on the
// signature for symmetry with the eventual http.Handler integration and
// future proxy-mode Host validation.
func (p *Parser) Parse(requestURI, hostHeader string) (*Request, error) {
	_ = hostHeader

	if requestURI == "" {
		return nil, ErrEmptyURI
	}

	parsed, err := parseRequestURI(requestURI)
	if err != nil {
		return nil, err
	}

	var (
		scheme, host, path string
		mode               Mode
	)

	switch {
	case parsed.absolute:
		mode = ModeProxy
		scheme = parsed.scheme
		host = parsed.host
		path = parsed.path
		if isHTTPSMagic(host, path) {
			realHost, realPath, err := splitHTTPSMagic(path)
			if err != nil {
				return nil, err
			}
			scheme = "https"
			host = realHost
			path = realPath
		}
	default:
		mode = ModeMirror
		route, rest, ok := p.matchMirror(parsed.path)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoMirrorMatch, parsed.path)
		}
		scheme = route.scheme
		host = route.host
		path = joinMirrorPath(route.basePath, rest)
	}

	if host == "" {
		return nil, ErrEmptyHost
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedScheme, scheme)
	}
	if path == "" || path[0] != '/' {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPath, path)
	}

	canonHost, upstreamAuthority := p.canonicalize(host)
	upstream := scheme + "://" + upstreamAuthority + path

	return &Request{
		Mode:            mode,
		CanonicalScheme: scheme,
		CanonicalHost:   canonHost,
		Path:            path,
		UpstreamURL:     upstream,
		IsMetadata:      IsMetadata(path),
		SuitePath:       SuitePath(path),
	}, nil
}

// canonicalize applies the first-matching remap rule (user rules first,
// built-ins after). Returns the canonical host (cache-key form, no port,
// per SPEC §3.2) and the upstream authority (host:port for the fetcher).
//
// Hostnames are case-folded to lowercase and stripped of any trailing
// FQDN dot before remap matching, so "US.ARCHIVE.UBUNTU.COM." and
// "us.archive.ubuntu.com" hit the same cache key.
//
// When a remap rule matches, the upstream authority equals the canonical
// host with no port — remap targets are public archive names per SPEC
// §3.3, and the user expects to talk to them on the scheme's default
// CanonicalHost returns the Remap-canonical hostname for a literal
// host (the bare host with port stripped, trailing-dot stripped, and
// any matching Remap rule applied). The CONNECT handler's fetch-gate
// predicate (§5.1.2) consults this so the upstream allowlist sees the
// same canonical form a plain GET would see after parser.Parse.
func (p *Parser) CanonicalHost(host string) string {
	canon, _ := p.canonicalize(host)
	return canon
}

// port. When no rule matches, the upstream authority preserves the
// caller's port (so "http://127.0.0.1:8080/repo/" still hits :8080).
func (p *Parser) canonicalize(host string) (canonHost, upstreamAuthority string) {
	bare, port := splitHostPort(strings.ToLower(host))
	bare = strings.TrimSuffix(bare, ".") // FQDN trailing dot
	for _, r := range p.remap {
		if r.regex.MatchString(bare) {
			return r.canonicalHost, r.canonicalHost
		}
	}
	if port == "" {
		return bare, bare
	}
	return bare, bare + ":" + port
}

// remapRule is a compiled SPEC §3 rule.
type remapRule struct {
	regex         *regexp.Regexp
	canonicalHost string
	source        string // "user" or "builtin", for diagnostics
}
