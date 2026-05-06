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
//
// IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) are checked against IPv4
// prefixes after Unmap() so an attacker can't dodge an IPv4 deny entry
// by reaching us over a dual-stack socket. Without this, a target like
// ::ffff:169.254.169.254 (cloud metadata via IPv6) would slip past a
// 169.254.0.0/16 deny rule. Addresses that are not 4-in-6 fall through
// the original loop unchanged.
func addrInDeny(addr netip.Addr, deny []netip.Prefix) (bool, netip.Prefix) {
	for _, p := range deny {
		if p.Contains(addr) {
			return true, p
		}
	}
	if addr.Is4In6() {
		unmapped := addr.Unmap()
		for _, p := range deny {
			if p.Contains(unmapped) {
				return true, p
			}
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
// When tracker is non-nil, the dialContext is wrapped so per-host dial
// failures register a cooldown (next dial within UnreachableCooldown
// runs as a single short-deadline probe and surfaces ErrHostUnreachable
// instead of consuming the full retry budget).
//
// AIDEV-NOTE: Proxy is set to nil. We never want to honor HTTP_PROXY for
// upstream fetches because that would route requests through whatever the
// host environment claims is a proxy — including, in some hosting
// environments, an attacker-controlled HTTP_PROXY value. apt-cacher-ultra
// is itself the proxy.
func newTransport(opts Options, deny []netip.Prefix, tracker *unreachableTracker) http.RoundTripper {
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
	if tracker != nil {
		dialContext = wrapDialWithTracker(dialContext, tracker)
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

// wrapDialWithTracker returns a DialContext that consults tracker before
// each dial: a host within cooldown gets a single probe with a short
// deadline; a probe failure surfaces ErrHostUnreachable (non-retryable)
// instead of the original timeout error so the outer Fetch loop bails
// fast. Successful dials clear the marker; deny-CIDR rejections do not
// touch it (the failure mode is config, not network).
//
// AIDEV-NOTE: Per-Fetch retry interaction. Because markFailed runs
// inside the dialer (one layer below the Fetch retry loop), the FIRST
// failed dial in a single Fetch puts the host in cooldown immediately,
// so the SECOND attempt of that same Fetch already hits the probe
// path and bails. Effectively MaxRetries collapses to 1 probe-retry on
// dial failures (it remains in full effect for non-dial retryable
// errors like 5xx or partial reads). This is intentional — SPEC §1
// "never hang" prefers fast aggregate failure over a single Fetch
// burning the full ConnectTimeout × MaxRetries budget on a host that
// already proved unreachable. Operators who want the legacy budget
// can set upstream.unreachable_cooldown = "0s".
func wrapDialWithTracker(
	inner func(ctx context.Context, network, addr string) (net.Conn, error),
	tracker *unreachableTracker,
) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, splitErr := net.SplitHostPort(addr)
		// If we can't extract the host, fall through unmodified — the
		// inner dialer's own error will surface and the request fails as
		// it would today. The tracker is best-effort, never load-bearing.
		if splitErr != nil {
			return inner(ctx, network, addr)
		}
		inCooldown, probeT := tracker.inCooldown(host)
		if inCooldown && probeT > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, probeT)
			defer cancel()
		}
		conn, err := inner(ctx, network, addr)
		if err != nil {
			// Don't penalize the host for a deny-CIDR rejection — that's
			// configuration, not unreachability. Every other dial-layer
			// error counts as a network failure.
			if !errors.Is(err, errDialDenied) {
				tracker.markFailed(host)
				if inCooldown {
					// Wrap with both sentinels so errors.Is handles two
					// dispatches: ErrHostUnreachable surfaces the
					// fast-fail diagnosis to anyone who cares, and
					// ErrUpstreamUnavailable routes the response
					// through the same handler path as a normal
					// upstream-down miss (502 + Retry-After, with
					// tryServeStale eligibility on metadata).
					return nil, fmt.Errorf("%w: %w: %v",
						ErrUpstreamUnavailable, ErrHostUnreachable, err)
				}
			}
			return nil, err
		}
		tracker.markOK(host)
		return conn, nil
	}
}
