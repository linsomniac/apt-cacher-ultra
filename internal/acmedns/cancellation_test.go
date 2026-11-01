package acmedns

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCancellationDuringPresentRetainsCleanupState(t *testing.T) {
	if !Supported("httpreq") {
		t.Skip("provider not compiled")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var created atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/present":
			created.Store(true)
			close(entered)
			<-release
		case "/cleanup":
			created.Store(false)
		default:
			t.Errorf("unexpected operation %s", r.URL.Path)
			http.NotFound(w, r)
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
		t.Fatal("Present not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not return")
	}
	cleanupCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := p.CleanUp(cleanupCtx, domain, token, keyAuth); err != nil || created.Load() {
		t.Fatalf("lost cleanup state: err=%v remaining=%v", err, created.Load())
	}
}

// Selected only by this test binary's TestMain, never by the daemon executable.
type stubbornProvider struct{}

func (*stubbornProvider) Present(context.Context, string, string, string) error {
	if err := os.WriteFile(os.Getenv("ACU_TEST_STUBBORN_MARKER"), []byte("entered"), 0600); err != nil {
		return err
	}
	select {} // Model an SDK that ignores its context.
}
func (*stubbornProvider) CleanUp(context.Context, string, string, string) error { return nil }

func TestUncooperativeProviderIsKilledAfterCancellationGrace(t *testing.T) {
	if !Supported("httpreq") {
		t.Skip("provider not compiled")
	}
	marker := filepath.Join(t.TempDir(), "entered")
	t.Setenv("ACU_TEST_STUBBORN_MARKER", marker)
	p, err := New(context.Background(), "httpreq", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Present(ctx, domain, token, keyAuth) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider not entered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("uncooperative worker survived the deadline")
	}
	if p.cmd.ProcessState == nil {
		t.Fatal("worker was not reaped")
	}
}

func TestCancellationIDsProtectLaterCleanup(t *testing.T) {
	control := &workerControl{}
	deadline := time.Now().Add(time.Minute)
	control.interrupt(1) // Cancellation can overtake dispatch of the request.
	first, endFirst := control.begin(1, deadline)
	defer endFirst()
	if !errors.Is(first.Err(), context.Canceled) {
		t.Fatal("early cancellation lost")
	}
	cleanup, endCleanup := control.begin(2, deadline)
	defer endCleanup()
	control.interrupt(1) // A late cancellation must not hit the next operation.
	if cleanup.Err() != nil {
		t.Fatal("cleanup cancelled by previous operation")
	}
	control.interrupt(2)
	if !errors.Is(cleanup.Err(), context.Canceled) {
		t.Fatal("active cancellation lost")
	}
}

// Request-stream EOF or malformed input must terminate a blocked control reader.
func TestWorkerExitsWhenRequestStreamEnds(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "eof"
		if malformed {
			name = "malformed"
		}
		t.Run(name, func(t *testing.T) {
			s, err := startWorker("/proc/self/exe", os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if malformed {
				_, err = io.WriteString(s.input, "{broken\n")
			} else {
				err = s.input.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.output.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.reader.ReadByte(); !errors.Is(err, io.EOF) {
				t.Fatalf("worker did not close response pipe on exit: %v", err)
			}
		})
	}
}
