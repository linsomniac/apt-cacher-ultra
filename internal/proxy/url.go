package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

// parsedURI is the intermediate form produced by parseRequestURI: it
// distinguishes the proxy-mode (absolute-URI) and mirror-mode (abs_path)
// cases the apt clients can present on the request line.
type parsedURI struct {
	absolute bool   // true if the request had scheme+authority
	scheme   string // populated when absolute
	host     string // raw authority including any :port
	path     string // always starts with "/"
}

// parseRequestURI splits the wire-form request URI into its components.
// It accepts:
//   - absolute-URI form: "http://host[:port]/path"  (proxy mode)
//   - abs_path form:     "/path"                    (mirror mode)
//
// Any other shape (relative reference like "foo/bar", "*", etc.) is
// rejected. We deliberately accept only http/https schemes here; the
// HTTPS/// magic is detected later and mutates the canonical form.
//
// Query strings and fragments are rejected: apt repository URLs never
// carry them, and silently dropping a query like "/Packages?x=1" would
// alias it to "/Packages" in the cache.
func parseRequestURI(raw string) (*parsedURI, error) {
	if strings.HasPrefix(raw, "/") {
		// Mirror mode. Validate and percent-decode the path the same way
		// url.Parse would, so callers see canonical bytes downstream.
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidURI, err)
		}
		// AIDEV-NOTE: a parsed mirror-mode URI must have empty scheme and
		// authority. If url.Parse produced anything else, reject — apt
		// does not send fragments and we don't want them silently
		// affecting cache key derivation.
		if u.Scheme != "" || u.Host != "" {
			return nil, fmt.Errorf("%w: unexpected scheme/host in relative URI %q", ErrInvalidURI, raw)
		}
		if u.RawQuery != "" {
			return nil, fmt.Errorf("%w: query string not supported: %q", ErrInvalidURI, raw)
		}
		if u.Fragment != "" {
			return nil, fmt.Errorf("%w: fragment not supported: %q", ErrInvalidURI, raw)
		}
		return &parsedURI{absolute: false, path: u.Path}, nil
	}

	// Absolute-URI. Must parse as an absolute URL with an authority.
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURI, err)
	}
	if !u.IsAbs() {
		return nil, fmt.Errorf("%w: not absolute and not abs_path: %q", ErrInvalidURI, raw)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedScheme, scheme)
	}
	// AIDEV-NOTE: userinfo in proxy-form requests is rejected here for two
	// reasons: (1) apt does not authenticate to its proxy via URL userinfo,
	// so any request that carries it is anomalous, and (2) the raw request
	// URI gets passed through structured logs and error messages where a
	// `user:pass@host` credential would otherwise leak. Reject upfront so
	// the credential never reaches a log line, a remap regex, or a fetch.
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo not supported in absolute URI", ErrInvalidURI)
	}
	if u.RawQuery != "" {
		return nil, fmt.Errorf("%w: query string not supported: %q", ErrInvalidURI, raw)
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("%w: fragment not supported: %q", ErrInvalidURI, raw)
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	return &parsedURI{
		absolute: true,
		scheme:   scheme,
		host:     u.Host,
		path:     path,
	}, nil
}

// isHTTPSMagic reports whether the request is using the
// `http://HTTPS///<real-host>/<path>` apt-cacher-ng convention for
// HTTPS-only upstreams. SPEC §2.3 / §3.4.
//
// After url.Parse:
//   - host is the literal "HTTPS"
//   - path begins with "///" (the host/path separator consumes one slash;
//     the convention's two extras stay in the path)
//
// Some clients vary the case ("HTTPS" vs "https"). We match
// case-insensitively to be liberal in what we accept.
func isHTTPSMagic(host, path string) bool {
	return strings.EqualFold(host, "HTTPS") && strings.HasPrefix(path, "///")
}

// splitHTTPSMagic extracts the real upstream host and path from a magic
// URL's path. Input path begins with "///" (verified by isHTTPSMagic).
// Returns (host, path, error) where path always begins with "/".
func splitHTTPSMagic(path string) (string, string, error) {
	rest := strings.TrimPrefix(path, "///")
	if rest == "" {
		return "", "", fmt.Errorf("%w: missing real upstream host", ErrInvalidHTTPSMagic)
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, "/", nil
	}
	host := rest[:slash]
	if host == "" {
		return "", "", fmt.Errorf("%w: empty upstream host", ErrInvalidHTTPSMagic)
	}
	return host, rest[slash:], nil
}

// splitHostPort separates a host[:port] string into its bare host and
// (optional) port pieces. Bracketed IPv6 hosts ("[::1]:80") are handled
// correctly. We don't reach for net.SplitHostPort because we'd rather
// not couple Parse to net's DNS semantics, and the canonical-host logic
// needs to handle malformed input gracefully (leave it alone) rather
// than erroring out.
func splitHostPort(host string) (bare, port string) {
	if host == "" {
		return "", ""
	}
	if host[0] == '[' {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return host, "" // malformed, treat whole string as host
		}
		bare = host[:end+1]
		if end+1 < len(host) && host[end+1] == ':' {
			port = host[end+2:]
		}
		return bare, port
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		return host[:i], host[i+1:]
	}
	return host, ""
}

// stripPort drops a trailing :port from a host. Kept for any caller that
// wants only the bare host; new code should reach for splitHostPort.
func stripPort(host string) string {
	bare, _ := splitHostPort(host)
	return bare
}
