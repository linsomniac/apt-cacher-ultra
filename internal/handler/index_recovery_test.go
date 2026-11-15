package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/ulikunitz/xz"
)

func recoveryCompressed(t *testing.T, codec string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if codec == ".gz" {
		w := gzip.NewWriter(&b)
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		w, err := xz.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func TestServeHTTP_ReconstructSnapshotIndex(t *testing.T) {
	for _, codec := range []string{".gz", ".xz"} {
		for _, mode := range []string{"cached_sibling", "missing_sibling", "chunked_missing_sibling", "by_hash", "range", "newer_upstream", "corrupt_sibling"} {
			t.Run(codec+"/"+mode, func(t *testing.T) {
				const suite = "/debian/dists/trixie"
				const member = "main/source/Sources"
				plain := bytes.Repeat([]byte("Package: hello\nVersion: 1\n\n"), 50)
				compressed := recoveryCompressed(t, codec, plain)
				served := compressed
				if mode == "newer_upstream" {
					served = recoveryCompressed(t, codec, []byte("newer release contents"))
				}
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.URL.Path != suite+"/"+member && r.URL.Path != suite+"/"+member+codec {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Location", "Sources"+codec)
					if mode != "chunked_missing_sibling" {
						w.Header().Set("Content-Length", fmt.Sprint(len(served)))
					}
					w.Header().Set("Last-Modified", "Fri, 01 May 2026 12:00:00 GMT")
					if mode == "chunked_missing_sibling" {
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
					}
					_, _ = w.Write(served)
				}))
				defer srv.Close()
				h := newTestHandler(t, nil, nil)
				defer h.Close()
				pHash := writeBlob(t, h, plain)
				cHash := writeBlob(t, h, compressed)
				anchor := writeBlob(t, h, compatAnchor("test snapshot"))
				alias := "main/source/by-hash/SHA256/" + pHash
				rows := []cache.SnapshotMember{
					{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor},
					{Path: member, BlobHash: pHash, DeclaredSHA256: pHash},
					{Path: member + codec, BlobHash: cHash, DeclaredSHA256: cHash},
					{Path: alias, BlobHash: pHash, DeclaredSHA256: pHash},
				}
				id := commitInlineSnapshot(t, h, "http", "127.0.0.1", suite, anchor, rows, nil)
				if err := os.Remove(h.cache.BlobPath(pHash)); err != nil {
					t.Fatal(err)
				}
				if mode == "missing_sibling" || mode == "chunked_missing_sibling" || mode == "newer_upstream" {
					if err := os.Remove(h.cache.BlobPath(cHash)); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "corrupt_sibling" {
					if err := os.WriteFile(h.cache.BlobPath(cHash), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				requested := member
				if mode == "by_hash" {
					requested = alias
				}
				req := proxyReq("GET", srv.URL, suite+"/"+requested)
				if mode == "range" {
					req.Header.Set("Range", "bytes=0-7")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if mode == "newer_upstream" || mode == "corrupt_sibling" {
					if rec.Code != 502 {
						t.Fatalf("accepted wrong snapshot bytes: %d %q", rec.Code, rec.Body.String())
					}
					if exists, _ := h.cache.BlobExists(pHash); exists {
						t.Fatal("published invalid plaintext")
					}
					return
				}
				expected := plain
				code := 200
				if mode == "range" {
					expected = plain[:8]
					code = 206
				}
				if rec.Code != code || !bytes.Equal(rec.Body.Bytes(), expected) {
					t.Fatalf("response=%d %q", rec.Code, rec.Body.String())
				}
				if rec.Header().Get("X-Cache-Snapshot") != strconv.FormatInt(id, 10) {
					t.Fatal("lost snapshot identity")
				}
				if rec.Header().Get("Last-Modified") == "Fri, 01 May 2026 12:00:00 GMT" {
					t.Fatal("copied compressed modification time to plaintext")
				}
				// Both the canonical URL and its alias now hit the exact same
				// verified plain blob, while the compressed URL still serves gzip/xz.
				before := requests.Load()
				for p, body := range map[string][]byte{member: plain, alias: plain, member + codec: compressed} {
					r := httptest.NewRecorder()
					h.ServeHTTP(r, proxyReq("GET", srv.URL, suite+"/"+p))
					if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), body) {
						t.Fatalf("wrong representation for %s", p)
					}
				}
				if requests.Load() != before {
					t.Fatal("re-fetched recovered cache hits")
				}
			})
		}
	}
}

// The suite may flip while a request for an old snapshot is in flight. Only
// that request's snapshot may supply the compressed sibling, even if the new
// snapshot and the mutable upstream both publish different bytes.
func TestSnapshotReconstructionUsesSelectedSnapshot(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	defer h.Close()
	const suite = "/debian/dists/trixie"
	const member = "main/source/Sources"
	old := []byte(strings.Repeat("old snapshot\n", 50))
	oldGz := recoveryCompressed(t, ".gz", old)
	oldHash := writeBlob(t, h, old)
	gzHash := writeBlob(t, h, oldGz)
	anchor := writeBlob(t, h, compatAnchor("old"))
	id := commitInlineSnapshot(t, h, "http", "127.0.0.1", suite, anchor, []cache.SnapshotMember{
		{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor},
		{Path: member, BlobHash: oldHash, DeclaredSHA256: oldHash},
		{Path: member + ".gz", BlobHash: gzHash, DeclaredSHA256: gzHash},
	}, nil)
	newHash := writeBlob(t, h, []byte("new snapshot"))
	newGz := writeBlob(t, h, recoveryCompressed(t, ".gz", []byte("new snapshot")))
	anchor = writeBlob(t, h, compatAnchor("new"))
	commitInlineSnapshot(t, h, "http", "127.0.0.1", suite, anchor, []cache.SnapshotMember{
		{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor},
		{Path: member, BlobHash: newHash, DeclaredSHA256: newHash},
		{Path: member + ".gz", BlobHash: newGz, DeclaredSHA256: newGz},
	}, nil)
	if err := os.Remove(h.cache.BlobPath(oldHash)); err != nil {
		t.Fatal(err)
	}
	mem, err := h.cache.GetSnapshotMember(context.Background(), id, member)
	if err != nil {
		t.Fatal(err)
	}
	req := &proxy.Request{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: suite, Path: suite + "/" + member, UpstreamURL: "http://127.0.0.1" + suite + "/" + member}
	res, ok := h.reconstructSnapshotIndex(context.Background(), req, mem, id)
	if !ok || res.err != nil || res.blobHash != oldHash {
		t.Fatalf("recovery=%+v handled=%v", res, ok)
	}
}
