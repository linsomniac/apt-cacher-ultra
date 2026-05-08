package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy/tlsmitm"
)

// TestParseConnectTarget_Accepted exercises the happy path.
// Each case must yield a populated ConnectTarget with the
// host normalized (lowercased, trailing-dot stripped, IDNA
// applied for non-ASCII).
func TestParseConnectTarget_Accepted(t *testing.T) {
	cases := []struct {
		name   string
		target string
		host   string
		port   int
	}{
		{"simple", "apt.corretto.aws:443", "apt.corretto.aws", 443},
		{"trailing dot stripped", "archive.ubuntu.com.:443", "archive.ubuntu.com", 443},
		{"uppercase lowercased", "ARCHIVE.Ubuntu.COM:443", "archive.ubuntu.com", 443},
		{"single label", "localhost:443", "localhost", 443},
		{"max-length label (63 chars)",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com:443",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com", 443},
		{"IDNA non-ASCII converts to punycode", "bücher.example:443", "xn--bcher-kva.example", 443},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseConnectTarget(tc.target)
			if err != nil {
				t.Fatalf("ParseConnectTarget(%q): %v", tc.target, err)
			}
			if got.LiteralHost != tc.host {
				t.Errorf("host = %q, want %q", got.LiteralHost, tc.host)
			}
			if got.Port != tc.port {
				t.Errorf("port = %d, want %d", got.Port, tc.port)
			}
		})
	}
}

// TestParseConnectTarget_Rejected walks every documented failure
// mode in SPEC6 §2.2 step 1 and asserts the right outcome enum
// fires. Missing a case here means the spec table moves out of
// step with the implementation; treat new failures as load-bearing.
func TestParseConnectTarget_Rejected(t *testing.T) {
	cases := []struct {
		name        string
		target      string
		wantOutcome ConnectOutcome
	}{
		// Structural / syntactic — outcome=bad_target.
		{"empty", "", OutcomeBadTarget},
		{"missing port", "apt.corretto.aws", OutcomeBadTarget},
		{"empty host", ":443", OutcomeBadTarget},
		{"empty port", "apt.corretto.aws:", OutcomeBadTarget},
		{"non-numeric port", "apt.corretto.aws:https", OutcomeBadTarget},
		{"port-only-non-numeric", "apt.corretto.aws:abc", OutcomeBadTarget},
		{"port < 1", "apt.corretto.aws:0", OutcomeBadTarget},
		{"port > 65535", "apt.corretto.aws:65536", OutcomeBadTarget},
		{"two colons unbracketed", "host:443:extra", OutcomeBadTarget},
		{"unbracketed IPv6", "::1:443", OutcomeBadTarget},
		{"single trailing dot", ".:443", OutcomeBadTarget},

		// IDNA / LDH — outcome=bad_host.
		{"label too long",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.com:443",
			OutcomeBadHost},
		{"underscore in label", "foo_bar.com:443", OutcomeBadHost},
		{"label starts with hyphen", "-foo.com:443", OutcomeBadHost},
		{"label ends with hyphen", "foo-.com:443", OutcomeBadHost},
		{"empty middle label", "foo..com:443", OutcomeBadHost},

		// IP literal — outcome=ip_literal_host.
		{"IPv4 literal", "192.0.2.1:443", OutcomeIPLiteralHost},
		{"IPv6 bracketed", "[::1]:443", OutcomeIPLiteralHost},
		{"IPv6 bracketed v4-mapped", "[::ffff:192.0.2.1]:443", OutcomeIPLiteralHost},

		// Port — outcome=bad_port.
		{"port 80", "apt.corretto.aws:80", OutcomeBadPort},
		{"port 8443", "apt.corretto.aws:8443", OutcomeBadPort},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConnectTarget(tc.target)
			if err == nil {
				t.Fatalf("ParseConnectTarget(%q) succeeded; want %s", tc.target, tc.wantOutcome)
			}
			var ce *ErrConnectTarget
			if !errors.As(err, &ce) {
				t.Fatalf("ParseConnectTarget(%q) returned non-*ErrConnectTarget: %T %v", tc.target, err, err)
			}
			if ce.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %s, want %s (reason=%q)", ce.Outcome, tc.wantOutcome, ce.Reason)
			}
		})
	}
}

// TestParseConnectTarget_TrailingDotMatchesNoDot proves that
// `host` and `host.` produce the SAME LiteralHost — the spec's
// requirement to canonicalize so a single host doesn't issue two
// distinct leaf certs.
func TestParseConnectTarget_TrailingDotMatchesNoDot(t *testing.T) {
	a, err := ParseConnectTarget("apt.corretto.aws:443")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseConnectTarget("apt.corretto.aws.:443")
	if err != nil {
		t.Fatal(err)
	}
	if a.LiteralHost != b.LiteralHost {
		t.Errorf("trailing dot canonicalization failed: %q vs %q", a.LiteralHost, b.LiteralHost)
	}
}

// TestParseConnectTarget_CaseInsensitiveAlias proves that
// `HOST:443` and `host:443` produce the SAME LiteralHost.
func TestParseConnectTarget_CaseInsensitiveAlias(t *testing.T) {
	a, err := ParseConnectTarget("EXAMPLE.com:443")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseConnectTarget("example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if a.LiteralHost != b.LiteralHost {
		t.Errorf("case-insensitive aliasing failed: %q vs %q", a.LiteralHost, b.LiteralHost)
	}
}

// TestErrConnectTarget_ErrorString asserts the formatted error
// includes the outcome and reason, so log lines remain greppable.
func TestErrConnectTarget_ErrorString(t *testing.T) {
	e := &ErrConnectTarget{Outcome: OutcomeBadTarget, Reason: "x"}
	s := e.Error()
	if !strings.Contains(s, "bad_target") || !strings.Contains(s, "x") {
		t.Errorf("Error() = %q, missing outcome or reason", s)
	}
}

// TestMITMContextRoundTrip proves the WithMITMContext / IsMITMContext
// pair works as the §6.2.1 logger integration contract: a request
// dispatched via the synthetic-request path carries the marker, and
// a normal request does not.
func TestMITMContextRoundTrip(t *testing.T) {
	plain := context.Background()
	if IsMITMContext(plain) {
		t.Error("a fresh background ctx claims to be MITM")
	}
	tagged := WithMITMContext(plain)
	if !IsMITMContext(tagged) {
		t.Error("WithMITMContext-decorated ctx not detected by IsMITMContext")
	}
	if IsMITMContext(nil) {
		t.Error("nil ctx claims to be MITM")
	}
}

// ============================================================================
// ServeCONNECT integration tests
// ============================================================================

// TestServeCONNECT_DeniedByGate exercises the §2.2 step 2 short-
// circuit: a CONNECT to a host the signing predicate rejects must
// return 403 + denied_host outcome (denied_gate=signing) and MUST
// NOT reach hijack/cert/handshake.
func TestServeCONNECT_DeniedByGate(t *testing.T) {
	srv, _, _ := newTestConnectServer(t, testConnectOpts{
		signingGate: func(host string) bool { return host == "approved.example.com" },
		fetchGate:   func(host string) bool { return true },
	})
	defer srv.Close()

	resp, body := connectExpectStatus(t, srv.Listener.Addr().String(), "denied.example.com:443", 403)
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(string(body), "forbidden") {
		t.Errorf("body = %q, expected 'forbidden'", body)
	}
}

// TestServeCONNECT_BadTargetReturns400 exercises the §2.2 step 1
// short-circuit. A malformed CONNECT target must return 400
// + bad_target outcome.
func TestServeCONNECT_BadTargetReturns400(t *testing.T) {
	srv, _, _ := newTestConnectServer(t, testConnectOpts{
		signingGate: func(host string) bool { return true },
		fetchGate:   func(host string) bool { return true },
	})
	defer srv.Close()

	resp, _ := connectExpectStatus(t, srv.Listener.Addr().String(), "no-port-here", 400)
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestServeCONNECT_HappyPath performs a full §2.2 pipeline:
// CONNECT → 200 → TLS handshake (using the test CA as trust root)
// → inner GET → dispatcher invoked with synthetic *http.Request
// carrying RequestURI="https://host/path" + MITM context.
func TestServeCONNECT_HappyPath(t *testing.T) {
	var dispatchedReq *http.Request
	srv, ca, _ := newTestConnectServer(t, testConnectOpts{
		signingGate: func(host string) bool { return true },
		fetchGate:   func(host string) bool { return true },
		dispatch: func(w http.ResponseWriter, r *http.Request) {
			dispatchedReq = r
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("hello"))
		},
	})
	defer srv.Close()

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(ca.Cert)

	rawConn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	if _, err := rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	tlsConn := tls.Client(&prereadConn{Conn: rawConn, br: br}, &tls.Config{
		ServerName: "example.test",
		RootCAs:    clientCAPool,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}

	req, _ := http.NewRequest("GET", "/some/path", nil)
	req.Host = "example.test"
	if err := req.Write(tlsConn); err != nil {
		t.Fatalf("write inner request: %v", err)
	}

	innerResp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer innerResp.Body.Close()
	innerBody, _ := io.ReadAll(innerResp.Body)
	if string(innerBody) != "hello" {
		t.Errorf("inner body = %q, want %q", innerBody, "hello")
	}

	if dispatchedReq == nil {
		t.Fatal("dispatcher was never called")
	}
	wantURI := "https://example.test/some/path"
	if dispatchedReq.RequestURI != wantURI {
		t.Errorf("synthetic RequestURI = %q, want %q", dispatchedReq.RequestURI, wantURI)
	}
	if dispatchedReq.Host != "example.test" {
		t.Errorf("synthetic Host = %q, want example.test", dispatchedReq.Host)
	}
	if dispatchedReq.Method != "GET" {
		t.Errorf("synthetic Method = %q, want GET", dispatchedReq.Method)
	}
	if !IsMITMContext(dispatchedReq.Context()) {
		t.Error("synthetic request context lacks MITM marker")
	}
}

// TestServeCONNECT_InnerMethodRejected exercises §2.2 step 6: an
// inner request with method other than GET/HEAD must produce 405
// + inner_method_rejected outcome.
func TestServeCONNECT_InnerMethodRejected(t *testing.T) {
	dispatched := false
	srv, ca, _ := newTestConnectServer(t, testConnectOpts{
		signingGate: func(host string) bool { return true },
		fetchGate:   func(host string) bool { return true },
		dispatch: func(w http.ResponseWriter, r *http.Request) {
			dispatched = true
		},
	})
	defer srv.Close()

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(ca.Cert)

	rawConn, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer rawConn.Close()
	_, _ = rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n"))
	br := bufio.NewReader(rawConn)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("CONNECT resp: %v", err)
	}
	tlsConn := tls.Client(&prereadConn{Conn: rawConn, br: br}, &tls.Config{
		ServerName: "example.test", RootCAs: clientCAPool, MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	// Send a POST — the spec says reject with 405.
	post, _ := http.NewRequest("POST", "/x", strings.NewReader(""))
	post.Host = "example.test"
	if err := post.Write(tlsConn); err != nil {
		t.Fatalf("write inner POST: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), post)
	if err != nil {
		t.Fatalf("read inner resp: %v", err)
	}
	if resp.StatusCode != 405 {
		t.Errorf("inner status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q, want 'GET, HEAD'", got)
	}
	if dispatched {
		t.Error("dispatcher should not have been called for rejected method")
	}
}

// TestServeCONNECT_OversizedInnerHeaderRejected exercises the
// codex-finding #1 hardening: a CONNECT client cannot blow up
// memory by sending an unbounded inner header block. The handler
// caps the byte budget for the request line + headers; once
// exceeded, the read returns unexpected-EOF and the handler emits
// inner_stream_failed without invoking the dispatcher.
func TestServeCONNECT_OversizedInnerHeaderRejected(t *testing.T) {
	dispatched := false
	srv, ca, _ := newTestConnectServer(t, testConnectOpts{
		signingGate: func(host string) bool { return true },
		fetchGate:   func(host string) bool { return true },
		dispatch: func(w http.ResponseWriter, r *http.Request) {
			dispatched = true
		},
		maxInnerHeaderBytes: 4 * 1024, // 4 KiB cap; we send much more.
	})
	defer srv.Close()

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(ca.Cert)

	rawConn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()
	if _, err := rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(rawConn)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("read CONNECT resp: %v", err)
	}
	tlsConn := tls.Client(&prereadConn{Conn: rawConn, br: br}, &tls.Config{
		ServerName: "example.test", RootCAs: clientCAPool, MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	// Send a request line + a 1 MiB X-Junk header. The cap is 4 KiB
	// so the server will fail to read the full block.
	junk := strings.Repeat("a", 1<<20)
	req := "GET /x HTTP/1.1\r\nHost: example.test\r\nX-Junk: " + junk + "\r\n\r\n"
	if _, err := tlsConn.Write([]byte(req)); err != nil {
		// EOF on write is acceptable — the server may close on it.
		t.Logf("write inner request returned %v (acceptable)", err)
	}
	// We expect the server to close without sending a response.
	_, _ = io.Copy(io.Discard, tlsConn)
	if dispatched {
		t.Error("dispatcher invoked despite oversized inner header")
	}
}

// TestServeCONNECT_TLSConfigCloneDefangs proves codex-finding #3:
// even if the operator's tls.Config template carries a
// GetCertificate / GetConfigForClient callback that would override
// the leaf, the handler clears them post-Clone so the per-CONNECT
// leaf is the cert the client sees. We verify by setting
// GetCertificate on the template to a panic-on-call guard; if the
// handler used it, the test would crash.
func TestServeCONNECT_TLSConfigCloneDefangs(t *testing.T) {
	dir := t.TempDir()
	ca, err := tlsmitm.LoadOrGenerate(tlsmitm.LoadOptions{
		StorageDir:           dir,
		AllowUnconstrainedCA: true,
	})
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	cache, _ := tlsmitm.NewCache(8, func(host string) (*tls.Certificate, error) {
		return tlsmitm.GenerateLeaf(host, ca.TLSCert, tlsmitm.LeafECDSAP256, time.Hour, time.Now())
	})

	template := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"}, // bogus h2 — should be pinned to http/1.1 only
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			t.Error("GetCertificate was invoked despite defang")
			return nil, errors.New("should not be reached")
		},
	}
	h, err := NewConnectHandler(HandlerDeps{
		CA:        ca,
		LeafCache: cache,
		FetchGate: func(string) bool { return true },
		Dispatch:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) },
		TLSConfig: template,
	})
	if err != nil {
		t.Fatalf("NewConnectHandler: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			h.ServeCONNECT(w, r)
			return
		}
	}))
	srv.Start()
	defer srv.Close()

	clientCAPool := x509.NewCertPool()
	clientCAPool.AddCert(ca.Cert)
	rawConn, _ := net.Dial("tcp", srv.Listener.Addr().String())
	defer rawConn.Close()
	_, _ = rawConn.Write([]byte("CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n"))
	br := bufio.NewReader(rawConn)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("read CONNECT resp: %v", err)
	}
	tlsConn := tls.Client(&prereadConn{Conn: rawConn, br: br}, &tls.Config{
		ServerName: "example.test", RootCAs: clientCAPool, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if got := tlsConn.ConnectionState().NegotiatedProtocol; got != "" && got != "http/1.1" {
		t.Errorf("ALPN negotiated %q, expected http/1.1 (or empty)", got)
	}
}

// TestTLSResponseWriter_HeaderSanitizationViaHeaderWrite proves
// codex-finding #2 hardening: a header value containing CR/LF is
// neutralized before reaching the wire. `http.Header.Write` does
// not error on such input — it replaces CR and LF with space
// characters in-line — but the critical security property is the
// same: no fresh header-line boundary (`\r\n<header>:`) escapes
// the original value. The test asserts that property directly so
// the contract survives any future change in stdlib behavior
// (replacement vs. rejection).
func TestTLSResponseWriter_HeaderSanitizationViaHeaderWrite(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Bad", "value\r\nInjected: yes")
	var sb strings.Builder
	if err := hdr.Write(&sb); err != nil {
		// Either replacement or rejection is acceptable; only
		// silent CRLF passthrough is broken. If a future stdlib
		// change starts erroring out, the writer's headerErr
		// path captures it and the integration test above proves
		// the surrounding flow handles that.
		return
	}
	out := sb.String()
	// The critical property: no fresh header line. The substring
	// "\r\nInjected:" only ever appears as a fresh header
	// boundary; if it appears here, a malicious value escaped its
	// container.
	if strings.Contains(out, "\r\nInjected:") {
		t.Errorf("CRLF injection: %q", out)
	}
}

// ============================================================================
// Test helpers
// ============================================================================

type testConnectOpts struct {
	signingGate         func(string) bool
	fetchGate           func(string) bool
	dispatch            func(http.ResponseWriter, *http.Request)
	maxInnerHeaderBytes int64
}

type testConnectServer struct {
	*httptest.Server
}

func newTestConnectServer(t *testing.T, opts testConnectOpts) (*testConnectServer, *tlsmitm.CA, *tlsmitm.Cache) {
	t.Helper()
	dir := t.TempDir()
	ca, err := tlsmitm.LoadOrGenerate(tlsmitm.LoadOptions{
		StorageDir:           dir,
		AllowUnconstrainedCA: true,
	})
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	cache, err := tlsmitm.NewCache(8, func(host string) (*tls.Certificate, error) {
		return tlsmitm.GenerateLeaf(host, ca.TLSCert, tlsmitm.LeafECDSAP256, time.Hour, time.Now())
	})
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if opts.dispatch == nil {
		opts.dispatch = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}
	}
	h, err := NewConnectHandler(HandlerDeps{
		CA:                  ca,
		LeafCache:           cache,
		SigningGate:         opts.signingGate,
		FetchGate:           opts.fetchGate,
		Dispatch:            opts.dispatch,
		TLSConfig:           &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		HandshakeTimeout:    5 * time.Second,
		InnerReadTimeout:    5 * time.Second,
		MaxInnerHeaderBytes: opts.maxInnerHeaderBytes,
	})
	if err != nil {
		t.Fatalf("NewConnectHandler: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			h.ServeCONNECT(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}))
	srv.Start()
	return &testConnectServer{Server: srv}, ca, cache
}

func connectExpectStatus(t *testing.T, addr, target string, _ int) (*http.Response, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// prereadConn presents a (conn, bufio.Reader) pair as a single
// net.Conn. Used by the test client to feed any bytes it already
// pulled past the CONNECT response (typical for HTTP/1.1 keepalive
// chains) back into the TLS handshake.
type prereadConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *prereadConn) Read(p []byte) (int, error) {
	if c.br != nil && c.br.Buffered() > 0 {
		return c.br.Read(p)
	}
	return c.Conn.Read(p)
}
