package acme

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
)

func TestHugeRetryAfterPreventsInAttemptOrderRetries(t *testing.T) {
	srv := fakeacme.New(t)
	m, _ := testManager(t, srv)
	base := m.cfg.Transport
	var orders atomic.Int64
	m.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/order" {
			orders.Add(1)
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Retry-After": []string{"18446744073709551615"}, "Content-Type": []string{"application/problem+json"}},
				Body:   io.NopCloser(strings.NewReader(`{"type":"urn:ietf:params:acme:error:rateLimited"}`)), Request: r}, nil
		}
		return base.RoundTrip(r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2200*time.Millisecond)
	defer cancel()
	if err := m.EnsureCertificate(ctx); err == nil {
		t.Fatal("rate limit ignored")
	}
	if orders.Load() != 1 {
		t.Fatalf("made %d order requests despite authority cooldown", orders.Load())
	}
	if m.retry.Next.Year() != 9999 {
		t.Fatalf("lost durable authority cooldown: %v", m.retry.Next)
	}
}

func TestHugeRetryAfterPreventsEarlyOrderPolling(t *testing.T) {
	srv := fakeacme.New(t, fakeacme.WithOrderReadyAfterPolls(1))
	m, _ := testManager(t, srv)
	base := m.cfg.Transport
	var polls atomic.Int64
	m.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err == nil && strings.HasPrefix(r.URL.Path, "/order/") {
			polls.Add(1)
			resp.Header.Set("Retry-After", "18446744073709551615")
		}
		return resp, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := m.EnsureCertificate(ctx); err == nil {
		t.Fatal("completed order before polling cooldown")
	}
	if polls.Load() != 1 || srv.IssueCount() != 0 {
		t.Fatalf("polled before Retry-After: polls=%d issued=%d", polls.Load(), srv.IssueCount())
	}
}

func TestBadNonceRecoveryAndAsynchronousOrderPolling(t *testing.T) {
	srv := fakeacme.New(t, fakeacme.WithOrderReadyAfterPolls(1))
	m, _ := testManager(t, srv)
	base := m.cfg.Transport
	var orders, polls atomic.Int64
	m.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/order" && orders.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusBadRequest,
				Header: http.Header{"Content-Type": []string{"application/problem+json"}},
				Body:   io.NopCloser(strings.NewReader(`{"type":"urn:ietf:params:acme:error:badNonce"}`)), Request: r}, nil
		}
		resp, err := base.RoundTrip(r)
		if err == nil && strings.HasPrefix(r.URL.Path, "/order/") {
			polls.Add(1)
			resp.Header.Set("Retry-After", "1")
		}
		return resp, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.EnsureCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	if orders.Load() != 2 || polls.Load() < 2 || srv.IssueCount() != 1 {
		t.Fatalf("recovery failed: orders=%d polls=%d issued=%d", orders.Load(), polls.Load(), srv.IssueCount())
	}
}
