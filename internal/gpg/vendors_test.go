package gpg

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
)

// vendorDir holds real repositories' signed anchors, fetched for the
// hardening plan's vendor survey (2026-10-28: PE/survey.txt, 81 vendors,
// 158 anchors) and committed byte for byte, each beside the public key that
// signs it: one anchor per signer layout the survey found (the plan's Task
// 9b). They are the regression guard against refusing a legitimate anchor:
// layer A's and P's canonical grammar (ruling HD-16) must hold every one,
// and every one must verify. The keys are as the vendors publish them, but
// that Debian's keyring keeps only the three keys that sign trixie: yarn's,
// nginx's and sublime's armored files hold two, three and three blocks,
// every one read (the re-review's V-RC1: the keyring loader read the
// first block only, and the committed yarn and nginx files were cut to the
// block holding the signing key until it read them all).
const vendorDir = "testdata/vendors"

// vendorAnchors names each anchor, its layout, and what layers A and P read
// in it, so that a fixture swapped for another layout fails here.
var vendorAnchors = []struct {
	dir     string
	inline  bool
	layout  string
	pks     []byte // each packet's public-key algorithm
	fprs    []bool // each packet names its issuer's fingerprint (33)
	sigType byte   // every packet's
	headers int    // armor header lines
	crc     bool
}{
	{"debian-trixie", true, "Debian's three signatures: two RSA by the archive keys' subkeys, one EdDSA (the release key); h:{33,2}|u:{16}", []byte{1, 1, 22}, []bool{true, true, true}, 0x01, 0, true},
	{"elastic", true, "GnuPG h:{33,2}|u:{16}, SHA-512", []byte{1}, []bool{true}, 0x01, 0, true},
	{"ms-code", true, "BSN Pgp: a Version header, no CRC line, h:{2}|u:{16} (the short key ID fallback)", []byte{1}, []bool{false}, 0x01, 1, false},
	{"ms-code", false, "BSN Pgp's Release.gpg: a Version header, binary mode, h:{2}|u:{16}", []byte{1}, []bool{false}, 0x00, 1, true},
	{"salt", true, "BouncyCastle: Version: BCPG v1.86, h:{2!}|u:{16}, SHA-512", []byte{1}, []bool{false}, 0x01, 1, true},
	{"signal", true, "Sequoia h:{2!,16,20,33}, no CRC line", []byte{1}, []bool{true}, 0x01, 0, false},
	{"nginx", true, "nginx: CRLF line ends throughout, h:{2,16,33}", []byte{1}, []bool{true}, 0x01, 0, true},
	{"yarn", true, "one EdDSA signature by a subkey in its key file's second armor block, h:{33,2}|u:{16}", []byte{22}, []bool{true}, 0x01, 0, true},
	{"sublime", true, "two signatures, by keys in its key file's first and third armor blocks, h:{33,2}|u:{16}", []byte{1, 1}, []bool{true, true}, 0x01, 0, true},
	{"google-cloud-sdk", false, "a text-mode Release.gpg, h:{2,16!,33}", []byte{1}, []bool{true}, 0x01, 0, true},
	{"mongodb", true, "h:{33,2,28}|u:{16}: the signer's user ID", []byte{1}, []bool{true}, 0x01, 0, true},
	{"brave", false, "GnuPG's binary-mode Release.gpg, h:{33,2}|u:{16}", []byte{1}, []bool{true}, 0x00, 0, true},
	{"cloudflared", true, "two signatures, the first from a key the cache lacks (a rotation), SHA-512", []byte{1, 1}, []bool{true, true}, 0x01, 0, true},
}

// TestVerifier_AcceptsTheVendorAnchors: every committed vendor anchor
// verifies with its own key, under the defaults (no pin, allow_short_keyid),
// and reads as its layout says; every fixture directory is listed.
func TestVerifier_AcceptsTheVendorAnchors(t *testing.T) {
	read := func(dir, file string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(vendorDir, dir, file))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	listed := make(map[string]bool)
	for _, va := range vendorAnchors {
		listed[va.dir] = true
		name := va.dir + "/InRelease"
		if !va.inline {
			name = va.dir + "/Release.gpg"
		}
		v := vendorVerifier(t, va.dir)
		var (
			lines [][]byte
			first int
			got   []byte
			err   error
		)
		if va.inline {
			body := read(va.dir, "InRelease")
			cs, err := apttext.Inline(body)
			if err != nil {
				t.Errorf("%s: layer T: %v", name, err)
				continue
			}
			lines, first = cs.Armor, cs.ArmorLine
			if got, err = v.VerifyInline(context.Background(), newSuite(), body); err != nil || !bytes.Equal(got, cs.Message) {
				t.Errorf("%s (%s): VerifyInline: %v; want it verified, to M", name, va.layout, err)
			}
		} else {
			release, sig := read(va.dir, "Release"), read(va.dir, "Release.gpg")
			if lines, err = apttext.Detached(sig); err != nil {
				t.Errorf("%s: layer T: %v", name, err)
				continue
			}
			first = 1
			if got, err = v.VerifyDetached(context.Background(), newSuite(), release, sig); err != nil || !bytes.Equal(got, release) {
				t.Errorf("%s (%s): VerifyDetached: %v; want it verified, to its Release", name, va.layout, err)
			}
		}
		pkts, err := armorPackets(lines, first)
		if err != nil {
			t.Errorf("%s: layers A and P: %v", name, err)
			continue
		}
		var pks []byte
		var fprs []bool
		for _, p := range pkts {
			pks = append(pks, p.pkAlgo)
			fprs = append(fprs, p.fpr != nil)
			if p.sigType != va.sigType {
				t.Errorf("%s: packet %d of type 0x%02x, want 0x%02x", name, p.n, p.sigType, va.sigType)
			}
		}
		headers, crc := 0, false
		for _, l := range lines[1:] {
			if len(l) == 0 {
				break
			}
			headers++
		}
		for _, l := range lines {
			crc = crc || isCRCLine(l)
		}
		if !slices.Equal(pks, va.pks) || !slices.Equal(fprs, va.fprs) || headers != va.headers || crc != va.crc {
			t.Errorf("%s: algorithms %v, fingerprints %v, %d headers, CRC %v; its layout (%s) says %v, %v, %d, %v",
				name, pks, fprs, headers, crc, va.layout, va.pks, va.fprs, va.headers, va.crc)
		}
	}
	dirs, err := os.ReadDir(vendorDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !listed[d.Name()] {
			t.Errorf("%s/%s: a fixture vendorAnchors does not list", vendorDir, d.Name())
		}
	}
}

// vendorVerifier is the verifier of a vendor fixture directory: its key
// files (every file but InRelease, Release and Release.gpg) as the host
// keyring, under the defaults (no pin, allow_short_keyid).
func vendorVerifier(t *testing.T, dir string) *Verifier {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(vendorDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	var keys []EmbeddedSource
	for _, e := range entries {
		if f := e.Name(); f != "InRelease" && f != "Release" && f != "Release.gpg" {
			b, err := os.ReadFile(filepath.Join(vendorDir, dir, f))
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, EmbeddedSource{Name: f, Data: b})
		}
	}
	keyring, err := LoadKeyringWithEmbedded(nil, keys, silentLogger())
	if err != nil || keyring.Empty() {
		t.Fatalf("%s: the keyring (%d files): %v, empty %v", dir, len(keys), err, keyring.Empty())
	}
	v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestVerifier_RefusesSublimesSecondSignatureFlipped: the re-review's
// V-RC1 on a real vendor. Sublime signs its InRelease twice, by
// F57D4F59BD3DF454 (a subkey in its key file's first block) and by
// 7FE2FA12CF6E38F2 (the third block). With the second signature's last
// octet flipped (no key needed), every apt refuses it with sublime's file
// as signed-by (measured: gpgv's BADSIG 7FE2FA12CF6E38F2; debian:13's sqv
// finds no valid signature, the first key being bound by SHA-1 signatures
// alone, the re-review's V-RC2), and the cache, reading the first block
// alone, adopted it on the first signature: S1 knows the second key now.
func TestVerifier_RefusesSublimesSecondSignatureFlipped(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(vendorDir, "sublime", "InRelease"))
	if err != nil {
		t.Fatal(err)
	}
	v := vendorVerifier(t, "sublime")
	if _, err := v.VerifyInline(context.Background(), newSuite(), body); err != nil {
		t.Fatalf("test premise: sublime's InRelease: %v; want it verified", err)
	}
	_, pkts := inlineParts(t, body)
	if len(pkts) != 2 || pkts[1].issuer != 0x7FE2FA12CF6E38F2 {
		t.Fatalf("test premise: %d packets; want two, the second by 7FE2FA12CF6E38F2", len(pkts))
	}
	second := append([]byte{}, pkts[1].raw...)
	second[len(second)-1] ^= 0x01
	head := body[:bytes.Index(body, []byte(apttext.SigArmorBegin))]
	flipped := joined(head, armorSignature(t, joined(pkts[0].raw, second)))
	if _, err := v.VerifyInline(context.Background(), newSuite(), flipped); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("sublime's InRelease, its second signature flipped: %v; want ErrBadSignature (gpgv's BADSIG 7FE2FA12CF6E38F2)", err)
	}
}
