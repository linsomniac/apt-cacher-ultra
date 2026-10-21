package aptpath

import (
	"strings"
	"testing"
)

func TestSuitePath_Dists(t *testing.T) {
	cases := []struct{ path, want string }{
		// unchanged behaviour
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex64, "/ubuntu/dists/noble"},
		{"/dists/stable/Release", "/dists/stable"},
		{"/debian/dists/bookworm-updates/InRelease", "/debian/dists/bookworm-updates"},
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
		{"/a/dists/b/dists/c/InRelease", "/a/dists/b/dists/c"},
		// deferred minor A-4: the last "dists" followed by a codename AND a
		// member wins; "/a/dists/b/dists/c" has no member after "c".
		{"/a/dists/b/dists/c", "/a/dists/b"},
		{"/dépôt/dists/x/InRelease", "/dépôt/dists/x"},     // UTF-8 is not a forbidden byte
		{"/my repo/dists/x/InRelease", "/my repo/dists/x"}, // deferred minor A-6: a space is not either
		{"/ubuntu/pool/main/h/hello/hello.deb", ""},
		{"/", ""},
		{"", ""},
		{"relative/dists/noble/InRelease", ""},
		// tightened (spec C2): a suite must have a member
		{"/ubuntu/dists/noble", ""},
		{"/dists/stable", ""},
		{"/foo/dists/InRelease", ""},
		// tightened: non-canonical segments
		{"/ubuntu//dists/noble/InRelease", ""},
		{"/ubuntu/./dists/noble/InRelease", ""},
		{"/ubuntu/pool/../dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/main/../InRelease", ""},
		{"/ubuntu/dists/noble//InRelease", ""},
		{"/ubuntu/dists/./InRelease", ""},
		{"/ubuntu/dists/noble/InRelease/", ""}, // deferred minor A-6: a trailing slash is an empty segment
		// tightened: forbidden decoded bytes (S-3)
		{"/ubuntu#x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble?x/InRelease", ""},
		{"/ubuntu/dists/noble/In%52elease", ""},
		{"/ubuntu\\x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/In\x00Release", ""},
		{"/ubuntu/dists/noble/In\x7fRelease", ""},
	}
	for _, c := range cases {
		if got := SuitePath(c.path); got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// Spec C2's flat grammar. The first block is PR #8's TestSuitePath flat
// cases, verbatim (plan overview, "PR #8 parity" item 1), with our results:
// the one intentional difference is Contents-* (C2 gives it a suite), and
// a valid 64-hex by-hash name is added.
func TestSuitePath_Flat(t *testing.T) {
	const k8s = "/core:/stable:/v1.34/deb"
	cases := []struct{ path, want string }{
		// PR #8, verbatim
		{k8s + "/InRelease", k8s},
		{k8s + "/Release", k8s},
		{k8s + "/Release.gpg", k8s},
		{k8s + "/Packages", k8s},
		{k8s + "/Packages.gz", k8s},
		{k8s + "/Packages.zst", k8s},
		{"/repo/flat/Sources.xz", "/repo/flat"},
		{k8s + "/amd64/kubelet_1.34.1-1.1_amd64.deb", ""},
		{"/repo/flat/Packages_1.0_all.deb", ""},
		{"/repo/flat/Translation-en", ""},
		{"/repo/flat/Contents-amd64.gz", "/repo/flat"}, // PR #8: "" — spec C2 gives Contents-* a suite
		{"/repo/flat/by-hash/SHA256/abc", ""},
		{"/InRelease", ""},
		{"/Packages.gz", ""},
		{"InRelease", ""}, // not absolute
		{k8s + "/./InRelease", k8s + "/."},
		{k8s + "/./Packages.gz", k8s + "/."},
		{"/repo//InRelease", ""},
		{"/repo/./sub/InRelease", ""},
		{"/evil/../pkgs/InRelease", ""},
		{"/repo/%2E/InRelease", ""}, // a decoded %
		{"/./InRelease", ""},
		{"/ubuntu/dists//noble/InRelease", ""},
		// added: a valid by-hash name
		{"/repo/flat/by-hash/SHA256/" + hex64, "/repo/flat"},
		// the rest of spec C2's member grammar
		{"/repo/flat/by-hash/SHA512/" + hex64 + hex64, "/repo/flat"},
		{"/repo/flat/by-hash/SHA512/" + hex64, ""}, // a SHA512 name is 128 hex digits
		{"/repo/flat/by-hash/SHA256/" + strings.ToUpper(hex64), ""},
		{"/repo/flat/by-hash/MD5Sum/0123456789abcdef0123456789abcdef", ""},
		{"/repo/flat/Packages.diff/Index", "/repo/flat"},
		{"/repo/flat/Sources.diff/Index", "/repo/flat"},
		{"/repo/flat/Packages.diff/2026-10-20-0000.00.gz", ""}, // a pdiff patch: Phase 1 only
		{"/repo/flat/Contents-amd64.diff/Index", ""},
		{"/repo/flat/Contents-udeb-arm64", "/repo/flat"},
		{"/repo/flat/Sources.bz2", "/repo/flat"},
		{"/repo/flat/Packages.lzma", "/repo/flat"},
		{"/repo/flat/Packages.lz4", "/repo/flat"},
		{"/repo/flat/Packages.Z", ""},
		{"/repo/flat/en", ""}, // a flat Translation file (apt's $(LANGUAGE)): Phase 1 only
		{"/repo/flat/Contents-AMD64.gz", ""},
		{"/repo/flat/Contents-", ""}, // Contents- needs a class
		// each member regexp is anchored at both ends, and a by-hash name
		// needs its by-hash directory (the grammar never widens)
		{"/repo/flat/x/SHA256/" + hex64, ""},
		{"/repo/flat/xInRelease", ""},
		{"/repo/flat/old-Packages.gz", ""},
		{"/repo/flat/by-hash/SHA256/" + hex64 + "0", ""},
		{"/repo/flat/by-hash/SHA512/x" + hex64 + hex64, ""},
		{"/repo/flat/xPackages.diff/Index", ""},
		{"/repo/InRelease/", ""},
		{"/repo/./InRelease/", ""},
		{"/repo/./.", ""},
		{"/repo/./x/./InRelease", ""},
		{"/repo/././InRelease", ""}, // exactly one trailing "/."
		{"/repo/../InRelease", ""},
		{"/repo/x\\y/InRelease", ""},
		{"/repo/x#y/InRelease", ""},
		{"/r/InRelease", "/r"},
		{"/./Packages.gz", ""},
		{"/by-hash/SHA256/" + hex64, ""},
		// a dists segment anywhere: never flat (spec C2)
		{"/repo/dists/InRelease", ""},
		{"/dists/Packages.gz", ""},
		{"/repo/dists/x/y/Packages.gz", "/repo/dists/x"},
		{"/dists/x/Packages.gz", "/dists/x"},
		{"/repo/distsy/Packages.gz", "/repo/distsy"},
		{"/repo/dists.d/InRelease", "/repo/dists.d"},
	}
	for _, c := range cases {
		if got := SuitePath(c.path); got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestIsCanonicalSuitePath(t *testing.T) {
	cases := []struct {
		suite string
		want  bool
	}{
		{"/ubuntu/dists/noble", true},
		{"/dists/stable", true},
		// "InRelease" is a legal codename under the spec C2 grammar
		// (SuitePath("/foo/dists/InRelease/x") = "/foo/dists/InRelease").
		// apt never requests such a suite, so phase F reaps any legacy row
		// by idleness, not as non-canonical.
		{"/foo/dists/InRelease", true},
		{"/ubuntu/dists/noble/", false},
		{"/ubuntu//dists/noble", false},
		{"/ubuntu/./dists/noble", false},
		{"/ubuntu#x/dists/noble", false},
		{"/foo/dists", false},               // no codename
		{"/ubuntu/dists/noble/main", false}, // deeper than a suite
		{"", false},
		// flat suites (phase G)
		{"/repo", true},
		{"/repo/.", true},
		{"/core:/stable:/v1.34/deb", true},
		{"/repo/./.", false},
		{"/repo/./x", false},
		{"/repo//x", false},
		{"/repo/", false},
		{"/", false},
		{"/.", false},
		{"/repo/%2E", false},
		{"/repo/dists", false},
	}
	for _, c := range cases {
		if got := IsCanonicalSuitePath(c.suite); got != c.want {
			t.Errorf("IsCanonicalSuitePath(%q) = %v, want %v", c.suite, got, c.want)
		}
	}
}

// IsFlatSuitePath: a canonical suite path of the flat grammar.
func TestIsFlatSuitePath(t *testing.T) {
	for _, c := range []struct {
		suite string
		want  bool
	}{
		{"/repo", true},
		{"/repo/.", true},
		{"/core:/stable:/v1.34/deb", true},
		{"/debian-stable/binary", true},
		{"/repo/distsy", true},
		{"/ubuntu/dists/noble", false},
		{"/dists/stable", false},
		{"/repo/dists", false},
		{"/repo/./.", false},
		{"/repo//x", false},
		{"/", false},
		{"/.", false},
		{"", false},
	} {
		if got := IsFlatSuitePath(c.suite); got != c.want {
			t.Errorf("IsFlatSuitePath(%q) = %v, want %v", c.suite, got, c.want)
		}
	}
}

// Spec C6: a flat suite's package base, shared by the twins.
func TestFlatBase(t *testing.T) {
	for in, want := range map[string]string{
		"/repo":                    "/repo",
		"/repo/.":                  "/repo",
		"/core:/stable:/v1.34/deb": "/core:/stable:/v1.34/deb",
		"/repo/./.":                "/repo/.", // only one trailing "/." is removed
	} {
		if got := FlatBase(in); got != want {
			t.Errorf("FlatBase(%q) = %q, want %q", in, got, want)
		}
		if got := MarkerBase(in); got != FlatBase(in) {
			t.Errorf("MarkerBase(%q) = %q, FlatBase %q: the two must agree", in, got, FlatBase(in))
		}
	}
}

// Spec C6: "." segments removed, nothing else.
func TestDotClean(t *testing.T) {
	for in, want := range map[string]string{
		"./cuda_12.6_amd64.deb": "cuda_12.6_amd64.deb",
		"amd64/kubectl.deb":     "amd64/kubectl.deb",
		"/flat/./amd64/x.deb":   "/flat/amd64/x.deb",
		"/flat/././x.deb":       "/flat/x.deb",
		"a/./b/./c.deb":         "a/b/c.deb",
		"/flat/../ubuntu/x.deb": "/flat/../ubuntu/x.deb",
		"/flat//x.deb":          "/flat//x.deb",
		"pkg_1%3a2.deb":         "pkg_1%3a2.deb",
		"./.hidden.deb":         ".hidden.deb",
		"..deb":                 "..deb",
		"x.deb/.":               "x.deb",
		".":                     "",
		"":                      "",
	} {
		if got := DotClean(in); got != want {
			t.Errorf("DotClean(%q) = %q, want %q", in, got, want)
		}
	}
}

// Spec C2: a flat member at the host root (`deb https://h/ /` and
// `deb https://h ./`) gets no suite; the handler logs it.
func TestIsFlatHostRootPath(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/InRelease", true},
		{"/Release.gpg", true},
		{"/Packages.gz", true},
		{"/./InRelease", true},
		{"/./Packages.xz", true},
		{"/by-hash/SHA256/" + hex64, true},
		{"/Packages.diff/Index", true},
		{"/repo/InRelease", false},
		{"/repo/./InRelease", false},
		{"/././InRelease", false},
		{"/pool/x.deb", false},
		{"/Translation-en", false},
		{"/dists/InRelease", false},
		{"InRelease", false},
		{"/In%52elease", false},
		{"", false},
	} {
		if got := IsFlatHostRootPath(c.path); got != c.want {
			t.Errorf("IsFlatHostRootPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
