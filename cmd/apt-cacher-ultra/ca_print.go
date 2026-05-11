// SPEC6 §14.1: `apt-cacher-ultra ca print` — write the PEM-encoded CA
// cert to stdout. Loads the same config the daemon would, then either:
//
//  1. **Operator-supplied CA**: read tls_mitm.ca_cert, parse, print.
//  2. **Auto-generated, daemon already started**: read
//     `<ca_storage_dir>/ca.crt`, parse, print.
//  3. **Auto-generated, daemon not yet started**: run the §4.2.1
//     atomic generate+persist path under the §4.2.2 interprocess
//     flock, then print.
//
// Does NOT open SQLite. Concurrent invocation alongside the daemon is
// safe — the flock serializes the only race window (CA generation),
// and the second process to acquire the lock takes the load branch.
//
// Exit codes (§14.1):
//   - 0: cert printed
//   - 1: tls_mitm.enabled = false
//   - 2: cert path unreadable / parse fails / atomic gen fails
//   - 3: config file unreadable
//   - 4: lock contention on `<ca_storage_dir>/.ca.lock`

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy/tlsmitm"
)

// caPrintLockTimeout is the §4.2.2 flock acquisition deadline used
// by the auto-generated path. Defaults to the tlsmitm default (30s)
// when zero; tests override via setCAPrintLockTimeoutForTest.
var caPrintLockTimeout time.Duration

// setCAPrintLockTimeoutForTest temporarily overrides
// caPrintLockTimeout. Returns a restore closure the caller defers.
// Test-only.
func setCAPrintLockTimeoutForTest(d time.Duration) func() {
	prev := caPrintLockTimeout
	caPrintLockTimeout = d
	return func() { caPrintLockTimeout = prev }
}

// runCAPrint executes the `ca print` subcommand. args is everything
// AFTER the subcommand selector (i.e. os.Args[3:] for `ca print
// -config foo.toml`). Returns the §14.1 exit code; the caller must
// os.Exit with the returned value.
//
// stdout is where the PEM cert is written (callers pass os.Stdout in
// production, a *bytes.Buffer in tests). stderr receives the audit
// warning and any error diagnostic.
func runCAPrint(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ca print", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "/etc/apt-cacher-ultra/config.toml", "path to TOML config file")
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already wrote a usage line to stderr.
		return 3
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ca print: load config %q: %v\n", *configPath, err)
		return 3
	}

	if !cfg.TlsMitm.Enabled {
		_, _ = fmt.Fprintln(stderr, "ca print: tls_mitm.enabled = false in config; refusing to print a CA that would never be used")
		return 1
	}

	suppliedCert := cfg.TlsMitm.CaCert
	suppliedKey := cfg.TlsMitm.CaKey

	switch {
	case suppliedCert != "":
		// Operator-supplied path: read+parse the cert, print, audit
		// the key file's mode bits.
		certPEM, err := os.ReadFile(suppliedCert)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "ca print: read %q: %v\n", suppliedCert, err)
			return 2
		}
		if err := validateCAPEM(certPEM); err != nil {
			_, _ = fmt.Fprintf(stderr, "ca print: parse %q: %v\n", suppliedCert, err)
			return 2
		}
		auditKeyMode(stderr, suppliedKey)
		// Surface short-writes / broken pipes so a `ca print > ca.crt`
		// pipeline cannot silently materialize a truncated trust
		// anchor and exit 0.
		if _, err := stdout.Write(certPEM); err != nil {
			_, _ = fmt.Fprintf(stderr, "ca print: write cert to stdout: %v\n", err)
			return 2
		}
		return 0

	default:
		// Auto-generated path: LoadOrGenerate runs the same §4.2 case
		// detection the daemon uses. The flock serializes any
		// concurrent daemon start. We pass a no-op LogFn — the
		// subcommand surfaces only stderr diagnostics, not structured
		// events; LoadOrGenerate's return error already carries the
		// human-readable failure reason.
		dir := cfg.EffectiveCaStorageDir()
		ca, err := tlsmitm.LoadOrGenerate(tlsmitm.LoadOptions{
			StorageDir:           dir,
			AllowedHostRegex:     cfg.TlsMitm.AllowedHostRegex,
			AllowUnconstrainedCA: cfg.TlsMitm.AllowUnconstrainedCA,
			CALifetime:           cfg.TlsMitm.CACertLifetime.Duration,
			LockTimeout:          caPrintLockTimeout, // 0 → tlsmitm default (30s)
			LogFn:                func(level, event string, fields map[string]any) {},
		})
		if err != nil {
			if errors.Is(err, tlsmitm.ErrCALockTimeout) {
				_, _ = fmt.Fprintf(stderr, "ca print: lock contention on %q (concurrent daemon CA generation or another `ca print`); retry shortly\n", dir)
				return 4
			}
			_, _ = fmt.Fprintf(stderr, "ca print: load/generate CA in %q: %v\n", dir, err)
			return 2
		}

		// On the auto-gen path the key file lives under StorageDir
		// at `ca.key` — audit its mode for the same reasons we would
		// audit a supplied key (operator may have changed it).
		auditKeyMode(stderr, dir+"/ca.key")

		// Re-encode from the parsed cert so what we print exactly
		// matches what's on disk in the supplied case (PEM canonical
		// form). The trailing newline matches stdlib pem.Encode.
		out := pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: ca.Cert.Raw,
		})
		// SHA-256 fingerprint goes to stderr as a diagnostic — useful
		// for an operator who pipes stdout to a file and wants to
		// confirm the fingerprint without re-parsing.
		sum := sha256.Sum256(ca.Cert.Raw)
		_, _ = fmt.Fprintf(stderr, "ca print: fingerprint sha256=%s\n", hex.EncodeToString(sum[:]))
		// Same rationale as the supplied-CA branch above.
		if _, err := stdout.Write(out); err != nil {
			_, _ = fmt.Fprintf(stderr, "ca print: write cert to stdout: %v\n", err)
			return 2
		}
		return 0
	}
}

// validateCAPEM confirms `b` decodes as at least one CERTIFICATE PEM
// block. It does NOT enforce CA-specific constraints (BasicConstraints,
// KeyUsage); those are validated by tlsmitm.LoadOrGenerate's supplied
// path at daemon startup. `ca print` only needs to know the bytes are
// printable PEM so a pipe-to-file lands a usable file.
func validateCAPEM(b []byte) error {
	rest := b
	saw := 0
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" {
			saw++
		}
	}
	if saw == 0 {
		return fmt.Errorf("no CERTIFICATE PEM block found")
	}
	return nil
}

// auditKeyMode emits a §14.1 stderr warning when `path` exists with
// mode bits other than 0600. Non-fatal — the caller's exit code is
// unchanged; operators may have a legitimate ownership-and-mode
// strategy. An unreadable / missing key file is silently ignored
// (the cert may have been printed without the operator owning the
// key, e.g. on a workstation extracting from `cache_dir`).
func auditKeyMode(stderr io.Writer, path string) {
	if path == "" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	mode := st.Mode().Perm()
	if mode != 0o600 {
		_, _ = fmt.Fprintf(stderr, "ca print: WARNING: %q is mode %#o; recommend `chmod 0600 %s` to remove other-readable bits\n", path, mode, path)
	}
}

