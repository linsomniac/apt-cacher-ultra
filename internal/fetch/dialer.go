package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// errDialDenied is the sentinel returned from Dialer.ControlContext when a
// resolved address falls inside one of the configured deny CIDRs. Fetch
// translates it to ErrTargetDenied for caller-facing errors.Is checks.
var errDialDenied = errors.New("fetch: dial address in deny range")

// parseDenyCIDRs compiles CIDR strings into netip.Prefix values.
func parseDenyCIDRs(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for i, s := range cidrs {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("fetch: deny_target_ranges[%d] %q: %w", i, s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// addrInDeny reports whether addr falls in any deny prefix and returns
// the matching prefix for diagnostic logging.
func addrInDeny(addr netip.Addr, deny []netip.Prefix) (bool, netip.Prefix) {
	for _, p := range deny {
		if p.Contains(addr) {
			return true, p
		}
	}
	return false, netip.Prefix{}
}

// newTransport builds the *http.Transport used by Client. The dialer's
// ControlContext callback enforces the deny-CIDR list immediately after
// DNS resolution and before the connect syscall — the right hook to
// defend against DNS rebinding (the recursive resolver's result is what
// `address` carries here) and against direct-IP requests like
// http://10.0.0.1/.
//
// AIDEV-NOTE: Proxy is set to nil. We never want to honor HTTP_PROXY for
// upstream fetches because that would route requests through whatever the
// host environment claims is a proxy — including, in some hosting
// environments, an attacker-controlled HTTP_PROXY value. apt-cacher-ultra
// is itself the proxy.
func newTransport(opts Options, deny []netip.Prefix) http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   opts.ConnectTimeout,
		KeepAlive: 30 * time.Second,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("fetch: ControlContext SplitHostPort %q: %w", address, err)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("fetch: ControlContext ParseAddr %q: %w", host, err)
			}
			if hit, p := addrInDeny(ip, deny); hit {
				return fmt.Errorf("%w: %s in %s", errDialDenied, ip, p)
			}
			return nil
		},
	}
	dialContext := dialer.DialContext
	if opts.dialContext != nil {
		dialContext = opts.dialContext
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   opts.ConnectTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
