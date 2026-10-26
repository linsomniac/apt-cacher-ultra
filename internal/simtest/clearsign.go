package simtest

import (
	"fmt"
	"strings"
)

// fakeSignatureBlock is a structurally valid armored signature block whose
// body is no real signature. go-crypto's clearsign.Decode accepts it (it
// reads the armor lazily); nothing verifies it cryptographically.
const fakeSignatureBlock = "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQBQJmAAAAAAkQAAAAAAAAAAAAAAA=\n=AAAA\n-----END PGP SIGNATURE-----\n"

// ClearsignedFixture wraps text — the Release text of an InRelease — in a
// clearsign envelope (spec §7 "Fixtures"): the first line is the marker
// apt requires, so the handler's sniff (spec C3) counts the body as signed
// metadata; clearsign.Decode returns text — less its final newline, and
// with each line's trailing spaces and tabs trimmed — as the plaintext, so
// reconcile parses it; a pass-through verifier hands the whole envelope to
// ParseRelease, which skips the armor lines. The signature is a fixed
// fake: tests that verify signatures sign for real. A line of text that
// begins with "-" would need dash-escaping, and its decoded plaintext would
// differ: the fixture panics on one (deferred minor D-7).
func ClearsignedFixture(text string) []byte {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "-") {
			panic(fmt.Sprintf("simtest.ClearsignedFixture: line %q begins with \"-\"", line))
		}
	}
	return []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" +
		strings.TrimSuffix(text, "\n") + "\n" + fakeSignatureBlock)
}
