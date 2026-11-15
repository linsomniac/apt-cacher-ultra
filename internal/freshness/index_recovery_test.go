package freshness

import (
	"bytes"
	"context"
	"crypto/sha512"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
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
				trackSuite(t, c, suite)
				release := dualHashRelease(map[string][]byte{member: plain, member + codec: compressed})
				if err := ad.Run(context.Background(), suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
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
					for _, alias := range byHashAliases(ReleaseMember{Path: p, SHA256: shaOf(expected), SHA512: fmt.Sprintf("%x", sha512.Sum512(expected))}) {
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
			if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
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
			if err := env.adopter.Run(ctx, env.suite, simtest.ClearsignedFixture(string(release)), "", ""); err == nil {
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
				_, _, _ = env.adopter.adoptMemberFromRelease(ctx, env.suite, decl[member], probePath, decl)
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
	ad := newReconcileAdopter(t, env, nil)
	const member = "main/source/Sources"
	plain := recoverySource("1")
	gz := gzipBytes(plain)
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	release := dualHashRelease(map[string][]byte{member: plain, member + ".gz": gz})
	env.fetcher.put(base+member, plain)
	env.fetcher.put(base+member+".gz", gz)
	if err := ad.Run(ctx, env.suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
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

func TestAdopter_ReconstructPackagesIncludingFlat(t *testing.T) {
	for _, flat := range []bool{false, true} {
		t.Run(fmt.Sprintf("flat=%t", flat), func(t *testing.T) {
			env := newAdoptionTestEnv(t)
			suite := env.suite
			member := "main/binary-amd64/Packages"
			if flat {
				suite.SuitePath = "/repo/flat"
				member = "Packages"
				trackSuite(t, env.cache, suite)
			}
			plain := fakePackagesStanzas(map[string]string{"pool/main/h/hello/hello_1_amd64.deb": strings.Repeat("a", 64)})
			gz := gzipBytes(plain)
			base := suite.CanonicalScheme + "://" + suite.CanonicalHost + suite.SuitePath + "/"
			env.fetcher.put(base+member, gz)
			env.fetcher.put(base+member+".gz", gz)
			release := dualHashRelease(map[string][]byte{member: plain, member + ".gz": gz})
			ctx := context.Background()
			if err := env.adopter.Run(ctx, suite, simtest.ClearsignedFixture(string(release)), "", ""); err != nil {
				t.Fatal(err)
			}
			id := currentID(t, env.cache, suite)
			snapshot, err := env.cache.GetSuiteSnapshot(ctx, id)
			if err != nil || !snapshot.PackageCoverageComplete {
				t.Fatalf("incomplete reconstructed binary index: %+v %v", snapshot, err)
			}
		})
	}
}
