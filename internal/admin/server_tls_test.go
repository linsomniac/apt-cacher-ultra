package admin

import (
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/tlscert"
)

// withAdminTLS points the admin listener at a PEM cert/key pair so
// Server.Serve takes the ServeTLS branch.
func withAdminTLS(certPath, keyPath string) adminOpt {
	return func(cfg *Config) {
		cfg.Admin.TLSCert = certPath
		cfg.Admin.TLSKey = keyPath
	}
}

// TestServe_AdminTLS pins the admin listener's TLS mode: with
// admin.tls_cert / admin.tls_key set, Serve terminates TLS on
// admin.listen — an HTTPS client that trusts the cert gets 200 from
// /healthz, and a plain-HTTP client never gets a 2xx (fail closed:
// the listener speaks TLS only). Go's http.Server answers a plain
// HTTP request on a TLS listener with a cleartext 400, so the
// rejection assertion accepts either a transport error or a 400.
func TestServe_AdminTLS(t *testing.T) {
	certPath, keyPath := testutil.WriteSelfSignedCert(t, t.TempDir())
	_, baseURL, cleanup := startAdminServer(t, withAdminTLS(certPath, keyPath))
	defer cleanup()
	httpsURL := "https://" + strings.TrimPrefix(baseURL, "http://")

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: testutil.CertPoolFromFile(t, certPath)},
		},
	}
	resp, err := client.Get(httpsURL + "/healthz")
	if err != nil {
		t.Fatalf("GET https /healthz: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d (body %q)", resp.StatusCode, http.StatusOK, body)
	}
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}

	// Plain HTTP against the TLS listener must not succeed.
	plainClient := &http.Client{Timeout: 5 * time.Second}
	resp, err = plainClient.Get(baseURL + "/healthz")
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("plain HTTP against TLS listener: status = %d, want transport error or >= 400", resp.StatusCode)
		}
	}
}

// TestNew_AdminTLSHalfSetRejected pins the defense-in-depth half of
// the admin TLS contract: config.Validate rejects a half-set pair for
// file-loaded configs, and New rejects it too, so a programmatically
// built Config can never silently fall back to plain HTTP with only
// one of tls_cert / tls_key set.
func TestNew_AdminTLSHalfSetRejected(t *testing.T) {
	certPath, keyPath := testutil.WriteSelfSignedCert(t, t.TempDir())

	for _, tc := range []struct {
		name string
		cert string
		key  string
	}{
		{name: "cert only", cert: certPath},
		{name: "key only", key: keyPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestAdminConfig(t)
			t.Cleanup(func() { _ = cfg.Cache.Close() })
			cfg.AdminAddr = "127.0.0.1:0"
			cfg.Admin.TLSCert = tc.cert
			cfg.Admin.TLSKey = tc.key

			_, err := New(cfg)
			if err == nil {
				t.Fatal("New accepted a half-set admin TLS pair, want error")
			}
			if !strings.Contains(err.Error(), "Admin.TLSCert / Admin.TLSKey") {
				t.Errorf("error %q does not name the offending pair", err.Error())
			}
		})
	}
}
