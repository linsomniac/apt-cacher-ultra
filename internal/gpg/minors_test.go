package gpg

// Task 9b fix round's minor findings: the unpinned suite before the
// anchor's shape (the re-review's V-RM1), the verifier's test clock
// (V-RM3).

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// TestVerifier_AnUnpinnedSuiteIsSaidWhateverTheAnchorsShape: the
// re-review's V-RM1. Under require_pinned_signer, a suite no
// [[trusted_signer]] block matches is a configuration fault: the verifier
// says so (ErrUnpinnedSuite, reason unpinned_suite, and its
// adoption_unpinned_suite WARN) whatever the anchor's shape — malformed,
// not clearsigned or well formed — where layers T, A and P ran first and a
// malformed anchor hid it. A pinned suite's malformed anchor keeps its
// structural reason, and so does an unpinned one's when pins are not
// required.
func TestVerifier_AnUnpinnedSuiteIsSaidWhateverTheAnchorsShape(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	pin := func(host string) SignerPin {
		return SignerPin{HostRegex: regexp.MustCompile(host), Fingerprints: map[string]struct{}{upperFP(signer.PrimaryKey.Fingerprint): {}}}
	}
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	malformed := append(append([]byte{}, good...), "\nan unsigned line after the END line\n"...) // T4
	release := []byte(fakeReleasePlaintext)
	detGood := detachSignWith(t, signer, release, true)
	detMalformed := append([]byte("x\n"), detGood...)
	for _, tc := range []struct {
		name          string
		pins          []SignerPin
		requirePinned bool
		inline        bool
		body          []byte
		reason        string
		warns         bool
	}{
		{"unpinned, required: a malformed InRelease", []SignerPin{pin(`^other\.example\.org$`)}, true, true, malformed, "unpinned_suite", true},
		{"unpinned, required: a body not clearsigned", nil, true, true, release, "unpinned_suite", true},
		{"unpinned, required: a well-formed InRelease", nil, true, true, good, "unpinned_suite", true},
		{"unpinned, required: a malformed Release.gpg", nil, true, false, detMalformed, "unpinned_suite", true},
		{"pinned, required: a malformed InRelease", []SignerPin{pin(`^archive\.example\.com$`)}, true, true, malformed, "malformed_clearsign", false},
		{"pinned, required: a malformed Release.gpg", []SignerPin{pin(`^archive\.example\.com$`)}, true, false, detMalformed, "malformed_signature", false},
		{"unpinned, not required: a malformed InRelease", nil, false, true, malformed, "malformed_clearsign", false},
		{"pinned, required: a well-formed InRelease", []SignerPin{pin(`^archive\.example\.com$`)}, true, true, good, "", false},
	} {
		var logs bytes.Buffer
		v, err := NewVerifier(VerifierConfig{
			Keyring: keyring, Pins: tc.pins, RequireSignature: true, RequirePinned: tc.requirePinned,
			Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if tc.inline {
			_, err = v.VerifyInline(context.Background(), newSuite(), tc.body)
		} else {
			_, err = v.VerifyDetached(context.Background(), newSuite(), release, tc.body)
		}
		if got := ClassifyVerifyErr(err); got != tc.reason {
			t.Errorf("%s: %v (reason %q); want reason %q", tc.name, err, got, tc.reason)
		}
		if tc.reason == "unpinned_suite" && !errors.Is(err, ErrUnpinnedSuite) {
			t.Errorf("%s: %v; want ErrUnpinnedSuite", tc.name, err)
		}
		if warned := strings.Contains(logs.String(), "adoption_unpinned_suite"); warned != tc.warns {
			t.Errorf("%s: the adoption_unpinned_suite WARN logged %v, want %v", tc.name, warned, tc.warns)
		}
	}
}

// TestVerifier_ChecksSignaturesAtItsClock: the verifier's test-only clock
// (Verifier.now; NewVerifier leaves it unset, the wall clock) is the time
// S2 checks a signature at, in both of its branches: E's good signature,
// made while E was valid, is refused now (E has expired: ErrKeyExpired) and
// counted at E's creation, with subpacket 33 or without it. (S1 passes an
// expired key's signature at any time, as gpgv's EXPKEYSIG.)
func TestVerifier_ChecksSignaturesAtItsClock(t *testing.T) {
	e := newExpiredEntity(t, "E", "e@example.com")
	created := e.PrimaryKey.CreationTime
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, e), RequireSignature: true, AllowShortKeyID: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(fakeReleasePlaintext)
	for name, pkt := range map[string][]byte{
		"with subpacket 33":    signedAt(t, e, msg, packet.SigTypeText, crypto.SHA256, true, created, 0),
		"without subpacket 33": signedAt(t, e, msg, packet.SigTypeText, crypto.SHA256, false, created, 0),
	} {
		body := inlineWithSignatures(t, pkt)
		v.now = nil
		if _, err := v.VerifyInline(context.Background(), newSuite(), body); !errors.Is(err, pgperrors.ErrKeyExpired) {
			t.Errorf("%s, at the wall clock: %v; want ErrKeyExpired (E has expired)", name, err)
		}
		v.now = func() time.Time { return created }
		if _, err := v.VerifyInline(context.Background(), newSuite(), body); err != nil {
			t.Errorf("%s, at E's creation: %v; want it adopted", name, err)
		}
	}
}
