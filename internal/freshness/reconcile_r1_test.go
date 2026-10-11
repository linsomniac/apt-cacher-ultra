package freshness

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

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

func TestReparseSnapshotRelease_PlainTextInReleaseStillParses(t *testing.T) {
	// Test fixtures (makeRelease + passThroughVerifier) still adopt
	// plain-text InRelease bodies until phase D migrates them to
	// clearsignedFixture(); reconcile keeps parsing them until then.
	// AIDEV-TODO(phase D): flip to _PlainTextInReleaseIsAnError.
	env := newAdoptionTestEnv(t)
	body := "Origin: Test\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	id := storeInlineSnapshot(t, env, []byte(body))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
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

// R-1: the verifier accepts whitespace before the BEGIN marker, so the
// clearsign detection must find a marker anywhere (never HasPrefix), and
// the injection must be ignored for a whitespace-prefixed envelope too.
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
	ent, err := openpgp.NewEntity("t", "", "t@example.invalid", nil)
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
