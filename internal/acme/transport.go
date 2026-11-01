package acme

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

func allowedURL(u *url.URL, authority string) bool {
	return u != nil && u.Scheme == "https" && origin(u) == authority && u.User == nil && u.Opaque == "" && u.Fragment == "" && u.RawQuery == "" && !u.ForceQuery
}

type authorityTransport struct {
	base      http.RoundTripper
	authority string
	observe   func(*http.Response) error
}

func (t authorityTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func (t authorityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !allowedURL(r.URL, t.authority) {
		return nil, errors.New("ACME resource URL rejected")
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if t.observe != nil {
		if err := t.observe(resp); err != nil {
			_ = resp.Body.Close()
			return nil, err
		}
	}
	// x/crypto/acme parses integer Retry-After values with time.Duration
	// arithmetic in both error retries and authorization/order polling. Give
	// those paths a date so an oversized value cannot wrap into a short retry.
	// The observer above must first persist the original authority cooldown.
	now := time.Now()
	if next := retryAfter(resp.Header.Get("Retry-After"), now); next.After(now) {
		// HTTP dates have second precision. Round upward to avoid shortening it.
		if next.Nanosecond() != 0 {
			next = next.Truncate(time.Second).Add(time.Second)
		}
		resp.Header.Set("Retry-After", next.UTC().Format(http.TimeFormat))
	}
	resp.Body = &boundedBody{ReadCloser: resp.Body, left: 2 << 20}
	return resp, nil
}

type boundedBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("ACME response exceeds size limit")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

func newHTTPClient(directory string, base http.RoundTripper, observe func(*http.Response) error) *http.Client {
	u, _ := url.Parse(directory)
	if base == nil {
		base = &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout: 30 * time.Second, MaxIdleConns: 2, MaxConnsPerHost: 4, MaxResponseHeaderBytes: 32 << 10, ForceAttemptHTTP2: true}
	}
	authority := origin(u)
	return &http.Client{Transport: authorityTransport{base: base, authority: authority, observe: observe}, Timeout: 30 * time.Second,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !allowedURL(r.URL, authority) {
				return errors.New("ACME redirect rejected")
			}
			return nil
		}}
}

// retryAfter accepts dates and bounded integer seconds without duration overflow.
func retryAfter(value string, now time.Time) time.Time {
	digits := value != ""
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		n, err := strconv.ParseUint(value, 10, 64)
		// Never shorten an authority cooldown due to integer/duration overflow.
		if err != nil || n > uint64((100*365*24*time.Hour)/time.Second) {
			return time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
		}
		return now.Add(time.Duration(n) * time.Second)
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		return t
	}
	return time.Time{}
}
