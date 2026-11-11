package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

const compatSuite = "/debian/dists/forky"

// These tests seed an already-adopted snapshot, as on an upgrade. Signature
// validity is tested separately with real signed vendor/corpus fixtures.
func compatAnchor(label string) []byte {
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		"Origin: " + label + "\nSuite: testing\nCodename: forky\nArchitectures: amd64\n" +
		"-----BEGIN PGP SIGNATURE-----\n\nAAAA\n-----END PGP SIGNATURE-----\n")
}

func compatCommit(t *testing.T, h *Handler, canon string, body []byte, extras ...cache.SnapshotMember) (int64, string) {
	t.Helper()
	hash := writeBlob(t, h, body)
	members := append([]cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, extras...)
	return commitInlineSnapshot(t, h, "http", canon, compatSuite, hash, members, nil), hash
}

func compatGet(h *Handler, base, method, name string, headers map[string]string) *httptest.ResponseRecorder {
	r := proxyReq(method, base, compatSuite+"/"+name)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestInlineRelease_ExistingSnapshotWorksOffline(t *testing.T) {
	h := newTestHandler(t, []string{}, nil) // no upstream is allowed
	defer h.Close()
	id, _ := compatCommit(t, h, "127.0.0.1", compatAnchor("adopted"))
	// An older url_path entry must never win over the selected snapshot.
	stale := writeBlob(t, h, []byte("Origin: stale upstream Release\n"))
	if err := h.cache.PutURLPath(context.Background(), cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: compatSuite + "/Release",
		BlobHash: &stale, UpstreamURL: "http://127.0.0.1" + compatSuite + "/Release", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Release":     "Origin: adopted\nSuite: testing\nCodename: forky\nArchitectures: amd64",
		"Release.gpg": "-----BEGIN PGP SIGNATURE-----\n\nAAAA\n-----END PGP SIGNATURE-----\n",
	}
	for name, body := range want {
		w := compatGet(h, "http://127.0.0.1", "GET", name, nil)
		if w.Code != 200 || w.Body.String() != body || w.Header().Get(hdrXCache) != cacheHit ||
			w.Header().Get(hdrXCacheSnapshot) != strconv.FormatInt(id, 10) {
			t.Fatalf("%s: %d %v %q", name, w.Code, w.Header(), w.Body.String())
		}
		if w.Header().Get("ETag") != fmt.Sprintf(`"sha256-%x"`, sha256.Sum256([]byte(body))) || w.Header().Get("Last-Modified") != "" {
			t.Fatalf("%s validators: %v", name, w.Header())
		}
	}
	// This is not a general bypass of the snapshot's membership contract.
	if w := compatGet(h, "http://127.0.0.1", "GET", "main/binary-arm64/Packages", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestInlineRelease_HTTPAndSnapshotChanges(t *testing.T) {
	h := newTestHandler(t, []string{}, nil)
	defer h.Close()
	oldID, _ := compatCommit(t, h, "127.0.0.1", compatAnchor("old"))
	base := "http://127.0.0.1"
	for _, name := range []string{"Release", "Release.gpg"} {
		first := compatGet(h, base, "GET", name, nil)
		tag := first.Header().Get("ETag")
		if w := compatGet(h, base, "HEAD", name, nil); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(first.Body.Len()) || w.Header().Get("ETag") != tag {
			t.Fatalf("HEAD %s: %d %v %q", name, w.Code, w.Header(), w.Body.String())
		}
		if w := compatGet(h, base, "GET", name, map[string]string{"If-None-Match": tag}); w.Code != 304 || w.Body.Len() != 0 {
			t.Fatalf("conditional %s: %d %q", name, w.Code, w.Body.String())
		}
		if w := compatGet(h, base, "GET", name, map[string]string{"Range": "bytes=0-5", "If-Range": tag}); w.Code != 206 || w.Body.String() != first.Body.String()[:6] {
			t.Fatalf("range %s: %d %q", name, w.Code, w.Body.String())
		}
	}
	old := compatGet(h, base, "GET", "Release", nil)
	compatCommit(t, h, "127.0.0.1", compatAnchor("new"))
	w := compatGet(h, base, "GET", "Release", map[string]string{"If-None-Match": old.Header().Get("ETag"), "If-Modified-Since": "Thu, 01 Jan 2099 00:00:00 GMT"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Origin: new") || w.Header().Get("ETag") == old.Header().Get("ETag") {
		t.Fatalf("flip: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	// Model a request whose snapshot pointer was read just before the flip.
	// Conversion must use that ID throughout, not re-read the current pointer.
	r := proxyReq("GET", base, compatSuite+"/Release")
	req, err := h.parser.Parse(r.RequestURI, r.Host)
	if err != nil {
		t.Fatal(err)
	}
	pinned := httptest.NewRecorder()
	if ok, code, _ := h.tryInlineRelease(pinned, r, req, oldID, "Release", time.Now()); !ok || code != 200 || !bytes.Equal(pinned.Body.Bytes(), old.Body.Bytes()) {
		t.Fatalf("pinned snapshot: %v %d %q", ok, code, pinned.Body.String())
	}
}

func TestInlineRelease_ExistingMembersTakePrecedence(t *testing.T) {
	h := newTestHandler(t, []string{}, nil)
	defer h.Close()
	rel := writeBlob(t, h, []byte("explicit Release"))
	sig := writeBlob(t, h, []byte("explicit Release.gpg"))
	compatCommit(t, h, "127.0.0.1", compatAnchor("inline"),
		cache.SnapshotMember{Path: "Release", BlobHash: rel, DeclaredSHA256: rel},
		cache.SnapshotMember{Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig})
	for _, name := range []string{"Release", "Release.gpg"} {
		w := compatGet(h, "http://127.0.0.1", "GET", name, nil)
		if w.Code != 200 || w.Body.String() != "explicit "+name {
			t.Fatalf("%s: %d %q", name, w.Code, w.Body.String())
		}
	}
}

func TestInlineRelease_MissingAnchorRecovery(t *testing.T) {
	for _, scenario := range []string{"same anchor", "upstream advanced", "upstream missing"} {
		t.Run(scenario, func(t *testing.T) {
			body := compatAnchor("adopted")
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != compatSuite+"/InRelease" {
					t.Errorf("recovery fetched %s, want InRelease", r.URL.Path)
				}
				switch scenario {
				case "same anchor":
					_, _ = w.Write(body)
				case "upstream advanced":
					_, _ = w.Write(compatAnchor("newer upstream"))
				case "upstream missing":
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			h := newTestHandler(t, nil, nil)
			defer h.Close()
			_, host, port := splitURL(t, srv.URL)
			_, hash := compatCommit(t, h, hostKey(host, port), body)
			if err := os.Remove(h.cache.BlobPath(hash)); err != nil {
				t.Fatal(err)
			}
			w := compatGet(h, srv.URL, "GET", "Release", nil)
			if scenario == "same anchor" {
				if w.Code != 200 || w.Header().Get(hdrXCache) != cacheMiss || !strings.Contains(w.Body.String(), "Origin: adopted") {
					t.Fatalf("recovery: %d %v %q", w.Code, w.Header(), w.Body.String())
				}
				if w = compatGet(h, srv.URL, "GET", "Release.gpg", nil); w.Code != 200 || w.Header().Get(hdrXCache) != cacheHit {
					t.Fatalf("recovered signature: %d %v", w.Code, w.Header())
				}
			} else if w.Code != 502 || w.Header().Get("Retry-After") != "30" {
				t.Fatalf("unsafe recovery: %d %v %q", w.Code, w.Header(), w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream requests = %d, want 1", calls.Load())
			}
		})
	}
}

func TestInlineRelease_LocalFaultsFailClosed(t *testing.T) {
	for _, scenario := range []string{"corrupt", "malformed envelope", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			h := newTestHandler(t, []string{}, nil)
			defer h.Close()
			body := compatAnchor("adopted")
			if scenario == "malformed envelope" {
				body = []byte("Origin: unsigned\n")
			}
			_, hash := compatCommit(t, h, "127.0.0.1", body)
			if scenario == "corrupt" {
				if err := os.WriteFile(h.cache.BlobPath(hash), compatAnchor("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "oversize" {
				if err := os.Truncate(h.cache.BlobPath(hash), maxInlineReleaseBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"Release", "Release.gpg"} {
				w := compatGet(h, "http://127.0.0.1", "GET", name, nil)
				if w.Code != 502 || w.Header().Get("Retry-After") != "30" || w.Header().Get("ETag") != "" {
					t.Fatalf("%s: %d %v %q", name, w.Code, w.Header(), w.Body.String())
				}
			}
		})
	}
}
