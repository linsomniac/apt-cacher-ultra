package admin

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/acme"
	"github.com/linsomniac/apt-cacher-ultra/internal/servertls"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

func TestAdminHTTPSAuthenticationAndMutationGuard(t *testing.T) {
	pair := certfixture.New(t)
	cert, key := pair.Write(t, t.TempDir())
	source, err := servertls.NewFiles(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	s, ln, c := newAdminServer(t, withHtpasswd(makeBcryptHtpasswd(t, "alice", "secret")), func(c *Config) {
		c.Admin.TLSCert = cert
		c.Admin.TLSKey = key
		c.TLS = source.TLSConfig()
		c.ServerTLS = func() map[string]servertls.Snapshot { return map[string]servertls.Snapshot{"admin": source.Snapshot()} }
	})
	defer serveAdminServer(t, s, ln, c)()
	transport := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: pair.Roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for _, tt := range []struct {
		name, method, path, fetch string
		auth, guard               bool
		want                      int
	}{
		{"anonymous health", "GET", "/healthz", "", false, false, 401},
		{"authenticated health", "GET", "/healthz", "", true, false, 200},
		{"authenticated status", "GET", "/?format=json", "", true, false, 200},
		{"mutation missing header", "POST", "/forget", "", true, false, 403},
		{"cross site mutation", "POST", "/forget", "cross-site", true, true, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := http.NewRequest(tt.method, "https://"+ln.Addr().String()+tt.path, nil)
			if tt.auth {
				r.SetBasicAuth("alice", "secret")
			}
			if tt.guard {
				r.Header.Set("X-ACU-Admin", "1")
			}
			if tt.fetch != "" {
				r.Header.Set("Sec-Fetch-Site", tt.fetch)
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.want {
				t.Fatalf("got %d, body %s", resp.StatusCode, body)
			}
			if tt.path == "/?format=json" && !strings.Contains(string(body), `"server_tls"`) {
				t.Fatalf("missing TLS status: %s", body)
			}
		})
	}
	// A plaintext request cannot reach the authenticated application.
	plain, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err == nil {
		defer func() { _ = plain.Body.Close() }()
		if plain.StatusCode == 200 {
			t.Fatal("HTTPS listener served plaintext application")
		}
	}
}

func serverTLSJSONFixture() statusModel {
	m := minimalJSONContractFixture()
	m.Listeners = []listenerInfo{{Role: "proxy", Addr: "127.0.0.1:3142", Scheme: "http"}, {Role: "tls", Addr: "127.0.0.1:3143", Scheme: "https"}, {Role: "admin", Addr: "127.0.0.1:6789", Scheme: "https"}}
	snap := servertls.Snapshot{Source: "acme", State: "degraded", Fingerprint: "0123456789abcdef", NotBefore: 1700000000, NotAfter: 1707776000, Error: "rate_limited"}
	m.ServerTLS = map[string]servertls.Snapshot{"admin": snap, "cache": snap}
	m.ACME = &acme.Status{Snapshot: snap, DNSName: "cache.example.net", ChallengeType: "dns-01", DNSProvider: "cloudflare", DirectoryURL: "https://ca.example/directory", NextAttempt: 1706000000, LastSuccess: 1700000100, Attempts: 2, Successes: 1, Failures: 1}
	return m
}

func TestServerTLSStatusHTML(t *testing.T) {
	m := newHealthyModel()
	m.statusModel = serverTLSJSONFixture()
	html := renderHTMLForGolden(t, m)
	for _, want := range []string{"Server TLS", "dns-01", "cloudflare", "rate_limited", "cache.example.net", "https://ca.example/directory", "0123456789abcdef"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
}
