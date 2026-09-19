// Package upstreamproxy validates the explicitly configured forwarding proxy.
package upstreamproxy

import (
	"errors"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Parse returns nil for direct access. Errors deliberately exclude the input
// and url.Parse's error, since either may contain proxy credentials.
// Both the config loader and fetch constructor use this policy.
func Parse(raw string, denyTargetRanges []string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 || hasControl(raw) {
		return nil, errors.New("upstream.proxy must not contain whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("upstream.proxy must be a valid absolute HTTP or HTTPS proxy URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("upstream.proxy requires http:// or https:// and a hostname")
	}
	if (u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return nil, errors.New("upstream.proxy must not contain a path other than /, a query, or a fragment")
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Is6() {
			return nil, errors.New("upstream.proxy bracketed host must be an IPv6 address")
		}
	} else if strings.ContainsAny(host, ":[]") {
		return nil, errors.New("upstream.proxy IPv6 addresses must be enclosed in brackets")
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("upstream.proxy port must be a number in 1..65535")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("upstream.proxy port must be a number in 1..65535")
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if u.User.Username() == "" || strings.Contains(u.User.Username(), ":") || hasControl(u.User.Username()) || hasControl(password) {
			return nil, errors.New("upstream.proxy requires a nonempty Basic-auth username without colons and credentials without control characters")
		}
	}
	if len(denyTargetRanges) > 0 {
		return nil, errors.New("upstream.proxy cannot be combined with upstream.deny_target_ranges; enforce destination IP restrictions on the upstream proxy")
	}
	return u, nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}
