package freshness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// Exercise actual HTTP 204 handling through adoption, including the required
// "all" index group. Only signature verification uses the existing test stub;
// member transport, hashing, publication and blob storage are real.
func TestAdopter_NoContentHTTP(t *testing.T) {
	empty256, empty512 := shaOf(nil), sha512Of(nil)
	for _, layout := range []struct{ name, suite, member string }{
		{"dists", "/debian/dists/stable", "main/debian-installer/binary-all/Packages"},
		{"flat", "/flat", "Packages"},
	} {
		for _, byHash := range []bool{false, true} {
			for _, tc := range []struct {
				name, sha256, sha512 string
				size                 int64
				wantErr              error
			}{
				{name: "empty_sha256", sha256: empty256},
				{name: "empty_sha512", sha512: empty512},
				{name: "empty_both", sha256: empty256, sha512: empty512},
				{name: "wrong_sha256", sha256: shaOf([]byte("x")), sha512: empty512, wantErr: ErrAdoptionMemberMismatch},
				{name: "wrong_sha512", sha256: empty256, sha512: sha512Of([]byte("x")), wantErr: ErrAdoptionMemberMismatch},
				{name: "nonempty", sha256: shaOf([]byte("x")), sha512: sha512Of([]byte("x")), size: 1, wantErr: ErrAdoptionMemberMismatch},
				{name: "empty_hash_wrong_size", sha256: empty256, sha512: empty512, size: 1, wantErr: ErrAdoptionMemberFetchFailed},
			} {
				t.Run(fmt.Sprintf("%s/by_hash=%t/%s", layout.name, byHash, tc.name), func(t *testing.T) {
					m := ReleaseMember{Path: layout.member, SHA256: tc.sha256, SHA512: tc.sha512, Size: tc.size}
					plainPath := layout.suite + "/" + layout.member
					byHashPath := layout.suite + "/" + byHashProbePath(m)
					var plainCalls, hashCalls atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case plainPath:
							plainCalls.Add(1)
						case byHashPath:
							hashCalls.Add(1)
						default:
							t.Errorf("unexpected member URL: %s", r.URL)
							http.NotFound(w, r)
							return
						}
						w.WriteHeader(http.StatusNoContent)
					}))
					defer srv.Close()
					env := newAdoptionTestEnv(t)
					ad, c := env.adopter, env.cache
					client, err := fetch.New(fetch.Options{Proxy: srv.URL, AllowedHostRegex: []string{`^archive\.example\.com$`}})
					if err != nil {
						t.Fatal(err)
					}
					ad.fetcher = client
					suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example.com", SuitePath: layout.suite}
					trackSuite(t, c, suite)
					release := "Origin: test\n"
					if byHash {
						release += "Acquire-By-Hash: yes\n"
					}
					if tc.sha256 != "" {
						release += fmt.Sprintf("SHA256:\n %s %d %s\n", tc.sha256, tc.size, layout.member)
					}
					if tc.sha512 != "" {
						release += fmt.Sprintf("SHA512:\n %s %d %s\n", tc.sha512, tc.size, layout.member)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					err = ad.Run(ctx, suite, []byte(release), "etag", "")
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("Run: %v, want %v", err, tc.wantErr)
					}
					wantPlain, wantHash := int32(1), int32(0)
					if byHash {
						wantHash = 1
						if tc.wantErr == nil {
							wantPlain = 0
						}
					}
					if plainCalls.Load() != wantPlain || hashCalls.Load() != wantHash {
						t.Errorf("plain requests=%d, by-hash=%d; want %d, %d", plainCalls.Load(), hashCalls.Load(), wantPlain, wantHash)
					}
					if tmp, err := os.ReadDir(filepath.Join(c.Dir(), "tmp")); err != nil || len(tmp) != 0 {
						t.Errorf("temporary blobs=%d, error=%v; want none", len(tmp), err)
					}
					sf, err := c.GetSuiteFreshness(ctx, suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
					if err != nil {
						t.Fatal(err)
					}
					if tc.wantErr != nil {
						if sf.CurrentSnapshotID != nil {
							t.Fatal("published a snapshot with a mismatched member")
						}
						if _, err := os.Stat(c.BlobPath(empty256)); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("unverified empty member entered the pool: %v", err)
						}
						return
					}
					if sf.CurrentSnapshotID == nil {
						t.Fatal("no adopted snapshot")
					}
					for _, path := range append([]string{layout.member}, byHashAliases(m)...) {
						member, err := c.GetSnapshotMember(ctx, *sf.CurrentSnapshotID, path)
						if err != nil {
							t.Fatal(err)
						}
						if member.BlobHash != empty256 || member.DeclaredSHA256 != empty256 {
							t.Errorf("%s: member=%+v, want empty-body hash", path, member)
						}
					}
					body, err := os.ReadFile(c.BlobPath(empty256))
					if err != nil || len(body) != 0 {
						t.Errorf("empty blob: bytes=%d, error=%v", len(body), err)
					}
				})
			}
		}
	}
}
