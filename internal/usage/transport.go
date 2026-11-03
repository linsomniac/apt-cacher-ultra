package usage

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// Transport observes application response bodies without changing upstream
// policy. Placing it beneath http.Client includes redirect response reads too.
type Transport struct {
	Base  http.RoundTripper
	Usage *Collector
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.Base.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = &body{ReadCloser: resp.Body, usage: t.Usage, start: start}
	}
	return resp, err
}

func (t Transport) CloseIdleConnections() {
	if c, ok := t.Base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

type body struct {
	io.ReadCloser
	usage    *Collector
	start    time.Time
	mu       sync.Mutex
	bytes    int64
	readers  int
	closed   bool
	recorded bool
	once     sync.Once
	err      error
}

func (b *body) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.readers++
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.bytes = add(b.bytes, int64(n))
	b.readers--
	b.recordClosed()
	b.mu.Unlock()
	return n, err
}

// Called under mu. Close may race with a blocked Read, so postpone accounting
// until its last return. Never hold mu across underlying I/O: Close must be
// able to interrupt Read. The ordinary sequential path records on Close.
func (b *body) recordClosed() {
	if b.closed && b.readers == 0 && !b.recorded {
		b.recorded = true
		b.usage.RecordFetch(b.bytes, time.Since(b.start))
	}
}

func (b *body) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.mu.Lock()
		b.closed = true
		b.recordClosed()
		b.mu.Unlock()
	})
	return b.err
}
