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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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

// parseKeyringFile reads one keyring file: parseKeyringReader.
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

// keyringMaxFile caps one keyring file, which is read whole: the largest
// real keyrings are well under a mebibyte (debian-archive-keyring.gpg, 56
// KiB; the vendor survey's largest armored file, 15 KiB).
const keyringMaxFile = 16 << 20

// parseKeyringReader reads one key file as apt reads it (the re-review's
// V-RC1): an armored file (its first 64 octets hold "-----BEGIN PGP") is
// every public key block in it, dearmored and joined in file order into one
// binary keyring (dearmorKeyring); any other file is a binary keyring,
// which must be whole OpenPGP packets to its end (wholePackets). The
// keyring's keys are then go-crypto's ReadKeyRing, which skips a key it
// does not support (an unknown algorithm), as gpgv cannot use one either.
// An empty file holds no key and is no error, as apt reads it.
//
// AIDEV-NOTE: apt reads every block of an armored file: apt 2.2 to 2.8's
// apt-key (dearmor_keyring: an awk script over every BEGIN..END block,
// "to support multiple keys in one file", then base64 -d), apt 3.x's
// dearmorKeyOrCheckFormat (apt-pkg/contrib/gpgv.cc, 3.2.0 :316-346: every
// public key block's base64 joined, then decoded), and debian:13's sqv,
// which reads the file itself. The loader read the first block only
// (openpgp.ReadArmoredKeyRing), so a key in a later block was unknown to
// S1 and untrusted by S2: yarn's signing subkey (block 2 of its published
// file) and sublime's second signer (block 3) among the surveyed vendors.
// Where apt's readers part — text outside the blocks (apt-key and apt 3
// skip it), a block of another type (apt-key reads any BEGIN block, apt 3
// public key blocks only), a block without its END line (apt 3 ignores
// the file, apt-key decodes what it has), a line after a block's checksum
// line, bytes after a binary keyring's packets (gpgv reads a keyring to
// its first bad packet; a .asc holding binary then armor is armor alone to
// apt 3) — the file is refused, and LoadKeyring logs that at WARN and
// skips it: no surveyed vendor's file is so (all 70 are clean binary or
// clean armor), and a skipped file is said where a misread one is not.
func parseKeyringReader(r io.Reader) (openpgp.EntityList, error) {
	b, err := io.ReadAll(io.LimitReader(r, keyringMaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(b) > keyringMaxFile {
		return nil, fmt.Errorf("larger than %d octets", keyringMaxFile)
	}
	kind := "binary"
	if bytes.Contains(b[:min(len(b), 64)], []byte("-----BEGIN PGP")) {
		kind = "armored"
		if b, err = dearmorKeyring(b); err != nil {
			return nil, fmt.Errorf("armored: %w", err)
		}
	} else if err := wholePackets(b); err != nil {
		return nil, fmt.Errorf("binary: %w", err)
	}
	entities, err := openpgp.ReadKeyRing(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	return entities, nil
}

// The public key block's armor lines (RFC 9580 §6.2).
var (
	keyBlockBegin = []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")
	keyBlockEnd   = []byte("-----END PGP PUBLIC KEY BLOCK-----")
)

// dearmorKeyring is apt's dearmoring of an armored key file
// (parseKeyringReader): every public key block's packets, in file order,
// as one binary keyring. Lines are compared without their surrounding
// blanks (a CR included), as apt 3 strips them; outside a block only blank
// lines are read; inside one, any line starting with "-----" but its END
// line, or any line after its checksum line ("=" first), is refused; each
// block is go-crypto's armor decoding of its lines (headers, the empty
// line, base64; the checksum is not checked, by apt either), and must
// decode to whole packets.
//
// The caller's sniff found "-----BEGIN PGP" in b's first 64 octets, so b
// holds a line: a public key block's BEGIN line, which opens a block, or
// one refused (another BEGIN line, or text).
func dearmorKeyring(b []byte) ([]byte, error) {
	var out []byte
	start := -1 // the open block's first octet in b, -1 outside a block
	crc := false
	for off, n := 0, 1; off < len(b); n++ {
		next := len(b)
		if i := bytes.IndexByte(b[off:], '\n'); i >= 0 {
			next = off + i + 1
		}
		l := bytes.TrimSpace(b[off:next])
		switch {
		case start < 0 && len(l) == 0:
		case start < 0 && bytes.Equal(l, keyBlockBegin):
			start, crc = off, false
		case start < 0 && bytes.HasPrefix(l, []byte("-----BEGIN ")):
			return nil, fmt.Errorf("line %d: an armor block that is no public key block", n)
		case start < 0:
			return nil, fmt.Errorf("line %d: text outside the public key blocks", n)
		case bytes.Equal(l, keyBlockEnd):
			data, err := decodeKeyBlock(b[start:next])
			if err != nil {
				return nil, fmt.Errorf("the block ending on line %d: %w", n, err)
			}
			out = append(out, data...)
			start = -1
		case bytes.HasPrefix(l, []byte("-----")):
			return nil, fmt.Errorf("line %d: a line starting with \"-----\" inside a public key block", n)
		case crc && len(l) > 0:
			return nil, fmt.Errorf("line %d: a line after the block's checksum line", n)
		case len(l) > 0 && l[0] == '=':
			crc = true
		}
		off = next
	}
	if start >= 0 {
		return nil, errors.New("a public key block without its END line")
	}
	return out, nil
}

// decodeKeyBlock is go-crypto's armor decoding of one public key block, its
// BEGIN line to its END line (dearmorKeyring), checked to be whole packets.
// (The block holds no other "-----" line, so the decoder can read no other
// block in it: its type is the BEGIN line's.)
func decodeKeyBlock(blk []byte) ([]byte, error) {
	block, err := armor.Decode(bytes.NewReader(blk))
	if err != nil {
		return nil, fmt.Errorf("its armor does not decode: %w", err)
	}
	data, err := io.ReadAll(block.Body)
	if err != nil {
		return nil, fmt.Errorf("its armor does not decode: %w", err)
	}
	if err := wholePackets(data); err != nil {
		return nil, err
	}
	return data, nil
}

// wholePackets checks that b is a run of whole OpenPGP packets of definite
// lengths (RFC 9580 §4.2: old-format one-, two- or four-octet lengths,
// new-format one-, two- or five-octet ones), any tag: a keyring as gpgv
// reads it to its end. The framing is layer P1's (sigpacket.go,
// sigPacketHeader), for every tag.
func wholePackets(b []byte) error {
	for off := 0; off < len(b); {
		h := b[off]
		if h&0x80 == 0 {
			return fmt.Errorf("offset %d: an octet (0x%02x) that starts no OpenPGP packet", off, h)
		}
		var hl int
		var n uint64
		if h&0x40 != 0 {
			if len(b)-off < 2 {
				return fmt.Errorf("offset %d: a truncated packet header", off)
			}
			switch l0 := b[off+1]; {
			case l0 < 192:
				hl, n = 2, uint64(l0)
			case l0 < 224:
				if len(b)-off < 3 {
					return fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 3, (uint64(l0)-192)<<8+uint64(b[off+2])+192
			case l0 == 255:
				if len(b)-off < 6 {
					return fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 6, uint64(binary.BigEndian.Uint32(b[off+2:off+6]))
			default:
				return fmt.Errorf("offset %d: a partial packet length", off)
			}
		} else {
			octets := [4]int{1, 2, 4, 0}[h&0x03]
			switch {
			case octets == 0:
				return fmt.Errorf("offset %d: an indeterminate packet length", off)
			case len(b)-off < 1+octets:
				return fmt.Errorf("offset %d: a truncated packet header", off)
			}
			for _, c := range b[off+1 : off+1+octets] {
				n = n<<8 | uint64(c)
			}
			hl = 1 + octets
		}
		if n > uint64(len(b)-off-hl) {
			return fmt.Errorf("offset %d: a packet running past the end", off)
		}
		off += hl + int(n)
	}
	return nil
}
