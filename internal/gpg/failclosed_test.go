package gpg

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
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
			// The genuine envelope still verifies, to the signed text, in
			// every mode (review M-1).
			if out, err := v.VerifyInline(context.Background(), newSuite(), good); err != nil || !bytes.Equal(out, []byte(fakeReleasePlaintext)) {
				t.Errorf("require=%v accept_any=%v: genuine envelope: out=%q err=%v, want the signed text %q",
					require, anyFlag, out, err, fakeReleasePlaintext)
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
	// The suite is tracked, so the closing check reads a row (deferred
	// minor A-52: it passed vacuously on ErrNotFound).
	if err := c.SeedSuiteForTest(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range [][]byte{append([]byte{}, good[:end]...), []byte(fakeReleasePlaintext)} {
		if err := ad.Run(context.Background(), suite, body, "", ""); !errors.Is(err, freshness.ErrAdoptionGPGFailed) {
			t.Fatalf("Run(%q...) err = %v, want ErrAdoptionGPGFailed", body[:20], err)
		}
	}
	if cov, err := c.HostCurrentSnapshotsCoverage(context.Background(), "http", "h"); err != nil || len(cov) != 0 {
		t.Fatalf("strict-mode participants = %v (%v), want none", cov, err)
	}
	if sf, err := c.GetSuiteFreshness(context.Background(), "http", "h", "/r/dists/x"); err != nil || sf.CurrentSnapshotID != nil {
		t.Fatalf("suite row = %+v (err %v), want the row with no current snapshot", sf, err)
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

// Ruling R-D2 (phase D): adoption accepts exactly what apt's first-line
// rule accepts. apt rejects an InRelease whose first line is not the
// clearsign marker — a leading blank line or leading whitespace included
// (apt-pkg/acquire-item.cc pkgAcqMetaClearSig::VerifyDone →
// StartsWithGPGClearTextSignature in apt-pkg/contrib/fileutl.cc, which
// strips only trailing whitespace with _strrstrip, apt-pkg/contrib/
// strutl.cc; spec §3's probes of real apt agree) — so the verifier refuses
// it too, as malformed_clearsign, and the handler's sniff agrees. Trailing
// blanks on the marker line are stripped by apt and accepted here.
func TestVerifyInline_FollowsAptsFirstLineRule(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	good := clearsignWith(t, signer, []byte(fakeReleasePlaintext))
	for name, prefix := range map[string]string{
		"leading newline": "\n",
		"leading CRLF":    "\r\n",
		"leading spaces":  "  ",
		"leading tab":     "\t",
		"leading BOM":     "\xef\xbb\xbf",
	} {
		body := append([]byte(prefix), good...)
		_, err := v.VerifyInline(context.Background(), newSuite(), body)
		if !errors.Is(err, ErrMalformedClearsign) || ClassifyVerifyErr(err) != "malformed_clearsign" {
			t.Errorf("%s: err = %v, want malformed_clearsign", name, err)
		}
		if aptpath.SniffInRelease(body) {
			t.Errorf("%s: the handler's sniff accepts a body adoption refuses", name)
		}
	}
	trailing := bytes.Replace(good, []byte(clearsignBeginMarker+"\n"), []byte(clearsignBeginMarker+" \t\r\n"), 1)
	if bytes.Equal(trailing, good) {
		t.Fatal("test premise: the marker line was not rewritten")
	}
	if _, err := v.VerifyInline(context.Background(), newSuite(), trailing); err != nil {
		t.Fatalf("trailing blanks on the marker line: %v, want accepted (apt strips them)", err)
	}
}

// Deferred minor D-6: the accept leg end to end for an envelope whose
// every line ends in CRLF and whose marker line carries trailing blanks —
// DecodableClearsign then hands go-crypto an LF marker line before CRLF
// lines: it verifies, and the handler's sniff accepts it too.
func TestVerifyInline_AcceptsACRLFEnvelopeWithABlankPaddedMarker(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: true, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	crlf := bytes.ReplaceAll(clearsignWith(t, signer, []byte(fakeReleasePlaintext)), []byte("\n"), []byte("\r\n"))
	body := bytes.Replace(crlf, []byte(clearsignBeginMarker+"\r\n"), []byte(clearsignBeginMarker+" \t\r\n"), 1)
	if bytes.Equal(body, crlf) {
		t.Fatal("test premise: the marker line was not rewritten")
	}
	if !aptpath.SniffInRelease(body) {
		t.Fatal("the handler's sniff refuses a body adoption accepts")
	}
	out, err := v.VerifyInline(context.Background(), newSuite(), body)
	// The exact signed text: the CRLF lines decode to the LF text that was
	// signed (review M-1).
	if err != nil || !bytes.Equal(out, []byte(fakeReleasePlaintext)) {
		t.Fatalf("VerifyInline = %q, %v; want the signed text %q", out, err, fakeReleasePlaintext)
	}
}

// Deferred minor A-53: the detached path names an empty keyring too.
func TestVerifyDetached_EmptyKeyringSaysSo(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t), RequireSignature: false, Logger: silentLogger()})
	if err != nil {
		t.Fatal(err)
	}
	rel := []byte(fakeReleasePlaintext)
	_, err = v.VerifyDetached(context.Background(), newSuite(), rel, detachSignWith(t, signer, rel, true))
	if !errors.Is(err, ErrNoUsableSignature) || !strings.Contains(err.Error(), "keyring is empty") {
		t.Fatalf("err = %v, want ErrNoUsableSignature naming the empty keyring", err)
	}
}
