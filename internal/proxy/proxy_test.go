package proxy

import (
	"errors"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

// newTestParser builds a Parser with no user remap rules and no mirrors,
// exercising only the built-in Remap defaults. It is the right starting
// point for any Parse-level test that does not specifically test config.
func newTestParser(t *testing.T) *Parser {
	t.Helper()
	p, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestParse_EmptyURI(t *testing.T) {
	p := newTestParser(t)
	if _, err := p.Parse("", ""); !errors.Is(err, ErrEmptyURI) {
		t.Fatalf("want ErrEmptyURI, got %v", err)
	}
}

func TestParse_ProxyMode_Basic(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://archive.ubuntu.com/ubuntu/dists/noble/InRelease", "archive.ubuntu.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Mode != ModeProxy {
		t.Errorf("Mode: got %s, want proxy", r.Mode)
	}
	if r.CanonicalScheme != "http" || r.CanonicalHost != "archive.ubuntu.com" {
		t.Errorf("canonical: got (%s, %s)", r.CanonicalScheme, r.CanonicalHost)
	}
	if r.Path != "/ubuntu/dists/noble/InRelease" {
		t.Errorf("path: %q", r.Path)
	}
	if r.UpstreamURL != "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease" {
		t.Errorf("upstream: %q", r.UpstreamURL)
	}
	if !r.IsMetadata {
		t.Errorf("IsMetadata: want true (InRelease)")
	}
	if r.SuitePath != "/ubuntu/dists/noble" {
		t.Errorf("SuitePath: %q", r.SuitePath)
	}
}

func TestParse_ProxyMode_GeoRemap(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://us.archive.ubuntu.com/ubuntu/dists/noble/InRelease", "us.archive.ubuntu.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "archive.ubuntu.com" {
		t.Errorf("CanonicalHost: got %q, want archive.ubuntu.com (built-in remap)", r.CanonicalHost)
	}
	if r.UpstreamURL != "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease" {
		t.Errorf("UpstreamURL: %q (should follow canonical, not the original geo mirror)", r.UpstreamURL)
	}
}

func TestParse_ProxyMode_PortStripped(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://archive.ubuntu.com:80/ubuntu/dists/noble/InRelease", "archive.ubuntu.com:80")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "archive.ubuntu.com" {
		t.Errorf("CanonicalHost: got %q (port should be stripped)", r.CanonicalHost)
	}
	if strings.Contains(r.UpstreamURL, ":80") {
		t.Errorf("UpstreamURL should not contain :80, got %q", r.UpstreamURL)
	}
}

func TestParse_ProxyMode_RootPath(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://archive.ubuntu.com", "archive.ubuntu.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Path != "/" {
		t.Errorf("Path: got %q, want /", r.Path)
	}
}

func TestParse_ProxyMode_RejectsUnsupportedScheme(t *testing.T) {
	p := newTestParser(t)
	_, err := p.Parse("ftp://archive.ubuntu.com/foo", "")
	if !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("want ErrUnsupportedScheme, got %v", err)
	}
}

func TestParse_HTTPSMagic(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://HTTPS///apt.corretto.aws/dists/stable/Release", "HTTPS")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalScheme != "https" {
		t.Errorf("CanonicalScheme: got %q, want https", r.CanonicalScheme)
	}
	if r.CanonicalHost != "apt.corretto.aws" {
		t.Errorf("CanonicalHost: got %q", r.CanonicalHost)
	}
	if r.Path != "/dists/stable/Release" {
		t.Errorf("Path: got %q", r.Path)
	}
	if r.UpstreamURL != "https://apt.corretto.aws/dists/stable/Release" {
		t.Errorf("UpstreamURL: %q", r.UpstreamURL)
	}
}

func TestParse_HTTPSMagic_CaseInsensitive(t *testing.T) {
	p := newTestParser(t)
	// apt-cacher-ng accepts lowercase "https" too.
	r, err := p.Parse("http://https///apt.corretto.aws/dists/stable/Release", "https")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalScheme != "https" || r.CanonicalHost != "apt.corretto.aws" {
		t.Errorf("got (%s, %s)", r.CanonicalScheme, r.CanonicalHost)
	}
}

func TestParse_HTTPSMagic_Empty(t *testing.T) {
	p := newTestParser(t)
	_, err := p.Parse("http://HTTPS///", "HTTPS")
	if !errors.Is(err, ErrInvalidHTTPSMagic) {
		t.Fatalf("want ErrInvalidHTTPSMagic, got %v", err)
	}
}

func TestParse_HTTPSMagic_OnlyHost(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://HTTPS///apt.corretto.aws", "HTTPS")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "apt.corretto.aws" || r.Path != "/" {
		t.Errorf("got (%s, %s)", r.CanonicalHost, r.Path)
	}
}

func TestParse_NotMagic_HTTPSHostOnly(t *testing.T) {
	// host=HTTPS but path doesn't start with `///` — not the magic form.
	// The literal host "HTTPS" is treated as-is. Cache lookup will then
	// fail the upstream allowlist, which is the correct behavior.
	p := newTestParser(t)
	r, err := p.Parse("http://HTTPS/foo/bar", "HTTPS")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalScheme != "http" {
		t.Errorf("CanonicalScheme: got %q, want http (not magic)", r.CanonicalScheme)
	}
	if r.CanonicalHost != "HTTPS" {
		t.Errorf("CanonicalHost: got %q", r.CanonicalHost)
	}
}

func TestParse_MirrorMode_Basic(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: "http://archive.ubuntu.com/ubuntu/"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("/ubuntu/dists/noble/InRelease", "cache.example.com:3142")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Mode != ModeMirror {
		t.Errorf("Mode: got %s", r.Mode)
	}
	if r.CanonicalHost != "archive.ubuntu.com" {
		t.Errorf("CanonicalHost: got %q", r.CanonicalHost)
	}
	if r.Path != "/ubuntu/dists/noble/InRelease" {
		t.Errorf("Path: %q", r.Path)
	}
	if r.UpstreamURL != "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease" {
		t.Errorf("UpstreamURL: %q", r.UpstreamURL)
	}
}

func TestParse_MirrorMode_PrefixMismatch(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: "http://archive.ubuntu.com/ubuntu/"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// /ubuntu-extra must not match /ubuntu (path boundary semantics).
	if _, err := p.Parse("/ubuntu-extra/foo", ""); !errors.Is(err, ErrNoMirrorMatch) {
		t.Fatalf("want ErrNoMirrorMatch for /ubuntu-extra (boundary), got %v", err)
	}
}

func TestParse_MirrorMode_NoRoutes(t *testing.T) {
	p := newTestParser(t)
	if _, err := p.Parse("/anything", ""); !errors.Is(err, ErrNoMirrorMatch) {
		t.Fatalf("want ErrNoMirrorMatch with no mirrors configured, got %v", err)
	}
}

func TestParse_MirrorMode_TrailingSlashPrefixNormalized(t *testing.T) {
	// User wrote `/ubuntu/` (trailing slash) — must behave identically.
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/ubuntu/", Upstream: "http://archive.ubuntu.com/ubuntu/"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("/ubuntu/dists/noble/InRelease", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Path != "/ubuntu/dists/noble/InRelease" {
		t.Errorf("Path: %q", r.Path)
	}
}

func TestParse_MirrorMode_PrefixIsExactPath(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: "http://archive.ubuntu.com/ubuntu"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("/ubuntu", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Upstream basePath="/ubuntu", request rest="/" → "/ubuntu"
	if r.Path != "/ubuntu" {
		t.Errorf("Path: %q", r.Path)
	}
}

func TestParse_MirrorMode_HTTPSUpstream(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/corretto", Upstream: "https://apt.corretto.aws/"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("/corretto/dists/stable/Release", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalScheme != "https" {
		t.Errorf("CanonicalScheme: got %q", r.CanonicalScheme)
	}
	if r.UpstreamURL != "https://apt.corretto.aws/dists/stable/Release" {
		t.Errorf("UpstreamURL: %q", r.UpstreamURL)
	}
}

func TestParse_MirrorMode_RootPrefix(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/", Upstream: "http://archive.ubuntu.com/ubuntu/"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("/dists/noble/InRelease", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.UpstreamURL != "http://archive.ubuntu.com/ubuntu/dists/noble/InRelease" {
		t.Errorf("UpstreamURL: %q", r.UpstreamURL)
	}
}

func TestNew_RejectsBadRemapRegex(t *testing.T) {
	_, err := New([]config.RemapRule{
		{MatchHostRegex: "(", CanonicalHost: "x"},
	}, nil)
	if err == nil {
		t.Fatal("want compile error from bad regex")
	}
}

func TestNew_RejectsEmptyRemapField(t *testing.T) {
	_, err := New([]config.RemapRule{
		{MatchHostRegex: "", CanonicalHost: "x"},
	}, nil)
	if err == nil {
		t.Fatal("want error for empty match_host_regex")
	}
	_, err = New([]config.RemapRule{
		{MatchHostRegex: "^x$", CanonicalHost: ""},
	}, nil)
	if err == nil {
		t.Fatal("want error for empty canonical_host")
	}
}

func TestNew_RejectsBadMirror(t *testing.T) {
	cases := []config.MirrorRule{
		{Prefix: "ubuntu", Upstream: "http://x/"}, // missing leading /
		{Prefix: "/ubuntu", Upstream: ""},         // empty upstream
		{Prefix: "/ubuntu", Upstream: "ftp://x/"}, // bad scheme
		{Prefix: "/ubuntu", Upstream: "http:///"}, // missing host
	}
	for i, c := range cases {
		if _, err := New(nil, []config.MirrorRule{c}); err == nil {
			t.Errorf("case %d: want error for %+v", i, c)
		}
	}
}

func TestRemap_UserRulesBeforeBuiltins(t *testing.T) {
	// User rule that targets archive.ubuntu.com itself — should win over
	// the built-in identity. This is the SPEC §3.2 "override built-ins"
	// scenario.
	p, err := New([]config.RemapRule{
		{MatchHostRegex: `^archive\.ubuntu\.com$`, CanonicalHost: "internal.example.com"},
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("http://archive.ubuntu.com/ubuntu/foo", "archive.ubuntu.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "internal.example.com" {
		t.Errorf("CanonicalHost: got %q, want internal.example.com (user rule must win)", r.CanonicalHost)
	}
}

func TestRemap_BuiltinFiresWhenNoUserRuleMatches(t *testing.T) {
	p, err := New([]config.RemapRule{
		{MatchHostRegex: `^never-matches\.example\.com$`, CanonicalHost: "wrong.example.com"},
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := p.Parse("http://us.archive.ubuntu.com/foo", "us.archive.ubuntu.com")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "archive.ubuntu.com" {
		t.Errorf("CanonicalHost: got %q (built-in should fire)", r.CanonicalHost)
	}
}

func TestRemap_BuiltinDebianGeo(t *testing.T) {
	p := newTestParser(t)
	for _, host := range []string{"de.debian.org", "ftp.de.debian.org", "deb.debian.org"} {
		r, err := p.Parse("http://"+host+"/debian/dists/stable/InRelease", host)
		if err != nil {
			t.Fatalf("Parse %s: %v", host, err)
		}
		if r.CanonicalHost != "deb.debian.org" {
			t.Errorf("%s -> got %q, want deb.debian.org", host, r.CanonicalHost)
		}
	}
}

func TestRemap_NoRuleKeepsHost(t *testing.T) {
	p := newTestParser(t)
	r, err := p.Parse("http://apt.corretto.aws/foo", "apt.corretto.aws")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "apt.corretto.aws" {
		t.Errorf("CanonicalHost: got %q, want apt.corretto.aws (passthrough)", r.CanonicalHost)
	}
}

func TestRemap_AnchoredPatterns(t *testing.T) {
	// The built-ins are anchored: a hostname that contains a built-in
	// pattern as a substring must not match. This guards against an
	// attacker-controlled host like "archive.ubuntu.com.attacker.tld".
	p := newTestParser(t)
	r, err := p.Parse("http://archive.ubuntu.com.attacker.tld/foo", "archive.ubuntu.com.attacker.tld")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CanonicalHost != "archive.ubuntu.com.attacker.tld" {
		t.Errorf("CanonicalHost: got %q, want passthrough (anchored regex must not match suffix)", r.CanonicalHost)
	}
}

func TestIsMetadata(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/ubuntu/dists/noble/InRelease", true},
		{"/ubuntu/dists/noble/Release", true},
		{"/ubuntu/dists/noble/Release.gpg", true},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages", true},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", true},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.xz", true},
		{"/ubuntu/dists/noble/main/source/Sources.gz", true},
		{"/ubuntu/dists/noble/main/Contents-amd64.gz", true},
		{"/ubuntu/dists/noble/main/i18n/Translation-en.bz2", true},
		{"/ubuntu/dists/noble/main/dep11/Components-amd64.yml.gz", true},
		{"/ubuntu/dists/noble/main/dep11/icons-32x32.tar.gz", true},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.diff/Index", true},
		{"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/abc", true},
		{"/ubuntu/pool/main/h/hello/hello_2.10-2_amd64.deb", false},
		{"/ubuntu/pool/main/h/hello/hello_2.10-2_amd64.udeb", false},
		{"/ubuntu/dists/noble/main/binary-amd64/Index", false}, // not under a *.diff/
		{"", false},
		{"/", false},
	}
	for _, c := range cases {
		got := IsMetadata(c.path)
		if got != c.want {
			t.Errorf("IsMetadata(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestSuitePath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", "/ubuntu/dists/noble"},
		{"/dists/stable/Release", "/dists/stable"},      // no repo prefix
		{"/dists/stable", "/dists/stable"},              // bare suite path
		{"/ubuntu/pool/main/h/hello/hello.deb", ""},     // not under /dists/
		{"/", ""},                                       // root
		{"", ""},                                        // empty
		{"/debian/dists/bookworm-updates/InRelease", "/debian/dists/bookworm-updates"},
		// nested repo paths
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
	}
	for _, c := range cases {
		got := SuitePath(c.path)
		if got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestStripPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"archive.ubuntu.com", "archive.ubuntu.com"},
		{"archive.ubuntu.com:80", "archive.ubuntu.com"},
		{"archive.ubuntu.com:3142", "archive.ubuntu.com"},
		{"[::1]", "[::1]"},
		{"[::1]:80", "[::1]"},
		{"[2001:db8::1]:443", "[2001:db8::1]"},
		{"", ""},
	}
	for _, c := range cases {
		got := stripPort(c.in)
		if got != c.want {
			t.Errorf("stripPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParse_ProxyMode_RejectsRelativeForm(t *testing.T) {
	p := newTestParser(t)
	// A path-only request line like "foo/bar" (no leading /) is neither
	// proxy nor mirror form. Must be rejected.
	if _, err := p.Parse("foo/bar", ""); !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("want ErrInvalidURI, got %v", err)
	}
}

func TestParse_ProxyMode_AcceptsHEAD_StyleURL(t *testing.T) {
	// HEAD requests carry the same absolute-URI form. There's no method
	// distinction inside this package — verify Parse is method-agnostic.
	p := newTestParser(t)
	r, err := p.Parse("http://archive.ubuntu.com/ubuntu/dists/noble/InRelease", "")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Mode != ModeProxy {
		t.Errorf("Mode: got %s", r.Mode)
	}
}

func TestModeString(t *testing.T) {
	if ModeProxy.String() != "proxy" {
		t.Errorf("ModeProxy.String() = %q", ModeProxy.String())
	}
	if ModeMirror.String() != "mirror" {
		t.Errorf("ModeMirror.String() = %q", ModeMirror.String())
	}
	if got := Mode(99).String(); got != "Mode(99)" {
		t.Errorf("Mode(99).String() = %q", got)
	}
}

func TestJoinMirrorPath(t *testing.T) {
	cases := []struct {
		basePath, rest, want string
	}{
		{"/", "/dists/noble/InRelease", "/dists/noble/InRelease"},
		{"", "/dists/noble/InRelease", "/dists/noble/InRelease"},
		{"/", "", "/"},
		{"/ubuntu", "/dists/noble", "/ubuntu/dists/noble"},
		{"/ubuntu/", "/dists/noble", "/ubuntu/dists/noble"}, // trailing slash collapsed
		{"/ubuntu", "", "/ubuntu"},                          // exact-prefix-match → mirror root
	}
	for _, c := range cases {
		got := joinMirrorPath(c.basePath, c.rest)
		if got != c.want {
			t.Errorf("joinMirrorPath(%q, %q) = %q, want %q", c.basePath, c.rest, got, c.want)
		}
	}
}
