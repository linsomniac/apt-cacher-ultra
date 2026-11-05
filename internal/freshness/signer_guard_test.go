package freshness

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// The signer downgrade guard's own decisions (signer_guard.go), with a
// fake SignerVerifier: what it reads, when it stands down (rulings G2, G6),
// the unsigned candidate (G1), and the refusal's classification (G5). The
// guard with internal/gpg's verifier is signer_guard_gpg_test.go.

// signersVerifier passes anchors through, as passThroughVerifier does, and
// reports for each the Signers its bytes are labelled with: an anchor whose
// signed text holds "strong:" has that strong signer; failCurrent makes the
// verification of the anchors it names fail. It counts its verifications.
type signersVerifier struct {
	calls atomic.Int32
	fail  map[string]bool
}

func (v *signersVerifier) signersOf(b []byte) Signers {
	if i := bytes.Index(b, []byte("strong:")); i >= 0 {
		fp := strings.Fields(string(b[i+len("strong:"):]))[0]
		return Signers{Strong: fp, Counted: []string{fp}}
	}
	return Signers{Counted: []string{"WEAK (not strong: a test key)"}}
}

func (v *signersVerifier) VerifyInline(ctx context.Context, s SuiteRef, b []byte) ([]byte, error) {
	m, _, err := v.VerifyInlineSigners(ctx, s, b)
	return m, err
}

func (v *signersVerifier) VerifyDetached(ctx context.Context, s SuiteRef, r, g []byte) ([]byte, error) {
	m, _, err := v.VerifyDetachedSigners(ctx, s, r, g)
	return m, err
}

func (v *signersVerifier) VerifyInlineSigners(_ context.Context, _ SuiteRef, b []byte) ([]byte, Signers, error) {
	v.calls.Add(1)
	if v.fail[string(b)] {
		return nil, Signers{}, errors.New("a test refusal")
	}
	return b, v.signersOf(b), nil
}

func (v *signersVerifier) VerifyDetachedSigners(_ context.Context, _ SuiteRef, r, g []byte) ([]byte, Signers, error) {
	v.calls.Add(1)
	if v.fail[string(g)] {
		return nil, Signers{}, errors.New("a test refusal")
	}
	return r, v.signersOf(g), nil
}

// guardEnv is an adoption env whose adopter verifies with a
// signersVerifier and logs JSON into logs.
type guardEnv struct {
	*adoptionTestEnv
	v    *signersVerifier
	logs *bytes.Buffer
}

func newGuardEnv(t *testing.T) *guardEnv {
	t.Helper()
	env := newAdoptionTestEnv(t)
	v := &signersVerifier{fail: map[string]bool{}}
	var logs bytes.Buffer
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: v,
		HostLimiter: hostsem.New(8), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	env.adopter = ad
	return &guardEnv{adoptionTestEnv: env, v: v, logs: &logs}
}

// adopt runs one inline adoption of a Release labelled label whose signed
// text says strong:<fp> when fp is set.
func (g *guardEnv) adopt(t *testing.T, label, fp string) ([]byte, error) {
	t.Helper()
	return g.adoptDated(t, label, fp, 0)
}

// adoptDated is adopt with the Release dated date (0: no Date field).
func (g *guardEnv) adoptDated(t *testing.T, label, fp string, date int64) ([]byte, error) {
	t.Helper()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	g.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	rel := datedRelease(label, date, map[string][]byte{"main/binary-amd64/Packages": pkgs})
	if fp != "" {
		rel = append([]byte("X-Note: strong:"+fp+"\n"), rel...)
	}
	body := asInRelease(rel)
	return body, g.adopter.Run(context.Background(), g.suite, body, "", "")
}

func (g *guardEnv) prior(t *testing.T) *cache.SuiteSnapshot {
	t.Helper()
	snap, err := g.adopter.priorSnapshot(context.Background(), g.suite)
	if err != nil || snap == nil {
		t.Fatalf("prior snapshot: %v, %v", snap, err)
	}
	return snap
}

// TestSignerGuard_RefusesBeforeAnyMemberDownload: a candidate without a
// strong signer after a strong current anchor is refused, with the WARN,
// before any member is fetched; the current snapshot keeps serving.
func TestSignerGuard_RefusesBeforeAnyMemberDownload(t *testing.T) {
	g := newGuardEnv(t)
	if _, err := g.adopt(t, "v1", "AAAA"); err != nil {
		t.Fatal(err)
	}
	cur := currentSnapshotID(t, g.adoptionTestEnv)
	calls := g.fetcher.calls.Load()
	_, err := g.adopt(t, "v2", "")
	if !errors.Is(err, ErrAdoptionSignerDowngrade) {
		t.Fatalf("err = %v, want ErrAdoptionSignerDowngrade", err)
	}
	if got := g.fetcher.calls.Load(); got != calls {
		t.Fatalf("member fetches during the refused adoption = %d, want 0", got-calls)
	}
	if got := currentSnapshotID(t, g.adoptionTestEnv); got != cur {
		t.Fatalf("current snapshot moved from %d to %d", cur, got)
	}
	if !strings.Contains(g.logs.String(), `"level":"WARN","msg":"adoption_signer_downgrade"`) ||
		!strings.Contains(g.logs.String(), `"current_strong_signer":"AAAA"`) {
		t.Fatalf("no WARN adoption_signer_downgrade naming AAAA:\n%s", g.logs.String())
	}
}

// TestSignerGuard_ARollbackStaysRollbackRefused (the task review's 9d-M1):
// a flapping mirror serving an adopted, displaced weak-only anchor again
// after a strong current one, undated or with the same Date (so the Date
// rule passes it), is refused rollback_refused, as before Task 9d, not
// signer_downgrade: the guard runs after Step 4's rollback check. The
// current snapshot is kept and no downgrade WARN is logged.
func TestSignerGuard_ARollbackStaysRollbackRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		date int64
	}{{"undated", 0}, {"same Date", time.Now().Add(-time.Hour).Unix()}} {
		t.Run(c.name, func(t *testing.T) {
			g := newGuardEnv(t)
			if _, err := g.adoptDated(t, "v1", "", c.date); err != nil {
				t.Fatalf("v1, weak: %v", err)
			}
			if _, err := g.adoptDated(t, "v2", "AAAA", c.date); err != nil {
				t.Fatalf("v2, strong: %v", err)
			}
			cur := currentSnapshotID(t, g.adoptionTestEnv)
			_, err := g.adoptDated(t, "v1", "", c.date)
			if !errors.Is(err, ErrAdoptionRollbackRefused) || errors.Is(err, ErrAdoptionSignerDowngrade) {
				t.Fatalf("v1 again: %v; want ErrAdoptionRollbackRefused, not signer_downgrade", err)
			}
			if got := currentSnapshotID(t, g.adoptionTestEnv); got != cur {
				t.Fatalf("current snapshot moved from %d to %d", cur, got)
			}
			if strings.Contains(g.logs.String(), `"msg":"adoption_signer_downgrade"`) {
				t.Fatalf("a rollback logged adoption_signer_downgrade:\n%s", g.logs.String())
			}
		})
	}
}

// TestSignerGuard_ACandidateWithAStrongSignerSkipsTheReread: the current
// anchor is verified again only when the candidate has no strong signer.
func TestSignerGuard_ACandidateWithAStrongSignerSkipsTheReread(t *testing.T) {
	g := newGuardEnv(t)
	if _, err := g.adopt(t, "v1", "AAAA"); err != nil {
		t.Fatal(err)
	}
	before := g.v.calls.Load()
	if _, err := g.adopt(t, "v2", "BBBB"); err != nil {
		t.Fatalf("v2 by another strong signer: %v", err)
	}
	if got := g.v.calls.Load() - before; got != 1 {
		t.Fatalf("%d verifications, want 1 (the candidate's alone)", got)
	}
}

// TestSignerGuard_StandsDownOnItsOwnErrors (ruling G2): an anchor blob
// that holds other bytes, one over the read cap, and one the verifier now
// refuses each make the guard stand down, logged at INFO with the reason;
// the candidate is adopted.
func TestSignerGuard_StandsDownOnItsOwnErrors(t *testing.T) {
	for _, c := range []struct {
		reason  string
		corrupt func(t *testing.T, g *guardEnv, anchor []byte, h string)
	}{
		{guardSkipHashMismatch, func(t *testing.T, g *guardEnv, _ []byte, h string) {
			if err := os.WriteFile(g.cache.BlobPath(h), []byte("other bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{guardSkipAnchorTooLarge, func(t *testing.T, _ *guardEnv, _ []byte, _ string) {
			prev := signerGuardAnchorCap
			signerGuardAnchorCap = 16
			t.Cleanup(func() { signerGuardAnchorCap = prev })
		}},
		{guardSkipUnverified, func(_ *testing.T, g *guardEnv, anchor []byte, _ string) { g.v.fail[string(anchor)] = true }},
		{guardSkipAnchorMissing, func(t *testing.T, g *guardEnv, _ []byte, h string) {
			if err := os.Remove(g.cache.BlobPath(h)); err != nil {
				t.Fatal(err)
			}
		}},
		// The task review's 9d-M3: a pool file that opens but cannot be
		// read (a directory in its place: EISDIR, neither missing nor too
		// large).
		{guardSkipAnchorUnreadable, func(t *testing.T, g *guardEnv, _ []byte, h string) {
			if err := os.Remove(g.cache.BlobPath(h)); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(g.cache.BlobPath(h), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.reason, func(t *testing.T) {
			g := newGuardEnv(t)
			anchor, err := g.adopt(t, "v1", "AAAA")
			if err != nil {
				t.Fatal(err)
			}
			c.corrupt(t, g, anchor, *g.prior(t).SingleAnchorHash)
			if _, err := g.adopt(t, "v2", ""); err != nil {
				t.Fatalf("v2: %v; want it adopted, the guard standing down", err)
			}
			if !strings.Contains(g.logs.String(), `"level":"INFO","msg":"adoption_signer_guard_skipped"`) ||
				!strings.Contains(g.logs.String(), `"reason":"`+c.reason+`"`) {
				t.Fatalf("no INFO adoption_signer_guard_skipped with reason %s:\n%s", c.reason, g.logs.String())
			}
		})
	}
}

// TestSignerGuard_StandsDownWithoutWhatItCompares: no current snapshot (a
// first adoption, or after /forget: ruling G6), a current unsigned anchor,
// a form it does not know (logged), and a Verifier that reports no signers
// (not a SignerVerifier): each stands down; an unsigned candidate (no
// signers) after a strong current anchor is refused (ruling G1).
func TestSignerGuard_StandsDownWithoutWhatItCompares(t *testing.T) {
	ctx := context.Background()
	g := newGuardEnv(t)
	if err := g.adopter.signerGuard(ctx, g.suite, nil, Signers{}); err != nil {
		t.Fatalf("no current snapshot: %v", err)
	}
	if _, err := g.adopt(t, "v1", "AAAA"); err != nil {
		t.Fatal(err)
	}
	prior := g.prior(t)
	if err := g.adopter.signerGuard(ctx, g.suite, prior, Signers{}); !errors.Is(err, ErrAdoptionSignerDowngrade) {
		t.Fatalf("an unsigned candidate (no signers) after a strong anchor: %v; want ErrAdoptionSignerDowngrade", err)
	}
	unsigned := *prior
	unsigned.Form = cache.FormUnsigned
	if err := g.adopter.signerGuard(ctx, g.suite, &unsigned, Signers{}); err != nil {
		t.Fatalf("an unsigned current anchor: %v", err)
	}
	odd := *prior
	odd.Form = "odd"
	if err := g.adopter.signerGuard(ctx, g.suite, &odd, Signers{}); err != nil ||
		!strings.Contains(g.logs.String(), `"reason":"`+guardSkipUnknownForm+`"`) {
		t.Fatalf("an unknown form: %v; want the guard standing down, logged:\n%s", err, g.logs.String())
	}
	plain := newAdoptionTestEnv(t)
	if plain.adopter.signers != nil {
		t.Fatal("test premise: passThroughVerifier is a SignerVerifier")
	}
	if err := plain.adopter.signerGuard(ctx, g.suite, prior, Signers{}); err != nil {
		t.Fatalf("a Verifier that reports no signers: %v", err)
	}
}

// TestSignerGuard_SaysWhenItIsOff (the task review's 9d-M2): an Adopter
// whose Verifier is not a SignerVerifier logs WARN
// adoption_signer_guard_unavailable, naming the verifier's type, once, at
// construction, and still constructs (the guard stands down); one whose
// Verifier is one logs nothing of the kind.
func TestSignerGuard_SaysWhenItIsOff(t *testing.T) {
	env := newAdoptionTestEnv(t)
	for _, c := range []struct {
		v    Verifier
		warn bool
	}{{passThroughVerifier{}, true}, {&signersVerifier{}, false}} {
		var logs bytes.Buffer
		ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: c.v,
			HostLimiter: hostsem.New(8), Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
		if err != nil || (ad.signers == nil) != c.warn {
			t.Fatalf("%T: NewAdopter %v, signers %v", c.v, err, ad.signers)
		}
		line := `"level":"WARN","msg":"adoption_signer_guard_unavailable","verifier":"freshness.passThroughVerifier"`
		if got := strings.Count(logs.String(), `"msg":"adoption_signer_guard_unavailable"`); got != map[bool]int{true: 1, false: 0}[c.warn] ||
			(c.warn && !strings.Contains(logs.String(), line)) {
			t.Fatalf("%T: logged %q; want the WARN %v", c.v, logs.String(), c.warn)
		}
	}
}

// TestSignerGuard_TheRefusalIsARefusalState (ruling G5): signer_downgrade
// is a refusal reason like rollback_refused's: outcome refused, reason
// signer_downgrade, cache.IsRefusalReason.
func TestSignerGuard_TheRefusalIsARefusalState(t *testing.T) {
	err := errors.Join(errors.New("x"), ErrAdoptionSignerDowngrade)
	out := classifyAdoptionOutcome(err)
	if r := classifyAdoptionReason(err, out, nil); out != "refused" || r != cache.ReasonSignerDowngrade || !cache.IsRefusalReason(r) {
		t.Fatalf("outcome %q reason %q (refusal %v), want refused and signer_downgrade", out, r, cache.IsRefusalReason(r))
	}
}
