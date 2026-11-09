package handler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// isAnchorClass reports whether p classifies as one of the three anchor
// classes (aptpath.ClassifySuiteFile: InRelease, Release, Release.gpg),
// as the handler classifies a request (newSuiteReq).
func isAnchorClass(p string) bool {
	switch aptpath.ClassifySuiteFile(aptpath.SuitePath(p), p) {
	case aptpath.FileInRelease, aptpath.FileRelease, aptpath.FileReleaseGPG:
		return true
	}
	return false
}

// captureRequestLog points h's logger at a JSON buffer and returns it, so
// a test can read the request line's upstream_status.
func captureRequestLog(h *Handler) *strictLogBuf {
	logs := &strictLogBuf{}
	h.logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logs
}

// oneRequestLine returns the only request line logs holds, and resets it.
func oneRequestLine(t *testing.T, logs *strictLogBuf) map[string]any {
	t.Helper()
	lines := logs.find("request")
	logs.reset()
	if len(lines) != 1 {
		t.Fatalf("request lines %v, want one", lines)
	}
	return lines[0]
}

// The generic metadata miss path has no declared hash. A 204 on a retry
// must clear the actual BlobWriter and its hash state before Finalize(0),
// otherwise the partial body can become a cached success.
func TestServeHTTP_NoContentMissThenHit(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 && partial {
					conn, out, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = conn.Close() }()
					_, _ = fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\nETag: \"v1\"\r\n\r\nhello ")
					_ = out.Flush()
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			logs := captureRequestLog(h)
			for _, cacheStatus := range []string{"MISS", "HIT"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, "/debian/dists/stable/main/binary-all/Packages"))
				if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
					t.Fatalf("%s: status=%d, body=%q, Content-Length=%q", cacheStatus, rec.Code, rec.Body.String(), rec.Header().Get("Content-Length"))
				}
				if got := rec.Header().Get("X-Cache"); got != cacheStatus {
					t.Errorf("X-Cache=%q, want %q", got, cacheStatus)
				}
				// The 204 is visible on the miss (F2-M3): the request line's
				// upstream_status and X-Upstream-Status say 204, not 200.
				line := oneRequestLine(t, logs)
				if cacheStatus == "MISS" && (line["upstream_status"] != float64(http.StatusNoContent) || rec.Header().Get("X-Upstream-Status") != "204") {
					t.Errorf("MISS: upstream_status %v, X-Upstream-Status %q; want 204", line["upstream_status"], rec.Header().Get("X-Upstream-Status"))
				}
			}
			// Check the pool key as well as the served bytes: truncation
			// must reset the hasher, not only the file and byte counter.
			emptyHash := fmt.Sprintf("%x", sha256.Sum256(nil))
			if body, err := os.ReadFile(h.cache.BlobPath(emptyHash)); err != nil || len(body) != 0 {
				t.Errorf("empty blob: bytes=%d, error=%v", len(body), err)
			}
			wantCalls := int32(1)
			if partial {
				wantCalls++
			}
			if got := calls.Load(); got != wantCalls {
				t.Errorf("upstream calls=%d, want %d", got, wantCalls)
			}
		})
	}
}

// The maintainer's rulings of 2026-11-05 (F2-I1, D2-M2; F2-R1-M1): on the
// client path a 204 is an empty success only for a path proxy.IsMetadata
// classifies as an index or metadata and that is no anchor. Each class is
// pinned here: the Packages*/Sources*/Contents-*/Translation-*/
// Components-*/icons-* indexes, by-hash and .diff/Index, and PR #22's
// GitLab case, an empty debian-installer binary-all/Packages. Each is
// answered 200 and empty, cached, and served as a HIT; the request line
// and X-Upstream-Status say 204. The anchors are refused
// (TestServeHTTP_NoContentAnchorIsAnUpstreamError).
func TestServeHTTP_NoContentMetadataIsAnEmptySuccess(t *testing.T) {
	for _, p := range []string{
		"/debian/dists/stable/main/debian-installer/binary-all/Packages",
		"/debian/dists/stable/main/binary-amd64/Packages.xz",
		"/debian/dists/stable/main/source/Sources.gz",
		"/debian/dists/stable/main/Contents-amd64.gz",
		"/debian/dists/stable/main/i18n/Translation-en.bz2",
		"/debian/dists/stable/main/dep11/Components-amd64.yml.gz",
		"/debian/dists/stable/main/dep11/icons-48x48.tar.gz",
		"/debian/dists/stable/main/binary-amd64/by-hash/SHA256/e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"/debian/dists/stable/main/binary-amd64/Packages.diff/Index",
		"/flat/Packages",
	} {
		t.Run(p, func(t *testing.T) {
			if !proxy.IsMetadata(p) || isAnchorClass(p) {
				t.Fatalf("%s is not metadata, or is an anchor: the table no longer pins the accepted class", p)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			logs := captureRequestLog(h)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, p))
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("X-Cache") != "MISS" {
				t.Fatalf("miss: status=%d, body=%q, X-Cache=%q; want an empty 200 MISS", rec.Code, rec.Body.String(), rec.Header().Get("X-Cache"))
			}
			line := oneRequestLine(t, logs)
			if line["upstream_status"] != float64(http.StatusNoContent) || rec.Header().Get("X-Upstream-Status") != "204" {
				t.Errorf("miss: upstream_status %v, X-Upstream-Status %q; want 204", line["upstream_status"], rec.Header().Get("X-Upstream-Status"))
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, p))
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("X-Cache") != "HIT" {
				t.Fatalf("hit: status=%d, body=%q, X-Cache=%q; want an empty 200 HIT", rec.Code, rec.Body.String(), rec.Header().Get("X-Cache"))
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("upstream calls=%d, want 1", got)
			}
		})
	}
}

// The maintainer's ruling of 2026-11-05 (F2-I1, D2-M2): on the client
// path a 204 for anything proxy.IsMetadata does not classify as an index
// or metadata — a .deb or any other package file, or another file — is
// an upstream error, as before commit 3a6d5ba: 502 with Retry-After,
// upstream_status 204, nothing cached, so the next request fetches the
// real file. With partial, the first answer is a cut 200 and the 204
// answers the resume.
func TestServeHTTP_NoContentPackageFileIsAnUpstreamError(t *testing.T) {
	for _, p := range []string{
		"/debian/pool/main/h/hello/hello_2.10-3_amd64.deb",
		"/debian/pool/main/h/hello/hello_2.10-3_amd64.udeb",
		"/debian/pool/main/h/hello/hello-dbgsym_2.10-3_amd64.ddeb",
		"/debian/pool/main/h/hello/hello_2.10-3.dsc",
		"/debian/pool/main/h/hello/hello_2.10.orig.tar.gz",
		"/debian/pool/main/h/hello/hello_2.10-3.debian.tar.xz",
		"/debian/pool/main/h/hello/hello_2.10-3.diff.gz",
		"/debian/pool/main/h/hello/hello_2.10-3_amd64.buildinfo",
		"/debian/pool/main/h/hello/hello_2.10-3_amd64.changes",
		"/debian/pool/main/i/icons-foo/icons-foo_1.0_all.deb",
		"/flat/Packages_1.0_all.deb",
		"/debian/project/trace/master",
	} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", p, partial), func(t *testing.T) {
				if proxy.IsMetadata(p) {
					t.Fatalf("%s is metadata: the table no longer pins the refused class", p)
				}
				checkNoContentRefused(t, p, partial, "60")
			})
		}
	}
}

// The maintainer's ruling of 2026-11-05 (F2-R1-M1): a 204 for an anchor —
// a path aptpath.ClassifySuiteFile classifies as InRelease, Release or
// Release.gpg, dists and flat suites alike (the "/." twin included) — is
// an upstream error like a package file's: apt reads no empty anchor
// (NOSPLIT/NODATA, exit 100), while a 502 only warns. 502 with
// Retry-After 30 (metadata's), upstream_status 204, nothing cached, and
// the retry fetches (MISS, never a HIT).
func TestServeHTTP_NoContentAnchorIsAnUpstreamError(t *testing.T) {
	for _, p := range []string{
		"/debian/dists/stable/InRelease",
		"/debian/dists/stable/Release",
		"/debian/dists/stable/Release.gpg",
		"/flat/InRelease",
		"/flat/Release",
		"/flat/Release.gpg",
		"/flat/./InRelease",
		"/flat/./Release.gpg",
		"/core:/stable:/v1.34/deb/Release",
	} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/partial=%t", p, partial), func(t *testing.T) {
				if !proxy.IsMetadata(p) || !isAnchorClass(p) {
					t.Fatalf("%s is not an anchor: the table no longer pins the refused class", p)
				}
				checkNoContentRefused(t, p, partial, "30")
			})
		}
	}
}

// checkNoContentRefused asserts the refused-204 shape for p: upstream
// answers 204 (after a cut 200 when partial), then the real file. The
// first request gets 502 with Retry-After retryAfter, outcome bad_gateway
// and upstream_status 204; nothing is cached; the second fetches the real
// file (MISS).
func checkNoContentRefused(t *testing.T, p string, partial bool, retryAfter string) {
	t.Helper()
	noContent := int32(1)
	if partial {
		noContent = 2
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		switch {
		case n == 1 && partial:
			conn, out, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 14\r\nETag: \"v1\"\r\n\r\nreal ")
			_ = out.Flush()
		case n <= noContent:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte("real file body"))
		}
	}))
	defer srv.Close()
	h := newTestHandler(t, nil, nil)
	logs := captureRequestLog(h)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, p))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("X-Cache") != "" || rec.Header().Get("Retry-After") != retryAfter {
		t.Fatalf("204: status=%d, X-Cache=%q, Retry-After=%q, body=%q; want 502 with Retry-After %s",
			rec.Code, rec.Header().Get("X-Cache"), rec.Header().Get("Retry-After"), rec.Body.String(), retryAfter)
	}
	line := oneRequestLine(t, logs)
	if line["upstream_status"] != float64(http.StatusNoContent) || line["outcome"] != "bad_gateway" || rec.Header().Get("X-Upstream-Status") != "204" {
		t.Errorf("204: upstream_status %v, outcome %v, X-Upstream-Status %q; want 204, bad_gateway, 204",
			line["upstream_status"], line["outcome"], rec.Header().Get("X-Upstream-Status"))
	}
	if got := calls.Load(); got != noContent {
		t.Errorf("upstream calls=%d, want %d", got, noContent)
	}
	// Nothing cached: no row, no empty blob.
	scheme, host, port := splitURL(t, srv.URL)
	if _, err := h.cache.LookupURL(context.Background(), scheme, hostKey(host, port), p); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("url_path row after a refused 204: %v, want none", err)
	}
	if _, err := os.Stat(h.cache.BlobPath(fmt.Sprintf("%x", sha256.Sum256(nil)))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty blob in the pool after a refused 204: %v", err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, p))
	if rec.Code != http.StatusOK || rec.Body.String() != "real file body" || rec.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("retry: status=%d, body=%q, X-Cache=%q; want the real file, fetched (MISS)",
			rec.Code, rec.Body.String(), rec.Header().Get("X-Cache"))
	}
	if got := calls.Load(); got != noContent+1 {
		t.Errorf("upstream calls=%d, want %d", got, noContent+1)
	}
}

// A metadata member of an adopted suite whose pool file is lost is fetched
// again (runSnapshotMemberFetch), and its declared hash decides: a 204 for
// a member declared empty is accepted (upstream_status 204), and a 204 for
// a member declared non-empty is refused (502), as before. The member is
// PR #22's GitLab case, an empty debian-installer binary-all/Packages.
func TestServeHTTP_NoContentSnapshotMemberRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared []byte // the member's bytes as its Release declares them
		want     int
	}{
		{"declared_empty", nil, http.StatusOK},
		{"declared_nonempty", []byte("x"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			logs := captureRequestLog(h)
			scheme, host, port := splitURL(t, srv.URL)
			anchor := writeBlob(t, h, []byte("anchor"))
			declared := writeBlob(t, h, tc.declared)
			const member = "main/debian-installer/binary-all/Packages"
			commitInlineSnapshot(t, h, scheme, hostKey(host, port), "/dists/noble", anchor,
				[]cache.SnapshotMember{
					{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor},
					{Path: member, BlobHash: declared, DeclaredSHA256: declared},
				}, nil)
			// The member's pool file is lost, so the request fetches it again.
			if err := os.Remove(h.cache.BlobPath(declared)); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, "/dists/noble/"+member))
			if rec.Code != tc.want || rec.Header().Get("X-Upstream-Status") != "204" {
				t.Fatalf("status=%d, X-Upstream-Status=%q, body=%q; want %d with 204", rec.Code, rec.Header().Get("X-Upstream-Status"), rec.Body.String(), tc.want)
			}
			if tc.want == http.StatusOK && (rec.Body.Len() != 0 || rec.Header().Get("X-Cache") != "MISS") {
				t.Errorf("body %q, X-Cache %q; want an empty MISS", rec.Body.String(), rec.Header().Get("X-Cache"))
			}
			// A recovery's success is logged as the snapshot hit it is,
			// without upstream fields (X-Upstream-Status carries the 204);
			// its refusal's line carries upstream_status.
			if tc.want == http.StatusBadGateway {
				var found bool
				for _, line := range logs.find("request") {
					if line["outcome"] == "snapshot_member_refetch_mismatch" {
						found = line["upstream_status"] == float64(http.StatusNoContent)
					}
				}
				if !found {
					t.Errorf("no snapshot_member_refetch_mismatch line with upstream_status 204: %v", logs.find("request"))
				}
			}
		})
	}
}
