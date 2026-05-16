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
// across three on-disk locations:
//   - /etc/apt/trusted.gpg.d/   (legacy whole-archive trust)
//   - /etc/apt/keyrings/        (Signed-By: per-source trust)
//   - /usr/share/keyrings/      (package-provided Signed-By: keys)
//
// SPEC2 §7.6.1 directs us to read all three so apt-cacher-ultra's
// adoption-time trust set matches what apt itself effectively trusts
// on the host. /usr/share/keyrings/ is broadly populated by distro
// packages (e.g. microsoft-prod.gpg, signal-desktop-keyring.gpg,
// docker, confluent) and pointed at by per-source Signed-By
// directives; excluding it from the default scan would cause
// adoption to fail with gpg_failed for any third-party repo whose
// key lives there. Operators append further paths via the
// adoption.keyring_dirs config setting. Canonical Ubuntu, Debian,
// and Ubuntu Pro ESM archive keys are additionally baked into the
// binary so a minimal host with unpopulated /etc/apt/ still has
// usable trust for stock Debian/Ubuntu repositories.
const (
	DefaultTrustedGPGDir       = "/etc/apt/trusted.gpg.d"
	DefaultKeyringsDir         = "/etc/apt/keyrings"
	DefaultUsrShareKeyringsDir = "/usr/share/keyrings"
)

// KeyringEntry is one loaded entity with its source attribution.
// SourcePath is either an absolute file path on disk, or the
// pseudo-path "embedded:<name>" for keys baked into the binary via
// EmbeddedSource. Exposed for the admin status page.
type KeyringEntry struct {
	Entity             *openpgp.Entity
	PrimaryFingerprint string   // uppercase 40-char hex
	PrimaryUID         string   // first user ID (may be empty)
	SourcePath         string   // file path or "embedded:<name>"
	SubkeyFingerprints []string // uppercase 40-char hex per subkey
}

// Keyring is the union of all trusted-key entities loaded at startup.
// The fingerprint set covers every loaded entity's primary key plus
// every subkey, so subset narrowing (per-suite trust set) can match on
// either.
type Keyring struct {
	entries  []KeyringEntry
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

// Entries returns a copy of the loaded keyring entries with source
// attribution. Safe for the caller to mutate the returned slice
// header; the embedded Entity pointers must not be mutated.
func (k *Keyring) Entries() []KeyringEntry {
	out := make([]KeyringEntry, len(k.entries))
	copy(out, k.entries)
	return out
}

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

// FindByIssuerKeyID returns the first loaded entity whose primary key
// or any subkey has the given 8-byte short keyid. Returns nil when no
// match exists. Used by the verifier's allow_short_keyid fallback
// path (SPEC2 §7.6.3) — a signature packet with no IssuerFingerprint
// subpacket carries only the 8-byte short key id, which is matched
// against the loaded keyring to recover the long-form fingerprint.
func (k *Keyring) FindByIssuerKeyID(keyID uint64) *openpgp.Entity {
	for _, e := range k.entities {
		if e.PrimaryKey != nil && e.PrimaryKey.KeyId == keyID {
			return e
		}
		for _, sub := range e.Subkeys {
			if sub.PublicKey != nil && sub.PublicKey.KeyId == keyID {
				return e
			}
		}
	}
	return nil
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
// expectation that a fresh deployment may have only a subset of the
// standard paths populated.
//
// Equivalent to LoadKeyringWithEmbedded(dirs, nil, logger).
func LoadKeyring(dirs []string, logger *slog.Logger) (*Keyring, error) {
	return LoadKeyringWithEmbedded(dirs, nil, logger)
}

// LoadKeyringWithEmbedded extends LoadKeyring by also merging in
// keys baked into the binary as EmbeddedSource entries. On-disk dirs
// load first (in slice order); embedded sources load last. Dedup is
// by primary-key fingerprint, first-seen wins — so a key present on
// disk takes precedence over the bundled copy of the same key, and
// its SourcePath in the resulting Entries reflects the disk path the
// operator actually staged.
func LoadKeyringWithEmbedded(dirs []string, embedded []EmbeddedSource, logger *slog.Logger) (*Keyring, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// AIDEV-NOTE: dedupe by primary-key fingerprint. apt's keyring
	// directories overlap (operators sometimes symlink keys into
	// multiple paths) and the embedded set can overlap with disk
	// copies; the same key bytes parsed twice as distinct *Entity
	// values would be picked twice by openpgp's signature
	// verification — wasted work, not a correctness bug, but we
	// filter at load time anyway.
	seen := make(map[string]struct{})
	var entries []KeyringEntry
	fingerprints := make(map[string]struct{})

	addEntities := func(parsed openpgp.EntityList, source string) {
		for _, e := range parsed {
			if e == nil || e.PrimaryKey == nil {
				continue
			}
			fp := upperFP(e.PrimaryKey.Fingerprint)
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}

			subFPs := make([]string, 0, len(e.Subkeys))
			fingerprints[fp] = struct{}{}
			for _, sub := range e.Subkeys {
				if sub.PublicKey != nil {
					subFP := upperFP(sub.PublicKey.Fingerprint)
					fingerprints[subFP] = struct{}{}
					subFPs = append(subFPs, subFP)
				}
			}

			entries = append(entries, KeyringEntry{
				Entity:             e,
				PrimaryFingerprint: fp,
				PrimaryUID:         firstUID(e),
				SourcePath:         source,
				SubkeyFingerprints: subFPs,
			})
		}
	}

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

		dirEntries, err := os.ReadDir(dir)
		if err != nil {
			logger.Warn("keyring: read dir failed",
				"dir", dir,
				"err", err,
			)
			continue
		}
		for _, ent := range dirEntries {
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
			addEntities(parsed, path)
		}
	}

	for _, es := range embedded {
		parsed, err := parseKeyringBytes(es.Data)
		if err != nil {
			logger.Warn("keyring: parse embedded failed; skipping",
				"name", es.Name,
				"err", err,
			)
			continue
		}
		addEntities(parsed, "embedded:"+es.Name)
	}

	entities := make(openpgp.EntityList, 0, len(entries))
	for _, e := range entries {
		entities = append(entities, e.Entity)
	}

	return &Keyring{
		entries:      entries,
		entities:     entities,
		fingerprints: fingerprints,
	}, nil
}

// firstUID returns the entity's first user-id identity string
// ("Name <email>"), or "" when the entity carries no UIDs. UIDs
// surface on the admin status page so operators can recognise which
// key is loaded without having to look up a fingerprint.
func firstUID(e *openpgp.Entity) string {
	for _, id := range e.Identities {
		if id != nil {
			return id.Name
		}
	}
	return ""
}

// parseKeyringFile reads one keyring file. The decoder probes the
// content for the ASCII-armor header; binary keyrings (sequences of
// public-key packets) decode through ReadKeyRing directly.
func parseKeyringFile(path string) (openpgp.EntityList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parseKeyringReader(f)
}

// parseKeyringBytes parses an in-memory keyring blob (used for
// embedded sources). Same armor-probe behavior as parseKeyringFile.
func parseKeyringBytes(b []byte) (openpgp.EntityList, error) {
	return parseKeyringReader(bytes.NewReader(b))
}

func parseKeyringReader(r io.Reader) (openpgp.EntityList, error) {
	// Buffer enough of the head to detect armor and rewind without
	// seeking (works on any io.Reader if needed).
	head := make([]byte, 64)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read head: %w", err)
	}
	head = head[:n]

	if seeker, ok := r.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind: %w", err)
		}
	} else {
		r = io.MultiReader(bytes.NewReader(head), r)
	}

	if bytes.Contains(head, []byte("-----BEGIN PGP")) {
		entities, err := openpgp.ReadArmoredKeyRing(r)
		if err != nil {
			return nil, fmt.Errorf("armored: %w", err)
		}
		return entities, nil
	}
	entities, err := openpgp.ReadKeyRing(r)
	if err != nil {
		return nil, fmt.Errorf("binary: %w", err)
	}
	return entities, nil
}
