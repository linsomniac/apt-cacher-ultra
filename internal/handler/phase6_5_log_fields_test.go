package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
)

// SPEC6_5 §2.3 log-field plumbing — the validated_hash and package_name
// fields populate per-request log lines whenever the served bytes were
// matched against a package_hash row's declared SHA256. These tests pin
// the field-presence-as-signal contract: emitted on validated serves,
// omitted on Phase 1 trust-upstream paths and on metadata.

// TestLogFields_HitPath_ValidatedDebSurfaces: hit-path SPEC2 §6.1 match
// — cached .deb plus matching package_hash row → log carries
// validated_hash=true + package_name="bash".
func TestLogFields_HitPath_ValidatedDebSurfaces(t *testing.T) {
	body := []byte("debian binary package bytes")
	hash := shaHex(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const debPath = "/pool/main/b/bash/bash_5.1-2_amd64.deb"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            debPath,
				DeclaredSHA256:  hash,
				PackageName:     "bash",
				Architecture:    "amd64",
			},
		},
	)

	// Prime the cache by issuing a miss-then-hit; isolate the hit log.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, proxyReq("GET", srv.URL, debPath))
	if rec1.Code != http.StatusOK {
		t.Fatalf("warmup status = %d, want 200", rec1.Code)
	}
	logBuf.Reset()

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, proxyReq("GET", srv.URL, debPath))
	if rec2.Code != http.StatusOK {
		t.Fatalf("hit status = %d, want 200", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}

	out := logBuf.String()
	if !strings.Contains(out, `"validated_hash":true`) {
		t.Errorf("hit-path log missing validated_hash=true; got:\n%s", out)
	}
	if !strings.Contains(out, `"package_name":"bash"`) {
		t.Errorf("hit-path log missing package_name=bash; got:\n%s", out)
	}
}

// TestLogFields_MissPath_ValidatedSourceSurfaces: miss-path SPEC2 §6.2
// match — first request fetches + validates, log carries
// validated_hash=true + package_name from the matching source row.
func TestLogFields_MissPath_ValidatedSourceSurfaces(t *testing.T) {
	body := []byte("Format: 3.0 (quilt)\nSource: bash\nVersion: 5.1-2\n")
	hash := shaHex(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const dscPath = "/pool/main/b/bash/bash_5.1-2.dsc"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            dscPath,
				DeclaredSHA256:  hash,
				PackageName:     "bash",
				Architecture:    "source",
			},
		},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, dscPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}

	out := logBuf.String()
	if !strings.Contains(out, `"validated_hash":true`) {
		t.Errorf("miss-path log missing validated_hash=true; got:\n%s", out)
	}
	if !strings.Contains(out, `"package_name":"bash"`) {
		t.Errorf("miss-path log missing package_name=bash; got:\n%s", out)
	}
	if !strings.Contains(out, `"path_class":"source_dsc"`) {
		t.Errorf("miss-path log missing path_class=source_dsc; got:\n%s", out)
	}
}

// TestLogFields_MissPath_NoPackageHashRow_NoFields: SPEC6_5 §2.3
// "absent on Phase 1 trust-upstream paths". No package_hash row →
// no validated_hash, no package_name in the log line.
func TestLogFields_MissPath_NoPackageHashRow_NoFields(t *testing.T) {
	body := []byte("trust-upstream binary bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, "/pool/main/u/unknown/unknown_1.0_amd64.deb"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	out := logBuf.String()
	if strings.Contains(out, `"validated_hash"`) {
		t.Errorf("Phase 1 trust path should NOT emit validated_hash; got:\n%s", out)
	}
	if strings.Contains(out, `"package_name"`) {
		t.Errorf("Phase 1 trust path should NOT emit package_name; got:\n%s", out)
	}
}

// TestLogFields_PdiffPatch_ValidatedNoPackageName: SPEC6_5 §2.3 +
// §7.3. pdiff patch rows have no Package: name, so package_name MUST
// be omitted while validated_hash IS emitted (the validation did
// occur, against the Index's declared SHA256-Download hash).
func TestLogFields_PdiffPatch_ValidatedNoPackageName(t *testing.T) {
	body := []byte("compressed pdiff patch contents")
	hash := shaHex(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const patchPath = "/dists/noble/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            patchPath,
				DeclaredSHA256:  hash,
				// No PackageName — pdiff rows are unnamed.
				Architecture: "amd64",
			},
		},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, patchPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	out := logBuf.String()
	if !strings.Contains(out, `"validated_hash":true`) {
		t.Errorf("pdiff log missing validated_hash=true; got:\n%s", out)
	}
	if strings.Contains(out, `"package_name"`) {
		t.Errorf("pdiff log should NOT emit package_name (no Package: in pdiff Index); got:\n%s", out)
	}
	if !strings.Contains(out, `"path_class":"pdiff_patch"`) {
		t.Errorf("pdiff log missing path_class=pdiff_patch; got:\n%s", out)
	}
}

// TestLogFields_HashMismatch_NoValidatedFields: SPEC6_5 §6.2.1.
// Mismatch fires the 502 path — the request is NOT validated, so
// validated_hash / package_name MUST be absent from the log line. The
// metric counter increments outcome=mismatch (verified separately).
func TestLogFields_HashMismatch_NoValidatedFields(t *testing.T) {
	declared := strings.Repeat("c", 64)
	body := []byte("upstream-served bytes that disagree with declared")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, logBuf := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const dscPath = "/pool/main/b/bash/bash_5.1-2.dsc"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            dscPath,
				DeclaredSHA256:  declared,
				PackageName:     "bash",
				Architecture:    "source",
			},
		},
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, dscPath))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	out := logBuf.String()
	// The mismatch path emits a Warn structured log AND a request log
	// line. Validate both: request line must NOT carry validated_hash,
	// and the outcome must be package_hash_mismatch.
	if !strings.Contains(out, `"outcome":"package_hash_mismatch"`) {
		t.Errorf("mismatch path missing outcome=package_hash_mismatch; got:\n%s", out)
	}
	// Request line: validated_hash must be absent (no successful
	// validation occurred). It's harder to grep "no validated_hash"
	// in the SAME line as the request outcome; do a fragment scan.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"outcome":"package_hash_mismatch"`) {
			continue
		}
		if strings.Contains(line, `"validated_hash":true`) {
			t.Errorf("mismatch request log unexpectedly carries validated_hash=true: %s", line)
		}
		if strings.Contains(line, `"package_name":`) {
			t.Errorf("mismatch request log unexpectedly carries package_name: %s", line)
		}
	}
}

// TestServeHashValidatedTotal_Match_Increments: SPEC6_5 §10.3 metric
// wiring. Each successful match (hit + miss path) increments
// acu_serve_hash_validated_total{outcome=match}. We snapshot the
// counter before and after a known-validated request and assert the
// delta is non-zero.
func TestServeHashValidatedTotal_Match_Increments(t *testing.T) {
	body := []byte("Format: 3.0 (quilt)\nSource: bash\nVersion: 5.1-2\n")
	hash := shaHex(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	h, _ := newPhase6_5Handler(t, srv)
	scheme, host, port := splitURL(t, srv.URL)
	canonHost := hostKey(host, port)

	const dscPath = "/pool/main/b/bash/bash_5.1-2.dsc"
	releaseBlob := writeBlob(t, h, []byte("InRelease for noble"))
	commitInlineSnapshot(t, h, scheme, canonHost, "/dists/noble", releaseBlob,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: releaseBlob, DeclaredSHA256: releaseBlob},
		},
		[]cache.PackageHash{
			{
				CanonicalScheme: scheme,
				CanonicalHost:   canonHost,
				Path:            dscPath,
				DeclaredSHA256:  hash,
				PackageName:     "bash",
				Architecture:    "source",
			},
		},
	)

	before := readMatchTotal(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq("GET", srv.URL, dscPath))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	after := readMatchTotal(t)
	if after-before < 1 {
		t.Errorf("acu_serve_hash_validated_total{outcome=match} did not increment; before=%v after=%v", before, after)
	}
}

// readMatchTotal returns the current sum of
// acu_serve_hash_validated_total{outcome="match"} across every
// path_class label by rendering metrics.Default to text and parsing
// the matching exposition-format lines. The package-global registry
// makes absolute values dependent on prior test runs, so callers
// take before/after deltas rather than asserting absolute values.
func readMatchTotal(t *testing.T) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	var total float64
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(line, "acu_serve_hash_validated_total{") {
			continue
		}
		if !strings.Contains(line, `outcome="match"`) {
			continue
		}
		// Format: acu_serve_hash_validated_total{labels} <value>
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[idx+1:]), 64)
		if err != nil {
			t.Fatalf("parse counter value %q: %v", line, err)
		}
		total += v
	}
	return total
}
