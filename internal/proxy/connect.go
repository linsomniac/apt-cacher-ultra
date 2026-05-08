// Package proxy / connect.go implements the SPEC6 §2.2 CONNECT
// method handler. When tls_mitm.enabled = true, the proxy listener
// accepts CONNECT and the handler in this file performs:
//
//  1. Request-target parsing (host, port) with all the structural,
//     IDNA, and IP-literal rejection rules.
//  2. The §5.1.2 effective allowlist (signing gate + fetch gate).
//  3. Hijack of the underlying TCP connection and a 200-Connection-
//     Established line.
//  4. Leaf cert lookup (singleflight per literal host) backed by
//     internal/proxy/tlsmitm.Cache.
//  5. TLS handshake on the hijacked conn using the leaf cert.
//  6. Read of exactly ONE inner HTTP/1.1 GET or HEAD request from
//     the encrypted stream.
//  7. Dispatch of a synthetic *http.Request into the existing
//     handler.Handler pipeline (via an injected dispatcher to
//     avoid an import cycle with the handler package).
//  8. Tunnel close. No multi-request keepalive — apt does not need
//     it and supporting it would expand attack surface.
//
// This file holds the handler skeleton and the request-target
// parser; subsequent revisions will fill in the TLS handshake and
// dispatch path. The parser is the critical-correctness piece (a
// shape it gets wrong would let an IP-literal or malformed CONNECT
// through to cert generation), so it is the first thing that
// lands and the most heavily unit-tested.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy/tlsmitm"
)

// ConnectOutcome names the SPEC6 §10.1 mitm_connect Warn outcomes.
// Strings match the spec's enum values exactly.
type ConnectOutcome string

const (
	OutcomeBadTarget           ConnectOutcome = "bad_target"
	OutcomeBadHost             ConnectOutcome = "bad_host"
	OutcomeIPLiteralHost       ConnectOutcome = "ip_literal_host"
	OutcomeBadPort             ConnectOutcome = "bad_port"
	OutcomeDeniedHost          ConnectOutcome = "denied_host"
	OutcomeCertGenFailed       ConnectOutcome = "cert_gen_failed"
	OutcomeTLSFailed           ConnectOutcome = "tls_failed"
	OutcomeTLSHandshakeTimeout ConnectOutcome = "tls_handshake_timeout"
	OutcomeInnerMethodRejected ConnectOutcome = "inner_method_rejected"
	OutcomeInnerHeaderTimeout  ConnectOutcome = "inner_header_timeout"
	OutcomeInnerHeaderTooLarge ConnectOutcome = "inner_header_too_large"
	OutcomeInnerStreamFailed   ConnectOutcome = "inner_stream_failed"
	OutcomeTunneled            ConnectOutcome = "tunneled"
)

// ConnectTarget is the parsed-and-validated CONNECT request target.
// Constructed by ParseConnectTarget; the caller never builds one by
// hand because the parser's validation is the single source of
// truth for "this CONNECT is acceptable."
type ConnectTarget struct {
	// LiteralHost is the lower-cased, IDNA-normalized host with any
	// trailing dot stripped. This is the key the leaf-cert cache uses
	// (SPEC6 §5.1.3) and the value that goes into the leaf cert's
	// Subject CN + dNSName SAN.
	LiteralHost string

	// Port is the validated TCP port (always 443 in Phase 6 — see
	// the bad_port branch in ParseConnectTarget).
	Port int
}

// ErrConnectTarget wraps a request-target parsing failure with the
// SPEC6 §10.1 outcome enum value the handler should log. Wrap-checked
// via errors.As so the dispatcher can route to the right
// HTTP-status / log shape.
type ErrConnectTarget struct {
	Outcome ConnectOutcome
	Reason  string
}

func (e *ErrConnectTarget) Error() string { return string(e.Outcome) + ": " + e.Reason }

// ParseConnectTarget implements SPEC6 §2.2 step 1 in full. The
// caller passes the raw request-target wire form (the value Go's
// `net/http` server hands us as `r.RequestURI` for a CONNECT). The
// parser returns either a fully-validated ConnectTarget or an
// *ErrConnectTarget naming the structural / host / port outcome
// the handler must log.
//
// Order of checks (the first failure short-circuits):
//
//  1. Empty request-target → bad_target.
//  2. Structural parse: split into (host, port). Bracketed IPv6
//     forms are recognized; missing brackets on what looks like
//     IPv6 fall to bad_target per §2.2 step 1.
//  3. Empty host / empty port / non-numeric port / port out of
//     range → bad_target.
//  4. Trailing-dot stripping on the host (legitimate FQDN form;
//     no Warn).
//  5. IDNA `Lookup.ToASCII` on the host → bad_host on failure.
//  6. Per-label LDH validation after IDNA → bad_host on failure.
//  7. IP-literal detection (dotted-quad IPv4 or bracket-stripped
//     IPv6 that parses as netip.Addr) → ip_literal_host.
//  8. Port == 443 → otherwise bad_port.
//
// Lower-casing happens AFTER IDNA so the punycode form is not
// re-cased; punycode is already ASCII.
func ParseConnectTarget(target string) (*ConnectTarget, error) {
	if target == "" {
		return nil, &ErrConnectTarget{OutcomeBadTarget, "empty request-target"}
	}

	host, port, err := splitConnectTarget(target)
	if err != nil {
		return nil, &ErrConnectTarget{OutcomeBadTarget, err.Error()}
	}
	if host == "" {
		return nil, &ErrConnectTarget{OutcomeBadTarget, "empty host"}
	}
	if port == "" {
		return nil, &ErrConnectTarget{OutcomeBadTarget, "empty port"}
	}
	portN, err := strconv.Atoi(port)
	if err != nil {
		return nil, &ErrConnectTarget{OutcomeBadTarget, "non-numeric port: " + port}
	}
	if portN < 1 || portN > 65535 {
		return nil, &ErrConnectTarget{OutcomeBadTarget, fmt.Sprintf("port out of range: %d", portN)}
	}

	// Bracketed IPv6 short-circuit: if splitConnectTarget handed us an
	// IPv6 host (we detect via "[]" stripping inside splitConnectTarget)
	// it is by definition an IP literal — reject before IDNA.
	if isIPv6Bracketed(target) {
		return nil, &ErrConnectTarget{OutcomeIPLiteralHost, "bracketed IPv6 literal: " + host}
	}

	// Trailing-dot canonicalization. Stripped silently — RFC 3986 /
	// RFC 1034 absolute-form FQDN is a legitimate input shape.
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return nil, &ErrConnectTarget{OutcomeBadTarget, "host was a single trailing dot"}
	}

	// IDNA normalization (Lookup profile). This rejects:
	// - invalid Unicode
	// - malformed punycode
	// - labels > 63 octets
	// - total length > 253 octets
	// - bidi / contextual rules.
	asciiHost, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return nil, &ErrConnectTarget{OutcomeBadHost, "IDNA: " + err.Error()}
	}
	asciiHost = strings.ToLower(asciiHost)

	// Per-label LDH validation. IDNA accepts more shapes than the
	// SPEC6 §2.2 contract (`[A-Za-z0-9-]` with no leading/trailing
	// hyphen), so we re-validate post-IDNA.
	if err := validateHostLDH(asciiHost); err != nil {
		return nil, &ErrConnectTarget{OutcomeBadHost, err.Error()}
	}

	// IP-literal post-IDNA check. A dotted-quad IPv4 has no IDNA
	// transformation effect; we test by attempting netip parse.
	if _, err := netip.ParseAddr(asciiHost); err == nil {
		return nil, &ErrConnectTarget{OutcomeIPLiteralHost, "host parses as IP literal: " + asciiHost}
	}

	if portN != 443 {
		return nil, &ErrConnectTarget{OutcomeBadPort, fmt.Sprintf("port %d not allowed (only 443)", portN)}
	}

	return &ConnectTarget{LiteralHost: asciiHost, Port: portN}, nil
}

// splitConnectTarget separates host and port. Distinguishes:
//   - bracketed IPv6: [::1]:443 → ("[::1]", "443") then strips
//     brackets → ("::1", "443"). The bracket presence is preserved
//     in `bracketed` so caller can route to the IPv6-literal
//     rejection.
//   - normal: host:443 → (host, 443).
//   - error shapes: "::1:443", "host:443:extra".
//
// Returns the err message bare so the caller can attach the outcome
// enum.
func splitConnectTarget(target string) (host, port string, err error) {
	// Use net.SplitHostPort for the common cases; it understands
	// bracketed IPv6 already.
	host, port, err = net.SplitHostPort(target)
	if err == nil {
		return host, port, nil
	}
	// SplitHostPort rejects "::1:443" as "too many colons" — that's
	// the unbracketed-IPv6 case the spec wants us to call bad_target.
	return "", "", err
}

// isIPv6Bracketed reports whether the original wire-form target
// had bracket-form host marker, i.e. begins with `[`. Useful AFTER
// SplitHostPort because by then the brackets are stripped.
func isIPv6Bracketed(target string) bool {
	return strings.HasPrefix(target, "[")
}

// validateHostLDH walks each label of a normalized hostname and
// asserts the LDH (letter-digit-hyphen) form: each label is
// `[A-Za-z0-9-]+`, neither leading nor trailing `-`, between 1 and
// 63 octets. The whole host must be ≤ 253 octets.
//
// Wildcards (`*.`) are NOT accepted at CONNECT time — apt never
// sends them and an attacker putting a `*` in the CONNECT target
// is anomalous, so the cert path rejects.
//
// Empty intermediate labels (`foo..com`) are rejected; trailing
// dot is stripped earlier in ParseConnectTarget so a final empty
// label cannot reach here.
func validateHostLDH(host string) error {
	if host == "" {
		return errors.New("empty host")
	}
	if len(host) > 253 {
		return fmt.Errorf("host too long: %d > 253", len(host))
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return errors.New("empty label (consecutive dots?)")
		}
		if len(label) > 63 {
			return fmt.Errorf("label too long: %d > 63", len(label))
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("label starts or ends with '-'")
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r >= '0' && r <= '9':
			case r == '-':
			default:
				return fmt.Errorf("illegal character %q in label", r)
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------------
// CONNECT handler skeleton
// ----------------------------------------------------------------------------

// HandlerDeps bundles the dependencies the CONNECT handler needs.
// The fields are wired by main.go (or tests) at startup; the
// connect package never imports the handler package directly so
// there is no import cycle.
type HandlerDeps struct {
	// CA is the loaded trust anchor. The package signs every leaf
	// from CA.PrivateKey.
	CA *tlsmitm.CA

	// LeafCache is the bounded LRU + per-host singleflight cache.
	// Get(host) hits this on every CONNECT.
	LeafCache *tlsmitm.Cache

	// SigningGate, when non-nil, is called per CONNECT against the
	// literal lower-cased host. nil = no MITM-side narrowing
	// (`tls_mitm.allowed_host_regex` empty); the fetch gate alone
	// applies. A non-nil function returning false denies with
	// outcome=denied_host, denied_gate=signing.
	SigningGate func(literalHost string) bool

	// FetchGate is the canonical-host allowlist predicate. The
	// implementation typically runs the literal host through
	// Remap canonicalization first (done by the inner handler's
	// parser when the synthetic request is dispatched), but the
	// CONNECT-time check uses the same allowlist on the literal
	// host with Remap applied here. Returning false denies with
	// outcome=denied_host, denied_gate=fetch.
	FetchGate func(canonicalHost string) bool

	// Canonicalize maps a literal host to the Remap-canonical form
	// used by the fetch gate. Provided by the proxy.Parser; nil =
	// identity mapping (use the literal host).
	Canonicalize func(literalHost string) string

	// Dispatch dispatches a synthetic *http.Request to the existing
	// handler.Handler.ServeHTTP. Wired at startup so the connect
	// package does not import the handler package. The
	// ResponseWriter is the encrypted-stream wrapper this package
	// constructs.
	Dispatch func(http.ResponseWriter, *http.Request)

	// TLSConfig is the base *tls.Config the handshake uses. The
	// handler clones it and sets Certificates per-CONNECT; the
	// caller-supplied template provides MinVersion,
	// CipherSuites, NextProtos (HTTP/1.1 only per §5.4), and any
	// other policy.
	TLSConfig *tls.Config

	// HandshakeTimeout caps the TLS handshake. Default 30s if zero.
	HandshakeTimeout time.Duration

	// InnerReadTimeout caps how long the handler waits for the
	// first byte of the inner request after handshake completes.
	// Default 30s if zero.
	InnerReadTimeout time.Duration

	// MaxInnerHeaderBytes caps the byte budget for the inner
	// request line + headers. Default 1 MiB if zero — matching
	// `net/http`'s `DefaultMaxHeaderBytes`. The TLS-protected
	// stream is wrapped in an `io.LimitReader` of this size before
	// `http.ReadRequest`, so a malicious-but-allowed CONNECT client
	// cannot drive memory growth by sending an unbounded header
	// block. If the cap is exceeded, ReadRequest fails with
	// `unexpected EOF` and the handler emits
	// `outcome=inner_stream_failed`.
	MaxInnerHeaderBytes int64

	// LogFn is the structured-event sink for §10.1 mitm_connect
	// log lines. nil = swallow.
	LogFn func(level, event string, fields map[string]any)

	// Stats, when non-nil, receives a Record(outcome) call for
	// every CONNECT outcome the handler emits. The §9.7.6
	// refresher reads its rolling window for the
	// `tls_mitm_enabled_ca_undistributed` Warn. Optional — leaving
	// it nil makes outcome recording a no-op.
	Stats *ConnectStats
}

// ConnectHandler holds the SPEC6 §2.2 CONNECT pipeline. Construct
// via NewConnectHandler; the zero value is unusable.
type ConnectHandler struct {
	deps HandlerDeps
}

// NewConnectHandler validates HandlerDeps and returns a handler.
// CA, LeafCache, FetchGate, Dispatch, and TLSConfig are required;
// SigningGate, Canonicalize, and LogFn are optional.
func NewConnectHandler(deps HandlerDeps) (*ConnectHandler, error) {
	if deps.CA == nil {
		return nil, errors.New("connect: nil CA")
	}
	if deps.LeafCache == nil {
		return nil, errors.New("connect: nil LeafCache")
	}
	if deps.FetchGate == nil {
		return nil, errors.New("connect: nil FetchGate")
	}
	if deps.Dispatch == nil {
		return nil, errors.New("connect: nil Dispatch")
	}
	if deps.TLSConfig == nil {
		return nil, errors.New("connect: nil TLSConfig")
	}
	if deps.HandshakeTimeout == 0 {
		deps.HandshakeTimeout = 30 * time.Second
	}
	if deps.InnerReadTimeout == 0 {
		deps.InnerReadTimeout = 30 * time.Second
	}
	if deps.MaxInnerHeaderBytes == 0 {
		deps.MaxInnerHeaderBytes = 1 << 20 // 1 MiB, matches net/http default
	}
	if deps.LogFn == nil {
		deps.LogFn = func(level, event string, fields map[string]any) {}
	}
	return &ConnectHandler{deps: deps}, nil
}

// ServeCONNECT performs the §2.2 CONNECT pipeline end-to-end. It
// MUST be called only when r.Method == "CONNECT" and the proxy
// listener has tls_mitm enabled; the handler.Handler dispatcher
// verifies this before calling.
//
// The handler emits exactly ONE mitm_connect log line per CONNECT
// at conn close, with the outcome that classifies the result. The
// outcome enum mirrors SPEC6 §10.1 and the wire-form per-outcome
// HTTP responses below.
func (h *ConnectHandler) ServeCONNECT(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	clientAddr := r.RemoteAddr

	// Step 1: parse the request-target.
	target, err := ParseConnectTarget(r.RequestURI)
	if err != nil {
		var ce *ErrConnectTarget
		errors.As(err, &ce)
		http.Error(w, "bad request", http.StatusBadRequest)
		h.warnConnect(ce.Outcome, "", 0, clientAddr, start, ce.Reason, "", false)
		return
	}

	// Step 2: gates.
	if h.deps.SigningGate != nil && !h.deps.SigningGate(target.LiteralHost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		h.warnConnect(OutcomeDeniedHost, target.LiteralHost, target.Port, clientAddr, start, "denied by signing predicate", "signing", false)
		return
	}
	canonicalHost := target.LiteralHost
	if h.deps.Canonicalize != nil {
		canonicalHost = h.deps.Canonicalize(target.LiteralHost)
	}
	if !h.deps.FetchGate(canonicalHost) {
		http.Error(w, "forbidden", http.StatusForbidden)
		h.warnConnect(OutcomeDeniedHost, target.LiteralHost, target.Port, clientAddr, start, "denied by fetch predicate", "fetch", false)
		return
	}

	// Step 3: hijack and 200 Connection Established.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "ResponseWriter is not Hijacker", "", false)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "hijack: "+err.Error(), "", false)
		return
	}
	defer conn.Close()

	if _, err := brw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "write 200: "+err.Error(), "", false)
		return
	}
	if err := brw.Flush(); err != nil {
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "flush 200: "+err.Error(), "", false)
		return
	}

	// Step 4: leaf cert lookup.
	leaf, err := h.deps.LeafCache.Get(target.LiteralHost)
	if err != nil {
		h.warnConnect(OutcomeCertGenFailed, target.LiteralHost, target.Port, clientAddr, start, "leaf gen: "+err.Error(), "", false)
		return
	}

	// Step 5: TLS handshake on the hijacked conn.
	//
	// Clone the operator template, then DEFANG it: explicitly clear
	// any cert-selection callbacks that could otherwise override the
	// per-CONNECT leaf cert this package owns. The "leaf cert is
	// chosen by ParseConnectTarget+LeafCache" invariant must not be
	// silently subverted by a future caller wiring up
	// `GetCertificate` on the template (e.g. for SNI-based fallback).
	// NextProtos is pinned to HTTP/1.1 per §5.4 — Phase 6 does not
	// implement HTTP/2 inside the tunnel.
	tlsCfg := h.deps.TLSConfig.Clone()
	tlsCfg.Certificates = []tls.Certificate{*leaf}
	tlsCfg.GetCertificate = nil
	tlsCfg.GetConfigForClient = nil
	tlsCfg.NextProtos = []string{"http/1.1"}
	tlsConn := tls.Server(&hijackedConn{Conn: conn, br: brw.Reader}, tlsCfg)
	if err := conn.SetDeadline(time.Now().Add(h.deps.HandshakeTimeout)); err != nil {
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "set deadline: "+err.Error(), "", false)
		return
	}
	handshakeStart := time.Now()
	if err := tlsConn.Handshake(); err != nil {
		outcome := OutcomeTLSFailed
		if isDeadlineExceeded(err) {
			outcome = OutcomeTLSHandshakeTimeout
		}
		h.warnConnect(outcome, target.LiteralHost, target.Port, clientAddr, start, "tls: "+err.Error(), "", false)
		return
	}
	mitmHandshakeDurationSeconds.Observe(time.Since(handshakeStart).Seconds())
	defer tlsConn.Close()
	// Past this point the TLS handshake has succeeded; record-call
	// sites pass tlsReached=true so post-handshake
	// `inner_stream_failed` correctly classifies as "TLS handshake
	// reached" per §5.3.

	// Step 6: read exactly one inner request.
	//
	// Cap the bytes ReadRequest may consume for the request line +
	// headers. http.ReadRequest by itself has no header-byte limit
	// (the HTTP server's MaxHeaderBytes is enforced by the server's
	// internal request reader, NOT by ReadRequest), so an authenticated
	// CONNECT client could otherwise drive arbitrary memory growth
	// by sending a huge header block. The cap is enforced via
	// io.LimitReader; when the limit is reached mid-header,
	// ReadRequest sees an unexpected EOF and we report
	// inner_stream_failed.
	if err := conn.SetDeadline(time.Now().Add(h.deps.InnerReadTimeout)); err != nil {
		h.warnConnect(OutcomeInnerStreamFailed, target.LiteralHost, target.Port, clientAddr, start, "set inner deadline: "+err.Error(), "", true)
		return
	}
	limited := &countingLimitReader{r: io.LimitReader(tlsConn, h.deps.MaxInnerHeaderBytes), cap: h.deps.MaxInnerHeaderBytes}
	innerBR := bufio.NewReader(limited)
	innerReq, err := http.ReadRequest(innerBR)
	if err != nil {
		outcome := OutcomeInnerStreamFailed
		switch {
		case isDeadlineExceeded(err):
			// SPEC6 §11 F11a: slowloris on the inner request.
			outcome = OutcomeInnerHeaderTimeout
		case limited.exhausted():
			// SPEC6 §11 F11b: byte cap fired before \r\n\r\n.
			outcome = OutcomeInnerHeaderTooLarge
		}
		h.warnConnect(outcome, target.LiteralHost, target.Port, clientAddr, start, "read inner: "+err.Error(), "", true)
		return
	}
	if innerReq.Method != http.MethodGet && innerReq.Method != http.MethodHead {
		writeInnerStatus(tlsConn, http.StatusMethodNotAllowed, "GET, HEAD")
		h.warnConnect(OutcomeInnerMethodRejected, target.LiteralHost, target.Port, clientAddr, start, "inner method: "+innerReq.Method, "", true)
		return
	}

	// Step 7: synthesize the *http.Request and dispatch into the
	// existing handler pipeline.
	syntheticURI := "https://" + target.LiteralHost + innerReq.RequestURI
	syntheticHeader := innerReq.Header.Clone()
	stripHopByHop(syntheticHeader)
	syntheticHeader.Set("Host", target.LiteralHost)
	synth := &http.Request{
		Method:     innerReq.Method,
		RequestURI: syntheticURI,
		URL:        innerReq.URL,
		Host:       target.LiteralHost,
		Header:     syntheticHeader,
		Body:       http.NoBody,
		RemoteAddr: clientAddr,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	synth = synth.WithContext(WithMITMContext(r.Context()))

	// Wrap the encrypted stream as an http.ResponseWriter the
	// handler can write to. Clear the deadline before dispatch so
	// the inner GET runs under handler timeouts, not connect's
	// inner-read-timeout.
	_ = conn.SetDeadline(time.Time{})
	rw := newTLSResponseWriter(tlsConn)
	rw.Header().Set("X-Acu-Mitm", "1")
	h.deps.Dispatch(rw, synth)

	// Tunnel close. Phase 6 does NOT support multi-request keepalive
	// (§2.2 step 8).
	h.infoConnect(OutcomeTunneled, target.LiteralHost, target.Port, clientAddr, start, "", true)
}

// stripHopByHop removes hop-by-hop header fields from h. Defensive
// only — apt never sends Connection-Upgrade etc., but a forwarded
// proxy chain might.
func stripHopByHop(h http.Header) {
	for _, k := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade",
	} {
		h.Del(k)
	}
}

// countingLimitReader wraps an io.LimitReader and tracks whether the
// limit was reached. Used to distinguish SPEC6 §11 F11a (slowloris,
// deadline-exceeded) from F11b (byte cap exhausted) when
// http.ReadRequest fails to parse the inner request.
type countingLimitReader struct {
	r        io.Reader
	cap      int64
	consumed int64
}

func (c *countingLimitReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.consumed += int64(n)
	return n, err
}

// exhausted reports whether the cap was reached. The wrapped
// io.LimitReader returns EOF once the cap is consumed, so the cap
// being reached is the F11b signal.
func (c *countingLimitReader) exhausted() bool {
	return c.consumed >= c.cap
}

func isDeadlineExceeded(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// ----------------------------------------------------------------------------
// Hijacked-conn adapter: pre-TLS bytes already consumed by the
// HTTP server live in the bufio.Reader brw, so when we wrap the
// raw conn for tls.Server we must replay those bytes first or the
// TLS handshake will see a corrupted ClientHello. The spec's
// "200 Connection established" line is written THEN the client
// starts speaking TLS — but the *http.Server's bufio buffer may
// have already pulled bytes past the CONNECT line. The hijackedConn
// drains brw before reading from the underlying conn so the TLS
// stream is byte-aligned.
// ----------------------------------------------------------------------------

type hijackedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *hijackedConn) Read(p []byte) (int, error) {
	if c.br != nil && c.br.Buffered() > 0 {
		return c.br.Read(p)
	}
	return c.Conn.Read(p)
}

// ----------------------------------------------------------------------------
// MITM context marker — the §10.1 logger integration contract.
// ----------------------------------------------------------------------------

type mitmCtxKey struct{}

// WithMITMContext attaches the MITM-dispatched marker so the
// handler's logRequest can detect the synthetic request originated
// from CONNECT. SPEC6 §6.2.1 names this as the logger integration
// contract for the §10.1 `mitm` log field on the inner GET's
// request log line.
func WithMITMContext(parent context.Context) context.Context {
	return context.WithValue(parent, mitmCtxKey{}, struct{}{})
}

// IsMITMContext reports whether ctx was decorated by WithMITMContext
// somewhere in the call chain. handler.Handler.logRequest calls
// this so the inner GET's log line carries `mitm=true` when the
// request originated from a §2.2 CONNECT.
func IsMITMContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	return ctx.Value(mitmCtxKey{}) != nil
}

// ----------------------------------------------------------------------------
// TLS-stream ResponseWriter — wraps the encrypted stream so the
// existing handler pipeline can write to it. Mirrors the minimal
// http.ResponseWriter contract the handler.Handler exercises:
// Header(), WriteHeader(int), Write([]byte). No Flusher / Hijacker
// — the inner request is single-shot and the outer hijack already
// happened.
// ----------------------------------------------------------------------------

type tlsResponseWriter struct {
	conn        *tls.Conn
	hdr         http.Header
	wroteHeader bool
	status      int
	headerErr   error // sticky; surfaced on the first Write after a failed header flush
}

func newTLSResponseWriter(c *tls.Conn) *tlsResponseWriter {
	return &tlsResponseWriter{conn: c, hdr: http.Header{}, status: http.StatusOK}
}

func (w *tlsResponseWriter) Header() http.Header { return w.hdr }

// WriteHeader flushes status + headers to the encrypted stream.
//
// Header field serialization goes through `http.Header.Write`,
// which validates field names and rejects values containing CR/LF
// — this closes a header-injection / response-splitting hole that
// a manual `fmt.Fprintf` loop would leave open. The status line is
// formatted by `fmt.Fprintf` against a hard-coded `HTTP/1.1` token
// and `http.StatusText`, both of which are not attacker-influenced.
//
// Any I/O error encountered during the header flush is captured on
// `headerErr` so the very next call to `Write` returns it (mirrors
// `net/http`'s own ResponseWriter contract — a header-write failure
// must not be silently dropped only to surface as a body-write
// success).
func (w *tlsResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if _, err := fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		w.headerErr = err
		return
	}
	if err := w.hdr.Write(w.conn); err != nil {
		w.headerErr = err
		return
	}
	if _, err := w.conn.Write([]byte("\r\n")); err != nil {
		w.headerErr = err
		return
	}
}

func (w *tlsResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.headerErr != nil {
		return 0, w.headerErr
	}
	return w.conn.Write(p)
}

// writeInnerStatus writes a tiny HTTP/1.1 status response onto the
// TLS conn. Used for §2.2 step 6 inner-method-rejected (405). Body
// is empty; status text mirrors http.StatusText.
func writeInnerStatus(c *tls.Conn, status int, allow string) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	if allow != "" {
		fmt.Fprintf(&sb, "Allow: %s\r\n", allow)
	}
	sb.WriteString("Content-Length: 0\r\n\r\n")
	_, _ = c.Write([]byte(sb.String()))
}

// ----------------------------------------------------------------------------
// Logging helpers
// ----------------------------------------------------------------------------

// warnConnect emits a §10.1 mitm_connect Warn AND records the
// outcome to the §9.7.6 rolling counter. `tlsReached` resolves the
// `OutcomeInnerStreamFailed` ambiguity for the counter (it doesn't
// affect the log line). Pre-handshake call sites pass false;
// post-handshake call sites pass true. The flag is irrelevant for
// every outcome other than `inner_stream_failed`.
func (h *ConnectHandler) warnConnect(outcome ConnectOutcome, host string, port int, clientAddr string, start time.Time, reason, deniedGate string, tlsReached bool) {
	dur := time.Since(start).Seconds()
	fields := map[string]any{
		"outcome":          string(outcome),
		"host":             host,
		"port":             port,
		"client_addr":      clientAddr,
		"duration_seconds": dur,
	}
	if reason != "" {
		fields["reason"] = reason
	}
	if deniedGate != "" {
		fields["denied_gate"] = deniedGate
	}
	h.deps.LogFn("warn", "mitm_connect", fields)
	h.recordOutcome(outcome, tlsReached)
	mitmConnectTotal.Inc(string(outcome))
	mitmConnectDurationSeconds.Observe(dur)
}

// infoConnect is the Info counterpart of warnConnect — used only
// for `OutcomeTunneled` (the only success outcome). tlsReached is
// always true at the call sites; it is taken as a parameter for
// signature symmetry with warnConnect.
func (h *ConnectHandler) infoConnect(outcome ConnectOutcome, host string, port int, clientAddr string, start time.Time, reason string, tlsReached bool) {
	dur := time.Since(start).Seconds()
	fields := map[string]any{
		"outcome":          string(outcome),
		"host":             host,
		"port":             port,
		"client_addr":      clientAddr,
		"duration_seconds": dur,
	}
	if reason != "" {
		fields["reason"] = reason
	}
	h.deps.LogFn("info", "mitm_connect", fields)
	h.recordOutcome(outcome, tlsReached)
	mitmConnectTotal.Inc(string(outcome))
	mitmConnectDurationSeconds.Observe(dur)
}

// recordOutcome forwards `outcome` to the §9.7.6 rolling counter
// when one is wired. The counter classifies internally; pre-TLS
// rejections are silently dropped, and `inner_stream_failed`
// classification is gated on tlsReached.
func (h *ConnectHandler) recordOutcome(outcome ConnectOutcome, tlsReached bool) {
	if h.deps.Stats != nil {
		h.deps.Stats.Record(outcome, tlsReached)
	}
}
