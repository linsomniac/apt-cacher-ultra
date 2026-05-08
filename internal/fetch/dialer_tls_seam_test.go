package fetch

// SPEC6 §15 #2 enabling primitive — fetch.SetRootCAsForTest
// smoke test.
//
// The seam exists so §12.2 integration tests can stand up an
// httptest.NewTLSServer (self-signed CA → leaf) and have
// fetch.Client validate against the test pool without bypassing
// peer verification (no InsecureSkipVerify in production code).
//
// Two assertions per the contract:
//
//  1. Default state (no seam): a Fetch through an httptest TLS
//     server fails verification because the server's CA is not in
//     the system trust store. This is the production-safety
//     invariant — pretending the seam is the whole story would
//     hide a regression where someone disables verification globally.
//
//  2. Seam set to the server's CA pool: the same Fetch succeeds.
//     Bytes round-trip end-to-end. Restoration via the returned
//     closure flips back to the default state.
//
// The seam is read at Client construction (newTransport caches the
// resulting tls.Config), so each test branch sets/clears the seam
// BEFORE calling New().

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSetRootCAsForTest_GatesUpstreamVerification(t *testing.T) {
	body := []byte("hello over tls")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	target := &Target{
		CanonicalHost: "127.0.0.1",
		URL:           srv.URL + "/foo",
	}

	t.Run("default — verification fails without seam", func(t *testing.T) {
		// Belt-and-braces: ensure no prior test left a seam set.
		restore := SetRootCAsForTest(nil)
		defer restore()

		c, err := New(Options{
			ConnectTimeout:   2 * time.Second,
			TotalTimeout:     5 * time.Second,
			MaxRetries:       0,
			AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dst := &bufDst{}
		_, err = c.Fetch(context.Background(), target, dst)
		if err == nil {
			t.Fatalf("Fetch: expected TLS verification failure, got nil")
		}
		// The error path goes through net/http → x509 → wrapped by
		// Fetch's retry loop. The substring "x509" or "certificate"
		// is present somewhere in the chain. We don't assert a
		// specific sentinel because Go's exact message text is
		// version-dependent.
		var unknownAuthErr x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthErr) &&
			!strings.Contains(err.Error(), "x509") &&
			!strings.Contains(err.Error(), "certificate") {
			t.Errorf("error %q does not look like a TLS verification failure", err)
		}
	})

	t.Run("seam set — verification succeeds against the test CA", func(t *testing.T) {
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		restore := SetRootCAsForTest(pool)
		defer restore()

		c, err := New(Options{
			ConnectTimeout:   2 * time.Second,
			TotalTimeout:     5 * time.Second,
			MaxRetries:       0,
			AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dst := &bufDst{}
		res, err := c.Fetch(context.Background(), target, dst)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if res.Status != http.StatusOK {
			t.Errorf("Status: got %d, want 200", res.Status)
		}
		if got := dst.String(); got != string(body) {
			t.Errorf("body: got %q, want %q", got, body)
		}
	})

	t.Run("restore closure clears the seam", func(t *testing.T) {
		// Set, then restore — the next New() must observe the
		// pre-set state (nil) so its transport rejects the
		// httptest server again. This pins the cleanup contract:
		// a misbehaving test that forgot to defer restore() would
		// leak the pool into sibling tests.
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		restore := SetRootCAsForTest(pool)
		restore()

		c, err := New(Options{
			ConnectTimeout:   2 * time.Second,
			TotalTimeout:     5 * time.Second,
			MaxRetries:       0,
			AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		dst := &bufDst{}
		_, err = c.Fetch(context.Background(), target, dst)
		if err == nil {
			t.Fatalf("Fetch after restore: expected verification failure, got nil")
		}
	})
}
