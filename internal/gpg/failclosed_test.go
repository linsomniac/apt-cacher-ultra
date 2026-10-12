package gpg

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Spec C5 (rev 4), plan review finding 4: only a decodable clearsigned
// envelope is inline metadata, whatever require_signature says. A body
// without the marker is ErrMissingSignature; a body with the marker that
// does not decode is ErrMalformedClearsign; the two stay distinct.
func TestVerifyInline_FailsClosedWhateverRequireSignature(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	end := bytes.LastIndex(good, []byte("-----END PGP SIGNATURE-----"))
	sigStart := bytes.Index(good, []byte("-----BEGIN PGP SIGNATURE-----"))
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"plain text", []byte(fakeReleasePlaintext), ErrMissingSignature},
		{"marker, no END line", append([]byte{}, good[:end]...), ErrMalformedClearsign},
		{"marker, no signature frame", append([]byte{}, good[:sigStart]...), ErrMalformedClearsign},
		{"marker only", []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\nOrigin: x\n"), ErrMalformedClearsign},
		{"zero plaintext (I-1)", []byte("-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----"), ErrMalformedClearsign},
	}
	for _, require := range []bool{true, false} {
		for _, anyFlag := range []bool{false, true} {
			v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: require,
				AcceptAnySigner: anyFlag, Logger: silentLogger()})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cases {
				out, err := v.VerifyInline(context.Background(), newSuite(), c.body)
				if !errors.Is(err, c.want) || out != nil {
					t.Errorf("require=%v accept_any=%v %s: out=%q err=%v, want %v", require, anyFlag, c.name, out, err, c.want)
				}
				if c.want == ErrMissingSignature && errors.Is(err, ErrMalformedClearsign) {
					t.Errorf("%s: missing and malformed must stay distinct", c.name)
				}
			}
			// The genuine envelope still verifies in every mode.
			if out, err := v.VerifyInline(context.Background(), newSuite(), good); err != nil || len(out) == 0 {
				t.Errorf("require=%v accept_any=%v: genuine envelope rejected: %v", require, anyFlag, err)
			}
		}
	}
}

// staticAdoptionFetcher answers every member fetch with 404, so a test
// adoption can only fail before or at member download.
type staticAdoptionFetcher struct{}

func (staticAdoptionFetcher) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	return &fetch.FetchResult{Status: 404}, nil
}

// End to end through a real Adopter and cache: with require_signature =
// false, marker-present bodies that do not decode create no snapshot, no
// vouching row and no strict-mode participant.
func TestAdopter_MalformedEnvelopeCreatesNoSnapshot(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: false, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ad, err := freshness.NewAdopter(freshness.AdoptionConfig{Cache: c, Fetcher: staticAdoptionFetcher{}, Verifier: v,
		HostLimiter: hostsem.New(4), Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	end := bytes.LastIndex(good, []byte("-----END PGP SIGNATURE-----"))
	suite := freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	for _, body := range [][]byte{append([]byte{}, good[:end]...), []byte(fakeReleasePlaintext)} {
		if err := ad.Run(context.Background(), suite, body, "", ""); !errors.Is(err, freshness.ErrAdoptionGPGFailed) {
			t.Fatalf("Run(%q...) err = %v, want ErrAdoptionGPGFailed", body[:20], err)
		}
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(context.Background(), "http", "h"); err != nil || len(cov) != 0 {
		t.Fatalf("strict-mode participants = %v (%v), want none", cov, err)
	}
	if sf, err := c.GetSuiteFreshness(context.Background(), "http", "h", "/r/dists/x"); err == nil && sf.CurrentSnapshotID != nil {
		t.Fatalf("a current snapshot was created: %+v", sf)
	}
}

func TestVerifyInline_EmptyKeyringSaysSo(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t), RequireSignature: false, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.VerifyInline(context.Background(), newSuite(), clearsignWith(t, signer, []byte(fakeReleasePlaintext)))
	if !errors.Is(err, ErrNoUsableSignature) || !strings.Contains(err.Error(), "keyring is empty") {
		t.Fatalf("err = %v, want ErrNoUsableSignature naming the empty keyring", err)
	}
}
