package gpg

// Tests of the hybrid verifier's layers (the hardening plan's DN-11, Task
// 9b): T (internal/apttext), A (armor_canon.go), P (sigpacket.go) and S
// (verifier.go), on the corpus and on shapes it does not hold.

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
)

// ruleOf names the rule that refused err: a structural rule of layers T, A
// and P (T1–T6, F0, T4/one-block, A1–A6, P1–P5), "S1" for ErrBadSignature,
// "S2:<reason>" for any other refusal (ClassifyVerifyErr's reason), "" for
// none.
func ruleOf(err error) string {
	var se *shapeError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se):
		return se.Rule
	case errors.Is(err, ErrBadSignature):
		return "S1"
	}
	return "S2:" + ClassifyVerifyErr(err)
}

// TestAptTextLayer_RefusesExactlyTheSplitRows is layer T's differential
// test (the hardening plan's DN-11; PE/approach-decision.md §6, its first
// residual risk: a port bug): the port alone — apttext.SplitClearsigned and
// apttext.SplitDetached, without the cache's rules on top (T5, T6, F0, one block) —
// refuses a corpus row exactly when apt's own text layer refused it in
// every image (classes.txt's "split": apt refused the file before its
// verifier ran). That holds the port to apt's splitter row by row, not only
// to the overall verdict: a row the port refuses and apt split accepted, or
// the reverse, fails here even when another layer gives the table test its
// verdict.
func TestAptTextLayer_RefusesExactlyTheSplitRows(t *testing.T) {
	classes := loadAptShapeClasses(t)
	shapes := loadAptShapes(t)
	splits := 0
	for _, s := range shapes {
		row := classes[s.name]
		split := len(row) > 0 && row[0] == "split" // a split row is split in every column (TestAptShapeClasses_AgreeWithTheVerdicts)
		var err error
		if s.inline {
			_, err = apttext.SplitClearsigned(s.body)
		} else {
			_, _, err = apttext.SplitDetached(s.body)
		}
		if split {
			splits++
		}
		switch {
		case split && err == nil:
			t.Errorf("%s: apt's text layer refused it (split), the port accepts it", s.name)
		case !split && err != nil:
			t.Errorf("%s: the port refuses it (%v), apt's text layer passed it on (classes %v)", s.name, err, row)
		}
	}
	// The corpus's split rows (64, Task 9a): a test that compared nothing
	// would pass too.
	if splits < 60 {
		t.Fatalf("test premise: %d split rows in classes.txt, want the corpus's 64", splits)
	}
}

// TestVerifier_RefusesEachRowByItsRule holds every corpus row to the rule
// aptShapeRefusedBy pins for it (the final review's V-M8): a refused row
// by exactly that rule, an unlisted row accepted, no stale pin.
func TestVerifier_RefusesEachRowByItsRule(t *testing.T) {
	v := aptShapeVerifier(t)
	seen := make(map[string]bool)
	for _, s := range loadAptShapes(t) {
		seen[s.name] = true
		got, want := ruleOf(verifyShape(v, s)), aptShapeRefusedBy[s.name]
		if got != want {
			t.Errorf("%s: refused by %q, pinned to %q (\"\": accepted)", s.name, got, want)
		}
	}
	for name := range aptShapeRefusedBy {
		if !seen[name] {
			t.Errorf("aptShapeRefusedBy: %s names no corpus row", name)
		}
	}
}

// TestVerifier_BypassRefusesEveryMalformedRow is the table test's bypass
// twin (the final review's V-I1; DN-8, DN-11): under accept_any_signer with
// no pin matched, the verifier runs layers T, A and P and nothing else. So
// every corpus row the default verifier refuses as malformed_clearsign or
// malformed_signature is refused there with the same reason and by the same
// rule, and every other row — refused by S1 or S2, or accepted — is
// accepted there. Both cases have rows (the premise at the end): a twin
// that compared nothing would pass too.
func TestVerifier_BypassRefusesEveryMalformedRow(t *testing.T) {
	trusted, bypass := aptShapeVerifier(t), aptShapeVerifier(t, true)
	structural, signatures := 0, 0
	for _, s := range loadAptShapes(t) {
		want, got := verifyShape(trusted, s), verifyShape(bypass, s)
		var se *shapeError
		if errors.As(want, &se) {
			structural++
			if ClassifyVerifyErr(got) != ClassifyVerifyErr(want) || ruleOf(got) != ruleOf(want) {
				t.Errorf("%s: refused by %s (%s); under accept_any_signer: %v (%s, %s), want the same refusal",
					s.name, ruleOf(want), ClassifyVerifyErr(want), got, ruleOf(got), ClassifyVerifyErr(got))
			}
			continue
		}
		if want != nil {
			signatures++
		}
		if got != nil {
			t.Errorf("%s: %v under accept_any_signer (%s); want it accepted: layers T, A and P pass it, and the bypass checks no signature",
				s.name, got, ruleOf(got))
		}
	}
	if structural == 0 || signatures == 0 {
		t.Fatalf("test premise: %d rows refused by layers T, A and P, %d by S1 and S2; want both", structural, signatures)
	}
}

// aptShapeAllocationMax is the most a verification may allocate, row by
// row (TotalAlloc's delta): the final review's V-C2 shapes made go-crypto
// allocate 1 or 4 GiB before any check; layers P2 and P4 refuse them
// before go-crypto reads a byte.
const aptShapeAllocationMax = 1 << 20

// aptShapeGenericCurveMax is the bound for a row whose keyring holds a
// brainpool or secp256k1 key (batch fx20): go-crypto verifies those curves
// with its generic big.Int arithmetic, a few mebibytes a signature
// (measured: 3.5 MiB for P-256's size, 11 MiB for brainpoolP512r1), set by
// the curve, never by a length the anchor claims; layer P bounds the
// packets.
const aptShapeGenericCurveMax = 16 << 20

// genericCurveKeyring reports whether v's keyring holds a key or subkey of
// a curve go-crypto verifies generically (aptShapeGenericCurveMax).
func genericCurveKeyring(v *Verifier) bool {
	for _, e := range v.keyring.EntityList() {
		keys := []*packet.PublicKey{e.PrimaryKey}
		for _, sub := range e.Subkeys {
			keys = append(keys, sub.PublicKey)
		}
		for _, k := range keys {
			if c := keyClass(k); c == "brainpool" || c == "secp256k1" {
				return true
			}
		}
	}
	return false
}

// allocatedBy returns what one call of f allocated, in bytes.
func allocatedBy(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestVerifier_BoundsAllocationOnEveryRow: no corpus row makes a
// verification allocate a mebibyte, so a future shape that reaches
// go-crypto's allocating parse with a hostile length fails here, by name,
// on its first run, and no row is ever held back from the table test by
// its name (the Task 9a review's 9A-M3).
func TestVerifier_BoundsAllocationOnEveryRow(t *testing.T) {
	v := aptShapeVerifier(t)
	for _, s := range loadAptShapes(t) {
		// The shape's own keyring, if it has one, is loaded first: the bound
		// is the verification's, not the key reader's (a brainpool or
		// secp256k1 key's self-signature costs go-crypto's generic curve
		// arithmetic megabytes, batch fx20).
		sv, err := shapeVerifier(v, s)
		if err != nil {
			t.Errorf("%s: its keyring: %v", s.name, err)
			continue
		}
		bound := uint64(aptShapeAllocationMax)
		if genericCurveKeyring(sv) {
			bound = aptShapeGenericCurveMax
		}
		if n := allocatedBy(func() { _, _ = verifyShapeSigners(sv, s) }); n >= bound {
			t.Errorf("%s: the verification allocated %d bytes; the bound is %d", s.name, n, bound)
		}
	}
}

// aptShapeVC2 are the final review's V-C2 shapes (batch fx11's vc2_ rows),
// each a few hundred bytes that made the verifier of Tasks 6 and 7, and
// main's, allocate gibibytes in go-crypto: a v6 packet claiming a hashed
// area of 0xFFFFFFF0 octets, and a v4 packet whose unhashed embedded
// signature (subpacket 32) holds a v6 header claiming 0x40000000, inline
// and detached; and the rule that now refuses each first.
var aptShapeVC2 = map[string]string{
	"vc2_i_v6_hashed_len_4g":  "P2",
	"vc2_d_v6_hashed_len_4g":  "P2",
	"vc2_i_v4_embedded_v6_1g": "P4",
	"vc2_d_v4_embedded_v6_1g": "P4",
}

// TestVerifier_BoundsAllocationOnVC2Shapes: VerifyInline and VerifyDetached
// on V-C2's shapes allocate under a mebibyte and return the form's
// malformed sentinel, from P2 or P4 (V-C2's fix). The list is guarded as the
// Task 9a review's 9A-M3 asks: it names every corpus row of batch fx11's
// vc2_ group and only those, and apt refuses each in every image.
func TestVerifier_BoundsAllocationOnVC2Shapes(t *testing.T) {
	v := aptShapeVerifier(t)
	listed := 0
	for _, s := range loadAptShapes(t) {
		rule, ok := aptShapeVC2[s.name]
		if !ok {
			if strings.HasPrefix(s.name, "vc2_") {
				t.Errorf("%s: a vc2_ row aptShapeVC2 does not list", s.name)
			}
			continue
		}
		listed++
		if !allReject(s.rejected) {
			t.Errorf("%s: apt's verdicts are %v; a V-C2 shape is refused by every apt", s.name, s.rejected)
		}
		sentinel := ErrMalformedClearsign
		if !s.inline {
			sentinel = ErrMalformedSignature
		}
		var err error
		if n := allocatedBy(func() { err = verifyShape(v, s) }); n >= aptShapeAllocationMax {
			t.Errorf("%s: the verification allocated %d bytes; the bound is %d", s.name, n, aptShapeAllocationMax)
		}
		if !errors.Is(err, sentinel) || ruleOf(err) != rule {
			t.Errorf("%s: err %v (rule %q); want %v from %s", s.name, err, ruleOf(err), sentinel, rule)
		}
	}
	if listed != len(aptShapeVC2) {
		t.Errorf("aptShapeVC2 lists %d shapes, the corpus holds %d of them", len(aptShapeVC2), listed)
	}
}

// TestRequireKnownSignaturesVerify_ContinuesPastAnUnknownKeysPacket: S1
// checks every packet gpgv checks, and a packet whose issuer the host
// keyring lacks (gpgv's NO_PUBKEY) is skipped, not the end of the check (the
// final review's V-I2; the corpus' u_i_unknown_bad_good and
// u_i_unknown_good_bad): B's packet, then A's bad one and A's good one, in
// either order, is refused (ErrBadSignature), by the verifier and by S1
// alone; without A's bad packet it is adopted.
func TestRequireKnownSignaturesVerify_ContinuesPastAnUnknownKeysPacket(t *testing.T) {
	a := newTestEntity(t, "A", "a@example.com")
	b := newTestEntity(t, "B", "b@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, a), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	bGood := signaturePacketsOf(t, clearsignWith(t, b, []byte(fakeReleasePlaintext)))
	aGood := signaturePacketsOf(t, clearsignWith(t, a, []byte(fakeReleasePlaintext)))
	aBad := signaturePacketsOf(t, clearsignWith(t, a, []byte("Origin: Other\n")))
	if _, err := v.VerifyInline(context.Background(), newSuite(), inlineWithSignatures(t, bGood, aGood)); err != nil {
		t.Fatalf("test premise: B's packet, then A's good one: %v; want it adopted", err)
	}
	for name, packets := range map[string][][]byte{
		"B, A's bad, A's good": {bGood, aBad, aGood},
		"B, A's good, A's bad": {bGood, aGood, aBad},
	} {
		body := inlineWithSignatures(t, packets...)
		if _, err := v.VerifyInline(context.Background(), newSuite(), body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: VerifyInline: %v; want ErrBadSignature", name, err)
		}
		m, pkts := inlineParts(t, body)
		if err := v.requireKnownSignaturesVerify(m, pkts); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: S1: %v; want ErrBadSignature", name, err)
		}
	}
}

// TestVerifyInline_ReturnsAptsMessage: VerifyInline returns M, the signed
// text as apt's splitter reads it and the client's apt parses it (DN-11),
// not go-crypto's plaintext. A signed text line ending in two CRs (the
// corpus' a_textline_crcr, accepted by every apt: no key needed, gpgv's
// text filter strips the CRs) comes back as the line signed; go-crypto's
// clearsign decoder strips one CR and would hand the Release parser
// "...\r". So do a dash-escaped line and trailing blanks.
func TestVerifyInline_ReturnsAptsMessage(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	for name, edit := range map[string][2]string{
		"two CRs at a line's end":       {"\nSuite: noble\n", "\nSuite: noble\r\r\n"},
		"a dash-escaped line":           {"\nSuite: noble\n", "\n- Suite: noble\n"},
		"trailing blanks, then a CR":    {"\nSuite: noble\n", "\nSuite: noble \t\r\n"},
		"CRLF line ends in the message": {"\nSuite: noble\nSHA256:\n", "\nSuite: noble\r\nSHA256:\r\n"},
	} {
		body := bytes.Replace(good, []byte(edit[0]), []byte(edit[1]), 1)
		if bytes.Equal(body, good) {
			t.Fatalf("%s: test premise: the edit changed nothing", name)
		}
		got, err := v.VerifyInline(context.Background(), newSuite(), body)
		if err != nil || !bytes.Equal(got, []byte(fakeReleasePlaintext)) {
			t.Errorf("%s: got %q, %v; want M, the signed text %q", name, got, err, fakeReleasePlaintext)
		}
	}
}

// inlineParts is what layers T, A and P make of an InRelease the test
// built: M and the signature packets.
func inlineParts(t *testing.T, body []byte) ([]byte, []sigPacket) {
	t.Helper()
	cs, err := apttext.Inline(body)
	if err != nil {
		t.Fatalf("test premise: layer T refuses the InRelease: %v", err)
	}
	pkts, err := armorPackets(cs.Armor, cs.ArmorLine)
	if err != nil {
		t.Fatalf("test premise: layers A and P refuse the InRelease: %v", err)
	}
	return cs.Message, pkts
}
