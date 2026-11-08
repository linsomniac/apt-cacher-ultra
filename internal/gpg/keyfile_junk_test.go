package gpg

// Tests of the key-file reader against junk in a file of more than one key
// (the fourth re-review's V-R4C1, its kr12 and kr13 rows, measured in the
// six images: final/V/rr4/meas). gpgv reads a keyring keyblock by keyblock,
// each ended by the next key's primary key packet, and loses only the
// keyblock whose packets run into bytes that are no whole packet, with
// whatever follows; fix round 3 refused the whole file, an outage for the
// keys before the junk and an S1 hole (a key-free bad signature by one of
// them beside a good one was adopted where the five gpgv-based apt refuse
// it, BADSIG).

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// withLineAfterCRC is data's armor block (of kind, PUBLIC KEY BLOCK unless
// given) with a line after its checksum line (V's kr6_after_crc, kr12 "crc"
// rows): both of apt's text readers fold it into the base64, where it
// decodes to octets that are no packet.
func withLineAfterCRC(t *testing.T, data []byte, kind ...string) []byte {
	t.Helper()
	arm := vArmor{kind: strings.Join(kind, "")}.of(data)
	end := bytes.LastIndex(arm, []byte("-----END"))
	if end < 0 || arm[end-6] != '=' {
		t.Fatalf("test premise: no checksum line before the END line in %q", arm[max(0, end-8):])
	}
	return joined(arm[:end], []byte("AAAA\n"), arm[end:])
}

// kr12Message is the MESSAGE block of text V appended (kr12_fb rows): apt
// 3 skips it, apt-key's base64 -d fails on it (apt-key then reads no key
// from the file), sqv fails the file.
const kr12Message = "-----BEGIN PGP MESSAGE-----\n\nThis is not base64!\n-----END PGP MESSAGE-----\n"

// blankInside is data's armor block with a blank inside its second base64
// line (kr15_asc_A_blank): GNU base64 -d refuses it, apt 3's decoder passes
// over it.
func blankInside(t *testing.T, data []byte) []byte {
	t.Helper()
	arm := vArmor{}.of(data)
	second := bytes.Index(arm, []byte("\n\n")) + 2
	second = second + bytes.IndexByte(arm[second:], '\n') + 1
	return joined(arm[:second+10], []byte(" "), arm[second+10:])
}

// TestParseKeyring_ReadsTheKeysBeforeJunk: each key whose packets end before
// the junk is read, the key the junk interrupts and what follows are not,
// and a file is refused only when that leaves no key (V's kr12 and kr13
// rows; a truncated download of a keyring of two keys, binary or armored,
// reads the first). In an armored file the awk reading (every block) and apt
// 3's (public key blocks only) are both taken where the awk's loses a key: a
// key apt-key's gpgv reads from a SIGNATURE block before the junk, and the
// keys apt 3 reads after a block of junk it skips.
func TestParseKeyring_ReadsTheKeysBeforeJunk(t *testing.T) {
	binA, binE, binS := corpusKeys(t)
	fpA, fpE, fpS := fingerprintOf(t, binA), fingerprintOf(t, binE), fingerprintOf(t, binS)
	arm := func(data []byte) []byte { return vArmor{}.of(data) }
	sig := func(data []byte) []byte { return vArmor{kind: "SIGNATURE"}.of(data) }
	armAE := joined(arm(binA), arm(binE))
	// E's block cut inside its fourth base64 line, 40 characters in, as
	// kr15_asc_A_Ecut_q0 cuts it (inside E's primary key packet; the joined
	// base64, A's whole and E's, a multiple of four characters; q2 is two
	// characters further), and inside its second-to-last line (E's last
	// packets), 40 and 42 characters in.
	eBody := bytes.Index(armAE, []byte("-----END")) + 1
	eBody = bytes.Index(armAE[eBody:], []byte("\n\n")) + eBody + 2
	cutPrimary := eBody + 3*65 + 40
	eCRC := bytes.LastIndex(armAE, []byte("\n="))
	cutLate := bytes.LastIndexByte(armAE[:eCRC], '\n') + 1 + 40 // in the line before the last
	if a64 := len(base64.StdEncoding.EncodeToString(binA)); a64%4 != 0 || cutLate-eBody < 6*65 || armAE[cutLate+2] == '\n' {
		t.Fatalf("test premise: the cuts at %d and %d of %d are not inside E's primary and late packets", cutPrimary, cutLate, len(armAE))
	}
	for _, tc := range []struct {
		name string
		file []byte
		want []string // nil: refused
	}{
		{"kr12_bin_AE_junk: A, E, then junk", joined(binA, binE, []byte("junk\n")), []string{fpA}},
		{"kr12_bin_EA_junk: E, A, then junk (A lost: NO_PUBKEY A in all six)", joined(binE, binA, []byte("junk\n")), []string{fpE}},
		{"kr12_bin_AES_junk_s1: A, E, S, then junk", joined(binA, binE, binS, []byte("junk\n")), []string{fpA, fpE}},
		{"kr12_asc_A_Ecrc: A's block, then E's with a line after its checksum", joined(arm(binA), withLineAfterCRC(t, binE)), []string{fpA}},
		{"kr12_asc_Ecrc_A: E's block with a line after its checksum, then A's", joined(withLineAfterCRC(t, binE), arm(binA)), nil},
		{"kr12_asc_AE_onecrc: one block holding A and E, a line after its checksum", withLineAfterCRC(t, joined(binA, binE)), []string{fpA}},
		{"kr13_s1_td_bin_ES_junk: E, S, then junk", joined(binE, binS, []byte("junk\n")), []string{fpE}},
		{"kr13_s1_td_asc_E_Scrc: E's block, then S's with a line after its checksum", joined(arm(binE), withLineAfterCRC(t, binS)), []string{fpE}},
		{"kr12_fb_A_sigE_msg: A's block, E's in a SIGNATURE block, a MESSAGE block of text", joined(arm(binA), sig(binE), []byte(kr12Message)), []string{fpA}},
		{"kr12_fb_A_sigE_sigS_msg: the same with S's SIGNATURE block too", joined(arm(binA), sig(binE), sig(binS), []byte(kr12Message)), []string{fpA}},
		{"A's block, E's in a SIGNATURE block, then S's with a line after its checksum: apt-key's gpgv reads E", joined(arm(binA), sig(binE), withLineAfterCRC(t, binS)), []string{fpA, fpE}},
		{"kr15_asc_A_sigEcrc_S: A's block, E's SIGNATURE block with a line after its checksum, S's block: apt-key's gpgv reads A, apt 3 A and S", joined(arm(binA), withLineAfterCRC(t, binE, "SIGNATURE"), arm(binS)), []string{fpA, fpS}},
		{"kr15_asc_AE_sigjunk_S: A's block, E's block, a SIGNATURE block of no packets, then S's block: apt-key none (base64 -d), apt 3 all three", joined(arm(binA), arm(binE), armorOf(t, "PGP SIGNATURE", []byte("no packets here")), arm(binS)), []string{fpA, fpE, fpS}},
		{"kr15_asc_A_blank: a blank inside a base64 line of A's block: apt-key none, apt 3 A", blankInside(t, binA), []string{fpA}},
		{"kr15_bin_A_Ecut_sig: a truncated download, binary: A, then E cut inside its last packet", joined(binA, binE[:len(binE)-20]), []string{fpA}},
		{"kr15_bin_A_Ecut_prim: A, then E cut inside its primary key packet: A's keyblock runs into it", joined(binA, binE[:40]), nil},
		{"kr15_asc_A_Ecut_q0: a truncated download, armored: A's block, then E's cut inside its primary key packet's base64 (A's keyblock runs into it: NO_PUBKEY A in apt-key's gpgv)", armAE[:cutPrimary], nil},
		{"kr15_asc_A_Ecut_q2: the same, cut two characters further (a partial quantum: apt-key aborts)", armAE[:cutPrimary+2], nil},
		{"A's block, then E's cut inside its last packets' base64, whole quanta: apt-key's gpgv reads A (the binary row's twin)", armAE[:cutLate], []string{fpA}},
		{"the same, cut two characters further: a partial quantum, apt-key aborts, apt 3 ignores the file (no END line)", armAE[:cutLate+2], nil},
		{"a truncated download of one key, binary", binA[:len(binA)-20], nil},
	} {
		ents, err := parseKeys(tc.file)
		switch {
		case tc.want == nil && err == nil:
			t.Errorf("%s: %d keys read; want the file refused", tc.name, len(ents))
		case tc.want == nil:
		case err != nil:
			t.Errorf("%s: %v; want %d keys read", tc.name, err, len(tc.want))
		default:
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
}

// TestVerifier_KeysBeforeJunkAreKnownToS1: V's kr12 and kr13 S1 rows, and
// their outage twins. A key read before the junk is known to S1: E's bad
// signature beside A's good one (i_expired_bad_plus_good) is refused as
// every gpgv-based apt refuses it, from a signed-by file (A, E, S, junk)
// and from trusted.gpg.d (A's file beside E and S then junk, binary or
// armored; and beside E in a SIGNATURE block before S's broken block). A's
// good signature alone is adopted from every file the five gpgv-based apt
// read A from, refused from the one none reads it from (E, A, junk: the
// file holds no key the cache reads). The kr12_fb files, from which apt 3.2
// reads A alone and apt-key no key: the cache reads A, as apt 3.2 does, so
// E's bad signature beside A's good one is adopted as apt 3.2 adopts it
// (as at 38c89be; apt-key's clients fail every anchor with that file).
func TestVerifier_KeysBeforeJunkAreKnownToS1(t *testing.T) {
	binA, binE, binS := corpusKeys(t)
	arm := func(data []byte) []byte { return vArmor{}.of(data) }
	sig := func(data []byte) []byte { return vArmor{kind: "SIGNATURE"}.of(data) }
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
	inline := func(v *Verifier, shape string) error {
		_, err := v.VerifyInline(context.Background(), newSuite(), corpusShape(t, shape))
		return err
	}
	for name, files := range map[string]map[string][]byte{
		"kr12_bin_AES_junk_s1":   {"keys.gpg": joined(binA, binE, binS, []byte("junk\n"))},
		"kr13_s1_td_bin_ES_junk": {"00-a.gpg": binA, "20-es.gpg": joined(binE, binS, []byte("junk\n"))},
		"kr13_s1_td_asc_E_Scrc":  {"00-a.gpg": binA, "20-es.asc": joined(arm(binE), withLineAfterCRC(t, binS))},
		"A's file beside E's SIGNATURE block before S's broken block": {"00-a.gpg": binA, "20-es.asc": joined(sig(binE), withLineAfterCRC(t, binS))},
	} {
		v := verifier(t, files)
		if err := inline(v, "i_expired_bad_plus_good.InRelease"); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: i_expired_bad_plus_good: %v; want ErrBadSignature (E's, as gpgv's BADSIG)", name, err)
		}
		if err := inline(v, "x7i_control_good.InRelease"); err != nil {
			t.Errorf("%s: x7i_control_good: %v; want it adopted", name, err)
		}
	}
	for name, file := range map[string][]byte{
		"kr12_bin_AE_junk":   joined(binA, binE, []byte("junk\n")),
		"kr12_asc_A_Ecrc":    joined(arm(binA), withLineAfterCRC(t, binE)),
		"kr12_asc_AE_onecrc": withLineAfterCRC(t, joined(binA, binE)),
		"kr12_fb_A_sigE_msg": joined(arm(binA), sig(binE), []byte(kr12Message)),
	} {
		if err := inline(verifier(t, map[string][]byte{"keys.asc": file}), "x7i_control_good.InRelease"); err != nil {
			t.Errorf("%s: x7i_control_good: %v; want it adopted", name, err)
		}
	}
	if err := inline(verifier(t, map[string][]byte{"keys.gpg": joined(binE, binA, []byte("junk\n"))}), "x7i_control_good.InRelease"); err == nil {
		t.Errorf("kr12_bin_EA_junk: x7i_control_good adopted; want it refused (no apt reads A from the file)")
	}
	for name, file := range map[string][]byte{
		"kr12_fb_A_sigE_msg_s1":      joined(arm(binA), sig(binE), []byte(kr12Message)),
		"kr12_fb_A_sigE_sigS_msg_s1": joined(arm(binA), sig(binE), sig(binS), []byte(kr12Message)),
	} {
		if err := inline(verifier(t, map[string][]byte{"keys.asc": file}), "i_expired_bad_plus_good.InRelease"); err != nil {
			t.Errorf("%s: i_expired_bad_plus_good: %v; want it adopted, as apt 3.2 adopts it (E is no key it reads)", name, err)
		}
	}
}

// TestLoadKeyring_WarnsOfAKeyLostToJunk: a key file whose packets run into
// junk loads the keys before it and logs the loss at WARN
// (keyring_file_truncated, docs/log-fields.md), naming the file and the
// junk, so the operator learns a key they installed is not read; a whole
// file logs nothing.
func TestLoadKeyring_WarnsOfAKeyLostToJunk(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	for _, tc := range []struct {
		name string
		file []byte
		warn bool
	}{
		{"A, then B cut short inside its last packet", joined(binaryKey(t, a), binaryKey(t, b)[:len(binaryKey(t, b))-20]), true},
		{"A's block, then B's with a line after its checksum", joined(armoredKey(t, a), withLineAfterCRC(t, binaryKey(t, b))), true},
		{"A and B whole", joined(binaryKey(t, a), binaryKey(t, b)), false},
	} {
		dir := makeTestDir(t)
		writeFile(t, filepath.Join(dir, "keys.gpg"), tc.file)
		var logs bytes.Buffer
		keyring, err := LoadKeyring([]string{dir}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil || !keyring.HasFingerprint(upperFP(a.PrimaryKey.Fingerprint)) {
			t.Fatalf("%s: LoadKeyring: %d keys, %v; want A read", tc.name, keyring.Size(), err)
		}
		got := strings.Contains(logs.String(), "keyring_file_truncated") && strings.Contains(logs.String(), "keys.gpg")
		if got != tc.warn || tc.warn && !strings.Contains(logs.String(), "offset") {
			t.Errorf("%s: WARN logged %v, want %v:\n%s", tc.name, got, tc.warn, logs.String())
		}
	}
}

var _ = openpgp.EntityList{} // the package's type, for the helpers above
