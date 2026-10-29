package gpg

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// The apt-shape table test (the hardening plan's Tasks 6 and 7,
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md, DN-2 to
// DN-6): the verifier, run on every shape of the committed corpus
// (testdata/aptshapes, e2e/apt-oracle/README.md), refuses a shape when any
// measured apt refuses it and accepts a shape every measured apt accepts,
// but for the exceptions listed below, each with its reason.
//
// AIDEV-NOTE: the corpus and verdicts.txt are one measurement (never edit a
// fixture by hand; e2e/apt-oracle/README.md, "Adding a shape"). A row this
// test fails is the cache disagreeing with apt: fix the verifier, or — only
// with a ruling — list the shape below with its reason. Never force a row.

// aptShapeColumns is the number of verdicts on each row of verdicts.txt, one
// per client image in DN-2's order (debian:11, ubuntu:22.04, ubuntu:24.04,
// debian:12, debian:13, ubuntu:26.04: COLUMNS in e2e/apt-oracle/run.sh).
// Another set of images is another set of columns: change it with them.
const aptShapeColumns = 6

// aptShapeDeviations are the strict deviations: shapes every measured apt
// accepts and the cache refuses, each with its reason — DN-6's eleven
// (ruling HD-9), ruling HD-13's two (batch fx8's p3) and ruling HD-14's one
// (batch fx9's t7g7b).
var aptShapeDeviations = map[string]string{
	"xi_twoblocks_goodgood":          aptShapeOneArmorBlock,
	"xi_twoblocks_good_unknown":      aptShapeOneArmorBlock,
	"xd_twoblocks_goodgood":          aptShapeOneArmorBlock,
	"xd_twoblocks_good_unknown":      aptShapeOneArmorBlock,
	"i_sigbegin_trailing_ws":         "go-crypto's decoder refuses the shape (clearsign.Decode strips one CR and then compares the signature BEGIN line exactly; rejected today too)",
	"x7i_sigbegin_double_cr":         "go-crypto's decoder refuses the shape (clearsign.Decode strips one CR and then compares the signature BEGIN line exactly; rejected today too)",
	"xi_cleartext_comment_hdr":       "go-crypto's decoder refuses the shape (clearsign.Decode allows no armor header but Hash in the signed message's header; rejected today too)",
	"x3i_armor_no_blank":             aptShapeNoBlankLine,
	"x3d_armor_no_blank":             aptShapeNoBlankLine,
	"xi_good_plus_unknownpk_A":       "go-crypto cannot parse an unknown public-key algorithm, so the packet cannot be told from garbage (F6); a vendor adding a signature with an algorithm go-crypto v1.4.1 lacks is refused until go-crypto supports it",
	"x3i_goodsig_unhashed_fpr_added": "S1's contradiction rule refuses it: the unhashed issuer fingerprint, the last issuer subpacket and so the one go-crypto verifies by, contradicts the hashed one gpgv reads (ruling HD-14); before S1, S2 refused it already (the unhashed fingerprint overrides the issuer go-crypto looks up)",
	"gi_p3_inner_zero_bytes":         aptShapePaddingInsidePacket,
	"gd_p3_inner_zero_bytes":         aptShapePaddingInsidePacket,
	"t7g7b_33junk_33A":               "its first hashed issuer fingerprint is in another version's form, so gpgv attributes the packet to no key, while S1, unable to attribute it as gpgv does, takes go-crypto's issuer (the second fingerprint, A's) and the forged packet fails against A (ruling HD-14); no real signer writes it",
}

const (
	aptShapePaddingInsidePacket = "go-crypto reads zero bytes inside a packet's declared length as the next packet's header, so F6 refuses the signature; every apt skips them; no surveyed vendor publishes padding inside a packet (2026-10-29)"
	aptShapeOneArmorBlock       = "one armor block: the decoders read only the first, so S1 and S2 cannot check a second; no surveyed vendor publishes two (2026-10-28)"
	aptShapeNoBlankLine         = "go-crypto's decoder refuses the shape, and so does F5 (with no blank line, every armor line counts as a header without \": \"): a go-crypto upgrade accepting it would leave it refused"
)

// aptShapeKnownUnsafe is DN-6's one known-unsafe shape: every measured apt
// refuses it and the cache accepts it. TestAptShapeKnownUnsafe_IsExactlyC3
// pins it to exactly this entry, so nobody adds to it lightly.
var aptShapeKnownUnsafe = map[string]string{
	"x3i_sha1_only": "C-3, weak digests: out of scope, recorded Open (the hardening plan's Task 8 (f)); only the signer can make it",
}

// pendingS1 held the twelve shapes only S1 refuses (Appendix A's rule
// column marks them REJ(S1): a bad signature from a known key beside a good
// one) while the table test waited for S1. The hardening plan's Task 7 added
// S1 (requireKnownSignaturesVerify) and emptied it, so every row runs; a
// shape goes back in it only with a ruling, like the exception maps above.
var pendingS1 = map[string]bool{}

const aptShapeDir = "testdata/aptshapes"

// aptShape is one corpus shape: an InRelease (inline) or a Release.gpg
// (detached), with the Release it signs.
type aptShape struct {
	name     string
	inline   bool
	body     []byte // the InRelease, or the Release.gpg
	release  []byte // detached: the shape's own <name>.Release, else the shared Release
	rejected []bool // apt's verdicts, one per column: true is REJ
}

// loadAptShapeVerdicts parses verdicts.txt (its format: the AIDEV-NOTE in
// e2e/apt-oracle/run.sh): "#" lines are comments, every other line is a
// shape's name and aptShapeColumns verdicts, ok or REJ, single spaces apart.
func loadAptShapeVerdicts(t *testing.T) map[string][]bool {
	t.Helper()
	f, err := os.Open(filepath.Join(aptShapeDir, "verdicts.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := make(map[string][]bool)
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) != 1+aptShapeColumns {
			t.Fatalf("verdicts.txt:%d: %q: want a name and %d verdicts", n, line, aptShapeColumns)
		}
		name := fields[0]
		if _, dup := out[name]; dup {
			t.Fatalf("verdicts.txt:%d: %s has two rows", n, name)
		}
		rejected := make([]bool, aptShapeColumns)
		for i, v := range fields[1:] {
			switch v {
			case "ok":
			case "REJ":
				rejected[i] = true
			default:
				t.Fatalf("verdicts.txt:%d: %s: verdict %q is neither ok nor REJ", n, name, v)
			}
		}
		out[name] = rejected
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("verdicts.txt holds no row")
	}
	return out
}

// loadAptShapes reads the corpus: every <name>.InRelease and
// <name>.Release.gpg (with <name>.Release when present), each with its row
// of verdicts.txt. A shape without a row, a row without a shape, or a file
// the corpus does not define fails the test.
func loadAptShapes(t *testing.T) []aptShape {
	t.Helper()
	verdicts := loadAptShapeVerdicts(t)
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(aptShapeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	shared := read("Release")
	entries, err := os.ReadDir(aptShapeDir)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*aptShape)
	ownRelease := make(map[string][]byte)
	for _, e := range entries {
		file := e.Name()
		switch {
		case file == "Release" || file == "keys.gpg" || file == "verdicts.txt":
			continue
		case e.IsDir():
			t.Fatalf("%s: unexpected directory in the corpus", file)
		case strings.HasSuffix(file, ".InRelease"), strings.HasSuffix(file, ".Release.gpg"):
			inline := strings.HasSuffix(file, ".InRelease")
			name := strings.TrimSuffix(strings.TrimSuffix(file, ".InRelease"), ".Release.gpg")
			if _, dup := byName[name]; dup {
				t.Fatalf("%s: two files for one shape", name)
			}
			byName[name] = &aptShape{name: name, inline: inline, body: read(file)}
		case strings.HasSuffix(file, ".Release"):
			ownRelease[strings.TrimSuffix(file, ".Release")] = read(file)
		default:
			t.Fatalf("%s: a file the corpus does not define", file)
		}
	}
	for name, rel := range ownRelease {
		s, ok := byName[name]
		if !ok || s.inline {
			t.Fatalf("%s.Release: no detached shape %s signs it", name, name)
		}
		s.release = rel
	}
	var out []aptShape
	for name, s := range byName {
		rejected, ok := verdicts[name]
		if !ok {
			t.Fatalf("%s: no row in verdicts.txt", name)
		}
		s.rejected = rejected
		if !s.inline && s.release == nil {
			s.release = shared
		}
		out = append(out, *s)
	}
	for name := range verdicts {
		if _, ok := byName[name]; !ok {
			t.Fatalf("verdicts.txt: %s has no shape file", name)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// anyRejects and allReject read a row of apt's verdicts.
func anyRejects(rejected []bool) bool {
	for _, r := range rejected {
		if r {
			return true
		}
	}
	return false
}

func allReject(rejected []bool) bool {
	for _, r := range rejected {
		if !r {
			return false
		}
	}
	return true
}

// aptShapeVerifier is the verifier the table holds to apt: the host keyring
// is keys.gpg alone (A, E and S: the clients' signed-by), with the defaults —
// require_signature = true, no [[trusted_signer]] pin, allow_short_keyid =
// true, no accept_any_signer. keys.gpg is loaded as an embedded source, not
// with LoadKeyring on the corpus directory, which would also try every
// <name>.Release.gpg as a keyring and warn for each.
func aptShapeVerifier(t *testing.T) *Verifier {
	t.Helper()
	keys, err := os.ReadFile(filepath.Join(aptShapeDir, "keys.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := LoadKeyringWithEmbedded(nil, []EmbeddedSource{{Name: "keys.gpg", Data: keys}}, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if keyring.Size() != 3 {
		t.Fatalf("keys.gpg holds %d keys, want 3 (A, E and S)", keyring.Size())
	}
	v, err := NewVerifier(VerifierConfig{
		Keyring:          keyring,
		RequireSignature: true,
		AllowShortKeyID:  true,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestVerifier_FollowsTheAptShapeTable holds the verifier to DN-3's rule over
// the corpus: rejected iff any measured apt rejects, but for a strict
// deviation (rejected, every apt accepts) and the known-unsafe shape
// (accepted, every apt rejects); a shape pending S1 is skipped.
func TestVerifier_FollowsTheAptShapeTable(t *testing.T) {
	v := aptShapeVerifier(t)
	for _, s := range loadAptShapes(t) {
		t.Run(s.name, func(t *testing.T) {
			if pendingS1[s.name] {
				t.Skipf("%s: only S1 refuses it (the hardening plan's Task 7)", s.name)
			}
			wantRejected, why := anyRejects(s.rejected), "apt's verdicts"
			if reason, ok := aptShapeDeviations[s.name]; ok {
				if anyRejects(s.rejected) || reason == "" {
					t.Fatalf("listed as a strict deviation (reason %q), but apt's verdicts are %v: a deviation is a shape every apt accepts, with a reason", reason, s.rejected)
				}
				wantRejected, why = true, "a strict deviation: "+reason
			}
			if reason, ok := aptShapeKnownUnsafe[s.name]; ok {
				if !allReject(s.rejected) || reason == "" {
					t.Fatalf("listed as known unsafe (reason %q), but apt's verdicts are %v: a known-unsafe shape is one every apt rejects, with a reason", reason, s.rejected)
				}
				wantRejected, why = false, "known unsafe: "+reason
			}
			var err error
			if s.inline {
				_, err = v.VerifyInline(context.Background(), newSuite(), s.body)
			} else {
				_, err = v.VerifyDetached(context.Background(), newSuite(), s.release, s.body)
			}
			switch {
			case wantRejected && err == nil:
				t.Errorf("accepted; want rejected (%s; apt REJ per column: %v)", why, s.rejected)
			case !wantRejected && err != nil:
				t.Errorf("rejected: %v (reason %s); want accepted (%s; apt REJ per column: %v)", err, ClassifyVerifyErr(err), why, s.rejected)
			case err != nil:
				t.Logf("rejected: %v (reason %s)", err, ClassifyVerifyErr(err))
			}
		})
	}
}

// TestAptShapeDeviations_NameShapesInTheTable: no stale entry — every shape
// the exception maps and pendingS1 name has a row in verdicts.txt, no shape
// is in two of them, and each listed reason is non-empty.
func TestAptShapeDeviations_NameShapesInTheTable(t *testing.T) {
	verdicts := loadAptShapeVerdicts(t)
	seen := make(map[string]string)
	check := func(set, name, reason string) {
		t.Helper()
		if _, ok := verdicts[name]; !ok {
			t.Errorf("%s: %s names no shape of verdicts.txt", set, name)
		}
		if reason == "" {
			t.Errorf("%s: %s has no reason", set, name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("%s is in both %s and %s", name, other, set)
		}
		seen[name] = set
	}
	for name, reason := range aptShapeDeviations {
		check("aptShapeDeviations", name, reason)
	}
	for name, reason := range aptShapeKnownUnsafe {
		check("aptShapeKnownUnsafe", name, reason)
	}
	for name, pending := range pendingS1 {
		if !pending {
			t.Errorf("pendingS1: %s is listed false: delete it instead", name)
		}
		check("pendingS1", name, "S1")
		if rejected, ok := verdicts[name]; ok && !anyRejects(rejected) {
			t.Errorf("pendingS1: every apt accepts %s, so no rule should refuse it", name)
		}
	}
}

// TestCRC24_RFC4880CheckValue: F3's checksum is RFC 4880 §6.1's CRC-24,
// whose check value (the CRC of "123456789") is 0x21CF02.
func TestCRC24_RFC4880CheckValue(t *testing.T) {
	if got := crc24([]byte("123456789")); got != 0x21CF02 {
		t.Fatalf("crc24(\"123456789\") = %#06x, want 0x21cf02", got)
	}
}

// TestCheckSignatureArmor_BlankLinesAreSpacesTabsAndCRs pins the two places
// checkSignatureArmor asks whether a line is blank — the line that ends the
// armor headers (F5) and the lines after the CRC line (F3) — to isBlankLine,
// spaces, tabs and CRs only, not bytes.TrimSpace, which also trims a
// vertical tab, a form feed, U+0085 and U+00A0 (ruling HD-13 keeps the
// after-CRC test so; F5's header end makes the same choice). A "\v" line in
// either place is refused; spaces, a tab and a CR there pass.
func TestCheckSignatureArmor_BlankLinesAreSpacesTabsAndCRs(t *testing.T) {
	decoded := []byte("the bytes the CRC line covers; the checks never parse them")
	good := string(armorSignature(t, decoded))
	const begin = "-----BEGIN PGP SIGNATURE-----\n\n"
	const end = "\n-----END PGP SIGNATURE-----"
	if !strings.HasPrefix(good, begin) || strings.Count(good, end) != 1 {
		t.Fatalf("test premise: an unexpected armor %q", good)
	}
	check := func(text string) error {
		b := []byte(text)
		return checkSignatureArmor(b, splitLines(b), 0, decoded)
	}
	headersEnd := func(blank string) string {
		return strings.Replace(good, begin, "-----BEGIN PGP SIGNATURE-----\n"+blank+"\n", 1)
	}
	afterCRC := func(blank string) string {
		return strings.Replace(good, end, "\n"+blank+end, 1)
	}
	for name, text := range map[string]string{
		"the armor as written":               good,
		"headers ended by spaces, tab, CR":   headersEnd("  \t\r"),
		"spaces, tab, CR after the CRC line": afterCRC("  \t\r"),
	} {
		if err := check(text); err != nil {
			t.Errorf("%s: %v; want it accepted", name, err)
		}
	}
	for name, text := range map[string]string{
		"headers ended by a vertical tab (F5)":   headersEnd("\v"),
		"a vertical tab after the CRC line (F3)": afterCRC("\v"),
	} {
		if err := check(text); err == nil {
			t.Errorf("%s: accepted; want refused (a blank line is spaces, tabs and CRs only)", name)
		}
	}
}

// TestRequireSignaturePacketsOnly_Framing pins F6's framing walk on what
// the corpus does not hold: one signature packet framed in each of the
// six length encodings of RFC 9580 §4.2 is accepted, and a partial or an
// indeterminate length, a truncated packet, a packet of another type or a
// byte that starts no packet is refused — though go-crypto's reader would
// parse the partial and indeterminate forms as a signature.
func TestRequireSignaturePacketsOnly_Framing(t *testing.T) {
	small := newEdDSAEntity(t)
	large := newTestEntity(t, "Signer", "signer@example.com")
	bodyOf := func(e *openpgp.Entity) []byte {
		t.Helper()
		var buf bytes.Buffer
		if err := openpgp.DetachSign(&buf, e, strings.NewReader(fakeReleasePlaintext), nil); err != nil {
			t.Fatal(err)
		}
		sig := buf.Bytes()
		// go-crypto writes new-format headers: 0xC2, then a one- or
		// two-octet length.
		switch {
		case sig[0] == 0xc2 && sig[1] < 192:
			return sig[2:]
		case sig[0] == 0xc2 && sig[1] < 224:
			return sig[3:]
		}
		t.Fatalf("test premise: an unexpected header % x", sig[:3])
		return nil
	}
	smallBody, largeBody := bodyOf(small), bodyOf(large)
	if len(smallBody) >= 192 || len(largeBody) < 256 {
		t.Fatalf("test premise: body lengths %d and %d, want one under 192 and one over 255", len(smallBody), len(largeBody))
	}
	be32 := func(n int) []byte { return []byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)} }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	l := len(largeBody) - 192
	for name, framed := range map[string][]byte{
		"new format, one-octet length":  cat([]byte{0xc2, byte(len(smallBody))}, smallBody),
		"new format, two-octet length":  cat([]byte{0xc2, byte(l>>8) + 192, byte(l)}, largeBody),
		"new format, five-octet length": cat([]byte{0xc2, 0xff}, be32(len(largeBody)), largeBody),
		"old format, one-octet length":  cat([]byte{0x88, byte(len(smallBody))}, smallBody),
		"old format, two-octet length":  cat([]byte{0x89, byte(len(largeBody) >> 8), byte(len(largeBody))}, largeBody),
		"old format, four-octet length": cat([]byte{0x8a}, be32(len(largeBody)), largeBody),
		"two packets":                   cat([]byte{0xc2, byte(len(smallBody))}, smallBody, []byte{0x88, byte(len(smallBody))}, smallBody),
	} {
		if err := requireSignaturePacketsOnly(framed); err != nil {
			t.Errorf("%s: %v; want accepted", name, err)
		}
	}
	// A partial length: a one-octet first part (0xE0), then the rest.
	partial := cat([]byte{0xc2, 0xe0}, smallBody[:1], []byte{byte(len(smallBody) - 1)}, smallBody[1:])
	indeterminate := cat([]byte{0x8b}, smallBody)
	for _, goCryptoParses := range [][]byte{partial, indeterminate} {
		if p, err := packet.Read(bytes.NewReader(goCryptoParses)); err != nil {
			t.Fatalf("test premise: go-crypto does not parse % x…: %v", goCryptoParses[:2], err)
		} else if _, ok := p.(*packet.Signature); !ok {
			t.Fatalf("test premise: go-crypto parses % x… as a %T", goCryptoParses[:2], p)
		}
	}
	for name, framed := range map[string][]byte{
		"a partial length":        partial,
		"an indeterminate length": indeterminate,
		"a truncated body":        cat([]byte{0xc2, byte(len(smallBody))}, smallBody[:len(smallBody)-1]),
		"a truncated header":      {0xc2, 0xff, 0x00},
		"a marker packet":         cat([]byte{0xc2, byte(len(smallBody))}, smallBody, []byte{0xca, 0x03, 'P', 'G', 'P'}),
		"a byte that is no tag":   cat([]byte{0xc2, byte(len(smallBody))}, smallBody, []byte{0x00}),
	} {
		if err := requireSignaturePacketsOnly(framed); err == nil {
			t.Errorf("%s: accepted; want refused", name)
		}
	}
}

// newEdDSAEntity makes an EdDSA signing key, whose signature packets are
// short enough (under 192 octets) for the one-octet length encodings.
func newEdDSAEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("EdDSA", "", "eddsa@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatalf("NewEntity (EdDSA): %v", err)
	}
	return e
}

// TestAptShapeKnownUnsafe_IsExactlyC3: the cache adopts exactly one shape
// every apt refuses, x3i_sha1_only (the security review's C-3, out of the
// hardening plan's scope). Adding to the map needs a ruling, not an edit.
func TestAptShapeKnownUnsafe_IsExactlyC3(t *testing.T) {
	if len(aptShapeKnownUnsafe) != 1 || !strings.HasPrefix(aptShapeKnownUnsafe["x3i_sha1_only"], "C-3") {
		t.Fatalf("aptShapeKnownUnsafe = %v, want exactly x3i_sha1_only (C-3)", aptShapeKnownUnsafe)
	}
}
