package freshness

import (
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func TestBuildMemberURL_UsesAptEncoding(t *testing.T) {
	s := SuiteRef{CanonicalScheme: "https", CanonicalHost: "example.org", SuitePath: "/repo/dists/x"}
	cases := []struct{ rel, want string }{
		{"main/binary-amd64/Packages.gz", "https://example.org/repo/dists/x/main/binary-amd64/Packages.gz"},
		{"main/by-hash/SHA256/" + testHex64, "https://example.org/repo/dists/x/main/by-hash/SHA256/" + testHex64},
		{"weird/a+b~c", "https://example.org/repo/dists/x/weird/a%2bb%7ec"},
	}
	for _, c := range cases {
		if got := buildMemberURL(s, c.rel); got != c.want {
			t.Errorf("buildMemberURL(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

func TestDefaultURLReconstruct_UsesAptEncoding(t *testing.T) {
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: testHostSem()})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.urlReconstruct("http", "h", "/a+b/InRelease"), "http://h/a%2bb/InRelease"; got != want {
		t.Errorf("urlReconstruct = %q, want %q", got, want)
	}
}

const testHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testHostSem() *hostsem.Sem { return hostsem.New(8) }
