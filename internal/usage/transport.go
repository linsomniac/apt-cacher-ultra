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
	Base    http.RoundTripper
	Usage   *Collector
	Observe func(Response)
}

// Response describes one transport attempt, including redirects and retries.
// Bytes counts actual reads, never Content-Length or destination file size.
type Response struct {
	Request   *http.Request
	Status    int
	Bytes     int64
	Duration  time.Duration
	Complete  bool
	ReadError bool
}

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.Base.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = &body{ReadCloser: resp.Body, usage: t.Usage, start: start,
			observe: t.Observe, response: Response{Request: r, Status: resp.StatusCode},
			length: resp.ContentLength}
	} else if t.Observe != nil {
		t.Observe(Response{Request: r, Duration: time.Since(start)})
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
	observe  func(Response)
	response Response
	length   int64
	eof      bool
}

func (b *body) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.readers++
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.bytes = add(b.bytes, int64(n))
	if err == io.EOF {
		b.eof = true
	} else if err != nil {
		b.response.ReadError = true
	}
	b.readers--
	r := b.recordClosed()
	b.mu.Unlock()
	b.record(r)
	return n, err
}

// Called under mu. Close may race with a blocked Read, so postpone accounting
// until its last return. Never hold mu across underlying I/O: Close must be
// able to interrupt Read. The ordinary sequential path records on Close.
func (b *body) recordClosed() *Response {
	if b.closed && b.readers == 0 && !b.recorded {
		b.recorded = true
		r := b.response
		r.Bytes, r.Duration = b.bytes, time.Since(b.start)
		r.Complete = !r.ReadError && (b.eof || b.length >= 0 && b.bytes == b.length)
		return &r
	}
	return nil
}

func (b *body) record(r *Response) {
	if r == nil {
		return
	}
	b.usage.RecordFetch(r.Bytes, r.Duration)
	if b.observe != nil {
		b.observe(*r)
	}
}

func (b *body) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		b.mu.Lock()
		b.closed = true
		r := b.recordClosed()
		b.mu.Unlock()
		b.record(r)
	})
	return b.err
}
