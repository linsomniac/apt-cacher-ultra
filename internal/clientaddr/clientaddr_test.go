package clientaddr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	trusted := []string{"10.0.0.0/24", "2001:db8:1::/48"}
	for _, tt := range []struct {
		name   string
		peer   string
		values []string
		want   string // empty means the unchanged peer
	}{
		{name: "absent", peer: "10.0.0.2:1234"},
		{name: "untrusted peer", peer: "192.0.2.1:1234", values: []string{"203.0.113.7"}},
		{name: "loopback not implicitly trusted", peer: "127.0.0.1:1234", values: []string{"203.0.113.7"}},
		{name: "private network not implicitly trusted", peer: "192.168.1.1:1234", values: []string{"203.0.113.7"}},
		{name: "invalid peer", peer: "proxy.example:1234", values: []string{"203.0.113.7"}},
		{name: "missing peer port", peer: "10.0.0.2", values: []string{"203.0.113.7"}},
		{name: "single", peer: "10.0.0.2:1234", values: []string{"203.0.113.7"}, want: "203.0.113.7"},
		{name: "private client", peer: "10.0.0.2:1234", values: []string{"192.168.2.5"}, want: "192.168.2.5"},
		{name: "loopback client", peer: "10.0.0.2:1234", values: []string{"127.0.0.1"}, want: "127.0.0.1"},
		{name: "spoofed leftmost", peer: "10.0.0.2:1234", values: []string{"203.0.113.99, 192.0.2.7"}, want: "192.0.2.7"},
		{name: "trusted chain", peer: "10.0.0.2:1234", values: []string{"203.0.113.99, 192.0.2.7, 10.0.0.3"}, want: "192.0.2.7"},
		{name: "all trusted", peer: "10.0.0.2:1234", values: []string{"10.0.0.4, 10.0.0.3"}, want: "10.0.0.4"},
		{name: "repeated headers", peer: "10.0.0.2:1234", values: []string{"203.0.113.99", "192.0.2.7, 10.0.0.3"}, want: "192.0.2.7"},
		{name: "chain across fields", peer: "10.0.0.2:1234", values: []string{"192.0.2.7", "10.0.0.3"}, want: "192.0.2.7"},
		{name: "junk beyond trust boundary", peer: "10.0.0.2:1234", values: []string{"garbage,,unknown, 192.0.2.7, 10.0.0.3"}, want: "192.0.2.7"},
		{name: "long attacker prefix", peer: "10.0.0.2:1234", values: []string{strings.Repeat("x", maxHeaderBytes+1) + ", 192.0.2.7, 10.0.0.3"}, want: "192.0.2.7"},
		{name: "many attacker entries", peer: "10.0.0.2:1234", values: []string{strings.Repeat("unknown,", maxHops+1) + "192.0.2.7, 10.0.0.3"}, want: "192.0.2.7"},
		{name: "many attacker fields", peer: "10.0.0.2:1234", values: append(strings.Split(strings.Repeat("unknown,", maxHops+1), ","), "192.0.2.7", "10.0.0.3"), want: "192.0.2.7"},
		{name: "optional whitespace", peer: "10.0.0.2:1234", values: []string{" \t192.0.2.7 \t,\t10.0.0.3 "}, want: "192.0.2.7"},
		{name: "IPv6", peer: "[2001:db8:1::2]:1234", values: []string{"2001:db8:2::7"}, want: "2001:db8:2::7"},
		{name: "mixed families", peer: "10.0.0.2:1234", values: []string{"192.0.2.7, 2001:db8:1::3"}, want: "192.0.2.7"},
		{name: "mapped peer", peer: "[::ffff:10.0.0.2]:1234", values: []string{"192.0.2.7"}, want: "192.0.2.7"},
		{name: "mapped chain", peer: "10.0.0.2:1234", values: []string{"::ffff:192.0.2.7, ::ffff:10.0.0.3"}, want: "192.0.2.7"},
		{name: "empty", peer: "10.0.0.2:1234", values: []string{""}},
		{name: "empty last field", peer: "10.0.0.2:1234", values: []string{"192.0.2.7", ""}},
		{name: "empty first trusted field", peer: "10.0.0.2:1234", values: []string{"", "10.0.0.3"}},
		{name: "trailing comma", peer: "10.0.0.2:1234", values: []string{"192.0.2.7,"}},
		{name: "invalid trusted suffix", peer: "10.0.0.2:1234", values: []string{"192.0.2.7, unknown, 10.0.0.3"}},
		{name: "max hops", peer: "10.0.0.2:1234", values: []string{"192.0.2.7" + strings.Repeat(",10.0.0.3", maxHops-1)}, want: "192.0.2.7"},
		{name: "too many hops", peer: "10.0.0.2:1234", values: []string{"192.0.2.7" + strings.Repeat(",10.0.0.3", maxHops)}},
		{name: "max hops across fields", peer: "10.0.0.2:1234", values: strings.Split("192.0.2.7"+strings.Repeat(",10.0.0.3", maxHops-1), ","), want: "192.0.2.7"},
		{name: "too many fields", peer: "10.0.0.2:1234", values: strings.Split("192.0.2.7"+strings.Repeat(",10.0.0.3", maxHops), ",")},
		{name: "oversized", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes) + "192.0.2.7"}},
		{name: "oversized ignored field", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes), "192.0.2.7"}, want: "192.0.2.7"},
		{name: "oversized trusted suffix across fields", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes/2) + "192.0.2.7", strings.Repeat(" ", maxHeaderBytes/2) + "10.0.0.3"}},
		{name: "byte limit", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes-len("192.0.2.7")) + "192.0.2.7"}, want: "192.0.2.7"},
		{name: "byte limit across fields", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes-len("192.0.2.7,10.0.0.3")) + "192.0.2.7", "10.0.0.3"}, want: "192.0.2.7"},
		{name: "one byte over across fields", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes-len("192.0.2.7,10.0.0.3")+1) + "192.0.2.7", "10.0.0.3"}},
		{name: "all trusted at hop limit", peer: "10.0.0.2:1234", values: []string{"10.0.0.4" + strings.Repeat(",10.0.0.3", maxHops-1)}, want: "10.0.0.4"},
		{name: "all trusted at byte limit", peer: "10.0.0.2:1234", values: []string{strings.Repeat(" ", maxHeaderBytes-len("10.0.0.4")) + "10.0.0.4"}, want: "10.0.0.4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolver, err := New(trusted)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			req.RemoteAddr = tt.peer
			for _, v := range tt.values {
				req.Header.Add("X-Forwarded-For", v)
			}
			want := tt.want
			if want == "" {
				want = tt.peer
			}
			if got := resolver.resolve(req); got != (Address{Client: want, Peer: tt.peer}) {
				t.Fatalf("resolve = %+v, want client %q, peer %q", got, want, tt.peer)
			}
		})
	}
}

func TestRejectMalformedForwardedAddresses(t *testing.T) {
	resolver, err := New([]string{"10.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"unknown", "proxy.example", "192.0.2.7:1234", "[2001:db8::7]", "[2001:db8::7]:1234",
		"fe80::1%eth0", "\"192.0.2.7\"", "192.0.2.0/24", "for=192.0.2.7", "192.000.002.7",
		"0xC0000207", "3221225991", "192.0.2.7\r\n", "192.0.2.7\x00", "\u00a0192.0.2.7",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "::ffff:0.0.0.0", "255.255.255.255",
	} {
		t.Run(value, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			req.RemoteAddr = "10.0.0.2:1234"
			req.Header.Set("X-Forwarded-For", value)
			if got := resolver.resolve(req); got.Client != req.RemoteAddr {
				t.Fatalf("accepted %q as %q", value, got.Client)
			}
		})
	}
}

func TestNew(t *testing.T) {
	for _, entry := range []string{"", "proxy.example", "10.0.0.2:80", "10.0.0.0/33", "2001:db8::/129", "fe80::1%eth0", "fe80::1%eth0/64", "::ffff:192.0.2.0/80", " 10.0.0.2", "*"} {
		if _, err := New([]string{entry}); err == nil {
			t.Errorf("accepted invalid entry %q", entry)
		}
	}
	for _, entry := range []string{"10.0.0.2", "10.0.0.9/24", "::ffff:10.0.0.2", "::ffff:10.0.0.9/120"} {
		resolver, err := New([]string{entry})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		req.Header.Set("X-Forwarded-For", "192.0.2.7")
		if got := resolver.resolve(req); got.Client != "192.0.2.7" {
			t.Errorf("%q failed to trust peer: %+v", entry, got)
		}
	}
}

func TestWrap(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var entries []string
		if enabled {
			entries = []string{"10.0.0.2"}
		}
		resolver, err := New(entries)
		if err != nil {
			t.Fatal(err)
		}
		// Reusing a peer must not reuse the previous request's client identity.
		for _, forwarded := range []string{"192.0.2.7", "192.0.2.8", ""} {
			req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
			req.RemoteAddr = "10.0.0.2:1234"
			req.Header.Set("X-Forwarded-For", forwarded)
			req.Header.Set("X-Real-IP", "203.0.113.99")
			req.Header.Set("Forwarded", "for=203.0.113.99")
			original := req.Header.Clone()
			recorder := httptest.NewRecorder()
			resolver.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := req.RemoteAddr
				if enabled && forwarded != "" {
					want = forwarded
				}
				addr := FromRequest(r)
				if addr.Client != want || addr.Peer != req.RemoteAddr {
					t.Errorf("address = %+v, want %q", addr, want)
				}
				if r.RemoteAddr != req.RemoteAddr || !reflect.DeepEqual(r.Header, original) || w != recorder {
					t.Error("middleware changed transport address, headers, or response writer")
				}
				// A tunnel can retain identity while using a different lifecycle context.
				parent, cancel := context.WithCancel(context.Background())
				inner := r.WithContext(addr.WithContext(parent))
				if got := FromRequest(inner); got != addr {
					t.Errorf("inner identity = %+v, want %+v", got, addr)
				}
				cancel()
				if inner.Context().Err() != context.Canceled {
					t.Error("tunnel context lost cancellation")
				}
			})).ServeHTTP(recorder, req)
			if got := FromRequest(req); got.Client != req.RemoteAddr {
				t.Error("original request context mutated")
			}
		}
	}
}

func FuzzResolve(f *testing.F) {
	for _, value := range []string{"", "192.0.2.7", "bad, 192.0.2.7, 10.0.0.3", "::ffff:192.0.2.7", ",", strings.Repeat("unknown,", maxHops+1), strings.Repeat("x", maxHeaderBytes+1)} {
		f.Add(value)
	}
	resolver, err := New([]string{"10.0.0.0/24"})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, value string) {
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.RemoteAddr = "10.0.0.2:1234"
		req.Header.Set("X-Forwarded-For", value)
		addr := resolver.resolve(req)
		if addr.Peer != req.RemoteAddr {
			t.Fatal("lost transport peer")
		}
		// Once an untrusted hop is appended by the trusted proxy, no prefix
		// supplied by that client may change or suppress the established IP.
		for _, values := range [][]string{
			{value + ", 192.0.2.7"},
			{value, "192.0.2.7", "10.0.0.3"},
		} {
			req.Header["X-Forwarded-For"] = values
			addr = resolver.resolve(req)
			if addr.Client != "192.0.2.7" {
				t.Fatalf("lost or spoofed address: %+v", addr)
			}
		}
	})
}
