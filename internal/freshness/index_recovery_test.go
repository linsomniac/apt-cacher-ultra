package freshness

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/ulikunitz/xz"
)

func recoverySource(version string) []byte {
	return []byte("Package: hello\nVersion: " + version + "\nDirectory: pool/main/h/hello\nChecksums-Sha256:\n " + strings.Repeat("a", 64) + " 1 hello_1.tar.xz\n\n")
}

func recoveryXZ(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
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
	return b.Bytes()
}

func dualHashRelease(members map[string][]byte) []byte {
	r, _ := makeRelease(members)
	r = append(r, []byte("SHA512:\n")...)
	for p, b := range members {
		r = append(r, []byte(fmt.Sprintf(" %x %d %s\n", sha512.Sum512(b), len(b), p))...)
	}
	return r
}

// Real HTTP responses exercise framing and the original download limit:
// Apache's negotiated gzip body, chunking, and a same-length wrong response.
func TestAdopter_ReconstructIndex(t *testing.T) {
	for _, codec := range []string{".gz", ".xz"} {
		for _, framing := range []string{"content_length", "chunked", "same_size_wrong", "empty_index"} {
			t.Run(codec+"/"+framing, func(t *testing.T) {
				plain := recoverySource("1")
				if framing == "empty_index" {
					plain = nil
				}
				compressed := gzipBytes(plain)
				if codec == ".xz" {
					compressed = recoveryXZ(t, plain)
				}
				const member = "main/source/Sources"
				suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example.com", SuitePath: "/debian/dists/trixie"}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := compressed
					if r.URL.Path == suite.SuitePath+"/"+member {
						w.Header().Set("Content-Location", "https://untrusted.invalid/unrelated.gz")
						w.Header().Set("Content-Type", "application/x-gzip")
						if framing == "same_size_wrong" {
							body = bytes.Repeat([]byte{'x'}, len(plain))
						}
					} else if r.URL.Path != suite.SuitePath+"/"+member+codec {
						http.NotFound(w, r)
						return
					}
					if framing == "chunked" {
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
					} else {
						w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					}
					_, _ = w.Write(body)
				}))
				defer srv.Close()
				ad, c := newRealClientAdopter(t, srv)

				release := dualHashRelease(map[string][]byte{member: plain, member + codec: compressed})
				if err := ad.Run(context.Background(), suite, release, "", ""); err != nil {
					t.Fatal(err)
				}
				id := currentID(t, c, suite)
				for p, expected := range map[string][]byte{member: plain, member + codec: compressed} {
					row, err := c.GetSnapshotMember(context.Background(), id, p)
					if err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(c.BlobPath(row.BlobHash))
					if err != nil || !bytes.Equal(got, expected) {
						t.Fatalf("%s: incorrect stored bytes: %v", p, err)
					}
					for _, alias := range []string{byHashAliasPath(p, shaOf(expected))} {
						m, err := c.GetSnapshotMember(context.Background(), id, alias)
						if err != nil || m.BlobHash != shaOf(expected) {
							t.Fatalf("bad by-hash alias %s: %+v %v", alias, m, err)
						}
					}
				}
			})
		}
	}
}

func TestAdopter_ReconstructionPreservesPriorSnapshot(t *testing.T) {
	for _, mode := range []string{"compressed_hash_wrong", "different_release", "undeclared_sibling", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			ctx := context.Background()
			const member = "main/source/Sources"
			base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
			old := recoverySource("old")
			release := dualHashRelease(map[string][]byte{member: old})
			env.fetcher.put(base+member, old)
			if err := env.adopter.Run(ctx, env.suite, release, "", ""); err != nil {
				t.Fatal(err)
			}
			id := currentID(t, env.cache, env.suite)
			plain := recoverySource("new")
			gz := gzipBytes(plain)
			declared := map[string][]byte{member: plain, member + ".gz": gz}
			if mode == "different_release" {
				gz = gzipBytes(old)
				declared[member+".gz"] = gz
			}
			if mode == "undeclared_sibling" {
				delete(declared, member+".gz")
			}
			release = dualHashRelease(declared)
			if mode == "compressed_hash_wrong" {
				gz = append(bytes.Clone(gz), 1)
			}
			env.fetcher.put(base+member, gz)
			env.fetcher.put(base+member+".gz", gz)
			if mode == "unauthorized" {
				env.fetcher.failWith(base+member, &fetch.StatusError{Code: 403})
			}
			if err := env.adopter.Run(ctx, env.suite, release, "", ""); err == nil {
				t.Fatal("adopted invalid index")
			}
			if got := currentID(t, env.cache, env.suite); got != id {
				t.Fatal("replaced previous snapshot")
			}
			if mode == "undeclared_sibling" || mode == "unauthorized" {
				// Check the direct recovery helper as Run may independently fetch
				// declared siblings before it encounters the failed plain member.
				members, _ := ParseRelease(release)
				decl := memberDeclarations(members)
				before := len(env.fetcher.requested())
				_, _ = env.adopter.adoptMemberFromRelease(ctx, env.suite, decl[member], false, decl)
				for _, u := range env.fetcher.requested()[before:] {
					if u != base+member {
						t.Fatalf("unexpected fallback: %s", u)
					}
				}
			}
		})
	}
}

func TestReconcileSnapshot_ReconstructIndex(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"source"})
	const member = "main/source/Sources"
	plain := recoverySource("1")
	gz := gzipBytes(plain)
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	release := dualHashRelease(map[string][]byte{member: plain, member + ".gz": gz})
	env.fetcher.put(base+member, plain)
	env.fetcher.put(base+member+".gz", gz)
	if err := ad.Run(ctx, env.suite, release, "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	if err := env.cache.DeleteSnapshotMembersForTest(ctx, id, member, member+".gz"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(env.cache.BlobPath(shaOf(plain))); err != nil {
		t.Fatal(err)
	}
	env.fetcher.put(base+member, gz)
	if n, err := ad.ReconcileSnapshot(ctx, env.suite, id, true); err != nil || n != 2 {
		t.Fatalf("healed=%d err=%v", n, err)
	}
	row, err := env.cache.GetSnapshotMember(ctx, id, member)
	if err != nil || row.BlobHash != shaOf(plain) {
		t.Fatalf("wrong healed source: %+v %v", row, err)
	}
}

func TestAdopter_ReconstructPackages(t *testing.T) {
	env := newAdoptionTestEnv(t)
	member := "main/binary-amd64/Packages"
	plain := fakePackagesStanzas(map[string]string{"pool/main/h/hello/hello_1_amd64.deb": strings.Repeat("a", 64)})
	gz := gzipBytes(plain)
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+member, gz)
	env.fetcher.put(base+member+".gz", gz)
	release := dualHashRelease(map[string][]byte{member: plain, member + ".gz": gz})
	ctx := context.Background()
	if err := env.adopter.Run(ctx, env.suite, release, "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, env.cache, env.suite)
	row, err := env.cache.GetSnapshotMember(ctx, id, member)
	if err != nil || row.BlobHash != shaOf(plain) {
		t.Fatalf("wrong recovered Packages: %+v %v", row, err)
	}
}

func newRealClientAdopter(t *testing.T, srv *httptest.Server) (*Adopter, *cache.Cache) {
	t.Helper()
	client, err := fetch.New(fetch.Options{Proxy: srv.URL, AllowedHostRegex: []string{`^archive\.example\.com$`}})
	if err != nil {
		t.Fatal(err)
	}
	env := newAdoptionTestEnv(t)
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: client, Verifier: passThroughVerifier{}, HostLimiter: hostsem.New(8), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return ad, env.cache
}

func currentID(t *testing.T, c *cache.Cache, suite SuiteRef) int64 {
	t.Helper()
	row, err := c.GetSuiteFreshness(context.Background(), suite.CanonicalScheme, suite.CanonicalHost, suite.SuitePath)
	if err != nil {
		t.Fatal(err)
	}
	if row.CurrentSnapshotID == nil {
		t.Fatal("no adopted snapshot")
	}
	return *row.CurrentSnapshotID
}

// No client request for Sources or deb-src configuration is needed: default
// adoption fetches both declared representations in the background.
func TestAdopter_ReconstructRaspberryPiSources(t *testing.T) {
	compressed, err := os.ReadFile("testdata/issue32/Sources.gz")
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if len(compressed) != 123988 || shaOf(compressed) != "e9ffbb3867746a0dce3633f770decef4337dfa6ccfd67379a53ab275ef54c773" || len(plain) != 443932 || shaOf(plain) != "9d688fee5dd02cf4f3b28b9d94f8d260aae7db4b95927b5b1382b037dd40720f" {
		t.Fatal("fixture does not match the archive's Release declarations")
	}
	const member = "main/source/Sources"
	suite := SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example.com", SuitePath: "/debian/dists/trixie"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != suite.SuitePath+"/"+member && r.URL.Path != suite.SuitePath+"/"+member+".gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Location", "Sources.gz")
		w.Header().Set("Content-Type", "application/x-gzip")
		w.Header().Set("Content-Length", fmt.Sprint(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer srv.Close()
	ad, c := newRealClientAdopter(t, srv)
	ctx := context.Background()
	_, err = ad.adoptMember(ctx, suite, ReleaseMember{Path: member, SHA256: shaOf(plain), Size: int64(len(plain))}, false)
	var me *AdoptionMemberError
	if !errors.As(err, &me) || me.Detail != "served 123988 vs declared 443932" {
		t.Fatalf("did not reproduce reported error: %v", err)
	}
	release, _ := makeRelease(map[string][]byte{member: plain, member + ".gz": compressed})
	if err := ad.Run(ctx, suite, release, "", ""); err != nil {
		t.Fatal(err)
	}
	id := currentID(t, c, suite)
	for path, want := range map[string][]byte{member: plain, member + ".gz": compressed} {
		row, err := c.GetSnapshotMember(ctx, id, path)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(c.BlobPath(row.BlobHash))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("wrong stored representation for %s: %v", path, err)
		}
	}
}
