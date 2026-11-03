package gpg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
)

// The 1.0.3 patch: a malformed upstream signature must be an ordinary
// verify error, never a panic. The verifier runs in the freshness adoption
// goroutine, which has no recover in 1.0.x, so a go-crypto panic killed the
// daemon and systemd's restart fetched the same body again (a crash loop).
// I-1 and A-RC3 are the two known go-crypto v1.4.1 panics, both fixed by
// the bump to v1.5.2; the AIDEV-NOTE in verifier.go describes the recover
// wrappers that keep any later go-crypto parse panic out of the goroutine.

// aptShapeTestdata holds the five A-RC3 shapes of the hardening branch's
// apt-shape corpus (fix/gc-and-upstream-hardening,
// internal/gpg/testdata/aptshapes; e2e/apt-oracle/README.md there), byte
// for byte, with the corpus's shared Release (which the detached shape
// signs) and keys.gpg (the public keys A, E, S and O; A signs the good
// packets). All of it is public test data.
const aptShapeTestdata = "testdata/aptshapes"

// readAptShape reads one file of aptShapeTestdata.
func readAptShape(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(aptShapeTestdata, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// aptShapeKeyring is keys.gpg loaded as the hardening branch's table test
// loads it: as an embedded source, so every key in it is trusted.
func aptShapeKeyring(t *testing.T) *Keyring {
	t.Helper()
	k, err := LoadKeyringWithEmbedded(nil, []EmbeddedSource{{Name: "keys.gpg", Data: readAptShape(t, "keys.gpg")}}, silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if k.Size() != 4 {
		t.Fatalf("keys.gpg holds %d keys, want 4 (A, E, S and O)", k.Size())
	}
	return k
}

// verifyNoPanic runs f and fails the test, instead of crashing the test
// binary, if f panics.
func verifyNoPanic(t *testing.T, f func() ([]byte, error)) (out []byte, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	return f()
}

// i1Bodies are I-1's envelopes (security review 2026-09, WS-I): a
// clearsigned message with no plaintext lines, without and with an END
// marker. go-crypto v1.4.1's clearsign.Decode panicked on both (slice
// bounds out of range [:-1], clearsign.go:182). v1.5.2 decodes neither, so
// they take the path of every body clearsign.Decode cannot decode.
var i1Bodies = []struct{ name, body string }{
	{"without END", "-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----"},
	{"with END", "-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----\n-----END PGP SIGNATURE-----\n"},
}

// i1ArmoredBody is the empty-plaintext envelope with a Hash header and an
// armored body (a truncated signature packet), which v1.5.2 does decode,
// to a block with no plaintext: it reaches the signature checks, which
// refuse it. v1.4.1 panicked on it as on i1Bodies.
const i1ArmoredBody = "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----\n"

// TestVerifyInline_I1EmptyPlaintextIsAnErrorNotAPanic: with
// require_signature = true (the default), I-1's envelopes are
// ErrMissingSignature (reason missing_signature), the error 1.0.x gives
// every body clearsign.Decode cannot decode, under accept_any_signer too;
// the armored variant is refused by the signature checks.
func TestVerifyInline_I1EmptyPlaintextIsAnErrorNotAPanic(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	for _, acceptAny := range []bool{false, true} {
		v, err := NewVerifier(VerifierConfig{
			Keyring:          keyring,
			RequireSignature: true,
			AcceptAnySigner:  acceptAny,
			Logger:           silentLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range i1Bodies {
			t.Run(fmt.Sprintf("accept_any_signer=%v/%s", acceptAny, c.name), func(t *testing.T) {
				out, err := verifyNoPanic(t, func() ([]byte, error) {
					return v.VerifyInline(context.Background(), newSuite(), []byte(c.body))
				})
				if !errors.Is(err, ErrMissingSignature) || out != nil {
					t.Errorf("out=%q err=%v; want ErrMissingSignature", out, err)
				}
				if got := ClassifyVerifyErr(err); got != "missing_signature" {
					t.Errorf("reason %q, want missing_signature", got)
				}
			})
		}
		t.Run(fmt.Sprintf("accept_any_signer=%v/armored body", acceptAny), func(t *testing.T) {
			out, err := verifyNoPanic(t, func() ([]byte, error) {
				return v.VerifyInline(context.Background(), newSuite(), []byte(i1ArmoredBody))
			})
			if err == nil || out != nil {
				t.Errorf("out=%q err=%v; want an error", out, err)
			}
			if got := ClassifyVerifyErr(err); got != "crypto_verify_failed" {
				t.Errorf("reason %q (err %v), want crypto_verify_failed", got, err)
			}
		})
	}
}

// TestVerifyInline_I1RequireSignatureFalse_NoPanic: with
// require_signature = false, 1.0.x hands back as unsigned text any body
// clearsign.Decode cannot decode (TestVerifier_MissingSignature_RequireFalse),
// and this patch leaves that mode as it is: I-1's envelopes come back
// verbatim, as every other undecodable envelope does, and adoption then
// refuses them as a Release (parse_failed: no SHA256 block). The armored
// variant decodes and is refused by the signature checks.
//
// AIDEV-NOTE: main removed this text fallback (plan review finding 4);
// 1.0.x keeps it. If a 1.0.x release takes that change, this test changes
// with it.
func TestVerifyInline_I1RequireSignatureFalse_NoPanic(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{
		Keyring:          newKeyring(t, signer),
		RequireSignature: false,
		Logger:           silentLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range i1Bodies {
		t.Run(c.name, func(t *testing.T) {
			out, err := verifyNoPanic(t, func() ([]byte, error) {
				return v.VerifyInline(context.Background(), newSuite(), []byte(c.body))
			})
			if err != nil || !bytes.Equal(out, []byte(c.body)) {
				t.Errorf("out=%q err=%v; want the body verbatim (1.0.x's require_signature = false fallback)", out, err)
			}
		})
	}
	t.Run("armored body", func(t *testing.T) {
		out, err := verifyNoPanic(t, func() ([]byte, error) {
			return v.VerifyInline(context.Background(), newSuite(), []byte(i1ArmoredBody))
		})
		if err == nil || out != nil {
			t.Errorf("out=%q err=%v; want an error", out, err)
		}
	})
}

// TestVerifyInline_EmptyPlaintextUnderAcceptAnySigner: I-1's shape with an
// armored signature that parses (a signed InRelease's own, its signed text
// cut out) decodes under v1.5.2 to a block with no plaintext.
// accept_any_signer's bypass only checks that the signature parses, so the
// verifier passes it with 0 bytes, and adoption's ParseRelease then refuses
// it (no SHA256 block: parse_failed), so nothing is adopted. Without the
// bypass the signature check refuses it: the packet signs other text.
// v1.4.1 panicked on it, as on i1Bodies.
func TestVerifyInline_EmptyPlaintextUnderAcceptAnySigner(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	signed := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	sig := bytes.Index(signed, []byte("-----BEGIN PGP SIGNATURE-----"))
	if sig < 0 {
		t.Fatalf("no signature block in %q", signed)
	}
	body := append([]byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n"), signed[sig:]...)
	for _, acceptAny := range []bool{true, false} {
		v, err := NewVerifier(VerifierConfig{
			Keyring:          keyring,
			RequireSignature: true,
			AcceptAnySigner:  acceptAny,
			Logger:           silentLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := verifyNoPanic(t, func() ([]byte, error) {
			return v.VerifyInline(context.Background(), newSuite(), body)
		})
		if !acceptAny {
			if got := ClassifyVerifyErr(err); err == nil || got != "crypto_verify_failed" {
				t.Errorf("accept_any_signer=false: out=%q err=%v (reason %q); want crypto_verify_failed", out, err, got)
			}
			continue
		}
		if err != nil || len(out) != 0 {
			t.Fatalf("accept_any_signer=true: out=%q err=%v; want accepted with 0 bytes", out, err)
		}
		if members, err := freshness.ParseRelease(out); err == nil {
			t.Errorf("ParseRelease accepted the empty plaintext (%d members); want it refused (no SHA256 block)", len(members))
		}
	}
}

// arc3Shapes are A-RC3's shapes (the hardening corpus's batch fx18, q10_):
// a signature packet from B, a key no client has, whose hashed area holds
// an exportable-certification subpacket (type 4; critical in _crit_) with
// no value octet, alone or beside A's good packet. go-crypto v1.4.1 read
// that empty body (subpacket[0], signature.go:458) and panicked: key-free,
// an unknown key's packet, alone or ahead of a good one, was enough. v1.5.0
// fixed I-1, v1.5.1 fixed A-RC3: it returns a StructuralError for this
// subpacket instead.
//
// want pins 1.0.3's verdict: "" accepts, else the refusal's reason. The
// verifier reads packets in order and stops at the first one go-crypto
// cannot parse, as 1.0.x does for every malformed packet, so B's packet
// first refuses the anchor even with A's good packet after it. apt (all
// six images of the corpus's oracle) accepts every shape but _only:
// _plus_good is a strict deviation, the safe direction (the suite keeps
// its previous snapshot). 1.0.2 panicked on _only and _plus_good, and
// accepted the other three (A's packet first verified before B's was
// read).
var arc3Shapes = []struct {
	file   string
	inline bool
	want   string
}{
	{"q10_good_plus_unknownB_hashed_t4_empty.InRelease", true, ""},
	{"q10_good_plus_unknownB_hashed_crit_t4_empty.InRelease", true, ""},
	{"q10_d_good_plus_unknownB_hashed_t4_empty.Release.gpg", false, ""},
	{"q10_unknownB_hashed_t4_empty_only.InRelease", true, "crypto_verify_failed"},
	{"q10_unknownB_hashed_t4_empty_plus_good.InRelease", true, "crypto_verify_failed"},
}

// TestVerifier_ARC3ShapesNoPanic runs A-RC3's shapes with keys.gpg as the
// host keyring and the defaults (require_signature, allow_short_keyid),
// and under accept_any_signer, whose structural checks parse the same
// packets: same verdicts, no panic. An accepted shape returns the Release
// it signs (the corpus's shared Release, also the InRelease's cleartext).
func TestVerifier_ARC3ShapesNoPanic(t *testing.T) {
	keyring := aptShapeKeyring(t)
	release := readAptShape(t, "Release")
	for _, acceptAny := range []bool{false, true} {
		v, err := NewVerifier(VerifierConfig{
			Keyring:          keyring,
			RequireSignature: true,
			AllowShortKeyID:  true,
			AcceptAnySigner:  acceptAny,
			Logger:           silentLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range arc3Shapes {
			t.Run(fmt.Sprintf("accept_any_signer=%v/%s", acceptAny, s.file), func(t *testing.T) {
				body := readAptShape(t, s.file)
				out, err := verifyNoPanic(t, func() ([]byte, error) {
					if s.inline {
						return v.VerifyInline(context.Background(), newSuite(), body)
					}
					return v.VerifyDetached(context.Background(), newSuite(), release, body)
				})
				switch {
				case s.want == "" && err != nil:
					t.Errorf("refused (%v, reason %s); want accepted", err, ClassifyVerifyErr(err))
				case s.want == "" && !bytes.Equal(out, release):
					t.Errorf("accepted but returned %q, want the shared Release", out)
				case s.want != "" && (err == nil || out != nil):
					t.Errorf("out=%q err=%v; want refused (%s)", out, err, s.want)
				case s.want != "" && ClassifyVerifyErr(err) != s.want:
					t.Errorf("reason %s (%v), want %s", ClassifyVerifyErr(err), err, s.want)
				}
			})
		}
	}
}

// setSeam points one of verifier.go's go-crypto entry points at a stand-in
// for the rest of the test.
//
// AIDEV-NOTE: the seams are package variables, so a test that sets one must
// never call t.Parallel, nor run beside a parallel test of this package.
func setSeam[T any](t *testing.T, seam *T, v T) {
	t.Helper()
	old := *seam
	*seam = v
	t.Cleanup(func() { *seam = old })
}

// panicReader stands in for a go-crypto armor body whose lazy decode
// panics on read.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("test: armor body read") }

// TestVerifier_RecoversGoCryptoPanics makes each go-crypto call the
// verifier wraps panic in turn (the seams in verifier.go), on inputs that
// verify without the panic (the control rows), and holds the verifier to
// the AIDEV-NOTE above decodeClearsigned: the panic comes back as the
// error its path gives a malformed body, with the same reason, in every
// mode that reaches the call, and never escapes. The one exception is
// accept_any_signer's signer log, which is best-effort: a panic there
// drops the signer from the log line, as a parse error does, and the
// anchor is still accepted.
func TestVerifier_RecoversGoCryptoPanics(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	keyring := newKeyring(t, signer)
	inline := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	shortKeyID := clearsignWithoutIssuerFingerprint(t, signer, []byte(fakeReleasePlaintext))
	release := []byte(fakeReleasePlaintext)
	armoredSig := detachSignWith(t, signer, release, true)
	binarySig := detachSignWith(t, signer, release, false)

	verifyInline := func(body []byte) func(*Verifier) ([]byte, error) {
		return func(v *Verifier) ([]byte, error) { return v.VerifyInline(context.Background(), newSuite(), body) }
	}
	verifyDetached := func(sig []byte) func(*Verifier) ([]byte, error) {
		return func(v *Verifier) ([]byte, error) {
			return v.VerifyDetached(context.Background(), newSuite(), release, sig)
		}
	}

	type mode struct {
		name             string
		requireSignature bool
		acceptAny        bool
	}
	strict := mode{"strict", true, false}
	unsignedOK := mode{"require_signature=false", false, false}
	bypass := mode{"accept_any_signer", true, true}

	panicNext := func(*packet.Reader) (packet.Packet, error) { panic("test: packet.Read") }
	cases := []struct {
		name   string
		seam   func(t *testing.T)
		verify func(*Verifier) ([]byte, error)
		modes  []mode
		want   string // ClassifyVerifyErr of the recovered panic's error
	}{
		{
			name: "clearsign.Decode",
			seam: func(t *testing.T) {
				setSeam(t, &clearsignDecode, func([]byte) (*clearsign.Block, []byte) { panic("test: clearsign.Decode") })
			},
			verify: verifyInline(inline),
			modes:  []mode{strict, unsignedOK, bypass},
			want:   "missing_signature",
		},
		{
			name: "clearsigned armor body read",
			seam: func(t *testing.T) {
				setSeam(t, &clearsignDecode, func(b []byte) (*clearsign.Block, []byte) {
					block, rest := clearsign.Decode(b)
					block.ArmoredSignature.Body = panicReader{}
					return block, rest
				})
			},
			verify: verifyInline(inline),
			modes:  []mode{strict, unsignedOK, bypass},
			want:   "crypto_verify_failed",
		},
		{
			name: "armor.Decode",
			seam: func(t *testing.T) {
				setSeam(t, &armorDecode, func(io.Reader) (*armor.Block, error) { panic("test: armor.Decode") })
			},
			verify: verifyDetached(armoredSig),
			modes:  []mode{strict, bypass},
			want:   "crypto_verify_failed",
		},
		{
			name: "detached armor body read",
			seam: func(t *testing.T) {
				setSeam(t, &armorDecode, func(r io.Reader) (*armor.Block, error) {
					block, err := armor.Decode(r)
					if err != nil {
						return nil, err
					}
					block.Body = panicReader{}
					return block, nil
				})
			},
			verify: verifyDetached(armoredSig),
			modes:  []mode{strict, bypass},
			want:   "crypto_verify_failed",
		},
		{
			name:   "packet.Read, inline",
			seam:   func(t *testing.T) { setSeam(t, &packetReaderNext, panicNext) },
			verify: verifyInline(inline),
			modes:  []mode{strict, unsignedOK, bypass},
			want:   "crypto_verify_failed",
		},
		{
			name:   "packet.Read, detached",
			seam:   func(t *testing.T) { setSeam(t, &packetReaderNext, panicNext) },
			verify: verifyDetached(binarySig),
			modes:  []mode{strict, bypass},
			want:   "crypto_verify_failed",
		},
		{
			name: "VerifyDetachedSignature, inline",
			seam: func(t *testing.T) {
				setSeam(t, &verifyDetachedSignature, func(openpgp.KeyRing, io.Reader, io.Reader, *packet.Config) (*packet.Signature, *openpgp.Entity, error) {
					panic("test: VerifyDetachedSignature")
				})
			},
			verify: verifyInline(inline),
			modes:  []mode{strict, unsignedOK},
			want:   "crypto_verify_failed",
		},
		{
			// The short-keyid fallback verifies through verifyAgainst.
			name: "VerifyDetachedSignature, inline, short keyid",
			seam: func(t *testing.T) {
				setSeam(t, &verifyDetachedSignature, func(openpgp.KeyRing, io.Reader, io.Reader, *packet.Config) (*packet.Signature, *openpgp.Entity, error) {
					panic("test: VerifyDetachedSignature")
				})
			},
			verify: verifyInline(shortKeyID),
			modes:  []mode{strict},
			want:   "crypto_verify_failed",
		},
		{
			name: "VerifyDetachedSignature, detached",
			seam: func(t *testing.T) {
				setSeam(t, &verifyDetachedSignature, func(openpgp.KeyRing, io.Reader, io.Reader, *packet.Config) (*packet.Signature, *openpgp.Entity, error) {
					panic("test: VerifyDetachedSignature")
				})
			},
			verify: verifyDetached(armoredSig),
			modes:  []mode{strict},
			want:   "crypto_verify_failed",
		},
	}
	for _, c := range cases {
		for _, m := range c.modes {
			v, err := NewVerifier(VerifierConfig{
				Keyring:          keyring,
				RequireSignature: m.requireSignature,
				AllowShortKeyID:  true,
				AcceptAnySigner:  m.acceptAny,
				Logger:           silentLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Run(c.name+"/"+m.name+"/control", func(t *testing.T) {
				if out, err := verifyNoPanic(t, func() ([]byte, error) { return c.verify(v) }); err != nil || len(out) == 0 {
					t.Fatalf("without the panic: out=%q err=%v; want accepted", out, err)
				}
			})
			t.Run(c.name+"/"+m.name, func(t *testing.T) {
				c.seam(t)
				out, err := verifyNoPanic(t, func() ([]byte, error) { return c.verify(v) })
				if !errors.Is(err, errGoCryptoPanicked) || out != nil {
					t.Fatalf("out=%q err=%v; want an error from the recovered panic", out, err)
				}
				if got := ClassifyVerifyErr(err); got != c.want {
					t.Errorf("reason %s (err %v), want %s", got, err, c.want)
				}
			})
		}
	}

	// accept_any_signer's signer log reads the signature a second time
	// (extractSignerInfo), after requireParseableSignature's read passed.
	t.Run("packet.Read, accept_any_signer signer log", func(t *testing.T) {
		var logs bytes.Buffer
		v, err := NewVerifier(VerifierConfig{
			Keyring:          keyring,
			RequireSignature: true,
			AcceptAnySigner:  true,
			Logger:           slog.New(slog.NewTextHandler(&logs, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		setSeam(t, &packetReaderNext, func(r *packet.Reader) (packet.Packet, error) {
			if calls++; calls == 1 {
				return r.Next()
			}
			panic("test: packet.Read")
		})
		out, err := verifyNoPanic(t, func() ([]byte, error) { return v.VerifyInline(context.Background(), newSuite(), inline) })
		if err != nil || len(out) == 0 {
			t.Fatalf("out=%q err=%v; want accepted (the signer log is best-effort)", out, err)
		}
		if calls != 2 {
			t.Errorf("%d packet reads, want 2 (the structural check, then the signer log)", calls)
		}
		if line := logs.String(); !strings.Contains(line, "adoption_unverified_signer") || strings.Contains(line, " signer=") {
			t.Errorf("log %q: want adoption_unverified_signer without a signer", line)
		}
	})
}
