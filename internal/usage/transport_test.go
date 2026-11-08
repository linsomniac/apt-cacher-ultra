package usage

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportCountsReadBytesOnce(t *testing.T) {
	c := collector(t, &memoryStore{})
	tr := Transport{Usage: c, Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, ContentLength: 1000, Body: io.NopCloser(strings.NewReader("abcdef"))}, nil
	})}
	r, err := tr.RoundTrip(&http.Request{})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Snapshot().Lifetime.FetchedBytes; got != 3 {
		t.Fatalf("read %d, want 3", got)
	}
}

type interruptedBody struct{ entered, closed, release chan struct{} }

func (b *interruptedBody) Read(p []byte) (int, error) {
	close(b.entered)
	<-b.closed
	<-b.release
	return copy(p, "abc"), errors.New("interrupted")
}
func (b *interruptedBody) Close() error { close(b.closed); return nil }

func TestCloseInterruptsReadWithoutLosingPartialBytes(t *testing.T) {
	c := collector(t, &memoryStore{})
	raw := &interruptedBody{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	b := &body{ReadCloser: raw, usage: c, start: time.Now()}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = b.Read(make([]byte, 8)) }()
	<-raw.entered
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	close(raw.release)
	<-done
	if got := c.Snapshot().Lifetime.FetchedBytes; got != 3 {
		t.Fatalf("read %d, want 3", got)
	}
}
