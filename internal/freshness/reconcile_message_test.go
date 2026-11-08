package freshness_test

// Reconcile reads a stored InRelease as adoption read it (the re-review's
// V-RI1): the verifier returns M, layer T's message (internal/apttext), the
// adopter parses its members from it, and reconcile re-derives them from
// the stored anchor; the two must agree on every anchor the verifier
// adopts. An external test: it runs internal/gpg's verifier, which imports
// this package.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/gpg"
)

// corpusDir is internal/gpg's apt-shape corpus (e2e/apt-oracle/README.md).
const corpusDir = "../gpg/testdata/aptshapes"

// corpusVerifier is the verifier internal/gpg's table test holds to apt:
// the corpus' keys.gpg as the host keyring, the defaults.
func corpusVerifier(t *testing.T) *gpg.Verifier {
	t.Helper()
	keys, err := os.ReadFile(filepath.Join(corpusDir, "keys.gpg"))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	keyring, err := gpg.LoadKeyringWithEmbedded(nil, []gpg.EmbeddedSource{{Name: "keys.gpg", Data: keys}}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	v, err := gpg.NewVerifier(gpg.VerifierConfig{Keyring: keyring, RequireSignature: true, AllowShortKeyID: true, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var suite = freshness.SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example.com", SuitePath: "/dists/test"}

// reconcilesAsAdopted fails t unless reconcile reads body, an InRelease the
// verifier adopted to m, to m, with the same members.
func reconcilesAsAdopted(t *testing.T, name string, body, m []byte) {
	t.Helper()
	got, err := freshness.InReleaseMessage(body)
	if err != nil {
		t.Errorf("%s: adopted, reconcile cannot read it: %v", name, err)
		return
	}
	if !bytes.Equal(got, m) {
		t.Errorf("%s: reconcile reads %q, adoption %q", name, got, m)
		return
	}
	adopted, aerr := freshness.ParseRelease(m)
	again, rerr := freshness.ParseRelease(got)
	if (aerr == nil) != (rerr == nil) || !reflect.DeepEqual(adopted, again) {
		t.Errorf("%s: members %v (%v) at adoption, %v (%v) at reconcile", name, adopted, aerr, again, rerr)
	}
}

// TestReconcile_ReadsEveryAdoptedCorpusAnchorAsAdoptionDid: every corpus
// InRelease the verifier adopts reconciles to the M it adopted (the
// re-review measured 105 of them, of which go-crypto's decoder, which
// reconcile used, could not read 6 and read 2 otherwise).
func TestReconcile_ReadsEveryAdoptedCorpusAnchorAsAdoptionDid(t *testing.T) {
	v := corpusVerifier(t)
	files, err := filepath.Glob(filepath.Join(corpusDir, "*.InRelease"))
	if err != nil {
		t.Fatal(err)
	}
	adopted := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		m, err := v.VerifyInline(context.Background(), suite, body)
		if err != nil {
			continue
		}
		adopted++
		reconcilesAsAdopted(t, strings.TrimSuffix(filepath.Base(f), ".InRelease"), body, m)
	}
	if adopted < 100 {
		t.Fatalf("test premise: %d corpus InRelease rows adopted, want the corpus' 100 and more", adopted)
	}
}

// TestReconcile_ReadsTheKeyFreeEditsAsAdoptionDid: the re-review's two
// key-free edits of x7i_control_good that every apt accepts and the
// verifier adopts — a cleartext "Comment:" header, and three CRs after
// "SHA256:" — reconcile to the adopted M; go-crypto's decoder refused the
// first ("not a valid clearsigned message") and lost the second's SHA256
// block ("no SHA256 or SHA512 block"), so reconcile failed on every tick.
func TestReconcile_ReadsTheKeyFreeEditsAsAdoptionDid(t *testing.T) {
	v := corpusVerifier(t)
	ctl, err := os.ReadFile(filepath.Join(corpusDir, "x7i_control_good.InRelease"))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string][2]string{
		"c_cleartext_comment": {"Hash: SHA256\n", "Hash: SHA256\nComment: x\n"},
		"c_sha256_crcrcr":     {"\nSHA256:\n", "\nSHA256:\r\r\r\n"},
	} {
		if bytes.Count(ctl, []byte(edit[0])) != 1 {
			t.Fatalf("%s: test premise: %q is not once in the control", name, edit[0])
		}
		body := bytes.Replace(ctl, []byte(edit[0]), []byte(edit[1]), 1)
		m, err := v.VerifyInline(context.Background(), suite, body)
		if err != nil {
			t.Fatalf("%s: test premise: the verifier refuses it: %v", name, err)
		}
		members, err := freshness.ParseRelease(m)
		if err != nil || len(members) == 0 {
			t.Fatalf("%s: test premise: the adopted M declares %d members (%v)", name, len(members), err)
		}
		reconcilesAsAdopted(t, name, body, m)
	}
}
