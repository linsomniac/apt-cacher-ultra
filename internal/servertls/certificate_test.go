package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
)

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*x509.Certificate)
	}{
		{"expired", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Minute) }},
		{"future", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) }},
		{"CA", func(c *x509.Certificate) { c.IsCA = true; c.BasicConstraintsValid = true }},
		{"client only", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }},
		{"no signing", func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageKeyEncipherment }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := certfixture.New(t, tc.change)
			if _, err := Parse(p.Cert, p.Key, "", time.Now()); err == nil {
				t.Fatal("invalid candidate accepted")
			}
		})
	}
	a, b := certfixture.New(t), certfixture.New(t)
	if _, err := Parse(a.Cert, b.Key, "", time.Now()); err == nil {
		t.Fatal("mismatched key accepted")
	}
	if _, err := Parse(a.Cert, a.Key, "wrong.example", time.Now()); err == nil {
		t.Fatal("wrong name accepted")
	}
	if _, err := Parse(a.Cert, a.Key, "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestFileRotationRetainsGoodPair(t *testing.T) {
	dir := t.TempDir()
	a, b := certfixture.New(t), certfixture.New(t)
	cp, kp := a.Write(t, dir)
	f, err := NewFiles(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	old := f.Current()
	if err := os.WriteFile(kp, b.Key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Reload(); err == nil {
		t.Fatal("partial rotation accepted")
	}
	if f.Current() != old || f.Snapshot().State != "degraded" {
		t.Fatal("lost good certificate")
	}
	if err := os.WriteFile(cp, b.Cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.Reload(); err != nil {
		t.Fatal(err)
	}
	if f.Current() == old || f.Snapshot().State != "usable" {
		t.Fatal("rotation not adopted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(cp, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFiles(link, kp); err != nil {
		t.Fatalf("Certbot-style link: %v", err)
	}
}

func TestTLSRotationAndResumptionGate(t *testing.T) {
	a, b := certfixture.New(t), certfixture.New(t)
	parse := func(p certfixture.Pair) *tls.Certificate {
		c, err := Parse(p.Cert, p.Key, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var h Holder
	h.Install(parse(a))
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	s.TLS = h.TLSConfig()
	s.StartTLS()
	defer s.Close()
	roots := a.Roots
	roots.AddCert(b.Leaf)
	cache := tls.NewLRUClientSessionCache(4)
	clientConfig := &tls.Config{RootCAs: roots, ServerName: "localhost", ClientSessionCache: cache}
	dial := func() (*tls.Conn, error) { return tls.Dial("tcp", s.Listener.Addr().String(), clientConfig) }
	c, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	// Read a response so TLS 1.3 session tickets have arrived.
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	_, _ = c.Read(buf)
	_ = c.Close()
	c, err = dial()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ConnectionState().DidResume {
		t.Fatal("test did not exercise session resumption")
	}
	_ = c.Close()
	h.Install(parse(b))
	full, err := tls.Dial("tcp", s.Listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if !full.ConnectionState().PeerCertificates[0].Equal(b.Leaf) {
		t.Fatal("new full handshake served old cert")
	}
	_ = full.Close()
	// Install an expired immutable snapshot to simulate time passing after
	// failed renewal. The existing resumable ticket must not bypass the gate.
	expired := *parse(b)
	leaf := *expired.Leaf
	leaf.NotAfter = time.Now().Add(-time.Second)
	expired.Leaf = &leaf
	h.Install(&expired)
	if c, err := dial(); err == nil {
		_ = c.Close()
		t.Fatal("expired source accepted resumed connection")
	}
	h.Install(parse(b))
	if c, err := tls.Dial("tcp", s.Listener.Addr().String(), &tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS11}); err == nil {
		_ = c.Close()
		t.Fatal("TLS 1.1 accepted")
	}
}
