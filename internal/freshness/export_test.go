package freshness

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// Exported for the external tests (package freshness_test), which import
// internal/gpg: an internal test cannot, as internal/gpg imports this
// package.

// InReleaseMessage is reconcile's reading of a stored InRelease
// (inReleaseMessage).
var InReleaseMessage = inReleaseMessage

// GuardRig is an Adopter over a real cache and a fake fetcher for one flat
// suite, for the signer downgrade guard's external tests
// (signer_guard_gpg_test.go), which run internal/gpg's verifier.
type GuardRig struct {
	t    *testing.T
	env  *adoptionTestEnv
	mu   sync.Mutex
	logs bytes.Buffer
}

// GuardRigSuite is the rig's suite: a flat one, as the apt-shape corpus'
// Release (a root Packages) is.
var GuardRigSuite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "vendor.example.com", SuitePath: "/repo"}

// NewGuardRig is a rig whose adopter verifies with v.
func NewGuardRig(t *testing.T, v Verifier) *GuardRig {
	t.Helper()
	env := newAdoptionTestEnv(t)
	env.suite = GuardRigSuite
	trackSuite(t, env.cache, env.suite)
	r := &GuardRig{t: t, env: env}
	r.SetVerifier(v)
	return r
}

// Write implements io.Writer for the rig's JSON log.
func (r *GuardRig) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logs.Write(p)
}

// Logs is what the rig's adopter has logged, one JSON line an event.
func (r *GuardRig) Logs() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logs.String()
}

// SetVerifier replaces the rig's adopter by one verifying with v, over the
// same cache: a restart with another keyring.
func (r *GuardRig) SetVerifier(v Verifier) {
	r.t.Helper()
	ad, err := NewAdopter(AdoptionConfig{
		Cache: r.env.cache, Fetcher: r.env.fetcher, Verifier: v, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewJSONHandler(r, nil)),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.env.adopter = ad
}

// Serve has the fake upstream serve body at the suite-relative path.
func (r *GuardRig) Serve(path string, body []byte) {
	r.env.fetcher.put("http://"+GuardRigSuite.CanonicalHost+GuardRigSuite.SuitePath+"/"+path, body)
}

// Run adopts inRelease, an inline anchor.
func (r *GuardRig) Run(inRelease []byte) error {
	return r.env.adopter.Run(context.Background(), r.env.suite, inRelease, "", "")
}

// RunDetached adopts release and its sig, a detached anchor.
func (r *GuardRig) RunDetached(release, sig []byte) error {
	return r.env.adopter.RunDetached(context.Background(), r.env.suite, release, sig, "", "")
}

// Current is the suite's current snapshot ID, 0 when it has none.
func (r *GuardRig) Current() int64 {
	r.t.Helper()
	sf, err := r.env.cache.GetSuiteFreshness(context.Background(), GuardRigSuite.CanonicalScheme,
		GuardRigSuite.CanonicalHost, GuardRigSuite.SuitePath)
	if err != nil {
		r.t.Fatal(err)
	}
	if sf == nil || sf.CurrentSnapshotID == nil {
		return 0
	}
	return *sf.CurrentSnapshotID
}

// RemoveCurrentAnchor deletes the pool file of the current snapshot's
// anchor (its InRelease, or its Release.gpg).
func (r *GuardRig) RemoveCurrentAnchor() {
	r.t.Helper()
	snap, err := r.env.cache.GetSuiteSnapshot(context.Background(), r.Current())
	if err != nil {
		r.t.Fatal(err)
	}
	h := snap.SingleAnchorHash
	if h == nil {
		h = snap.ReleaseGPGHash
	}
	if err := os.Remove(r.env.cache.BlobPath(*h)); err != nil {
		r.t.Fatal(err)
	}
}
