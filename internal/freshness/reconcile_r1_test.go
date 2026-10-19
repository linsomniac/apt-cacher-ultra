package freshness

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// R-1 (security review 2026-09): lines placed after a forged "=CRC" line
// inside the signature armor are unauthenticated. Reconcile must parse
// only the clearsigned plaintext.
func TestReparseSnapshotRelease_IgnoresArmorInjection(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()

	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte{}, signed[:idx]...), evil...), signed[idx:]...)

	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(ctx, id)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, want only the signed member", got)
	}
}

// Phase D removed reconcile's plain-text fallback: a stored InRelease
// without a clearsign marker is an error, never parsed (the fixtures that
// needed it use simtest.ClearsignedFixture).
func TestReparseSnapshotRelease_PlainTextInReleaseIsAnError(t *testing.T) {
	env := newAdoptionTestEnv(t)
	body := "Origin: Test\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	id := storeInlineSnapshot(t, env, []byte(body))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "not clearsigned") {
		t.Fatalf("got %+v, %v; want a not-clearsigned error and no members", got, err)
	}
}

// R-1, second unsigned region: the armor's own header block. A
// declaration whose path carries a colon survives armor.Decode as a
// "Key: Value" header and still verifies; the pre-fix raw parse ingested it.
func TestReparseSnapshotRelease_IgnoresArmorHeaderInjection(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	sigBegin := bytes.Index(signed, []byte("-----BEGIN PGP SIGNATURE-----"))
	hdrEnd := sigBegin + bytes.IndexByte(signed[sigBegin:], '\n') + 1
	tampered := append(append(append([]byte{}, signed[:hdrEnd]...),
		"SHA256:\n "+strings.Repeat("c", 64)+" 10 main/binary-evilarch/Packages:x\n"...), signed[hdrEnd:]...)
	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want only the signed member", got, err)
	}
}

// R-1 fail-closed: an envelope whose clearsign frame no longer decodes
// (END marker gone) but whose RAW text still carries a parseable SHA256
// block must be an error — never a fallback parse of the raw envelope.
func TestReparseSnapshotRelease_UndecodableEnvelopeFailsClosed(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	id := storeInlineSnapshot(t, env, append([]byte{}, signed[:idx]...))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 {
		t.Fatalf("got %d members, err=%v; want a decode error and no members", len(got), err)
	}
}

// R-1: until phase D the verifier accepted whitespace before the BEGIN
// marker (ruling R-D2 now refuses it), so snapshots adopted earlier can
// hold such an envelope: the clearsign detection must find a marker
// anywhere (never HasPrefix), and the injection must be ignored for a
// whitespace-prefixed envelope too.
func TestReparseSnapshotRelease_IgnoresInjectionWithLeadingWhitespace(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte("\n \n"), signed[:idx]...), evil...), signed[idx:]...)
	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want only the signed member", got, err)
	}
}

func clearsignForTest(t *testing.T, text string) []byte {
	t.Helper()
	// Ed25519: an RSA key per call is slow under -race (deferred minor A-33).
	ent, err := openpgp.NewEntity("t", "", "t@example.invalid", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, ent.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Deferred minor A-34: an injection after the armor CRC of a CRLF envelope
// is ignored like one in an LF envelope.
func TestReparseSnapshotRelease_IgnoresInjectionInACRLFEnvelope(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte{}, signed[:idx]...), evil...), signed[idx:]...)
	id := storeInlineSnapshot(t, env, bytes.ReplaceAll(tampered, []byte("\n"), []byte("\r\n")))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want only the signed member", got, err)
	}
}

// Deferred minor A-34: a declaration placed in the signed message's own
// header block (beside "Hash:") makes the envelope undecodable — an error,
// never a parse of the raw bytes.
func TestReparseSnapshotRelease_HeaderBlockInjectionFailsClosed(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	hashLine := bytes.Index(signed, []byte("Hash: "))
	lineEnd := hashLine + bytes.IndexByte(signed[hashLine:], '\n') + 1
	tampered := append(append(append([]byte{}, signed[:lineEnd]...),
		"SHA256: "+strings.Repeat("d", 64)+" 10 main/binary-evilarch/Packages\n"...), signed[lineEnd:]...)
	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 {
		t.Fatalf("got %d members, err=%v; want a decode error and no members", len(got), err)
	}
}

// Ruling R-D2: a stored envelope whose marker line carries trailing blanks
// — apt accepts it, go-crypto's decoder alone would not — still decodes.
func TestReparseSnapshotRelease_DecodesAptsMarkerLine(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	const marker = "-----BEGIN PGP SIGNED MESSAGE-----"
	blanks := bytes.Replace(signed, []byte(marker+"\n"), []byte(marker+" \t\n"), 1)
	id := storeInlineSnapshot(t, env, blanks)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want the signed member", got, err)
	}
}

// storeInlineSnapshot writes blob to the pool and records an adopted
// inline snapshot whose InRelease member points at it.
func storeInlineSnapshot(t *testing.T, env *adoptionTestEnv, blob []byte) int64 {
	t.Helper()
	ctx := context.Background()
	h, err := env.adopter.writeBlobBytes(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: env.suite.CanonicalScheme, CanonicalHost: env.suite.CanonicalHost,
		SuitePath: env.suite.SuitePath, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cache.CommitAdoption(ctx, id,
		[]cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}
