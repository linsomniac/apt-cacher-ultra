package gpg

// Tests of the key-file reader against the third re-review's measurements
// (V-R3C1, its section 3, final/V/rr3/meas): every key that any supported
// apt reads from a key file is read, by one reader whose keys S1 and S2
// both use (the controller's ruling for Task 9b's fix round 3).

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// vArmor is the re-review's armored() (rr3/mkkr8.py): data in a block of
// kind (PUBLIC KEY BLOCK unless given), base64 lines width wide (64 unless
// given), the header lines headers, a checksum line crc (the CRC-24's
// unless given), a line inserted before base64 line insertAt (when > 0),
// and no END line with noEnd.
type vArmor struct {
	kind, headers string
	width         int
	crc           *string
	insertAt      int
	insert        string
	noEnd         bool
}

func (o vArmor) of(data []byte) []byte {
	kind, width := o.kind, o.width
	if kind == "" {
		kind = "PUBLIC KEY BLOCK"
	}
	if width == 0 {
		width = 64
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	var lines []string
	for i := 0; i < len(b64); i += width {
		lines = append(lines, b64[i:min(i+width, len(b64))])
	}
	if o.insertAt > 0 {
		lines = append(lines[:o.insertAt], append([]string{o.insert}, lines[o.insertAt:]...)...)
	}
	c := crc24(data)
	crc := "=" + base64.StdEncoding.EncodeToString([]byte{byte(c >> 16), byte(c >> 8), byte(c)})
	if o.crc != nil {
		crc = *o.crc
	}
	text := fmt.Sprintf("-----BEGIN PGP %s-----\n%s\n%s\n%s\n", kind, o.headers, strings.Join(lines, "\n"), crc)
	if !o.noEnd {
		text += fmt.Sprintf("-----END PGP %s-----\n", kind)
	}
	return []byte(text)
}

// corpusKeys returns the apt-shape corpus' keys A, E and S (keys.gpg), each
// binary, as the re-review cut them.
func corpusKeys(t *testing.T) (a, e, s []byte) {
	t.Helper()
	keyring, err := os.ReadFile(filepath.Join(aptShapeDir, "keys.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	keys := rawKeyBlocks(t, keyring)
	if len(keys) != 4 || !bytes.Contains(keys[0], []byte("Apt-shape A")) || !bytes.Contains(keys[1], []byte("Apt-shape E")) || !bytes.Contains(keys[2], []byte("Apt-shape S")) {
		t.Fatalf("test premise: keys.gpg holds %d keys, want A, E, S and O", len(keys))
	}
	return keys[0], keys[1], keys[2]
}

// fingerprintOf is the primary fingerprint of the one key in binary.
func fingerprintOf(t *testing.T, binary []byte) string {
	t.Helper()
	ents, err := openpgp.ReadKeyRing(bytes.NewReader(binary))
	if err != nil || len(ents) != 1 {
		t.Fatalf("test premise: %d keys, %v", len(ents), err)
	}
	return upperFP(ents[0].PrimaryKey.Fingerprint)
}

// corpusShape reads one shape of the apt-shape corpus.
func corpusShape(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aptShapeDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParseKeyring_ReadsWhatAnyAptReads: the rows of the third re-review's
// section 3, the key files a supported apt reads that fix round 2's
// loader skipped (V-R3C1), and its rr2 rows that some apt read: each key
// any apt reads from the file is read. A's base64 on one line or in lines
// of 100 (all six apt read them, go-crypto's armor decoder refused a line
// over 96); a header line without a colon, a checksum line of 4 or 6
// characters, a "-x" body line (the five gpgv-based apt read them); A's
// block without its END line, alone or followed by E's (apt-key reads
// them, and apt 3.2 the second, both blocks' base64 joined); A's packets
// in a PRIVATE KEY BLOCK or a SIGNATURE block alone, and E's beside A's
// public block, either order (apt-key and sqv read them); S's key on one
// line, or without its END line.
func TestParseKeyring_ReadsWhatAnyAptReads(t *testing.T) {
	binA, binE, binS := corpusKeys(t)
	fpA, fpE, fpS := fingerprintOf(t, binA), fingerprintOf(t, binE), fingerprintOf(t, binS)
	crcA := string(vArmor{}.of(binA))
	crcA = crcA[strings.LastIndex(crcA, "\n=")+1 : strings.LastIndex(crcA, "\n-----END")]
	if len(crcA) != 5 {
		t.Fatalf("test premise: A's checksum line %q", crcA)
	}
	crc4, crc6 := crcA[:4], crcA+"A"
	if b64 := base64.StdEncoding.EncodeToString(binA); !strings.HasSuffix(b64, "=") {
		t.Fatalf("test premise: A's base64 ends in no padding, so the joined blocks below would test nothing")
	}
	armA := vArmor{}.of(binA)
	second := bytes.Index(armA, []byte("\n\n")) + 2
	second = second + bytes.IndexByte(armA[second:], '\n') + 1 // the second base64 line
	blankInside := joined(armA[:second+10], []byte(" "), armA[second+10:])
	for _, tc := range []struct {
		name string
		file []byte
		want []string
	}{
		{"kr8_control", vArmor{}.of(binA), []string{fpA}},
		{"kr8_long_line_all: A's base64 on one line", vArmor{width: 1 << 20}.of(binA), []string{fpA}},
		{"kr8_long_line_100: A's base64 in lines of 100", vArmor{width: 100}.of(binA), []string{fpA}},
		{"kr8_hdr_nocolon: a header line without a colon", vArmor{headers: "Comment hello\n"}.of(binA), []string{fpA}},
		{"kr8_crc_len4: a checksum line of four characters", vArmor{crc: &crc4}.of(binA), []string{fpA}},
		{"kr8_crc_len6: a checksum line of six characters", vArmor{crc: &crc6}.of(binA), []string{fpA}},
		{"kr8_body_dash: a body line \"-x\" among the base64", vArmor{insertAt: 2, insert: "-x"}.of(binA), []string{fpA}},
		{"kr8_unterminated_A_then_E: A's block without its END line, then E's", joined(vArmor{noEnd: true}.of(binA), vArmor{}.of(binE)), []string{fpA, fpE}},
		{"kr6_unterminated (rr2): A's block without its END line", vArmor{noEnd: true}.of(binA), []string{fpA}},
		{"kr7_private_block (rr2): A's packets in a PRIVATE KEY BLOCK alone", vArmor{kind: "PRIVATE KEY BLOCK"}.of(binA), []string{fpA}},
		{"kr7_signature_block (rr2): A's packets in a SIGNATURE block alone", vArmor{kind: "SIGNATURE"}.of(binA), []string{fpA}},
		{"kr8_s1_pub_then_sig_E: E's packets in a SIGNATURE block after A's", joined(vArmor{}.of(binA), vArmor{kind: "SIGNATURE"}.of(binE)), []string{fpA, fpE}},
		{"kr8_s1_pub_then_priv_E: E's packets in a PRIVATE KEY BLOCK after A's", joined(vArmor{}.of(binA), vArmor{kind: "PRIVATE KEY BLOCK"}.of(binE)), []string{fpA, fpE}},
		{"kr8_s1_sig_E_then_pub: E's packets in a SIGNATURE block before A's", joined(vArmor{kind: "SIGNATURE"}.of(binE), vArmor{}.of(binA)), []string{fpA, fpE}},
		{"kr8_s1_pub_then_pub_E: E's public block after A's", joined(vArmor{}.of(binA), vArmor{}.of(binE)), []string{fpA, fpE}},
		{"kr9_s1_longline_S: S's key on one line", vArmor{width: 1 << 20}.of(binS), []string{fpS}},
		{"kr11_s1_unterm_S: S's key without its END line", vArmor{noEnd: true}.of(binS), []string{fpS}},
		// apt 3's decoder passes over a character outside the alphabet (apt-key's
		// base64 -d refuses it): a blank inside a base64 line.
		{"a base64 line with a blank inside", blankInside, []string{fpA}},
	} {
		ents, err := parseKeyringBytes(tc.file)
		if err != nil {
			t.Errorf("%s: %v; want %d keys read", tc.name, err, len(tc.want))
			continue
		}
		got := fingerprintsOf(ents)
		for _, fp := range tc.want {
			if !got[fp] {
				t.Errorf("%s: key %s not read (got %d keys)", tc.name, fp, len(ents))
			}
		}
		if len(ents) != len(tc.want) {
			t.Errorf("%s: %d keys, want %d", tc.name, len(ents), len(tc.want))
		}
	}
}

// secretArmored is e's secret-key export in a PRIVATE KEY BLOCK, as gpg
// --armor --export-secret-keys writes it.
func secretArmored(t *testing.T, e *openpgp.Entity) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}

// TestParseKeyring_ReadsASecretKeyExportsPublicPart: the re-review's
// kr9_s1_pubG_then_secW, a secret-key export appended to a key file (gpg
// --export-secret-keys), from which apt-key's gpgv learns the key: G's
// public block, then W's PRIVATE KEY BLOCK of secret packets. Both keys
// are read, W's from its secret packets' public part, and the cache keeps
// no secret: W's primary key and subkey are public keys, with W's
// fingerprints. So in a binary secret keyring.
func TestParseKeyring_ReadsASecretKeyExportsPublicPart(t *testing.T) {
	g := newTestEntity(t, "G", "g@example.com")
	w := newTestEntity(t, "W", "w@example.com")
	var binSecret bytes.Buffer
	if err := w.SerializePrivate(&binSecret, nil); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string][]byte{
		"G's public block, then W's secret export": joined(armoredKey(t, g), secretArmored(t, w)),
		"W's secret export, then G's public block": joined(secretArmored(t, w), armoredKey(t, g)),
		"W's binary secret keyring":                binSecret.Bytes(),
	} {
		ents, err := parseKeyringBytes(file)
		if err != nil {
			t.Errorf("%s: %v; want W read", name, err)
			continue
		}
		var gotW *openpgp.Entity
		for _, e := range ents {
			if bytes.Equal(e.PrimaryKey.Fingerprint, w.PrimaryKey.Fingerprint) {
				gotW = e
			}
		}
		switch {
		case gotW == nil:
			t.Errorf("%s: W's key not read (%d keys)", name, len(ents))
		case gotW.PrivateKey != nil || len(gotW.Subkeys) != 1 || gotW.Subkeys[0].PrivateKey != nil:
			t.Errorf("%s: W read with a private key (%v) or %d subkeys", name, gotW.PrivateKey != nil, len(gotW.Subkeys))
		case !bytes.Equal(gotW.Subkeys[0].PublicKey.Fingerprint, w.Subkeys[0].PublicKey.Fingerprint):
			t.Errorf("%s: W's subkey read as %X, want %X", name, gotW.Subkeys[0].PublicKey.Fingerprint, w.Subkeys[0].PublicKey.Fingerprint)
		}
	}
}

// keyShape summarises what of a key S1 and S2 rely on: its subkeys and
// their bindings, its identities and their self-signatures, its
// revocations.
func keyShape(e *openpgp.Entity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%X rev=%d", e.PrimaryKey.Fingerprint, len(e.Revocations))
	for name, id := range e.Identities {
		fmt.Fprintf(&b, " uid=%q self=%v certs=%d rev=%d", name, id.SelfSignature.CreationTime.Unix(), len(id.Signatures), len(id.Revocations))
	}
	for _, s := range e.Subkeys {
		fmt.Fprintf(&b, " sub=%X bind=%v rev=%d", s.PublicKey.Fingerprint, s.Sig.CreationTime.Unix(), len(s.Revocations))
	}
	return b.String()
}

// TestParseKeyring_PacketsBesideAKeyDoNotDropIt: packets that are no part of
// a key, appended to its key file in a block of their own (as the
// re-review's kr_asc_plus_sigblock appends a detached signature: every apt
// reads the key) or after its binary packets, neither drop nor alter the
// key: go-crypto alone drops a whole key for a document signature after a
// user ID or a subkey, for the key's certification of another user ID, for
// another key's binding after a subkey, for a subkey without a binding, or
// for a packet it does not parse there. Each on A with its subkey (the
// packet after the subkey's binding) and on A without one (after its user
// ID's self-signature); and before the key.
func TestParseKeyring_PacketsBesideAKeyDoNotDropIt(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	o := newTestEntity(t, "O", "o@example.com")
	if err := o.SignIdentity(firstUID(o), a, &packet.Config{}); err != nil {
		t.Fatalf("SignIdentity: %v", err)
	}
	oTags, oPkts := rawPacketList(t, binaryKey(t, o))
	bTags, bPkts := rawPacketList(t, binaryKey(t, b))
	aTags, aPkts := rawPacketList(t, binaryKey(t, a))
	if len(oTags) < 4 || oTags[3] != 2 || len(bTags) != 5 || bTags[3] != 14 || len(aTags) != 5 || aTags[3] != 14 {
		t.Fatalf("test premise: packets %v, %v, %v", oTags, bTags, aTags)
	}
	withSub, noSub := binaryKey(t, a), joined(aPkts[0], aPkts[1], aPkts[2])
	frame := func(tag byte, body []byte) []byte { // a new-format packet, a one-octet length
		if len(body) >= 192 {
			t.Fatalf("test premise: a %d-octet body", len(body))
		}
		return joined([]byte{0xc0 | tag, byte(len(body))}, body)
	}
	badCert := frame(2, []byte{4, 0x13, 1, 8, 0, 0, 0, 0, 0xff}) // a certification go-crypto cannot parse
	junk := map[string][]byte{
		"A's detached signature of a document (kr_asc_plus_sigblock)": detachSignWith(t, a, []byte(fakeReleasePlaintext), false),
		"another key's detached signature of a document":              detachSignWith(t, b, []byte(fakeReleasePlaintext), false),
		"A's certification of another key's user ID":                  oPkts[3],
		"another key's subkey binding":                                bPkts[4],
		"another key's subkey, without its binding":                   bPkts[3],
		"a certification go-crypto cannot parse":                      badCert,
		"a literal data packet":                                       frame(11, []byte("b\x00\x00\x00\x00\x00hello")),
		"a marker packet":                                             frame(10, []byte("PGP")),
		"a trust packet":                                              frame(12, []byte{0, 0}),
		"a user attribute and another key's certification of it":      joined(frame(17, []byte{5, 101, 1, 2, 3, 4}), oPkts[2]),
		"a packet of an unknown critical tag":                         frame(15, []byte("x")),
	}
	for keyName, key := range map[string][]byte{"A with its subkey": withSub, "A without a subkey": noSub} {
		clean, err := parseKeyringBytes(key)
		if err != nil || len(clean) != 1 {
			t.Fatalf("test premise: %s alone: %d keys, %v", keyName, len(clean), err)
		}
		want := keyShape(clean[0])
		for junkName, j := range junk {
			for layout, file := range map[string][]byte{
				"in a signature block after the key's block":  joined(armorOf(t, openpgp.PublicKeyType, key), armorOf(t, "PGP SIGNATURE", j)),
				"in a signature block before the key's block": joined(armorOf(t, "PGP SIGNATURE", j), armorOf(t, openpgp.PublicKeyType, key)),
				"after the key's binary packets":              joined(key, j),
			} {
				ents, err := parseKeyringBytes(file)
				switch {
				case err != nil:
					t.Errorf("%s, %s %s: %v; want A read", keyName, junkName, layout, err)
				case len(ents) != 1:
					t.Errorf("%s, %s %s: %d keys, want A alone", keyName, junkName, layout, len(ents))
				case keyShape(ents[0]) != want:
					t.Errorf("%s, %s %s: A read as\n  %s\nwant\n  %s", keyName, junkName, layout, keyShape(ents[0]), want)
				}
			}
		}
	}
}

// TestParseKeyring_ReadsAsApt3WhereTheAwkFails: where apt-key's awk, which
// reads every block, makes a keyring of no whole packets of a block apt 3
// skips (a signature block whose base64 is no packets, one with a line after
// its checksum line, a clearsigned message's text), the file's public key
// blocks are read as apt 3 reads them: apt 3 reads A from each.
func TestParseKeyring_ReadsAsApt3WhereTheAwkFails(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	ka := armoredKey(t, a)
	sig := detachSignWith(t, a, []byte(fakeReleasePlaintext), true)
	crcAt := bytes.LastIndex(sig, []byte("\n=")) + 1
	sigAfterCRC := joined(sig[:crcAt+6], []byte("AAAA\n"), sig[crcAt+6:])
	if !bytes.HasPrefix(sig[crcAt+6:], []byte("-----END")) {
		t.Fatalf("test premise: no checksum line before the END line in %q", sig[crcAt:])
	}
	clearsigned := clearsignWith(t, a, []byte(fakeReleasePlaintext))
	for name, file := range map[string][]byte{
		"a signature block whose base64 is no packets, after the key": joined(ka, armorOf(t, "PGP SIGNATURE", []byte("no packets here"))),
		"the same, before the key":                                    joined(armorOf(t, "PGP SIGNATURE", []byte("no packets here")), ka),
		"a signature block with a line after its checksum line":       joined(ka, sigAfterCRC),
		"a clearsigned message before the key":                        joined(clearsigned, []byte("\n"), ka),
		"a clearsigned message after the key":                         joined(ka, clearsigned),
	} {
		ents, err := parseKeyringBytes(file)
		if err != nil || len(ents) != 1 || !fingerprintsOf(ents)[upperFP(a.PrimaryKey.Fingerprint)] {
			t.Errorf("%s: %d keys, %v; want A read, as apt 3 reads it", name, len(ents), err)
		}
	}
}

// TestVerifier_KeysAnyAptReadsAreKnownToS1: the third re-review's S1 rows
// and their outage twins (V-R3C1, section 3). Each key file below holds a
// key some gpgv-based apt reads and fix round 2's loader skipped: a bad
// signature by it beside a good one, a key-free edit, is refused
// (bad_signature) where those apt refuse it; and a good signature by a
// key read only from such a file is adopted, as apt-key's clients adopt
// it.
func TestVerifier_KeysAnyAptReadsAreKnownToS1(t *testing.T) {
	binA, binE, binS := corpusKeys(t)
	verifier := func(t *testing.T, files map[string][]byte) *Verifier {
		t.Helper()
		dir := makeTestDir(t)
		for name, b := range files {
			writeFile(t, filepath.Join(dir, name), b)
		}
		keyring, err := LoadKeyring([]string{dir}, silentLogger())
		if err != nil {
			t.Fatal(err)
		}
		v, err := NewVerifier(VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	inline := func(v *Verifier, shape []byte) error {
		_, err := v.VerifyInline(context.Background(), newSuite(), shape)
		return err
	}
	// i_expired_bad_plus_good: E's bad packet, then A's good one; the gpgv
	// rows refuse it with E read from a block of another type.
	for name, file := range map[string][]byte{
		"kr8_s1_pub_then_sig_E":  joined(vArmor{}.of(binA), vArmor{kind: "SIGNATURE"}.of(binE)),
		"kr8_s1_pub_then_priv_E": joined(vArmor{}.of(binA), vArmor{kind: "PRIVATE KEY BLOCK"}.of(binE)),
		"kr8_s1_sig_E_then_pub":  joined(vArmor{kind: "SIGNATURE"}.of(binE), vArmor{}.of(binA)),
		"kr8_s1_pub_then_pub_E":  joined(vArmor{}.of(binA), vArmor{}.of(binE)),
	} {
		v := verifier(t, map[string][]byte{"keys.asc": file})
		if err := inline(v, corpusShape(t, "i_expired_bad_plus_good.InRelease")); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: i_expired_bad_plus_good: %v; want ErrBadSignature (E's, as gpgv's BADSIG)", name, err)
		}
		if err := inline(v, corpusShape(t, "x7i_control_good.InRelease")); err != nil {
			t.Errorf("%s: x7i_control_good: %v; want it adopted", name, err)
		}
	}
	// x3i_subkey_bad_plus_good: S's subkey's bad packet, then A's good one,
	// A in trusted.gpg.d beside S's key on one line or without its END line;
	// x3i_subkey_good, S's subkey's good packet alone, adopted.
	for name, s := range map[string][]byte{
		"kr9_s1_longline_S":                    vArmor{width: 1 << 20}.of(binS),
		"kr11_s1_unterm_S, kr11_unterm_S_good": vArmor{noEnd: true}.of(binS),
	} {
		v := verifier(t, map[string][]byte{"00-a.gpg": binA, "20-s.asc": s})
		if err := inline(v, corpusShape(t, "x3i_subkey_bad_plus_good.InRelease")); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: x3i_subkey_bad_plus_good: %v; want ErrBadSignature (S's subkey's, as gpgv's BADSIG)", name, err)
		}
		if err := inline(v, corpusShape(t, "x3i_subkey_good.InRelease")); err != nil {
			t.Errorf("%s: x3i_subkey_good: %v; want it adopted", name, err)
		}
	}
	// kr9: G's public block, W's secret-key export after it; W's packet over
	// another text, then G's good one: refused as the gpgv rows refuse it;
	// G's good packet alone (kr9b) adopted; with G's block alone (kr9's
	// control) W is no key, and the pair is adopted.
	g := newTestEntity(t, "G", "g@example.com")
	w := newTestEntity(t, "W", "w@example.com")
	gGood := signaturePacketsOf(t, clearsignWith(t, g, []byte(fakeReleasePlaintext)))
	wBad := signaturePacketsOf(t, clearsignWith(t, w, []byte("other text\n")))
	withSecW := verifier(t, map[string][]byte{"keys.asc": joined(armoredKey(t, g), secretArmored(t, w))})
	if err := inline(withSecW, inlineWithSignatures(t, wBad, gGood)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("kr9_s1_pubG_then_secW: %v; want ErrBadSignature (W's, as gpgv's BADSIG)", err)
	}
	if err := inline(withSecW, inlineWithSignatures(t, gGood)); err != nil {
		t.Errorf("kr9b_pubG_then_secW_goodonly: %v; want it adopted", err)
	}
	if err := inline(verifier(t, map[string][]byte{"keys.asc": armoredKey(t, g)}), inlineWithSignatures(t, wBad, gGood)); err != nil {
		t.Errorf("kr9_s1_pubG_only: %v; want it adopted (W is no key)", err)
	}
	// The outage rows: A read only from each file the re-review measured,
	// x7i_control_good adopted.
	crcA := string(vArmor{}.of(binA))
	crcA = crcA[strings.LastIndex(crcA, "\n=")+1 : strings.LastIndex(crcA, "\n-----END")]
	crc4, crc6 := crcA[:4], crcA+"A"
	for name, file := range map[string][]byte{
		"kr8_long_line_all":         vArmor{width: 1 << 20}.of(binA),
		"kr8_long_line_100":         vArmor{width: 100}.of(binA),
		"kr8_hdr_nocolon":           vArmor{headers: "Comment hello\n"}.of(binA),
		"kr8_crc_len4":              vArmor{crc: &crc4}.of(binA),
		"kr8_crc_len6":              vArmor{crc: &crc6}.of(binA),
		"kr8_body_dash":             vArmor{insertAt: 2, insert: "-x"}.of(binA),
		"kr8_unterminated_A_then_E": joined(vArmor{noEnd: true}.of(binA), vArmor{}.of(binE)),
		"kr6_unterminated":          vArmor{noEnd: true}.of(binA),
		"kr7_private_block":         vArmor{kind: "PRIVATE KEY BLOCK"}.of(binA),
		"kr7_signature_block":       vArmor{kind: "SIGNATURE"}.of(binA),
	} {
		if err := inline(verifier(t, map[string][]byte{"keys.asc": file}), corpusShape(t, "x7i_control_good.InRelease")); err != nil {
			t.Errorf("%s: x7i_control_good: %v; want it adopted", name, err)
		}
	}
}
