package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// withKeyringDirs swaps the package-level keyringDirs slice for the
// duration of one test. Tests that touch keyringDirs must NOT call
// t.Parallel().
func withKeyringDirs(t *testing.T, dirs []string) {
	t.Helper()
	prev := keyringDirs
	keyringDirs = dirs
	t.Cleanup(func() { keyringDirs = prev })
}

// withoutEmbeddedKeys suppresses the canonical archive keys baked into
// the binary for one test, so a tempdir keyringDirs override actually
// represents the full keyring the loader will see. Tests must NOT
// call t.Parallel().
func withoutEmbeddedKeys(t *testing.T) {
	t.Helper()
	prev := keyringEmbeddedSources
	keyringEmbeddedSources = nil
	t.Cleanup(func() { keyringEmbeddedSources = prev })
}

func TestBuildAdopter_EmptyKeyring_RequireSignatureTrue_Errors(t *testing.T) {
	// SPEC2 §7.6.1: empty keyring + require_signature=true is a
	// startup error. Misconfiguration here would silently disable
	// adoption — operators wouldn't see why their suites never
	// adopted. This is a stealth security regression we want loud.
	//
	// We suppress the bundled canonical archive keys for this test:
	// without that, the keyring is never truly empty in production
	// builds (the embedded set always parses). The "empty disk +
	// no embed" path remains a real configuration shape for
	// minimal builds and stripped binaries, so the guard still
	// matters.
	withKeyringDirs(t, []string{t.TempDir()})
	withoutEmbeddedKeys(t)

	cfg := newAdoptionEnabledCfg(t, true /* requireSignature */)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	_, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err == nil {
		t.Fatal("expected error for empty keyring + require_signature=true")
	}
	if !strings.Contains(err.Error(), "keyring is empty") {
		t.Fatalf("error doesn't mention empty keyring: %v", err)
	}
}

// TestBuildAdopter_BundledKeysSatisfyRequireSignature confirms the
// new default behavior: even with an empty on-disk keyring, the
// canonical Ubuntu/Debian/ESM archive keys baked into the binary
// satisfy require_signature=true and adoption starts cleanly.
func TestBuildAdopter_BundledKeysSatisfyRequireSignature(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})

	cfg := newAdoptionEnabledCfg(t, true /* requireSignature */)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, k, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if a == nil {
		t.Fatal("nil Adopter on success path")
	}
	if k == nil || k.Empty() {
		t.Fatal("expected bundled keys to populate keyring")
	}
}

func TestBuildAdopter_EmptyKeyring_RequireSignatureFalse_OK(t *testing.T) {
	// With require_signature = false, an empty keyring is allowed —
	// the operator opted into unsigned-OK mode and saw the WARN at
	// startup.
	withKeyringDirs(t, []string{t.TempDir()})

	cfg := newAdoptionEnabledCfg(t, false /* requireSignature */)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if a == nil {
		t.Fatal("nil Adopter on success path")
	}
}

func TestBuildAdopter_PopulatedKeyring_RequireSignatureTrue_OK(t *testing.T) {
	keyDir := t.TempDir()
	writeTestKeyAt(t, filepath.Join(keyDir, "k.asc"))
	withKeyringDirs(t, []string{keyDir})

	cfg := newAdoptionEnabledCfg(t, true)
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, k, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatalf("buildAdopter: %v", err)
	}
	if a == nil {
		t.Fatal("nil Adopter on success path")
	}
	if k == nil || k.Empty() {
		t.Fatal("expected non-empty keyring on success path")
	}
}

// TestBuildAdopter_EmptyKeyring_AcceptAnySigner_OK asserts the relaxed
// startup guard: empty disk keyring + no embedded keys + adoption
// enabled + require_signature = true is permitted when
// accept_any_signer = true. The bypass branch runs for unpinned
// suites without consulting any key, so the empty keyring is not a
// startup error.
func TestBuildAdopter_EmptyKeyring_AcceptAnySigner_OK(t *testing.T) {
	withKeyringDirs(t, []string{t.TempDir()})
	withoutEmbeddedKeys(t)

	cfg := newAdoptionEnabledCfg(t, true /* requireSignature */)
	cfg.Adoption.AcceptAnySigner = true
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	a, k, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger())
	if err != nil {
		t.Fatalf("buildAdopter: %v (want nil under accept_any_signer)", err)
	}
	if a == nil {
		t.Fatal("nil Adopter on success path")
	}
	if k == nil || !k.Empty() {
		t.Fatalf("expected empty keyring; got %v", k)
	}
}

func TestBuildAdopter_TrustedSignerCompiled(t *testing.T) {
	// A populated [[trusted_signer]] block survives compilePins and
	// reaches the verifier without error.
	keyDir := t.TempDir()
	writeTestKeyAt(t, filepath.Join(keyDir, "k.asc"))
	withKeyringDirs(t, []string{keyDir})

	cfg := newAdoptionEnabledCfg(t, true)
	cfg.TrustedSigners = []config.TrustedSigner{{
		MatchCanonicalHost: `^archive\.example\.com$`,
		Fingerprints:       []string{"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"},
	}}
	c, fetcher, hosts := newAdoptionWiringDeps(t)
	if _, _, err := buildAdopter(cfg, c, fetcher, hosts, silentBuildLogger()); err != nil {
		t.Fatalf("buildAdopter with one [[trusted_signer]]: %v", err)
	}
}

func TestCompilePins_LowercaseFingerprintCanonicalizedToUpper(t *testing.T) {
	pins, err := compilePins([]config.TrustedSigner{{
		MatchCanonicalHost: `^a\.b\.c$`,
		Fingerprints:       []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	}})
	if err != nil {
		t.Fatalf("compilePins: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("len=%d, want 1", len(pins))
	}
	if _, ok := pins[0].Fingerprints["DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"]; !ok {
		t.Fatal("lowercase fingerprint not canonicalized to upper")
	}
}

func TestCompilePins_BadRegexBubblesUp(t *testing.T) {
	// config.Validate normally rejects invalid regex up front, but
	// the runtime translation guards against the surface failure too.
	_, err := compilePins([]config.TrustedSigner{{
		MatchCanonicalHost: `(unbalanced`,
		Fingerprints:       []string{"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"},
	}})
	if err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

// silentBuildLogger discards everything; buildAdopter emits an INFO
// line we don't want in test output.
func silentBuildLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newAdoptionEnabledCfg returns a default-baseline cfg with
// adoption.enabled = true and the requested require_signature value.
func newAdoptionEnabledCfg(t *testing.T, requireSignature bool) *config.Config {
	t.Helper()
	cacheDir := t.TempDir()
	cfg := minimalCfg(cacheDir, nil)
	cfg.Adoption.Enabled = true
	cfg.Adoption.RequireSignature = requireSignature
	return cfg
}

// newAdoptionWiringDeps stands up a real *cache.Cache + *fetch.Client
// + *hostsem.Sem. The cache is opened in a tempdir; t.Cleanup closes
// it.
func newAdoptionWiringDeps(t *testing.T) (*cache.Cache, *fetch.Client, *hostsem.Sem) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.Open(context.Background(), dir, silentBuildLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fc, err := fetch.New(fetch.Options{
		AllowedHostRegex: []string{`^127\.0\.0\.1$`},
		DenyTargetRanges: []string{},
	})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	hs := hostsem.New(2)
	return c, fc, hs
}

// writeTestKeyAt drops a freshly-generated armored pubkey at path.
// Used to make a tempdir keyring non-empty for buildAdopter tests.
func writeTestKeyAt(t *testing.T, path string) {
	t.Helper()
	cfg := &packet.Config{
		Algorithm: packet.PubKeyAlgoRSA,
		RSABits:   2048,
		Time:      func() time.Time { return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	e, err := openpgp.NewEntity("Wiring Test", "", "wt@example.com", cfg)
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("armor close: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
