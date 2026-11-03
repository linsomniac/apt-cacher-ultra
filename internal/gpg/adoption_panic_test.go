package gpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/freshness"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// The 1.0.3 crash end to end: a freshness tick that sees a changed
// InRelease hands it to the adoption goroutine (freshness.Checker.Check),
// which runs the real verifier and has no recover in 1.0.x. 1.0.2 died
// there on every body below; 1.0.3 records one failed adoption per body.
//
// AIDEV-NOTE: the test lives in package gpg, not with freshness's own
// adoption harness: that harness is in package freshness, which cannot
// import gpg (gpg imports freshness), so it cannot run the real verifier.
// It drives freshness's exported Checker and Adopter instead, wired as
// cmd/apt-cacher-ultra wires them (gpg.ClassifyVerifyErr, an AdoptionRing).

// adoptionTestSuite is the suite every adoption below runs on.
var adoptionTestSuite = freshness.SuiteRef{
	CanonicalScheme: "http",
	CanonicalHost:   "archive.example.com",
	SuitePath:       "/ubuntu/dists/noble",
}

// upstreamInRelease serves one InRelease body to the freshness checker's
// conditional GET, as a changed 200.
type upstreamInRelease struct{ body []byte }

func (u upstreamInRelease) Conditional(_ context.Context, _ *fetch.Target, _, _ string, _ int64) (*fetch.ConditionalResult, error) {
	return &fetch.ConditionalResult{Status: 200, Body: u.body}, nil
}

// noMemberFetch is the adopter's member fetcher. Verification is the
// adoption's first step, so a refused anchor fetches no member; calls
// counts any that does.
type noMemberFetch struct{ calls *atomic.Int32 }

func (f noMemberFetch) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	f.calls.Add(1)
	return nil, errors.New("noMemberFetch: no member fetch expected")
}

// TestAdoption_MalformedSignatureIsARecordedFailure runs each body that
// crashed 1.0.2's daemon through one freshness tick and its adoption
// goroutine: I-1's two envelopes, and A-RC3's two shapes whose malformed
// packet go-crypto parses first. Each must come back as one gpg_failed
// adoption in the ring, with the verifier's reason, no member fetched and
// no snapshot adopted. On 1.0.2 the goroutine panics and takes the test
// binary down with it.
func TestAdoption_MalformedSignatureIsARecordedFailure(t *testing.T) {
	keyring := aptShapeKeyring(t)
	bodies := []struct {
		name   string
		body   []byte
		reason string
	}{
		{"I-1 " + i1Bodies[0].name, []byte(i1Bodies[0].body), "missing_signature"},
		{"I-1 " + i1Bodies[1].name, []byte(i1Bodies[1].body), "missing_signature"},
		{"A-RC3 q10_unknownB_hashed_t4_empty_only", readAptShape(t, "q10_unknownB_hashed_t4_empty_only.InRelease"), "crypto_verify_failed"},
		{"A-RC3 q10_unknownB_hashed_t4_empty_plus_good", readAptShape(t, "q10_unknownB_hashed_t4_empty_plus_good.InRelease"), "crypto_verify_failed"},
	}
	for _, b := range bodies {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			c, err := cache.Open(ctx, t.TempDir(), silentLogger())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })

			// The baseline InRelease anchor the tick compares against:
			// any hash but the body's makes the 200 a change.
			baseline := sha256.Sum256([]byte("baseline InRelease"))
			baselineHash := hex.EncodeToString(baseline[:])
			if err := c.PutBlob(ctx, baselineHash, 18); err != nil {
				t.Fatal(err)
			}
			if err := c.PutURLPath(ctx, cache.URLPath{
				CanonicalScheme: adoptionTestSuite.CanonicalScheme,
				CanonicalHost:   adoptionTestSuite.CanonicalHost,
				Path:            adoptionTestSuite.SuitePath + "/InRelease",
				BlobHash:        &baselineHash,
				UpstreamURL:     "http://archive.example.com/ubuntu/dists/noble/InRelease",
				IsMetadata:      true,
			}); err != nil {
				t.Fatal(err)
			}

			v, err := NewVerifier(VerifierConfig{
				Keyring:          keyring,
				RequireSignature: true,
				AllowShortKeyID:  true,
				Logger:           silentLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			var memberFetches atomic.Int32
			adopter, err := freshness.NewAdopter(freshness.AdoptionConfig{
				Cache:       c,
				Fetcher:     noMemberFetch{calls: &memberFetches},
				Verifier:    v,
				HostLimiter: hostsem.New(8),
				Logger:      silentLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ring := observability.NewRing(8)
			checker, err := freshness.New(freshness.Config{
				Cache:               c,
				Fetcher:             upstreamInRelease{body: b.body},
				HostLimiter:         hostsem.New(8),
				Adopter:             adopter,
				AdoptionRing:        ring,
				GPGReasonClassifier: ClassifyVerifyErr,
				Logger:              silentLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}

			checker.Check(ctx, adoptionTestSuite.CanonicalScheme, adoptionTestSuite.CanonicalHost, adoptionTestSuite.SuitePath)
			checker.WaitForAdoptions()

			events := ring.Snapshot()
			if len(events) != 1 {
				t.Fatalf("%d adoption events recorded, want 1: %+v", len(events), events)
			}
			if e := events[0]; e.Outcome != "gpg_failed" || e.Reason != b.reason {
				t.Errorf("adoption recorded outcome %q reason %q, want gpg_failed %s", e.Outcome, e.Reason, b.reason)
			}
			if n := memberFetches.Load(); n != 0 {
				t.Errorf("%d member fetches after a refused anchor, want 0", n)
			}
			sf, err := c.GetSuiteFreshness(ctx, adoptionTestSuite.CanonicalScheme, adoptionTestSuite.CanonicalHost, adoptionTestSuite.SuitePath)
			if err != nil {
				t.Fatal(err)
			}
			if sf.CurrentSnapshotID != nil {
				t.Errorf("suite adopted snapshot %d from a refused anchor", *sf.CurrentSnapshotID)
			}
		})
	}
}
