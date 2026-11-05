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
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
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
// EmbeddedSource: the first source the key was read from, when it was
// read more than once (its copies are merged: LoadKeyringWithEmbedded).
// Exposed for the admin status page.
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

	// uncountable holds the keys and subkeys, by uppercase fingerprint, that
	// S2 must not count, each with why (keyFile.uncountable, the reader's
	// marks under ruling R2: a revocation or a newer binding the cache
	// cannot read, which gpgv honours), a mark on a primary key extended to
	// its subkeys. S1 knows them as it knows every key (uncountableKey).
	uncountable map[string]error
}

// uncountableKey says why S2 must not count the key or subkey whose
// uppercase fingerprint is fp (uncountable), nil when it may.
func (k *Keyring) uncountableKey(fp string) error { return k.uncountable[fp] }

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
// load first (in slice order); embedded sources load last. A key read
// more than once (two files, two copies in one file, a disk copy and
// the bundled one) is one entity, each later copy merged into the
// first (mergeEntity); its entry's SourcePath is the first copy's, so
// a key present on disk shows the disk path the operator staged.
func LoadKeyringWithEmbedded(dirs []string, embedded []EmbeddedSource, logger *slog.Logger) (*Keyring, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// AIDEV-NOTE: one entity per primary-key fingerprint, each later copy
	// of a key merged into the first (the re-review's V-R2C2). apt's
	// keyring directories overlap (a key symlinked into two paths, a
	// vendor's file beside an older copy, the bundled set beside a disk
	// copy), and one file can hold a key twice (a concatenated export).
	// The loader kept the first copy and dropped the rest, which is
	// harmless only for identical copies: vendors add signing subkeys
	// (google's key held eight in the survey, about one a year), and an
	// older copy read first hid the new one — the cache refused an anchor
	// every apt accepts (untrusted_signer: gpgv finds the keyblock holding
	// the subkey, sqv reads every certificate it is given), and S1 did not
	// see a bad signature by the subkey beside a good one, which the five
	// gpgv-based apt refuse. Merged rather than kept as separate entities:
	// a key stays one entity for the short-keyid fallback (which refuses a
	// key ID two entities claim), Subset and the admin listing; and a
	// revocation in any copy counts, as after gpg's import (which merges a
	// keyblock's signatures so) and in sqv (which merges the certificates
	// it is given) — kept apart, a copy without it read first would still
	// verify. A subkey's binding and an identity's self-signature are the
	// newest of the copies' (go-crypto's rule within one copy, gpg's after
	// a merge). Two residual gaps, both measured (the third re-review's
	// kr10: P a primary key that expired and was renewed, K its signing
	// subkey):
	//   - gpgv reads only the first keyblock that holds the key ID, so where
	//     an older copy read first has P expired and a later one renews it,
	//     gpgv refuses an anchor by K that the cache accepts (sqv agrees
	//     with the cache: kr10_renew_old_first); a client that cannot use the
	//     key fails with or without the cache, as with V-R2M3's gaps.
	//   - the third re-review's V-R3M1, open (Task 9c records it): from a
	//     copy holding K with P expired and a renewed copy without K, the
	//     merged key verifies an anchor by K (K's binding from the one, P's
	//     renewal from the other), which all six apt refuse (sqv: K "is not
	//     bound") and each copy alone refuses too (kr10_mixed_*, either
	//     order). sqv does not agree with the cache here. It is no
	//     attacker's lever: it needs the vendor's own signature by K, and
	//     every client of that keyring refuses every anchor by K, so the
	//     cache's adopting one changes nothing a client accepts. Counting a
	//     packet only when some single copy holding its issuer verifies it
	//     too would close it without reopening V-R2C2.
	seen := make(map[string]int) // primary fingerprint -> its index in entries
	var entries []KeyringEntry
	uncountable := make(map[string]error)

	addEntities := func(parsed *keyFile, source string) {
		// A mark from any copy of a key holds, as a revocation in any
		// copy counts (the merge below).
		for fp, why := range parsed.uncountable {
			if _, dup := uncountable[fp]; !dup {
				uncountable[fp] = why
			}
		}
		for _, e := range parsed.entities {
			if e == nil || e.PrimaryKey == nil {
				continue
			}
			fp := upperFP(e.PrimaryKey.Fingerprint)
			if i, dup := seen[fp]; dup {
				mergeEntity(entries[i].Entity, e)
				continue
			}
			seen[fp] = len(entries)
			entries = append(entries, KeyringEntry{
				Entity:             e,
				PrimaryFingerprint: fp,
				PrimaryUID:         firstUID(e),
				SourcePath:         source,
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
			// AIDEV-NOTE: the fourth re-review's V-R4M1: the loader opened
			// anything that was not a directory. open(2) of a FIFO waits for
			// a writer, so a FIFO named .gpg in a keyring directory blocked
			// the start forever (apt-key and apt 3 block on it too), and a
			// symlink to /dev/zero (its stat's size 0) was read to the cap,
			// 828 MiB, at every start. os.Stat follows a symlink and opens
			// nothing; only a regular file (a symlinked one included:
			// /usr/share/keyrings is full of them) is read.
			fi, err := os.Stat(path)
			if err != nil {
				logger.Warn("keyring_file_skipped",
					"path", path,
					"reason", "stat_failed",
					"err", err,
				)
				continue
			}
			if !fi.Mode().IsRegular() {
				logger.Warn("keyring_file_skipped",
					"path", path,
					"reason", "not_regular_file",
					"mode", fi.Mode().String(),
				)
				continue
			}
			parsed, err := parseKeyringFile(path)
			if err != nil {
				logger.Warn("keyring: parse failed; skipping file",
					"path", path,
					"err", err,
				)
				continue
			}
			if parsed.broken != nil {
				// The key the junk interrupts, and what follows, is not
				// read, as gpgv does not read it (readKeyring).
				logger.Warn("keyring_file_truncated",
					"path", path,
					"err", parsed.broken,
				)
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
		if parsed.broken != nil {
			logger.Warn("keyring_file_truncated",
				"name", es.Name,
				"err", parsed.broken,
			)
		}
		addEntities(parsed, "embedded:"+es.Name)
	}

	// The fingerprints last: a merge adds subkeys to an entry already
	// listed. A mark on a primary key marks every subkey of the merged
	// entity: a signature by a subkey of a revoked key is REVKEYSIG too.
	fingerprints := make(map[string]struct{})
	entities := make(openpgp.EntityList, 0, len(entries))
	for i := range entries {
		e := entries[i].Entity
		fingerprints[entries[i].PrimaryFingerprint] = struct{}{}
		primaryMark := uncountable[entries[i].PrimaryFingerprint]
		subFPs := make([]string, 0, len(e.Subkeys))
		for _, sub := range e.Subkeys {
			if sub.PublicKey != nil {
				subFP := upperFP(sub.PublicKey.Fingerprint)
				fingerprints[subFP] = struct{}{}
				subFPs = append(subFPs, subFP)
				if _, marked := uncountable[subFP]; primaryMark != nil && !marked {
					uncountable[subFP] = fmt.Errorf("its primary key %s: %w", entries[i].PrimaryFingerprint, primaryMark)
				}
			}
		}
		entries[i].SubkeyFingerprints = subFPs
		entities = append(entities, e)
	}

	return &Keyring{
		entries:      entries,
		entities:     entities,
		fingerprints: fingerprints,
		uncountable:  uncountable,
	}, nil
}

// mergeEntity folds dup, a later copy of e's key (the same primary
// fingerprint, so the same key packet), into e, as gpg's import merges
// keyblocks: the union of the copies' subkeys (by fingerprint) and
// identities (by name), and of every revocation and signature, a subkey's
// binding, an identity's self-signature and the key's direct-key
// self-signature each the newest of the copies' (go-crypto's rule within
// one copy: a later one only if strictly newer). go-crypto verified every
// self-signature of both copies against the one primary key as it read
// them, so the union holds no signature it did not. e and dup are the
// loader's own, freshly parsed; dup is not used after.
func mergeEntity(e, dup *openpgp.Entity) {
	newer := func(have, cand *packet.Signature) *packet.Signature {
		if cand != nil && (have == nil || cand.CreationTime.After(have.CreationTime)) {
			return cand
		}
		return have
	}
	e.Revocations = append(e.Revocations, dup.Revocations...)
	e.Signatures = append(e.Signatures, dup.Signatures...)
	e.SelfSignature = newer(e.SelfSignature, dup.SelfSignature)
	if e.Identities == nil {
		e.Identities = make(map[string]*openpgp.Identity, len(dup.Identities))
	}
	for name, id := range dup.Identities {
		have, ok := e.Identities[name]
		if !ok || have == nil {
			e.Identities[name] = id
			continue
		}
		if id == nil {
			continue
		}
		have.Revocations = append(have.Revocations, id.Revocations...)
		have.Signatures = append(have.Signatures, id.Signatures...)
		have.SelfSignature = newer(have.SelfSignature, id.SelfSignature)
	}
	for _, sub := range dup.Subkeys {
		if sub.PublicKey == nil {
			continue
		}
		i := slices.IndexFunc(e.Subkeys, func(s openpgp.Subkey) bool {
			return s.PublicKey != nil && bytes.Equal(s.PublicKey.Fingerprint, sub.PublicKey.Fingerprint)
		})
		if i < 0 {
			e.Subkeys = append(e.Subkeys, sub)
			continue
		}
		have := &e.Subkeys[i]
		have.Revocations = append(have.Revocations, sub.Revocations...)
		have.Sig = newer(have.Sig, sub.Sig)
	}
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
