package gpg

import (
	"context"
	"testing"
)

// I-1 (security review 2026-09): go-crypto v1.4.1's clearsign.Decode
// panics when a clearsigned message has no plaintext lines. VerifyInline
// must return an ordinary error in both signature modes, never panic and
// never hand the envelope back as "unsigned" text.
func TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	bodies := []string{
		"-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----",
		"-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----\n",
	}
	for _, require := range []bool{true, false} {
		v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: require, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range bodies {
			out, err := v.VerifyInline(context.Background(), newSuite(), []byte(body))
			if err == nil || out != nil {
				t.Errorf("require_signature=%v body=%q: out=%q err=%v; want an error", require, body, out, err)
			}
		}
	}
}
