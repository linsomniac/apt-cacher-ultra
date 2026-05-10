package handler

import "testing"

// TestClassifyPath covers the SPEC6_5 §6.1 / §2.3 path-class enum.
// One sub-case per enum value (binary_deb, binary_udeb, source_dsc,
// source_tarball, pdiff_index, pdiff_patch, metadata, other) plus
// edge-case overlaps (e.g. .debian.tar.xz under source_tarball, the
// per-component-arch Release file under metadata).
func TestClassifyPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"binary-deb", "pool/main/f/foo/foo_1.0_amd64.deb", "binary_deb"},
		{"binary-udeb", "pool/main/f/foo/foo_1.0_amd64.udeb", "binary_udeb"},
		{"source-dsc", "pool/main/b/bash/bash_5.1-2.dsc", "source_dsc"},
		{"source-orig-tarball", "pool/main/b/bash/bash_5.1.orig.tar.xz", "source_tarball"},
		{"source-debian-tar", "pool/main/b/bash/bash_5.1-2.debian.tar.xz", "source_tarball"},
		{"source-tar-bz2", "pool/main/b/bash/bash_5.1.orig.tar.bz2", "source_tarball"},
		{"source-tar-gz", "pool/main/b/bash/bash_5.1.orig.tar.gz", "source_tarball"},
		{"source-diff-gz", "pool/main/b/bash/bash_5.1-2.diff.gz", "source_tarball"},
		{"pdiff-index-binary", "dists/noble/main/binary-amd64/Packages.diff/Index", "pdiff_index"},
		{"pdiff-index-source", "dists/noble/main/source/Sources.diff/Index", "pdiff_index"},
		{"pdiff-patch", "dists/noble/main/binary-amd64/Packages.diff/2026-05-09-1234.56.gz", "pdiff_patch"},
		{"pdiff-patch-source", "dists/noble/main/source/Sources.diff/2026-05-09-1234.56.gz", "pdiff_patch"},
		{"metadata-Release", "dists/noble/Release", "metadata"},
		{"metadata-InRelease", "dists/noble/InRelease", "metadata"},
		{"metadata-Release-gpg", "dists/noble/Release.gpg", "metadata"},
		{"metadata-Packages", "dists/noble/main/binary-amd64/Packages", "metadata"},
		{"metadata-Packages-gz", "dists/noble/main/binary-amd64/Packages.gz", "metadata"},
		{"metadata-Packages-xz", "dists/noble/main/binary-amd64/Packages.xz", "metadata"},
		{"metadata-Sources", "dists/noble/main/source/Sources", "metadata"},
		{"metadata-Sources-gz", "dists/noble/main/source/Sources.gz", "metadata"},
		{"metadata-Contents", "dists/noble/main/Contents-amd64.gz", "metadata"},
		{"metadata-translation", "dists/noble/main/i18n/Translation-en.bz2", "metadata"},
		{"other-no-match", "some/random/path", "other"},
		{"other-empty", "", "other"},
		{"other-html", "html-error-page.html", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPath(tc.path)
			if got != tc.want {
				t.Errorf("classifyPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestArchFromPath covers the SPEC6_5 §2.3 architecture extractor.
// binary-<arch>/ paths yield <arch>; source paths yield "source";
// everything else (metadata-without-arch-segment, plain pool paths)
// yields "" (caller omits the field).
func TestArchFromPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"binary-amd64-deb", "pool/main/f/foo/foo_amd64.deb", ""},
		{"binary-amd64-packages", "dists/noble/main/binary-amd64/Packages.gz", "amd64"},
		{"binary-arm64-pdiff-patch", "dists/noble/main/binary-arm64/Packages.diff/2026-01-01-0000.00.gz", "arm64"},
		{"binary-i386-pdiff-index", "dists/noble/main/binary-i386/Packages.diff/Index", "i386"},
		{"d-i-binary-amd64", "dists/noble/main/debian-installer/binary-amd64/Packages.gz", "amd64"},
		{"source-dsc", "pool/main/b/bash/bash_5.1-2.dsc", "source"},
		{"source-tarball-orig", "pool/main/b/bash/bash_5.1.orig.tar.xz", "source"},
		{"source-debian-tar", "pool/main/b/bash/bash_5.1-2.debian.tar.xz", "source"},
		{"source-diff-gz", "pool/main/b/bash/bash_5.1-2.diff.gz", "source"},
		{"source-Sources", "dists/noble/main/source/Sources.gz", "source"},
		{"source-pdiff", "dists/noble/main/source/Sources.diff/Index", "source"},
		{"non-arch-metadata", "dists/noble/Release", ""},
		{"contents-amd64-not-tagged", "dists/noble/main/Contents-amd64.gz", ""},
		{"empty", "", ""},
		{"random", "foo/bar/baz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := archFromPath(tc.path)
			if got != tc.want {
				t.Errorf("archFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
