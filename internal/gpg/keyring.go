// Package gpg implements SPEC2 §7.6 GPG verification: load the host
// apt keyring at startup and verify clearsigned InRelease bodies
// against per-suite-narrowed trust sets.
//
// The package wraps github.com/ProtonMail/go-crypto/openpgp (SPEC2
// §7.6.4). It is consumed by the freshness checker through the
// freshness.Verifier interface; this package's Verifier type satisfies
// that interface.
package gpg

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// Standard apt keyring directories. Modern apt installs split trust
// between the legacy /etc/apt/trusted.gpg.d/ (whole-archive trust) and
// /etc/apt/keyrings/ (Signed-By: per-source trust). SPEC2 §7.6.1
// directs us to read both.
const (
	DefaultTrustedGPGDir = "/etc/apt/trusted.gpg.d"
	DefaultKeyringsDir   = "/etc/apt/keyrings"
)

// Keyring is the union of all trusted-key entities loaded at startup.
// The fingerprint set covers every loaded entity's primary key plus
// every subkey, so subset narrowing (per-suite trust set) can match on
// either.
type Keyring struct {
	entities openpgp.EntityList

	// fingerprints is the set of uppercase 40-char hex fingerprints of
	// every key (primary + subkeys) in entities. Used by trust-set
	// narrowing and by the IssuerFingerprint check in the verifier.
	fingerprints map[string]struct{}
}

// Empty reports whether the keyring loaded zero entities.
func (k *Keyring) Empty() bool { return len(k.entities) == 0 }

// Size returns the number of loaded entities (each entity is one
// primary key, possibly with subkeys).
func (k *Keyring) Size() int { return len(k.entities) }

// HasFingerprint reports whether fp (any case) names a key in the
// keyring. Comparison is case-insensitive on the canonical 40-char
// hex form.
func (k *Keyring) HasFingerprint(fp string) bool {
	_, ok := k.fingerprints[strings.ToUpper(fp)]
	return ok
}

// EntityList returns the underlying entity list (broad trust set).
// Caller must not mutate.
func (k *Keyring) EntityList() openpgp.EntityList { return k.entities }

// Subset returns the entities whose primary key OR any subkey
// fingerprint is in fps (uppercase). This is the SPEC2 §7.6.2
// host-keyring narrowing: a [[trusted_signer]] block declares
// fingerprints, and only entities containing one of those fingerprints
// participate in the per-suite trust set.
func (k *Keyring) Subset(fps map[string]struct{}) openpgp.EntityList {
	if len(fps) == 0 {
		return nil
	}
	out := make(openpgp.EntityList, 0, len(fps))
	for _, e := range k.entities {
		if entityHasAnyFingerprint(e, fps) {
			out = append(out, e)
		}
	}
	return out
}

func entityHasAnyFingerprint(e *openpgp.Entity, fps map[string]struct{}) bool {
	if _, ok := fps[upperFP(e.PrimaryKey.Fingerprint)]; ok {
		return true
	}
	for _, sub := range e.Subkeys {
		if _, ok := fps[upperFP(sub.PublicKey.Fingerprint)]; ok {
			return true
		}
	}
	return false
}

// upperFP renders a 20-byte fingerprint as 40-char uppercase hex.
func upperFP(b []byte) string { return strings.ToUpper(hex.EncodeToString(b)) }

// LoadKeyring walks dirs and parses every *.gpg (binary) and *.asc
// (ASCII-armored) file beneath them, returning a deduplicated
// Keyring. Per SPEC2 §7.6.1, files that fail to parse are logged at
// WARN and skipped; whatever subset parsed cleanly is returned. A
// Keyring with zero entities is NOT an error at this layer — the
// caller decides whether to abort startup based on
// adoption.require_signature.
//
// dirs that do not exist are quietly skipped; this matches the
// expectation that a fresh deployment may have only one of the two
// standard paths populated.
func LoadKeyring(dirs []string, logger *slog.Logger) (*Keyring, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// AIDEV-NOTE: dedupe by primary-key fingerprint. apt's two
	// keyring directories overlap (operators sometimes symlink
	// keys into both), and the same key bytes parsed twice as
	// distinct *Entity values would be picked twice by openpgp's
	// signature verification — wasted work, not a correctness bug,
	// but we filter at load time anyway.
	seen := make(map[string]struct{})
	var entities openpgp.EntityList
	fingerprints := make(map[string]struct{})

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			logger.Warn("keyring: stat dir failed",
				"dir", dir,
				"err", err,
			)
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			logger.Warn("keyring: read dir failed",
				"dir", dir,
				"err", err,
			)
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			name := ent.Name()
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".gpg" && ext != ".asc" {
				continue
			}
			path := filepath.Join(dir, name)
			parsed, err := parseKeyringFile(path)
			if err != nil {
				logger.Warn("keyring: parse failed; skipping file",
					"path", path,
					"err", err,
				)
				continue
			}
			for _, e := range parsed {
				if e == nil || e.PrimaryKey == nil {
					continue
				}
				fp := upperFP(e.PrimaryKey.Fingerprint)
				if _, dup := seen[fp]; dup {
					continue
				}
				seen[fp] = struct{}{}
				entities = append(entities, e)
				fingerprints[fp] = struct{}{}
				for _, sub := range e.Subkeys {
					if sub.PublicKey != nil {
						fingerprints[upperFP(sub.PublicKey.Fingerprint)] = struct{}{}
					}
				}
			}
		}
	}

	return &Keyring{
		entities:     entities,
		fingerprints: fingerprints,
	}, nil
}

// parseKeyringFile reads one keyring file. The decoder probes the
// content for the ASCII-armor header; binary keyrings (sequences of
// public-key packets) decode through ReadKeyRing directly.
func parseKeyringFile(path string) (openpgp.EntityList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Buffer enough of the file head to detect armor and rewind
	// without seeking (works on any io.Reader if needed).
	head := make([]byte, 64)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read head: %w", err)
	}
	head = head[:n]

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind: %w", err)
	}

	if bytes.Contains(head, []byte("-----BEGIN PGP")) {
		entities, err := openpgp.ReadArmoredKeyRing(f)
		if err != nil {
			return nil, fmt.Errorf("armored: %w", err)
		}
		return entities, nil
	}
	entities, err := openpgp.ReadKeyRing(f)
	if err != nil {
		return nil, fmt.Errorf("binary: %w", err)
	}
	return entities, nil
}
