package gpg

// Tests of the keyring loader's reading of a key file as apt reads it: every
// public key block of an armored file, wherever it stands, and nothing
// around them (the re-review's V-RC1, its second's V-R2C1), a binary file to
// its end, and a refusal where some apt cannot use the file.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// armoredKey is e's public key in one armor block, as gpg --export --armor
// writes it.
func armoredKey(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// binaryKey is e's public key in binary, as gpg --export writes it.
func binaryKey(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := e.Serialize(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// armorOf armors data as one block of type typ.
func armorOf(t *testing.T, typ string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, typ, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// fingerprintsOf are the primary fingerprints of ents.
func fingerprintsOf(ents openpgp.EntityList) map[string]bool {
	out := make(map[string]bool)
	for _, e := range ents {
		out[upperFP(e.PrimaryKey.Fingerprint)] = true
	}
	return out
}

// TestParseKeyring_ReadsEveryArmorBlock: every apt reads every public key
// block of an armored key file (apt-key's dearmor_keyring in apt 2.x,
// dearmorKeyOrCheckFormat in 3.x, sqv's own reader on debian:13), as
// yarn's, nginx's and sublime's published key files need (two and three
// blocks); the loader read the first only. Blank lines between and after
// the blocks, armor headers, CRLF line ends, no final newline and a
// checksum line apt does not check are all read. So are the shapes the
// re-review measured every apt reading (V-R2C1, its rr2 kr cases), which
// fix round 1 refused: a comment line before the block, text after it or
// between blocks, binary bytes after it, a signature block beside it,
// text past the first 64 octets before it; apt 3 skips every line
// outside a public key block and every block of another type, apt-key's
// awk prints only lines inside blocks, sqv reads them all.
func TestParseKeyring_ReadsEveryArmorBlock(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	c := newTestEntity(t, "C", "c@example.com")
	ka, kb, kc := armoredKey(t, a), armoredKey(t, b), armoredKey(t, c)
	withHeaders := bytes.Replace(kb, []byte("-----\n\n"), []byte("-----\nVersion: GnuPG v1\nComment: the second key\n\n"), 1)
	crc := bytes.LastIndex(kb, []byte("\n=")) + 2 // the checksum's four characters
	badCRC := joined(kb[:crc], []byte("AAAA"), kb[crc+4:])
	if bytes.Equal(badCRC, kb) {
		t.Fatal("test premise: the checksum was AAAA already")
	}
	for _, tc := range []struct {
		name string
		file []byte
		want []*openpgp.Entity
	}{
		{"one block", ka, []*openpgp.Entity{a}},
		{"two blocks", joined(ka, kb), []*openpgp.Entity{a, b}},
		{"three blocks, blank lines between", joined(ka, []byte("\n"), kb, []byte("\n \t\n"), kc), []*openpgp.Entity{a, b, c}},
		{"armor headers in the second block", joined(ka, withHeaders), []*openpgp.Entity{a, b}},
		{"CRLF line ends", bytes.ReplaceAll(joined(ka, kb), []byte("\n"), []byte("\r\n")), []*openpgp.Entity{a, b}},
		{"no final newline", bytes.TrimSuffix(joined(ka, kb), []byte("\n")), []*openpgp.Entity{a, b}},
		{"blank lines after the last block", joined(ka, kb, []byte("\n\n  \n")), []*openpgp.Entity{a, b}},
		{"a checksum line that does not match", joined(ka, badCRC), []*openpgp.Entity{a, b}},
		{"two keys in one block, then a third", joined(armorOf(t, openpgp.PublicKeyType, joined(binaryKey(t, a), binaryKey(t, b))), kc), []*openpgp.Entity{a, b, c}},
		// The re-review's measured shapes, each read by all six apt (V-R2C1;
		// its kr, kr6 and kr7 cases).
		{"kr_asc_leading_text: a text line and a blank line before the block", joined([]byte("Apt-shape A, the repository's key:\n\n"), ka), []*openpgp.Entity{a}},
		{"kr_asc_trailing_text: a blank line and a text line after the block", joined(ka, []byte("\nSee https://example.invalid/key for details.\n")), []*openpgp.Entity{a}},
		{"kr_asc_plus_sigblock: a signature block after the key block", joined(ka, detachSignWith(t, a, []byte(fakeReleasePlaintext), true)), []*openpgp.Entity{a}},
		{"kr6_text_between: a text line between two blocks", joined(ka, []byte("junk\n"), kb), []*openpgp.Entity{a, b}},
		{"kr6_long_leading_text: a 99-octet line before the block", joined([]byte("# "+strings.Repeat("x", 96)+"\n"), ka), []*openpgp.Entity{a}},
		{"kr7_armor_then_binary: the key's binary packets after the block", joined(ka, binaryKey(t, a)), []*openpgp.Entity{a}},
		// Their kin, as apt 3 reads them: no line outside a public key block
		// (another block's keys neither: apt 3.2 reads none), a block opened
		// by a line starting with its BEGIN line and closed by any line
		// starting "-----END".
		{"a comment line before the block, text lines after it", joined([]byte("# A's key, from the vendor's page\n"), ka, []byte("junk\n(checked)\n")), []*openpgp.Entity{a}},
		{"a signature block holding C's key after the key block", joined(ka, armorOf(t, "PGP SIGNATURE", binaryKey(t, c))), []*openpgp.Entity{a}},
		{"a signature block holding C's key before the key block", joined(armorOf(t, "PGP SIGNATURE", binaryKey(t, c)), ka), []*openpgp.Entity{a}},
		{"a private key block holding C's key, then B's block", joined(armorOf(t, "PGP PRIVATE KEY BLOCK", binaryKey(t, c)), kb), []*openpgp.Entity{b}},
		{"a signature block left open after the key block", joined(ka, []byte("-----BEGIN PGP SIGNATURE-----\n\nAAAA\n")), []*openpgp.Entity{a}},
		{"a BEGIN line with text after it", bytes.Replace(ka, []byte("BLOCK-----\n"), []byte("BLOCK----- (A)\n"), 1), []*openpgp.Entity{a}},
		{"a block closed by another type's END line", bytes.Replace(ka, []byte("-----END PGP PUBLIC KEY BLOCK-----"), []byte("-----END PGP SIGNATURE-----"), 1), []*openpgp.Entity{a}},
	} {
		ents, err := parseKeyringBytes(tc.file)
		if err != nil {
			t.Errorf("%s: %v; want every block read", tc.name, err)
			continue
		}
		got := fingerprintsOf(ents)
		for _, e := range tc.want {
			if !got[upperFP(e.PrimaryKey.Fingerprint)] {
				t.Errorf("%s: %s's key not read (got %d keys)", tc.name, firstUID(e), len(ents))
			}
		}
		if len(ents) != len(tc.want) {
			t.Errorf("%s: %d keys, want %d", tc.name, len(ents), len(tc.want))
		}
	}
	// A binary keyring of two keys, its first packet in each framing gpgv
	// reads, and an empty file (apt: no keys, no error), as before.
	bin := binaryKey(t, a)
	if bin[0] != 0xc6 || bin[1] < 192 || bin[1] >= 224 {
		t.Fatalf("test premise: go-crypto frames the key packet % x, not new-format with a two-octet length", bin[:2])
	}
	n := (int(bin[1])-192)<<8 + int(bin[2]) + 192
	body, rest := bin[3:3+n], bin[3+n:]
	for name, file := range map[string][]byte{
		"a binary keyring of two keys":            joined(bin, binaryKey(t, b)),
		"the key packet with a five-octet length": joined(binary.BigEndian.AppendUint32([]byte{0xc6, 0xff}, uint32(n)), body, rest),
		"the key packet old-format, four octets":  joined(binary.BigEndian.AppendUint32([]byte{0x9a}, uint32(n)), body, rest),
		"the key packet old-format, two octets":   joined(binary.BigEndian.AppendUint16([]byte{0x99}, uint16(n)), body, rest),
	} {
		if ents, err := parseKeyringBytes(file); err != nil || len(ents) == 0 || !fingerprintsOf(ents)[upperFP(a.PrimaryKey.Fingerprint)] {
			t.Errorf("%s: %d keys, %v; want A's key read", name, len(ents), err)
		}
	}
	if ents, err := parseKeyringBytes(nil); err != nil || len(ents) != 0 {
		t.Errorf("an empty file: %d keys, %v; want none and no error", len(ents), err)
	}
	// Through the directory loader, as production reads /etc/apt/keyrings.
	dir := makeTestDir(t)
	writeFile(t, filepath.Join(dir, "vendor.asc"), joined(ka, kb, kc))
	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil || k.Size() != 3 {
		t.Fatalf("LoadKeyring on a three-block .asc: %d keys, %v; want 3", k.Size(), err)
	}
}

// TestParseKeyring_RefusesWhatAptReadsOtherwise: a file some apt cannot use
// is refused, which LoadKeyring logs at WARN and skips (SPEC2 §7.6.1):
// binary packets with anything after them (every apt fails such a
// keyring: measured), a public key block without its END line (apt 3
// ignores the whole file), a line after a block's checksum line (apt 2
// and 3 both fold it into the base64, and every apt then fails: measured),
// a block that decodes to anything but whole packets, and a file holding
// no public key block (key packets only in a private key or signature
// block: apt 3.2's gpgv path reads no key from it, measured). A file
// larger than the cap is refused before it is read whole.
func TestParseKeyring_RefusesWhatAptReadsOtherwise(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	ka, bin := armoredKey(t, a), binaryKey(t, a)
	crcAt := bytes.LastIndex(ka, []byte("\n=")) + 1
	afterCRC := joined(ka[:crcAt+6], []byte("AAAA\n"), ka[crcAt+6:])
	if !bytes.HasPrefix(ka[crcAt:], []byte("=")) || !bytes.HasPrefix(ka[crcAt+6:], []byte("-----END")) {
		t.Fatalf("test premise: no checksum line before the END line in %q", ka[crcAt:])
	}
	unterminated := ka[:bytes.LastIndex(ka, []byte("-----END"))]
	for _, tc := range []struct {
		name, why string // why: a substring of the refusal
		file      []byte
	}{
		{"binary, then an armor block", "starts no OpenPGP packet", joined(bin, ka)},
		{"binary, then a newline (kr_gpg_trailing_lf)", "starts no OpenPGP packet", joined(bin, []byte("\n"))},
		{"binary, then a text line (kr_gpg_trailing_junk)", "starts no OpenPGP packet", joined(bin, []byte("junk\n"))},
		{"binary, a truncated packet", "running past the end", bin[:len(bin)-1]},
		{"binary, a partial length", "partial packet length", joined(bin, []byte{0xc6, 0xe0, 0x00})},
		{"binary, an indeterminate length", "indeterminate packet length", joined(bin, []byte{0x9b, 0x00})},
		{"binary, a truncated one-octet new-format header", "truncated packet header", joined(bin, []byte{0xc6})},
		{"binary, a truncated two-octet new-format length", "truncated packet header", joined(bin, []byte{0xc6, 0xc0})},
		{"binary, a truncated five-octet new-format length", "truncated packet header", joined(bin, []byte{0xc6, 0xff, 0, 0})},
		{"binary, a truncated old-format length", "truncated packet header", joined(bin, []byte{0x99, 0x00})},
		{"text alone", "no public key block", []byte("# no key here\n")},
		{"kr7_private_block: the key in a private key block alone", "no public key block", bytes.ReplaceAll(ka, []byte("PUBLIC KEY"), []byte("PRIVATE KEY"))},
		{"kr7_signature_block: the key in a signature block alone", "no public key block", armorOf(t, "PGP SIGNATURE", bin)},
		{"kr6_unterminated: a block without its END line", "without its END line", unterminated},
		{"a block, then one without its END line", "without its END line", joined(ka, unterminated)},
		{"a BEGIN line inside a block", "inside a public key block", joined(unterminated, ka)},
		{"kr6_after_crc: a line after the checksum line", "after the block's checksum line", afterCRC},
		{"another type's BEGIN line inside a block", "inside a public key block", joined(unterminated, []byte("-----BEGIN PGP SIGNATURE-----\n"), ka[bytes.LastIndex(ka, []byte("-----END")):])},
		{"a block of bytes that are no packets", "starts no OpenPGP packet", armorOf(t, openpgp.PublicKeyType, []byte("no packets here"))},
		{"a block of a key, then bytes that are no packets", "starts no OpenPGP packet", armorOf(t, openpgp.PublicKeyType, joined(bin, []byte("junk")))},
		{"a block with no empty line after BEGIN", "does not decode", bytes.Replace(ka, []byte("-----\n\n"), []byte("-----\n"), 1)},
	} {
		if ents, err := parseKeyringBytes(tc.file); err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: %d keys, %v; want the file refused, %q", tc.name, len(ents), err, tc.why)
		}
	}
	// The cap, at a small limit (TestParseKeyring_ReadsADebianKeyringSizedFile
	// reads a file the size of the largest real one at the real cap).
	if _, err := parseKeyringReaderMax(bytes.NewReader(make([]byte, 1025)), 1024); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file of 1025 octets at a cap of 1024: %v; want it refused as larger than the cap", err)
	}
	if _, err := parseKeyringReaderMax(bytes.NewReader(make([]byte, 1024)), 1024); err == nil || strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file of 1024 octets at a cap of 1024: %v; want it read (and refused as no keyring)", err)
	}
	// LoadKeyring skips the refused file and loads the rest.
	dir := makeTestDir(t)
	b := newTestEntity(t, "B", "b@example.com")
	writeFile(t, filepath.Join(dir, "mixed.gpg"), joined(bin, ka))
	writeFile(t, filepath.Join(dir, "good.asc"), armoredKey(t, b))
	k, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil || k.Size() != 1 || !k.HasFingerprint(upperFP(b.PrimaryKey.Fingerprint)) {
		t.Fatalf("LoadKeyring: %d keys, %v; want B's alone, the mixed file skipped", k.Size(), err)
	}
}

// rawPacketList splits binary OpenPGP data into its packets, byte for byte,
// and their tags.
func rawPacketList(t *testing.T, b []byte) (tags []byte, packets [][]byte) {
	t.Helper()
	for off := 0; off < len(b); {
		h := b[off]
		var tag byte
		var hl, n int
		switch {
		case h&0xc0 == 0xc0: // new format
			tag = h & 0x3f
			switch l0 := int(b[off+1]); {
			case l0 < 192:
				hl, n = 2, l0
			case l0 < 224:
				hl, n = 3, (l0-192)<<8+int(b[off+2])+192
			default:
				hl, n = 6, int(binary.BigEndian.Uint32(b[off+2:off+6]))
			}
		case h&0x80 != 0: // old format
			tag = (h >> 2) & 0x0f
			octets := []int{1, 2, 4}[h&3]
			for _, c := range b[off+1 : off+1+octets] {
				n = n<<8 | int(c)
			}
			hl = 1 + octets
		default:
			t.Fatalf("test premise: offset %d starts no packet", off)
		}
		tags = append(tags, tag)
		packets = append(packets, b[off:off+hl+n])
		off += hl + n
	}
	return tags, packets
}

// rawKeyBlocks splits a binary keyring into its keys' packets, each from a
// public key packet (tag 6) to the next, byte for byte (the re-review's
// mkasc.py).
func rawKeyBlocks(t *testing.T, keyring []byte) [][]byte {
	t.Helper()
	tags, packets := rawPacketList(t, keyring)
	var out [][]byte
	for i, pkt := range packets {
		if tags[i] == 6 {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], pkt...)
	}
	return out
}

// TestVerifier_AKeyInAnArmoredFilesLaterBlockIsKnown: the re-review's
// V-RC1, as it measured it. The corpus' keys A and E in one two-block
// keys.asc, either order (its fx-AE and fx-EA): every gpgv-based apt
// refuses i_expired_bad_plus_good (gpgv's BADSIG from E) whichever block
// E is in, and every apt accepts x7i_control_good and x7d_control_good
// whichever block A is in. The cache, loading the file from a keyring
// directory, read the first block alone: it adopted
// i_expired_bad_plus_good with E second, and refused the controls with A
// second.
func TestVerifier_AKeyInAnArmoredFilesLaterBlockIsKnown(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(aptShapeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	keys := rawKeyBlocks(t, read("keys.gpg"))
	if len(keys) != 4 {
		t.Fatalf("test premise: keys.gpg holds %d keys, want A, E, S and O", len(keys))
	}
	keyA, keyE := keys[0], keys[1]
	for _, k := range []struct {
		key []byte
		uid string
	}{{keyA, "Apt-shape A"}, {keyE, "Apt-shape E"}} {
		if !bytes.Contains(k.key, []byte(k.uid)) {
			t.Fatalf("test premise: keys.gpg's key holds no %q", k.uid)
		}
	}
	release := read("Release")
	for name, file := range map[string][]byte{
		"A then E": joined(armorOf(t, openpgp.PublicKeyType, keyA), armorOf(t, openpgp.PublicKeyType, keyE)),
		"E then A": joined(armorOf(t, openpgp.PublicKeyType, keyE), armorOf(t, openpgp.PublicKeyType, keyA)),
	} {
		dir := makeTestDir(t)
		writeFile(t, filepath.Join(dir, "keys.asc"), file)
		keyring, err := LoadKeyring([]string{dir}, silentLogger())
		if err != nil || keyring.Size() != 2 {
			t.Fatalf("%s: LoadKeyring: %d keys, %v; want A and E", name, keyring.Size(), err)
		}
		v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), read("i_expired_bad_plus_good.InRelease")); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: i_expired_bad_plus_good: %v; want ErrBadSignature (E's, as gpgv's BADSIG)", name, err)
		}
		if _, err := v.VerifyInline(context.Background(), newSuite(), read("x7i_control_good.InRelease")); err != nil {
			t.Errorf("%s: x7i_control_good: %v; want it adopted", name, err)
		}
		if _, err := v.VerifyDetached(context.Background(), newSuite(), release, read("x7d_control_good.Release.gpg")); err != nil {
			t.Errorf("%s: x7d_control_good: %v; want it adopted", name, err)
		}
	}
}

// TestVerifier_AKeyFromAFileWithTextAroundItsBlockIsKnown: the re-review's
// V-R2C1, its S1 hole. A key in an armored file with a comment line before
// its block and a note after it, as an operator pastes one: every apt
// reads the key (measured), and fix round 1's loader skipped the file, so
// S1 no longer knew the key, and a bad signature by it beside a good one —
// a key-free edit the five gpgv-based apt refuse — was adopted; and an
// anchor it signs alone was refused (untrusted_signer), an outage. Both
// read as apt reads them now.
func TestVerifier_AKeyFromAFileWithTextAroundItsBlockIsKnown(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	c := newTestEntity(t, "C", "c@example.com")
	dir := makeTestDir(t)
	writeFile(t, filepath.Join(dir, "a.asc"), armoredKey(t, a))
	writeFile(t, filepath.Join(dir, "c.asc"), joined([]byte("# C's key, pasted from the vendor's page\n"), armoredKey(t, c), []byte("(fingerprint checked)\n")))
	keyring, err := LoadKeyring([]string{dir}, silentLogger())
	if err != nil || keyring.Size() != 2 {
		t.Fatalf("LoadKeyring: %d keys, %v; want A and C", keyring.Size(), err)
	}
	v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	cGood := signaturePacketsOf(t, clearsignWith(t, c, []byte(fakeReleasePlaintext)))
	cBad := signaturePacketsOf(t, clearsignWith(t, c, []byte("Origin: Other\n")))
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, aGood, cBad)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("A's good signature and C's bad one: %v; want ErrBadSignature (gpgv's BADSIG from C)", err)
	}
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, cGood)); err != nil {
		t.Errorf("C's good signature alone: %v; want it adopted", err)
	}
}

// TestParseKeyring_ReadsADebianKeyringSizedFile: the re-review's V-R2M2. The
// loader's 16 MiB cap skipped debian-keyring.gpg (31.3 MB in
// debian-keyring 2025.07.26, in /usr/share/keyrings, a default directory)
// with a WARN at every start; apt has no limit. A generated keyring past
// that size, built as the real one is (a key whose user ID carries many
// certifications by other keys, which go-crypto keeps unverified), loads at
// the real cap, binary as the real one is and armored; and the cap stands
// far above it.
func TestParseKeyring_ReadsADebianKeyringSizedFile(t *testing.T) {
	const debianKeyring = 31_300_000 // octets, debian-keyring.gpg 2025.07.26
	if keyringMaxFile < 8*debianKeyring {
		t.Errorf("keyringMaxFile is %d octets, want at least eight times debian-keyring.gpg's %d", keyringMaxFile, debianKeyring)
	}
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	if err := a.SignIdentity(firstUID(a), b, &packet.Config{Time: func() time.Time { return time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC) }}); err != nil {
		t.Fatalf("SignIdentity: %v", err)
	}
	tags, packets := rawPacketList(t, binaryKey(t, a))
	// A's packets: its key, its user ID, the self-signature and B's
	// certification (identity.Signatures, in order), then its subkey.
	if len(tags) < 4 || tags[0] != 6 || tags[1] != 13 || tags[2] != 2 || tags[3] != 2 {
		t.Fatalf("test premise: A's packets have tags %v, want a key, a user ID and two signatures first", tags)
	}
	var big bytes.Buffer
	big.Write(joined(packets[0], packets[1], packets[2]))
	for big.Len() < 32<<20 {
		big.Write(packets[3])
	}
	for _, pkt := range packets[4:] {
		big.Write(pkt)
	}
	if big.Len() <= debianKeyring {
		t.Fatalf("test premise: %d octets, want more than debian-keyring.gpg's", big.Len())
	}
	dir := makeTestDir(t)
	writeFile(t, filepath.Join(dir, "debian-keyring.gpg"), big.Bytes())
	writeFile(t, filepath.Join(dir, "debian-keyring.asc"), armorOf(t, openpgp.PublicKeyType, big.Bytes()))
	for _, name := range []string{"debian-keyring.gpg", "debian-keyring.asc"} {
		ents, err := parseKeyringFile(filepath.Join(dir, name))
		if err != nil || len(ents) != 1 || !fingerprintsOf(ents)[upperFP(a.PrimaryKey.Fingerprint)] {
			t.Errorf("%s: %d keys, %v; want A's key read", name, len(ents), err)
		}
	}
}
