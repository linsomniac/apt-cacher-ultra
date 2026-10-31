package aptpath

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestAptEncodePath_Table(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble/InRelease"},
		{"/core:/stable:/v1.34/deb/InRelease", "/core:/stable:/v1.34/deb/InRelease"},
		{"/repo/./x.deb", "/repo/./x.deb"},
		{"/x/a@b/c", "/x/a@b/c"},
		{"/pool/libstdc++-11-dev_11.4.0-1ubuntu1~22.04.3_amd64.deb",
			"/pool/libstdc%2b%2b-11-dev_11.4.0-1ubuntu1%7e22.04.3_amd64.deb"},
		{"/p/foo_1%3a2.deb", "/p/foo_1%253a2.deb"},
		{"/a b/c", "/a%20b/c"},
		{"/a#b?c", "/a%23b%3fc"},
		{"/x/pkg+x|y.deb", "/x/pkg%2bx%7cy.deb"},
		{"/x/{}^`\"<>\\", "/x/%7b%7d%5e%60%22%3c%3e%5c"},
		{"/é", "/%c3%a9"},
	}
	for _, c := range cases {
		if got := AptEncodePath(c.in); got != c.want {
			t.Errorf("AptEncodePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Every single byte, in every position that matters, must survive
// BuildUpstreamURL -> url.Parse as the same decoded path, and Go must use
// our encoding verbatim (EscapedPath == RawPath).
func TestBuildUpstreamURL_AllBytesRoundTrip(t *testing.T) {
	for b := 0; b < 256; b++ { // NUL included (deferred minor A-9)
		if b == '/' {
			continue
		}
		path := "/a" + string([]byte{byte(b)}) + "z/" + string([]byte{byte(b)})
		raw := BuildUpstreamURL("http", "h.example:8080", path)
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("byte %#x: url.Parse(%q): %v", b, raw, err)
		}
		if u.Path != path {
			t.Fatalf("byte %#x: round trip %q -> %q", b, path, u.Path)
		}
		if u.EscapedPath() != AptEncodePath(path) {
			t.Fatalf("byte %#x: Go re-escaped: %q vs ours %q", b, u.EscapedPath(), AptEncodePath(path))
		}
		if u.Host != "h.example:8080" || u.Scheme != "http" {
			t.Fatalf("byte %#x: authority changed: %s", b, raw)
		}
	}
}

// Real-world public path shapes (spec C1.4; deviation: synthetic table,
// see the overview plan). Plain paths are byte-identical to the old
// scheme://host+path concatenation.
func TestBuildUpstreamURL_PlainPathsUnchanged(t *testing.T) {
	plain := []string{
		"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex64,
		"/debian/pool/main/c/curl/curl_8.5.0-2_amd64.deb",
		"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.0-1.1_amd64.deb",
		"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-keyring_1.1-1_all.deb",
		"/debian-stable/binary/Release.gpg",
	}
	for _, p := range plain {
		if got, want := BuildUpstreamURL("https", "example.org", p), "https://example.org"+p; got != want {
			t.Errorf("BuildUpstreamURL(%q) = %q, want %q", p, got, want)
		}
	}
}

// Real-world public path shapes that DO change: each expected URL is what
// apt itself sends for that Filename.
func TestBuildUpstreamURL_RealWorldSpecialShapes(t *testing.T) {
	cases := []struct{ path, want string }{
		// '+' and '~' (Ubuntu toolchain backports)
		{"/ubuntu/pool/main/g/gcc-14/libstdc++6_14.2.0-4ubuntu2~24.04_amd64.deb",
			"https://example.org/ubuntu/pool/main/g/gcc-14/libstdc%2b%2b6_14.2.0-4ubuntu2%7e24.04_amd64.deb"},
		// '~' in a Debian stable update
		{"/debian/pool/main/s/systemd/systemd_252.33-1~deb12u1_amd64.deb",
			"https://example.org/debian/pool/main/s/systemd/systemd_252.33-1%7edeb12u1_amd64.deb"},
		// a repository that wrote the epoch colon pre-encoded into Filename:
		// apt quotes the literal '%'
		{"/p/foo_1%3a2.0_amd64.deb", "https://example.org/p/foo_1%253a2.0_amd64.deb"},
		// flat repository ("./" kept literally, as apt sends it) with '+'
		{"/compute/cuda/repos/ubuntu2204/x86_64/./libnvjitlink-dev-12-6_12.6.85-1+cuda12.6_amd64.deb",
			"https://example.org/compute/cuda/repos/ubuntu2204/x86_64/./libnvjitlink-dev-12-6_12.6.85-1%2bcuda12.6_amd64.deb"},
		// ':' is not escaped (pkgs.k8s.io paths)
		{"/core:/stable:/v1.34/deb/./kubectl_1.34.0-1.1_amd64.deb",
			"https://example.org/core:/stable:/v1.34/deb/./kubectl_1.34.0-1.1_amd64.deb"},
	}
	for _, c := range cases {
		if got := BuildUpstreamURL("https", "example.org", c.path); got != c.want {
			t.Errorf("BuildUpstreamURL(%q)\n got %q\nwant %q", c.path, got, c.want)
		}
	}
}

// Opt-in: walk a real cache database's url_path paths.
func TestBuildUpstreamURL_FixtureDB(t *testing.T) {
	dbPath := os.Getenv("ACU_URLPATH_FIXTURE_DB")
	if dbPath == "" {
		t.Skip("set ACU_URLPATH_FIXTURE_DB to a cache.db to run")
	}
	paths := readURLPaths(t, dbPath)
	for _, p := range paths { // deferred minor A-11: each message says what was got
		raw := BuildUpstreamURL("http", "h", p)
		u, err := url.Parse(raw)
		switch {
		case err != nil:
			t.Errorf("url.Parse(%q) for path %q: %v", raw, p, err)
		case u.Path != p:
			t.Errorf("round trip of %q: got %q (URL %q)", p, u.Path, raw)
		}
		if strings.ContainsAny(p, "+~") && !strings.Contains(raw, "%") {
			t.Errorf("path %q: got %q, want apt's percent-encoding", p, raw)
		}
	}
}
