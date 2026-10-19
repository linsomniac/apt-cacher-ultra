package simtest

import "strings"

// fakeSignatureBlock is a structurally valid armored signature block whose
// body is no real signature. go-crypto's clearsign.Decode accepts it (it
// reads the armor lazily); nothing verifies it cryptographically.
const fakeSignatureBlock = "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQBQJmAAAAAAkQAAAAAAAAAAAAAAA=\n=AAAA\n-----END PGP SIGNATURE-----\n"

// ClearsignedFixture wraps text — the Release text of an InRelease — in a
// clearsign envelope (spec §7 "Fixtures"): the first line is the marker
// apt requires, so the handler's sniff (spec C3) counts the body as signed
// metadata; clearsign.Decode returns text, less its final newline, as the
// plaintext, so reconcile parses it; a pass-through verifier hands the
// whole envelope to ParseRelease, which skips the armor lines. The
// signature is a fixed fake: tests that verify signatures sign for real.
// No line of text may begin with "-" (it would need dash-escaping).
func ClearsignedFixture(text string) []byte {
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		strings.TrimSuffix(text, "\n") + "\n" + fakeSignatureBlock)
}
