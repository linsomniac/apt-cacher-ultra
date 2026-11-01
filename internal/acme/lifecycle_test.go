package acme

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/fakeacme"
)

func TestRenewalCancellationJoinsAndClearsChallenges(t *testing.T) {
	srv := fakeacme.New(t)
	m, _ := testManager(t, srv)
	base := m.cfg.Transport
	entered := make(chan struct{})
	m.cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/order" {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return base.RoundTrip(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("order not attempted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal did not cancel promptly")
	}
	m.cfg.Challenge.mu.RLock()
	count := len(m.cfg.Challenge.tokens)
	m.cfg.Challenge.mu.RUnlock()
	if count != 0 {
		t.Fatal("active challenge survived cancellation")
	}
	if !m.retry.Next.After(time.Now()) {
		t.Fatal("interrupted attempt lost durable retry")
	}
}

func TestChallengeListenerFailureReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ch := StartChallenge(ln, testName)
	defer ch.Close()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ch.Errors():
		if err == nil {
			t.Fatal("missing listener failure")
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure unreported")
	}
}

func TestDeactivatedAccountIsNotReplaced(t *testing.T) {
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
	old := m.Current()
	srv.DeactivateAccounts()
	if err := m.ensure(context.Background(), true); err == nil {
		t.Fatal("deactivated account accepted")
	}
	if m.Snapshot().Error != "action_required" || srv.AccountCount() != 1 || !m.Current().Leaf.Equal(old.Leaf) {
		t.Fatalf("account replacement or status error: %+v", m.Snapshot())
	}
}

func TestErrorCategoriesNeverContainRemoteDetails(t *testing.T) {
	const marker = "SECRET-AUTHORITY-TOKEN"
	for _, err := range []error{errors.New("https://ca.example/" + marker), context.DeadlineExceeded, context.Canceled} {
		got := classify(err)
		if !safeProblem(got) {
			t.Fatalf("unbounded category %q", got)
		}
	}
}
