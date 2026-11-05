// Package clientaddr resolves logging addresses from explicitly trusted proxies.
// It never changes the socket peer or makes authorization decisions.
package clientaddr

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// Bound work independently of the HTTP server's total header limit.
const (
	maxHeaderBytes = 8192
	maxHops        = 32
)

// Resolver is immutable after construction and safe for concurrent requests.
// Its zero value ignores all forwarding headers.
type Resolver struct {
	trusted []netip.Prefix
}

// New accepts literal IPv4/IPv6 addresses and CIDRs. Hostnames and scoped
// addresses are rejected; trusting a proxy never depends on DNS resolution.
func New(entries []string) (*Resolver, error) {
	r := &Resolver{}
	for i, entry := range entries {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			addr, addrErr := netip.ParseAddr(entry)
			if addrErr != nil || addr.Zone() != "" {
				return nil, fmt.Errorf("trusted_proxies[%d] %q: expected an IP address or CIDR without a zone", i, entry)
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		// netip keeps IPv4-mapped IPv6 distinct from IPv4. Normalize both
		// configuration and incoming addresses so equivalent peers match.
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("trusted_proxies[%d] %q: IPv4-mapped CIDR must have at least 96 prefix bits", i, entry)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		r.trusted = append(r.trusted, prefix.Masked())
	}
	return r, nil
}

func (r *Resolver) trusts(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Address preserves the transport peer alongside the effective client address.
// Client equals Peer unless a trusted forwarding chain supplied a valid IP.
// A forwarded Client is an IP only: X-Forwarded-For supplies no client port.
type Address struct {
	Client string
	Peer   string
}

type contextKey struct{}

// FromRequest returns the identity established at the listener boundary, or
// the original socket address if forwarding was disabled or unusable.
func FromRequest(r *http.Request) Address {
	if addr, ok := r.Context().Value(contextKey{}).(Address); ok {
		return addr
	}
	return Address{Client: r.RemoteAddr, Peer: r.RemoteAddr}
}

// WithContext carries an established identity into a CONNECT tunnel's context,
// whose cancellation comes from the tunnel manager rather than net/http.
func (a Address) WithContext(parent context.Context) context.Context {
	return context.WithValue(parent, contextKey{}, a)
}

// Wrap resolves each outer HTTP request independently, including keep-alive
// requests. It leaves RemoteAddr, headers and ResponseWriter capabilities intact.
// Inner CONNECT requests inherit this identity and must not reparse their headers.
func (r *Resolver) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		addr := r.resolve(req)
		if addr.Client != addr.Peer {
			req = req.WithContext(addr.WithContext(req.Context()))
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Resolver) resolve(req *http.Request) Address {
	fallback := Address{Client: req.RemoteAddr, Peer: req.RemoteAddr}
	if len(r.trusted) == 0 {
		return fallback
	}
	peer, err := netip.ParseAddrPort(req.RemoteAddr)
	if err != nil || !r.trusts(peer.Addr().Unmap()) {
		return fallback
	}
	values := req.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return fallback
	}
	// Treat repeated header fields as one ordered list. Only a trusted hop
	// can vouch for the address immediately to its left. Ignore everything
	// left of the first untrusted address, including any attacker-supplied junk.
	var client netip.Addr
	bytesLeft, hops := maxHeaderBytes, 0
	for i := len(values) - 1; i >= 0; i-- {
		remaining := values[i]
		for {
			if hops == maxHops || bytesLeft <= 0 {
				return fallback
			}
			// Bound the scan itself, not just the number of parsed IPs. Limits
			// apply only to the suffix we need to establish the client: an
			// attacker-controlled prefix must not erase a proxy-appended IP.
			start := max(0, len(remaining)-bytesLeft)
			comma := strings.LastIndexByte(remaining[start:], ',')
			if comma < 0 && start > 0 {
				return fallback // cannot locate the complete token within budget
			}
			comma += start
			token := remaining[comma+1:]
			bytesLeft -= len(token)
			hops++
			addr, err := netip.ParseAddr(strings.Trim(token, " \t"))
			if err != nil || addr.Zone() != "" {
				return fallback
			}
			addr = addr.Unmap()
			if addr.IsUnspecified() || addr.IsMulticast() || addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
				return fallback
			}
			client = addr
			if !r.trusts(addr) {
				return Address{Client: addr.String(), Peer: req.RemoteAddr}
			}
			if comma < 0 {
				if i > 0 {
					bytesLeft-- // repeated fields are separated by a virtual comma
				}
				break
			}
			bytesLeft-- // comma between entries in this field
			remaining = remaining[:comma]
		}
	}
	return Address{Client: client.String(), Peer: req.RemoteAddr}
}
