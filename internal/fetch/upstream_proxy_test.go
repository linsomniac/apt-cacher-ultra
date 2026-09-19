package fetch

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newUpstreamProxyClient(t *testing.T, opts Options) *Client {
	t.Helper()
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = time.Second
	}
	if opts.TotalTimeout == 0 {
		opts.TotalTimeout = 3 * time.Second
	}
	if opts.AllowedHostRegex == nil {
		opts.AllowedHostRegex = []string{`^archive\.example\.com$`}
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(c.httpClient.CloseIdleConnections)
	return c
}

type upstreamProxyRequest struct {
	method, requestURI, host, authorization string
}

// newFixedForwardProxy only connects to origin. It records the actual wire
// request before implementing ordinary HTTP forwarding or a CONNECT tunnel.
func newFixedForwardProxy(t *testing.T, origin *httptest.Server, useTLS bool) (*httptest.Server, func() []upstreamProxyRequest) {
	t.Helper()
	backend, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []upstreamProxyRequest
	tunnels := make(map[net.Conn]struct{})
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, upstreamProxyRequest{r.Method, r.RequestURI, r.Host, r.Header.Get("Proxy-Authorization")})
		mu.Unlock()
		if r.Method == http.MethodConnect {
			upstream, err := net.DialTimeout("tcp", origin.Listener.Addr().String(), time.Second)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer func() { _ = upstream.Close() }()
			conn, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("proxy hijack: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			mu.Lock()
			tunnels[conn] = struct{}{}
			mu.Unlock()
			defer func() {
				mu.Lock()
				delete(tunnels, conn)
				mu.Unlock()
			}()
			if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				return
			}
			if err := buffered.Flush(); err != nil {
				return
			}
			copyDone := make(chan struct{})
			go func() {
				defer close(copyDone)
				_, _ = io.Copy(upstream, buffered)
				_ = upstream.Close()
			}()
			_, _ = io.Copy(conn, upstream)
			_ = conn.Close()
			_ = upstream.Close()
			<-copyDone
			return
		}
		out := r.Clone(r.Context())
		out.URL.Scheme = backend.Scheme
		out.URL.Host = backend.Host
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		resp, err := transport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
	var proxy *httptest.Server
	if useTLS {
		proxy = httptest.NewUnstartedServer(handler)
		proxy.EnableHTTP2 = true
		proxy.StartTLS()
	} else {
		proxy = httptest.NewServer(handler)
	}
	t.Cleanup(func() {
		// httptest.Server cannot close connections after Hijack.
		mu.Lock()
		for conn := range tunnels {
			_ = conn.Close()
		}
		mu.Unlock()
		proxy.Close()
	})
	return proxy, func() []upstreamProxyRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamProxyRequest(nil), requests...)
	}
}

func TestUpstreamProxy_HTTPAndHTTPS(t *testing.T) {
	for _, proxyTLS := range []bool{false, true} {
		for _, originTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("proxyTLS=%t/originTLS=%t", proxyTLS, originTLS), func(t *testing.T) {
				const body = "Package: proxied\nVersion: 1.0\n"
				var originHits atomic.Int32
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					originHits.Add(1)
					if r.Host != "archive.example.com" || r.URL.RequestURI() != "/dists/stable/InRelease?test=1" {
						t.Errorf("origin target: Host=%q URI=%q", r.Host, r.URL.RequestURI())
					}
					if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "" {
						t.Error("proxy credentials leaked into origin request")
					}
					_, _ = io.WriteString(w, body)
				})
				var origin *httptest.Server
				if originTLS {
					origin = httptest.NewTLSServer(handler)
				} else {
					origin = httptest.NewServer(handler)
				}
				t.Cleanup(origin.Close)
				proxy, recorded := newFixedForwardProxy(t, origin, proxyTLS)
				roots := x509.NewCertPool()
				if originTLS {
					roots.AddCert(origin.Certificate())
				}
				if proxyTLS {
					roots.AddCert(proxy.Certificate())
				}
				t.Cleanup(SetRootCAsForTest(roots))
				proxyURL, _ := url.Parse(proxy.URL)
				proxyURL.User = url.UserPassword("proxy-user", "p@ss:word")
				// Even NO_PROXY=* must not bypass an explicitly configured proxy.
				t.Setenv("NO_PROXY", "*")
				t.Setenv("no_proxy", "*")
				c := newUpstreamProxyClient(t, Options{Proxy: proxyURL.String()})
				scheme := "http"
				if originTLS {
					scheme = "https"
				}
				targetURL := scheme + "://archive.example.com/dists/stable/InRelease?test=1"
				dst := &bufDst{}
				if _, err := c.Fetch(context.Background(), &Target{"archive.example.com", targetURL}, dst); err != nil {
					t.Fatalf("Fetch through proxy: %v", err)
				}
				if dst.String() != body || originHits.Load() != 1 {
					t.Fatalf("body=%q origin hits=%d", dst.String(), originHits.Load())
				}
				requests := recorded()
				if len(requests) != 1 {
					t.Fatalf("proxy requests=%v, want one", requests)
				}
				wantMethod, wantURI, wantHost := http.MethodGet, targetURL, "archive.example.com"
				if originTLS {
					wantMethod, wantURI, wantHost = http.MethodConnect, "archive.example.com:443", "archive.example.com:443"
				}
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:p@ss:word"))
				want := upstreamProxyRequest{wantMethod, wantURI, wantHost, wantAuth}
				if requests[0] != want {
					t.Errorf("proxy request=%+v, want %+v", requests[0], want)
				}
			})
		}
	}
}

func TestUpstreamProxy_VerifiesTLS(t *testing.T) {
	for _, test := range []struct {
		name, host        string
		proxyTLS, trusted bool
	}{
		{name: "untrusted origin", host: "archive.example.com"},
		{name: "wrong origin hostname", host: "wrong.invalid", trusted: true},
		{name: "untrusted proxy", host: "archive.example.com", proxyTLS: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var originHits atomic.Int32
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				originHits.Add(1)
				_, _ = io.WriteString(w, "must not be served")
			}))
			t.Cleanup(origin.Close)
			proxy, recorded := newFixedForwardProxy(t, origin, test.proxyTLS)
			roots := x509.NewCertPool()
			if test.trusted {
				roots.AddCert(origin.Certificate())
			}
			t.Cleanup(SetRootCAsForTest(roots))
			c := newUpstreamProxyClient(t, Options{Proxy: proxy.URL, AllowedHostRegex: []string{`^` + test.host + `$`}})
			_, err := c.Fetch(context.Background(), &Target{test.host, "https://" + test.host + "/InRelease"}, &bufDst{})
			if err == nil {
				t.Fatal("Fetch accepted an invalid TLS peer")
			}
			var unknownAuthority x509.UnknownAuthorityError
			var hostnameError x509.HostnameError
			if !errors.As(err, &unknownAuthority) && !errors.As(err, &hostnameError) {
				t.Fatalf("Fetch error=%v, want certificate verification error", err)
			}
			if originHits.Load() != 0 {
				t.Error("origin HTTP handler received request despite TLS failure")
			}
			wantProxyRequests := 1
			if test.proxyTLS {
				wantProxyRequests = 0
			}
			if len(recorded()) != wantProxyRequests {
				t.Errorf("proxy requests=%d, want %d", len(recorded()), wantProxyRequests)
			}
		})
	}
}

func TestUpstreamProxy_EmptyIgnoresEnvironment(t *testing.T) {
	for _, variable := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(variable, "http://environment-proxy.invalid:3128")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	t.Cleanup(origin.Close)
	var mu sync.Mutex
	var addresses []string
	c := newUpstreamProxyClient(t, Options{
		dialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			addresses = append(addresses, addr)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
		},
	})
	dst := &bufDst{}
	if _, err := c.Fetch(context.Background(), &Target{"archive.example.com", "http://archive.example.com/InRelease"}, dst); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(addresses) != 1 || addresses[0] != "archive.example.com:80" || dst.String() != "direct" {
		t.Fatalf("dialed=%v body=%q, want direct origin dial", addresses, dst.String())
	}
}

func TestUpstreamProxy_ConditionalAndAccessPolicy(t *testing.T) {
	var mu sync.Mutex
	var hosts []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.URL.Hostname())
		mu.Unlock()
		if !r.URL.IsAbs() {
			t.Errorf("proxy request URI is not absolute: %q", r.RequestURI)
		}
		switch r.URL.Path {
		case "/conditional":
			if r.Header.Get("If-None-Match") != `"v1"` || r.Header.Get("If-Modified-Since") != "Mon, 01 Jan 2024 00:00:00 GMT" {
				t.Error("conditional validators did not reach proxy")
			}
			w.WriteHeader(http.StatusNotModified)
		case "/blocked-redirect":
			http.Redirect(w, r, "http://blocked.invalid/final", http.StatusFound)
		case "/allowed-redirect":
			http.Redirect(w, r, "http://cdn.example.com/final", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "redirected")
		}
	}))
	t.Cleanup(proxy.Close)
	c := newUpstreamProxyClient(t, Options{Proxy: proxy.URL, AllowedHostRegex: []string{`^(archive|cdn)\.example\.com$`}})
	res, err := c.Conditional(context.Background(), &Target{"archive.example.com", "http://archive.example.com/conditional"}, `"v1"`, "Mon, 01 Jan 2024 00:00:00 GMT", 1024)
	if err != nil || res.Status != http.StatusNotModified || len(res.Body) != 0 {
		t.Fatalf("Conditional=%+v err=%v", res, err)
	}
	for _, conditional := range []bool{false, true} {
		target := &Target{"blocked.invalid", "http://blocked.invalid/initial"}
		if conditional {
			_, err = c.Conditional(context.Background(), target, "", "", 1024)
		} else {
			_, err = c.Fetch(context.Background(), target, &bufDst{})
		}
		if !errors.Is(err, ErrHostNotAllowed) {
			t.Fatalf("conditional=%t initial host error=%v", conditional, err)
		}
		target = &Target{"archive.example.com", "http://archive.example.com/blocked-redirect"}
		if conditional {
			_, err = c.Conditional(context.Background(), target, "", "", 1024)
		} else {
			_, err = c.Fetch(context.Background(), target, &bufDst{})
		}
		if !errors.Is(err, ErrRedirectBlocked) {
			t.Fatalf("conditional=%t redirect error=%v", conditional, err)
		}
	}
	dst := &bufDst{}
	if _, err := c.Fetch(context.Background(), &Target{"archive.example.com", "http://archive.example.com/allowed-redirect"}, dst); err != nil || dst.String() != "redirected" {
		t.Fatalf("allowed redirect body=%q err=%v", dst.String(), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(hosts, ","); got != "archive.example.com,archive.example.com,archive.example.com,archive.example.com,cdn.example.com" {
		t.Fatalf("proxy hosts=%q; blocked destinations must never reach proxy", got)
	}
}

func TestUpstreamProxy_ResumesRange(t *testing.T) {
	const body = "hello through a proxy"
	const offset = 6
	var attempts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI != "http://archive.example.com/package.deb" {
			t.Errorf("proxy request URI=%q", r.RequestURI)
		}
		if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("range-user:range-secret")); r.Header.Get("Proxy-Authorization") != want {
			t.Error("proxy authentication missing on initial request or retry")
		}
		if attempts.Add(1) == 1 {
			conn, buffered, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\n\r\n%s", len(body), body[:offset])
			_ = buffered.Flush()
			return
		}
		if r.Header.Get("Range") != "bytes=6-" || r.Header.Get("If-Range") != `"v1"` {
			t.Errorf("Range=%q If-Range=%q", r.Header.Get("Range"), r.Header.Get("If-Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(body)-1, len(body)))
		w.Header().Set("Content-Length", fmt.Sprint(len(body)-offset))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body[offset:])
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("range-user", "range-secret")
	c := newUpstreamProxyClient(t, Options{Proxy: proxyURL.String(), MaxRetries: 1})
	dst := &bufDst{}
	if _, err := c.Fetch(context.Background(), &Target{"archive.example.com", "http://archive.example.com/package.deb"}, dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if dst.String() != body || attempts.Load() != 2 {
		t.Fatalf("body=%q attempts=%d", dst.String(), attempts.Load())
	}
}

func TestUpstreamProxy_AuthenticationFailure(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		for _, conditional := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/conditional=%t", scheme, conditional), func(t *testing.T) {
				var hits atomic.Int32
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if (r.Method == http.MethodConnect) != (scheme == "https") {
						t.Errorf("proxy method=%s for %s origin", r.Method, scheme)
					}
					w.Header().Set("Proxy-Authenticate", `Basic realm="upstream"`)
					w.WriteHeader(http.StatusProxyAuthRequired)
					_, _ = io.WriteString(w, "proxy-secret-user proxy-secret-password")
				}))
				t.Cleanup(proxy.Close)
				proxyURL, _ := url.Parse(proxy.URL)
				proxyURL.User = url.UserPassword("proxy-secret-user", "proxy-secret-password")
				var logs bytes.Buffer
				c := newUpstreamProxyClient(t, Options{
					Proxy:      proxyURL.String(),
					MaxRetries: 3,
					Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
				})
				target := &Target{"archive.example.com", scheme + "://archive.example.com/InRelease"}
				dst := &bufDst{}
				var err error
				if conditional {
					var res *ConditionalResult
					res, err = c.Conditional(context.Background(), target, "", "", 1024)
					if res != nil {
						t.Errorf("authentication error returned conditional result: %+v", res)
					}
				} else {
					_, err = c.Fetch(context.Background(), target, dst)
				}
				if !errors.Is(err, ErrProxyAuthRequired) || !errors.Is(err, ErrUpstreamUnavailable) || errors.Is(err, ErrUpstreamStatus) {
					t.Fatalf("authentication error=%v, want proxy unavailable rather than origin 4xx", err)
				}
				if hits.Load() != 1 || dst.Written() != 0 {
					t.Errorf("proxy hits=%d cached bytes=%d, want one request and no body", hits.Load(), dst.Written())
				}
				for _, secret := range []string{"proxy-secret-user", "proxy-secret-password"} {
					if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
						t.Error("proxy credentials leaked into error or log")
					}
				}
			})
		}
	}
}

func TestUpstreamProxy_NewRejectsInvalidOptions(t *testing.T) {
	for _, test := range []struct {
		name, proxy string
		deny        []string
	}{
		{name: "unsupported scheme", proxy: "socks5://proxy.invalid:1080"},
		{name: "missing host", proxy: "http:///"},
		{name: "invalid port", proxy: "http://proxy.invalid:65536"},
		{name: "path with credentials", proxy: "http://secret-user:secret-password@proxy.invalid/path"},
		{name: "origin CIDR policy", proxy: "http://secret-user:secret-password@proxy.invalid:3128", deny: []string{"10.0.0.0/8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(Options{Proxy: test.proxy, DenyTargetRanges: test.deny})
			if err == nil {
				t.Fatal("New accepted invalid proxy configuration")
			}
			if strings.Contains(err.Error(), "secret-user") || strings.Contains(err.Error(), "secret-password") {
				t.Fatal("New error exposes proxy credentials")
			}
		})
	}
}

func TestUpstreamProxy_FailureNeverFallsBack(t *testing.T) {
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		originHits.Add(1)
		_, _ = io.WriteString(w, "direct bypass")
	}))
	t.Cleanup(origin.Close)
	for _, failure := range []string{"unreachable", "bad gateway"} {
		t.Run(failure, func(t *testing.T) {
			var proxyHits atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proxyHits.Add(1)
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(proxy.Close)
			if failure == "unreachable" {
				proxy.Close()
			}
			c := newUpstreamProxyClient(t, Options{Proxy: proxy.URL, MaxRetries: 1, AllowedHostRegex: []string{`^127\.0\.0\.1$`}})
			_, err := c.Fetch(context.Background(), &Target{"127.0.0.1", origin.URL + "/InRelease"}, &bufDst{})
			if !errors.Is(err, ErrUpstreamUnavailable) {
				t.Fatalf("Fetch error=%v, want ErrUpstreamUnavailable", err)
			}
			if failure == "bad gateway" && proxyHits.Load() != 2 {
				t.Errorf("proxy hits=%d, want two bounded attempts", proxyHits.Load())
			}
			if originHits.Load() != 0 {
				t.Fatal("failed proxy fell back to direct origin")
			}
		})
	}
}

func TestUpstreamProxy_TotalTimeoutIncludesCONNECT(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("method=%q, want CONNECT", r.Method)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(func() { close(release) })
	c := newUpstreamProxyClient(t, Options{Proxy: proxy.URL, TotalTimeout: 100 * time.Millisecond, MaxRetries: 3})
	begin := time.Now()
	_, err := c.Fetch(context.Background(), &Target{"archive.example.com", "https://archive.example.com/InRelease"}, &bufDst{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Errorf("CONNECT ignored total timeout: %v", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("timeout test never reached proxy CONNECT handler")
	}
}
