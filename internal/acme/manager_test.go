package acme

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/certfixture"
	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
	"golang.org/x/sys/unix"
)

const testName = "cache.example.net"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testManager(t *testing.T, srv *fakeacme.Server) (*Manager, Config) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := StartChallenge(ln, testName)
	t.Cleanup(ch.Close)
	srv.SetChallengeBaseURL("http://" + ln.Addr().String())
	cfg := Config{DirectoryURL: srv.DirectoryURL(), DNSName: testName, StateDir: filepath.Join(t.TempDir(), "acme"), AcceptTerms: true, Challenge: ch, Transport: srv.Transport()}
	m, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, cfg
}

func TestIssueRenewAndOfflineRestart(t *testing.T) {
	srv := fakeacme.New(t)
	m, cfg := testManager(t, srv)
	if err := m.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := m.Current()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(srv.CACertPEM())
	if _, err := first.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: testName}); err != nil {
		t.Fatal(err)
	}
	var record certificateRecord
	if err := m.store.read(m.store.certName, &record); err != nil {
		t.Fatal(err)
	}
	record.RenewAt = time.Now().Add(-time.Second)
	if err := m.store.write(m.store.certName, record); err != nil {
		t.Fatal(err)
	}
	if err := m.ensure(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if m.Current().Leaf.Equal(first.Leaf) || srv.AccountCount() != 1 || srv.IssueCount() != 2 {
		t.Fatal("renewal/account reuse failed")
	}
	cfg.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		t.Error("offline restart contacted CA")
		return nil, errors.New("offline")
	})
	restarted, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !restarted.Current().Leaf.Equal(m.Current().Leaf) {
		t.Fatal("restart lost certificate")
	}
}

func TestPendingAccountRecoveredWithSameKey(t *testing.T) {
	srv := fakeacme.New(t)
	m, cfg := testManager(t, srv)
	if err := m.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var account accountRecord
	if err := m.store.read("account.json", &account); err != nil {
		t.Fatal(err)
	}
	account.URL = "" // Registration succeeded but account response was not committed.
	if err := m.store.write("account.json", account); err != nil {
		t.Fatal(err)
	}
	if err := unix.Unlinkat(m.store.fd, m.store.certName, 0); err != nil {
		t.Fatal(err)
	}
	next, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.AccountCount() != 1 {
		t.Fatal("recovery registered a different account")
	}
}

func TestInvalidIssuedKeyNeverReplacesGoodCertificate(t *testing.T) {
	srv := fakeacme.New(t)
	m, _ := testManager(t, srv)
	if err := m.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := m.Current()
	var rec certificateRecord
	if err := m.store.read(m.store.certName, &rec); err != nil {
		t.Fatal(err)
	}
	rec.RenewAt = time.Now().Add(-time.Second)
	if err := m.store.write(m.store.certName, rec); err != nil {
		t.Fatal(err)
	}
	wrong := certfixture.New(t, func(c *x509.Certificate) { c.DNSNames = []string{testName}; c.IPAddresses = nil })
	base := m.cfg.Transport
	m.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && strings.HasPrefix(r.URL.Path, "/cert/") {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(wrong.Cert))
			resp.ContentLength = int64(len(wrong.Cert))
		}
		return resp, err
	})
	if err := m.ensure(context.Background(), true); err == nil {
		t.Fatal("mismatched key accepted")
	}
	if !m.Current().Leaf.Equal(old.Leaf) {
		t.Fatal("working certificate replaced")
	}
	var disk certificateRecord
	if err := m.store.read(m.store.certName, &disk); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk.Key, rec.Key) || !bytes.Equal(disk.Certificate, rec.Certificate) {
		t.Fatal("working disk generation replaced")
	}
	if m.Snapshot().Error != "certificate_invalid" {
		t.Fatalf("status %+v", m.Snapshot())
	}
}

func TestRetryAfterSurvivesTimeoutAndRestart(t *testing.T) {
	for _, tt := range []struct {
		name, header     string
		minimum, maximum time.Duration
	}{
		{"shorter than reservation", "120", 119 * time.Second, 3 * time.Minute},
		{"longer than reservation", "7200", 119 * time.Minute, 121 * time.Minute},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeacme.New(t)
			m, cfg := testManager(t, srv)
			var orders atomic.Int64
			base := cfg.Transport
			transport := roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/order" {
					orders.Add(1)
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{tt.header}, "Content-Type": []string{"application/problem+json"}},
						Body: io.NopCloser(strings.NewReader(`{"type":"urn:ietf:params:acme:error:rateLimited","detail":"https://ca.example/recover?token=SENSITIVE-MARKER"}`)), Request: r}, nil
				}
				return base.RoundTrip(r)
			})
			m.cfg.Transport, cfg.Transport = transport, transport
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if err := m.EnsureCertificate(ctx); err == nil {
				t.Fatal("rate limit ignored")
			}
			if m.retry.Next.Before(time.Now().Add(tt.minimum)) {
				t.Fatal("CA cooldown shortened")
			}
			if m.retry.Next.After(time.Now().Add(tt.maximum)) {
				t.Fatal("completed attempt kept excess crash reservation")
			}
			if strings.Contains(m.Snapshot().Error, "SENSITIVE") {
				t.Fatal("raw problem leaked")
			}
			before := orders.Load()
			next, err := NewManager(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err := next.EnsureCertificate(context.Background()); err == nil {
				t.Fatal("restart ignored cooldown")
			}
			if orders.Load() != before {
				t.Fatal("restart contacted rate-limited authority")
			}

		})
	}
}

func TestInitialFailureAndStorageFailureDoNotLoop(t *testing.T) {
	srv := fakeacme.New(t)
	srv.SetFailChallenges(true)
	m, cfg := testManager(t, srv)
	if err := m.EnsureCertificate(context.Background()); err == nil {
		t.Fatal("challenge failure accepted")
	}
	if m.Current() != nil || m.retry.Next.Before(time.Now()) {
		t.Fatal("failure not persisted")
	}
	next, err := NewManager(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.EnsureCertificate(context.Background()); err == nil || err.Error() != "acme: retry_deferred" {
		t.Fatalf("got %v", err)
	}
	// A failed reservation prevents any registration or order network call.
	srv2 := fakeacme.New(t)
	broken, _ := testManager(t, srv2)
	broken.store.fail = func(string) error { return errors.New("disk full") }
	if err := broken.EnsureCertificate(context.Background()); err == nil {
		t.Fatal("storage failure ignored")
	}
	broken.store.fail = nil
	if err := broken.EnsureCertificate(context.Background()); err == nil {
		t.Fatal("unsafe retries resumed in same process")
	}
	if srv2.AccountCount() != 0 {
		t.Fatal("network before durable retry reservation")
	}
}

func TestRenewalScheduleUsesActualLifetime(t *testing.T) {
	base := time.Now()
	for _, life := range []time.Duration{6 * time.Hour, 6 * 24 * time.Hour, 90 * 24 * time.Hour} {
		c := &tls.Certificate{Leaf: &x509.Certificate{NotBefore: base, NotAfter: base.Add(life)}}
		at := renewalTime(c, 0)
		want := base.Add(life - life/3)
		if life < 10*24*time.Hour {
			want = base.Add(life / 2)
		}
		if !at.Equal(want) {
			t.Fatalf("lifetime %v: %v != %v", life, at, want)
		}
	}
}

func TestConcurrentReadersDuringRenewal(t *testing.T) {
	srv := fakeacme.New(t)
	m, _ := testManager(t, srv)
	if err := m.EnsureCertificate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rec certificateRecord
	if err := m.store.read(m.store.certName, &rec); err != nil {
		t.Fatal(err)
	}
	rec.RenewAt = time.Now().Add(-time.Second)
	if err := m.store.write(m.store.certName, rec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = m.Snapshot()
				_ = m.Status()
				_ = m.Available(time.Now())
			}
		}()
	}
	err := m.ensure(context.Background(), true)
	cancel()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
}
