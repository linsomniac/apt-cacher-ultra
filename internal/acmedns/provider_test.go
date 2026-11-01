package acmedns

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/dnsfixture"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == WorkerArgument {
		os.Exit(RunWorker())
	}
	os.Exit(m.Run())
}

const domain = "cache.example.net"
const token = "abcdefghijklmnopqrstuvwxyz012345"

var keyAuth = token + "." + strings.Repeat("a", 43)

func TestLegoRFC2136LifecycleAndDelegation(t *testing.T) {
	if !Supported("dnsupdate") {
		t.Skip("provider not compiled")
	}
	srv := dnsfixture.New(t)
	srv.ConfigureLego(t)
	original := "_acme-challenge." + domain + "."
	delegated := "validation.example.net."
	srv.Alias(original, delegated)
	srv.Add("unrelated.example.net", "unrelated-record")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	provider, err := New(ctx, "dnsupdate", []string{srv.Address})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Present(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if err := provider.Wait(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(keyAuth))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	records := srv.Records(delegated)
	if len(records) != 1 || records[0] != want {
		t.Fatalf("records %v", records)
	}
	if err := provider.CleanUp(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	records = srv.Records(delegated)
	if len(records) != 0 || len(srv.Records("unrelated.example.net")) != 1 {
		t.Fatalf("cleanup damaged records: %v", records)
	}
}

func TestProviderErrorsAreRedacted(t *testing.T) {
	if !Supported("httpreq") {
		t.Skip("provider not compiled")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "SENSITIVE-PROVIDER-DETAIL", 400) }))
	defer srv.Close()
	t.Setenv("HTTPREQ_ENDPOINT", srv.URL)
	t.Setenv("HTTPREQ_MODE", "RAW")
	t.Setenv("LEGO_DEBUG_CLIENT_VERBOSE", "true")
	p, err := New(context.Background(), "httpreq", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Present(context.Background(), domain, token, keyAuth); err == nil || err.Error() != "acme: dns_present_failed" {
		t.Fatalf("unsanitized error: %v", err)
	}
	if err := p.CleanUp(context.Background(), domain, token, keyAuth); err == nil || err.Error() != "acme: dns_cleanup_failed" {
		t.Fatalf("unsanitized cleanup: %v", err)
	}
}

func TestStalledProviderCancelledAndReaped(t *testing.T) {
	if !Supported("httpreq") {
		t.Skip("provider not compiled")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	t.Setenv("HTTPREQ_ENDPOINT", srv.URL)
	t.Setenv("HTTPREQ_MODE", "RAW")
	p, err := New(context.Background(), "httpreq", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Present(ctx, domain, token, keyAuth) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider request not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("worker not reaped")
	}
}

func TestPropagationRequiresExpectedValueAtEveryResolver(t *testing.T) {
	a, b := dnsfixture.New(t), dnsfixture.New(t)
	sum := sha256.Sum256([]byte(keyAuth))
	value := base64.RawURLEncoding.EncodeToString(sum[:])
	a.Add("_acme-challenge."+domain, value)
	b.Add("_acme-challenge."+domain, "wrong")
	s := Session{resolvers: []string{a.Address, b.Address}, timeout: time.Second, interval: 50 * time.Millisecond}
	if err := s.Wait(context.Background(), domain, token, keyAuth); err == nil || fmt.Sprint(err) != "acme: dns_propagation_timeout" {
		t.Fatalf("premature readiness: %v", err)
	}
	b.Add("_acme-challenge."+domain, value)
	if err := s.Wait(context.Background(), domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
}

func TestNoInteractiveOrCommandProviders(t *testing.T) {
	for _, name := range []string{"exec", "manual", "not-a-provider"} {
		if Supported(name) {
			t.Fatalf("supported %s", name)
		}
		if p, err := New(context.Background(), name, nil); err == nil {
			p.Close()
			t.Fatalf("started %s", name)
		}
	}
}

func TestLongProviderTimingDefaultsAndBounds(t *testing.T) {
	if !Supported("httpreq") {
		t.Skip("provider not compiled")
	}
	// Same timings as the slowest native catalogue defaults, without network I/O.
	t.Setenv("HTTPREQ_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("HTTPREQ_PROPAGATION_TIMEOUT", "3600")
	t.Setenv("HTTPREQ_POLLING_INTERVAL", "80")
	p, err := New(context.Background(), "httpreq", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.timeout != time.Hour || p.interval != 80*time.Second {
		t.Fatalf("provider timings %s/%s", p.timeout, p.interval)
	}
	t.Setenv("HTTPREQ_PROPAGATION_TIMEOUT", "3601")
	if invalid, err := New(context.Background(), "httpreq", nil); err == nil {
		invalid.Close()
		t.Fatal("unbounded provider wait accepted")
	}
}
