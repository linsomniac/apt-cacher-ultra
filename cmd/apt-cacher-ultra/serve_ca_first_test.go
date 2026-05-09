package main

// SPEC6 §9.5 step 2: the CA must be materialized BEFORE the proxy
// listener binds. The rationale is that a daemon binding :3142 first
// then failing on CA load (flock contention up to 30s, inconsistent
// ca/ files, or `mitm_ca_unconstrained_refused` from the §5.1.1.1
// fail-closed default) would leave the proxy port accepting (and
// 503-ing CONNECT) for the duration of the bind-then-fail window —
// surfacing as transient connect/reset behaviour to clients during
// startup. Failing fast at CA load is the spec contract.
//
// This test pins the ordering by configuring BOTH a known
// CA-load failure AND a known net.Listen failure on the same call,
// then asserting that the CA error is the one returned. The
// distinct error wrappings ("tls_mitm CA: …" vs. "listen …: …") let
// the test observe which step ran first without race-prone
// time-of-bind assertions: the deferred listener Close on serve()
// failure paths would otherwise un-bind the port before the test
// can probe it.

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestServe_TlsMitmCALoadFailsBeforeProxyBind(t *testing.T) {
	cfg := minimalCfg(t.TempDir(), nil)

	// Out-of-range TCP port — net.Listen("tcp", ":99999") fails with
	// `address :99999: invalid port`. If serve() reaches the listen
	// step first, the error wraps `listen ...: ...`. If it reaches
	// the CA step first (per §9.5), the listen step is never
	// attempted and the error wraps `tls_mitm CA: ...`.
	cfg.Cache.Listen = "127.0.0.1:99999"

	// Trigger the §5.1.1.1 fail-closed CA refusal: empty regex with
	// allow_unconstrained_ca = false (the spec default) forces
	// `mitm_ca_unconstrained_refused`. Deterministic, no fixture.
	cfg.TlsMitm.Enabled = true
	cfg.TlsMitm.AllowedHostRegex = ""
	cfg.TlsMitm.AllowUnconstrainedCA = false
	cfg.TlsMitm.CACertLifetime.Duration = 30 * 24 * time.Hour
	cfg.TlsMitm.LeafCertLifetime.Duration = time.Hour
	cfg.TlsMitm.LeafAlgorithm = "ecdsa-p256"
	cfg.TlsMitm.CertCacheSize = 16

	ctx := context.Background()
	err := serve(ctx, cfg, newTestLogger())
	if err == nil {
		t.Fatalf("serve() returned nil; expected a CA-load error")
	}

	msg := err.Error()
	if strings.HasPrefix(msg, "listen ") {
		t.Fatalf("serve() returned a listen error %q; SPEC6 §9.5 step 2 says CA load must run before net.Listen, but net.Listen ran first", msg)
	}
	if !strings.Contains(msg, "tls_mitm CA") {
		t.Fatalf("serve() error = %q; want the wrapped tls_mitm CA error (signals CA load ran before bind)", msg)
	}
}
