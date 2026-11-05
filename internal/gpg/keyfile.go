package gpg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// keyFile is what the reader read of one key file: its keys, each once (a
// key read twice, in two blocks or two readings, merged as the loader
// merges copies: addEntity), and S2's marks on them (uncountable: the key
// or subkey fingerprints, uppercase, S2 must not count, each with why; the
// controller's ruling R2, keyBlock.marks). broken is the error at which
// the file's packets stopped being whole (packetsOf), nil when they are to
// the end: the key whose packets run into it, and whatever follows, gpgv
// does not read, and nor did this reading (readKeyring); LoadKeyring warns.
type keyFile struct {
	entities    openpgp.EntityList
	uncountable map[string]error
	broken      error
}

// addEntity adds e to kf's keys, merged into the key of its primary
// fingerprint when kf holds one (mergeEntity).
func (kf *keyFile) addEntity(e *openpgp.Entity) {
	for _, have := range kf.entities {
		if bytes.Equal(have.PrimaryKey.Fingerprint, e.PrimaryKey.Fingerprint) {
			mergeEntity(have, e)
			return
		}
	}
	kf.entities = append(kf.entities, e)
}

// addMarks adds marks to kf's, the first mark on a fingerprint kept.
func (kf *keyFile) addMarks(marks map[string]error) {
	for fp, why := range marks {
		if kf.uncountable == nil {
			kf.uncountable = make(map[string]error)
		}
		if _, dup := kf.uncountable[fp]; !dup {
			kf.uncountable[fp] = why
		}
	}
}

// add folds another reading of the same file, o, into kf: its keys
// (addEntity) and marks (addMarks); kf's broken stands.
func (kf *keyFile) add(o *keyFile) {
	for _, e := range o.entities {
		kf.addEntity(e)
	}
	kf.addMarks(o.uncountable)
}

// parseKeyringFile reads one keyring file (readKeyFile, under
// keyringMaxFile) and its keys: parseKeyring.
func parseKeyringFile(path string) (*keyFile, error) {
	b, err := readKeyFile(path, keyringMaxFile)
	if err != nil {
		return nil, err
	}
	return parseKeyring(b)
}

// readKeyFile reads the key file at path whole, under maxFile. A file its
// stat says is larger is refused before an octet of it is read; one under it
// is read into one buffer of its size and one octet more, which finds a file
// that grew since the stat (or whose stat gives no size, a special file):
// that one is read on, under maxFile.
//
// AIDEV-NOTE: the third re-review's V-R3M2: the loader read every file
// whole before it judged it, so a misplaced non-keyring in a keyring
// directory (a disk image named .gpg) cost up to 256 MiB, and io.ReadAll's
// growth, at every start, before its refusal
// (TestParseKeyringFile_RefusesAFileOverTheCapUnread). A real keyring
// costs its size: debian-keyring.gpg, if installed, still costs its 31 MB
// and its keys' parse (3.6 s) at every start, as on main.
func readKeyFile(path string, maxFile int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > int64(maxFile) {
		return nil, fmt.Errorf("larger than %d octets (%d)", maxFile, fi.Size())
	}
	buf := make([]byte, fi.Size()+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return buf[:n], nil
	case err != nil:
		return nil, fmt.Errorf("read: %w", err)
	}
	rest, err := io.ReadAll(io.LimitReader(f, int64(maxFile)+1-int64(n)))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if b := append(buf, rest...); len(b) <= maxFile {
		return b, nil
	}
	return nil, fmt.Errorf("larger than %d octets", maxFile)
}

// parseKeyringBytes parses an in-memory keyring blob (used for
// embedded sources), as parseKeyringFile reads a file.
func parseKeyringBytes(b []byte) (*keyFile, error) {
	return parseKeyringReader(bytes.NewReader(b))
}

// keyringMaxFile caps one keyring file, which is read whole: 256 MiB, eight
// times the largest keyring a distribution ships (debian-keyring.gpg, 31.3
// MB in debian-keyring 2025.07.26: every Debian developer's key with its
// certifications, in /usr/share/keyrings, a default directory), where the
// archive keyrings are well under a mebibyte (debian-archive-keyring.gpg,
// 56 KiB; the vendor survey's largest file, 15 KiB). apt has no limit, and
// keyring files are the operator's trusted input; the cap only keeps a
// file that is no keyring (a disk image named .gpg) from being read whole
// at startup, and room for decades of debian-keyring's growth.
//
// AIDEV-NOTE: the re-review's V-R2M2: round 1's 16 MiB cap skipped
// debian-keyring.gpg with a WARN at every start
// (TestParseKeyring_ReadsADebianKeyringSizedFile loads a generated file
// past its size).
const keyringMaxFile = 256 << 20

// parseKeyringReader reads one key file whole, under keyringMaxFile, and
// reads its keys: parseKeyring.
func parseKeyringReader(r io.Reader) (*keyFile, error) {
	return parseKeyringReaderMax(r, keyringMaxFile)
}

// parseKeyringReaderMax is parseKeyringReader with the cap given (tests
// read past a small one).
func parseKeyringReaderMax(r io.Reader, maxFile int) (*keyFile, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(maxFile)+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("larger than %d octets", maxFile)
	}
	return parseKeyring(b)
}

// parseKeyring reads the keys of one key file, b, as the supported apt read
// them: every key any of them reads (the third re-review's V-R3C1). A file
// is armored when its first octet is text (an OpenPGP packet's first octet
// has its top bit set), and dearmorKeyring reads its blocks as apt's two
// text readers do; any other file is a binary keyring (readKeyring). Either
// way the keyring is read as gpgv reads it, key by key, up to the first
// octet that is no whole packet (the fourth re-review's V-R4C1). An empty
// file holds no key and is no error, as apt reads it.
//
// AIDEV-NOTE: the controller's ruling (Task 9b's fix round 3): the keyring
// is trusted operator input. The cache reads every key that any supported
// apt reads from a key file, and refuses a key file only where every apt
// fails it; one reader feeds one key list, which S1 and S2 both use. S1 is
// complete only if it knows every key some apt knows: otherwise a key-free
// bad signature beside a good one is adopted where that apt refuses it.
// Counting a good signature by a key that only some apt read needs that
// key's private half, so it only makes the cache more permissive than apt
// 3.2 for an odd file the operator installed. History: main read an armored
// file's first block with go-crypto's armor decoder; fix round 1 read every
// public key block and refused anything else; fix round 2 read the file as
// apt 3 does, and still skipped keys apt-key or sqv read (the third
// re-review's section 3, measured in all six images): a key whose base64
// lines run past 96 octets, or with a header line without a colon, a
// checksum line not of five characters or a "-x" body line (go-crypto's
// decoder refused them, every gpgv-based apt reads them), a block without
// its END line (apt-key reads it), key packets in a PRIVATE KEY or SIGNATURE
// block (apt-key and sqv read them) — each a key S1 did not know. A key is
// refused only where every apt fails it, each measured REJ in all six
// images: the key whose packets run into bytes that are no whole packet —
// a line after its block's checksum line (kr6_after_crc: the awk and apt 3
// fold it into the base64), bytes after a binary keyring's packets
// (kr_gpg_trailing_lf, kr_gpg_trailing_junk), binary packets followed by
// armor (kr8_asc_bin_A_then_armor_A, kr8_asc_bin_E_then_armor_A,
// kr8_gpg_bin_A_then_armor_A) — and whatever follows it. Fix round 3 refused
// the whole file for such bytes, which holds for a file of one key only:
// gpgv reads a keyring keyblock by keyblock, each ended by the next key's
// primary key packet, and loses only the keyblock the junk interrupts (the
// fourth re-review's V-R4C1, its kr12 and kr13 rows: A, E, then junk reads
// A in all five gpgv-based apt; E, A, then junk none; A's block then E's
// with a line after its checksum reads A; E, S, then junk beside A in
// trusted.gpg.d has E's bad signature BADSIG in all five, which the cache
// adopted). So each reading keeps the keys before the junk (readKeyring), a
// file is refused only when no reading yields a key (no key at all, or the
// junk breaking the only one), and LoadKeyring logs a refusal, or a key
// lost to junk, at WARN. All 70 surveyed vendor files load.
//
// AIDEV-NOTE: where the cache reads more than some apt, the gaps left
// (each one a file a client that cannot read fails with or without the
// cache; S1 is only stricter, and S2 trusts the file's keys, as the ruling
// accepts):
//   - content, not the name (the re-review's V-R2M3): apt dearmors a
//     ".asc" and needs any other keyring to start with a public key
//     packet's first octet, 0x98, 0x99 or 0xc6 (apt-key's
//     is_supported_keyring, apt 3's dearmorKeyOrCheckFormat; sqv, handed a
//     signed-by file, reads either). An armored file named .gpg, a binary
//     one named .asc, and a binary keyring whose first packet is framed
//     otherwise (0x9a) or is a secret key load here, and not from apt's
//     trusted.gpg.d.
//   - a CRLF .asc, or blanks around its lines: apt 2.2, 2.4 and 2.6 read no
//     key from it (apt-key's awk wants an empty line, "\r" is not one, and
//     base64 -d refuses a blank; measured for CRLF), apt 2.7 and 3 do.
//   - keys apt 3.2 does not read and apt-key (2.2 to 2.8) or debian:13's
//     sqv does: key packets in a block of any other type than PUBLIC KEY
//     BLOCK (a PRIVATE KEY BLOCK, a SIGNATURE block; of a secret-key export,
//     its public part), and a block without its END line (apt 3 ignores the
//     whole file).
//
// What is left of the other direction (some apt reads a key the cache does
// not, so a bad signature by it beside a good one is adopted where that apt
// refuses it; each needs a key go-crypto cannot use, which is why it is
// Open: the fourth re-review's V-R4M2):
//   - a key go-crypto does not read: an algorithm it lacks, a primary key
//     that cannot sign, no user ID with a self-signature it verifies (a key
//     revocation it cannot verify, a designated revoker's or a corrupt one,
//     no longer drops the key: the strict re-read drops the signature and
//     marks the key for S2, keyBlock.marks);
//   - a subkey whose only binding go-crypto cannot verify (one hashed with
//     RIPEMD-160, which its packet reader skips): the subkey is dropped
//     (keyBlock.packets keeps a subkey only with a binding), where gpg
//     lists it; in two of the six Debian keys below those are revoked
//     authentication subkeys, which gpg lists as revoked (the fifth
//     re-review's V-R5M3), so nothing gpg would use is lost there; S1 does
//     not know such a subkey, and S2 cannot count it (fail closed, as
//     ruling R2 asks);
//   - what apt-key's gpgv reads from a file whose awk reading the cache
//     stops at junk and apt 3's reading lacks: nothing measured (apt-key
//     reads no partial decode: base64 -d's failure ends it, dearmorAs).
//
// And the ruling R2 outcome (keyBlock.marks): a key or subkey on which stands
// a revocation or a newer self-signature the cache cannot read is known to
// S1 and never counted by S2, where gpgv may count it (an unreadable
// revocation that does not verify for gpgv either, say): the cache refuses
// an anchor some apt accepts, by that key alone. Measured over
// debian-keyring, debian-maintainers and the 70 surveyed vendor files: one
// mark, on a debian-keyring subkey the reader drops anyway (keyBlock.marks'
// note), so no loaded key or subkey is marked.
//
// A key beside packets that are no part of it, which go-crypto alone drops,
// is read (readKeyBlocks): of the real debian-keyring.pgp (2025.07.26) the
// loader reads 937 keys where go-crypto's ReadKeyRing reads 933, of
// debian-maintainers.pgp 249 where it reads 247 (it dropped each for a
// subkey it cannot read, whose binding it then took for the previous
// subkey's; for a subkey whose one binding uses RIPEMD-160; for a
// certification after a subkey).
func parseKeyring(b []byte) (*keyFile, error) {
	if len(b) == 0 {
		return &keyFile{}, nil
	}
	if b[0]&0x80 == 0 {
		return dearmorKeyring(b)
	}
	kf, err := readKeyring(b)
	if err != nil {
		return nil, fmt.Errorf("binary: %w", err)
	}
	return kf, nil
}

// readKeyring reads the keys of one joined keyring, data, as gpgv reads it:
// keyblock by keyblock (readKeyBlocks), up to the first octet that is no
// whole packet (packetsOf's error). The key whose packets run into it, the
// last to open before it, and whatever follows, gpgv does not read, and nor
// does the cache (keyFile.broken, which LoadKeyring warns of). A keyring of
// which no key is read is refused, naming the junk when there is some.
func readKeyring(data []byte) (*keyFile, error) {
	pkts, broken := packetsOf(data)
	if broken != nil {
		pkts = pkts[:lastKeyStart(pkts)]
	}
	kf, err := readKeyBlocks(pkts)
	if err != nil {
		if broken != nil {
			return nil, fmt.Errorf("no key whose packets end before %w", broken)
		}
		return nil, err
	}
	kf.broken = broken
	return kf, nil
}

// lastKeyStart is the index in pkts of the last primary key packet, public
// or secret, where a key opens (keyBlocksOf); 0 when there is none.
func lastKeyStart(pkts []rawPacket) int {
	for i := len(pkts) - 1; i >= 0; i-- {
		if pkts[i].tag == tagPublicKey || pkts[i].tag == tagSecretKey {
			return i
		}
	}
	return 0
}

// The armor lines an armored key file's blocks open and close at (a line,
// stripped of its blanks, starting with one), and the one apt 3 opens a
// block at.
var (
	armorBeginLine = []byte("-----BEGIN")
	armorEndLine   = []byte("-----END")
	keyBlockBegin  = []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")
)

// dearmorKeyring reads the keys of an armored key file as apt-key's awk
// reads its blocks (dearmorAs: every block, of any type, joined into one
// keyring, read as gpgv reads it: readKeyring) and, where that reading fails
// or loses a key to junk (keyFile.broken), also as apt 3's
// dearmorKeyOrCheckFormat reads them (public key blocks only): the union of
// the two readings, each key once (keyFile.add). A block of another type
// that apt 3 skips (a clearsigned message's text, a SIGNATURE block of
// junk, or with a line after its checksum line) so cannot cost the file the
// keys apt 3 reads after it, nor the keys apt-key's gpgv reads before it
// from such blocks (the fourth re-review's V-R4C1: fix round 3 took apt 3's
// reading only when the awk's failed whole); where base64 -d refuses the
// awk's input and apt-key reads nothing, apt 3's reading alone stands. When
// the awk reading is whole, apt 3 reads no key it does not: apt 3's blocks
// are among the awk's, each whole. Where both readings fail, so does the
// file, with the awk's reason.
func dearmorKeyring(b []byte) (*keyFile, error) {
	var out *keyFile
	var firstErr error
	for _, apt3 := range []bool{false, true} {
		if apt3 && out != nil && out.broken == nil {
			break
		}
		data, err := dearmorAs(b, apt3)
		if err == nil {
			var kf *keyFile
			if kf, err = readKeyring(data); err == nil {
				if out == nil {
					out = kf
				} else {
					out.add(kf)
				}
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if out == nil {
		return nil, fmt.Errorf("armored: %w", firstErr)
	}
	return out, nil
}

// dearmorAs reads the blocks of an armored key file as one of apt's text
// readers does: apt-key's awk (apt 2.2 to 2.8's dearmor_keyring,
// `/^-----BEGIN/{x=1} /^$/{if(x==1)x=2} /^[^=-]/{if(x==2)print}
// /^-----END/{x=0}`, then base64 -d) or, with apt3, apt 3's
// dearmorKeyOrCheckFormat (3.2.0 apt-pkg/contrib/gpgv.cc:316-346). Over
// lines stripped of their blanks (a CR included), as apt 3 strips them: a
// block opens at a line starting "-----BEGIN" (apt 3: only "-----BEGIN PGP
// PUBLIC KEY BLOCK-----"), also inside a block, which restarts its header
// lines and keeps what was read of it; its header lines run to the first
// empty line, whatever they hold; then every line starting with neither "="
// (the checksum) nor "-" is base64; a line starting "-----END" closes the
// block. A block without its END line runs on into what follows, as the awk
// reads it; apt 3 ignores such a file (its state at the end must be after
// an END line). Every block's base64 is joined and decoded quantum by
// quantum (base64Quanta). A line after a block's checksum line is folded
// into the base64, as both readers fold it (kr6_after_crc): it decodes to
// octets that are no packet, where the keyring ends for gpgv and for the
// cache (readKeyring), the key they follow lost (measured REJ in all six
// images for a file of that key alone; the keys before it read in the five
// gpgv-based apt: the fourth re-review's kr12 rows). The awk's base64 goes
// to GNU base64 -d, which refuses a line holding any octet outside its
// alphabet (a blank, a CR, a "!") and, on the supported images, a final
// quantum short of four characters (a file cut inside a base64 line), and
// apt-key, under `set -e`, then reads no key from the file at all (the
// fourth re-review's section 4, its kr12_fb rows: not even the keys before
// it): that reading fails here too, where apt 3's decoder passes over the
// octet or the quantum (measured in the six images, Task 9b's fix round 4:
// kr15_asc_A_blank and kr15_asc_AE_sigjunk_S, apt-key none and apt 3.2 every
// key; kr15_asc_A_Ecut_q2 against q0, two characters apart, apt-key aborting
// where it ran gpgv).
func dearmorAs(b []byte, apt3 bool) ([]byte, error) {
	const (
		outside = iota // no block open (apt 3: before the first)
		header         // a block's header lines, to the first empty line
		body           // a block's base64 lines
		closed         // apt 3: after an END line
	)
	var b64 []byte
	state, blocks := outside, 0
	for off, n := 0, 1; off < len(b); n++ {
		next := len(b)
		if i := bytes.IndexByte(b[off:], '\n'); i >= 0 {
			next = off + i + 1
		}
		raw := bytes.TrimSuffix(b[off:next], []byte("\n")) // the line as base64 -d reads it
		l := bytes.Trim(raw, " \t\v\f\r")                  // apt's String::Strip: isspace
		off = next
		begin := bytes.HasPrefix(l, armorBeginLine)
		if apt3 {
			begin = bytes.HasPrefix(l, keyBlockBegin)
		}
		switch {
		case begin:
			state = header
			blocks++
		case state == header && len(l) == 0:
			state = body
		case state == body && len(l) > 0 && l[0] != '=' && l[0] != '-':
			if !apt3 {
				for _, c := range raw {
					if base64Rank[c] == 0xff {
						return nil, fmt.Errorf("line %d: an octet (0x%02x) base64 -d refuses, after which apt-key reads no key from the file", n, c)
					}
				}
			}
			b64 = append(b64, l...)
		case bytes.HasPrefix(l, armorEndLine):
			state = outside
			if apt3 {
				state = closed
			}
		}
	}
	switch {
	case blocks == 0 && apt3:
		return nil, errors.New("no public key block")
	case blocks == 0:
		return nil, errors.New("no armor block")
	case apt3 && state != closed:
		return nil, errors.New("a public key block without its END line")
	case !apt3 && len(b64)%4 != 0:
		return nil, errors.New("a partial final base64 quantum, which base64 -d refuses, after which apt-key reads no key from the file")
	}
	return base64Quanta(b64), nil
}

// base64Rank is apt's Base64Decode alphabet (3.2.0
// apt-pkg/contrib/strutl.cc): each character's six bits, "=" counting as
// zero bits, 0xff for every octet outside it.
var base64Rank = func() (r [256]byte) {
	for i := range r {
		r[i] = 0xff
	}
	for i, c := range []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/") {
		r[c] = byte(i)
	}
	r['='] = 0
	return r
}()

// base64Quanta is apt 3's Base64Decode (3.2.0 apt-pkg/contrib/strutl.cc,
// from dpkg): an octet outside the alphabet is skipped, and each four
// characters give three octets, two when the third is "=", one when the
// fourth is too. Padding is so read quantum by quantum: padded base64
// streams joined decode to their octets joined, as the blocks of a file do
// whose first lacks its END line. A trailing partial quantum gives nothing.
func base64Quanta(s []byte) []byte {
	out := make([]byte, 0, len(s)/4*3)
	var v uint32
	var n int
	var third, fourth byte
	for _, c := range s {
		r := base64Rank[c]
		if r == 0xff {
			continue
		}
		third, fourth = fourth, c
		v = v<<6 | uint32(r)
		if n++; n < 4 {
			continue
		}
		out = append(out, byte(v>>16))
		if third != '=' {
			out = append(out, byte(v>>8))
		}
		if fourth != '=' {
			out = append(out, byte(v))
		}
		v, n = 0, 0
	}
	return out
}

// OpenPGP packet tags (RFC 9580 §5) the key reader tells apart.
const (
	tagSignature     = 2
	tagSecretKey     = 5
	tagPublicKey     = 6
	tagSecretSubkey  = 7
	tagUserID        = 13
	tagPublicSubkey  = 14
	tagUserAttribute = 17
)

// rawPacket is one packet of a joined keyring: its tag, its octets (header
// included) and its body.
type rawPacket struct {
	tag       byte
	raw, body []byte
}

// packetsOf splits b into its OpenPGP packets, whole and of definite lengths
// (RFC 9580 §4.2: old-format one-, two- or four-octet lengths, new-format
// one-, two- or five-octet ones), any tag: a keyring as gpgv reads it. At
// the first octet that starts no whole packet it returns the packets before
// it with the error naming it (readKeyring reads the keys among them, as
// gpgv does). The framing is layer P1's (sigpacket.go, sigPacketHeader), for
// every tag.
func packetsOf(b []byte) ([]rawPacket, error) {
	var out []rawPacket
	for off := 0; off < len(b); {
		h := b[off]
		if h&0x80 == 0 {
			return out, fmt.Errorf("offset %d: an octet (0x%02x) that starts no OpenPGP packet", off, h)
		}
		var tag byte
		var hl int
		var n uint64
		if h&0x40 != 0 {
			tag = h & 0x3f
			if len(b)-off < 2 {
				return out, fmt.Errorf("offset %d: a truncated packet header", off)
			}
			switch l0 := b[off+1]; {
			case l0 < 192:
				hl, n = 2, uint64(l0)
			case l0 < 224:
				if len(b)-off < 3 {
					return out, fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 3, (uint64(l0)-192)<<8+uint64(b[off+2])+192
			case l0 == 255:
				if len(b)-off < 6 {
					return out, fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 6, uint64(binary.BigEndian.Uint32(b[off+2:off+6]))
			default:
				return out, fmt.Errorf("offset %d: a partial packet length", off)
			}
		} else {
			tag = (h >> 2) & 0x0f
			octets := [4]int{1, 2, 4, 0}[h&0x03]
			switch {
			case octets == 0:
				return out, fmt.Errorf("offset %d: an indeterminate packet length", off)
			case len(b)-off < 1+octets:
				return out, fmt.Errorf("offset %d: a truncated packet header", off)
			}
			for _, c := range b[off+1 : off+1+octets] {
				n = n<<8 | uint64(c)
			}
			hl = 1 + octets
		}
		if n > uint64(len(b)-off-hl) {
			return out, fmt.Errorf("offset %d: a packet running past the end", off)
		}
		end := off + hl + int(n)
		out = append(out, rawPacket{tag: tag, raw: b[off:end], body: b[off+hl : end]})
		off = end
	}
	return out, nil
}

// keyBlock is one key of a joined keyring, as readKeyBlocks hands it to
// go-crypto: its public primary key packet, the key, and its components in
// the keyring's order, each with the signatures that may stand on it; and
// the key's designated revokers (classify): the fingerprints the
// revocation-key subpackets of its self-signatures name, and whether a
// self-signature's subpackets could not be read whole (anyRevoker: then any
// third party may be one, failing closed).
type keyBlock struct {
	primary    []byte
	key        *packet.PublicKey
	components []keyComponent
	revokers   [][]byte
	anyRevoker bool
}

// The components a keyBlock's signatures stand on.
const (
	onPrimary = iota // the primary key itself: its revocations, direct-key signatures
	onUserID         // a user ID: its certifications and their revocations
	onSubkey         // a subkey: its bindings and revocations
)

// keyComponent is one component of a keyBlock: the primary key's own
// signatures, a user ID (raw, uid) or a subkey (raw, key, public), with its
// signatures.
type keyComponent struct {
	kind int
	raw  []byte
	uid  string
	key  *packet.PublicKey
	sigs []keySig
}

// keySig is one signature packet standing on a component of a keyBlock, of
// a type go-crypto reads there (sigFits): the packet, and what the reader
// made of it (keySig.read): whether it is the key's own, when it was made,
// and whether go-crypto keeps it on the component without failing the key
// (ok), else why not.
type keySig struct {
	raw     rawPacket
	typ     byte
	legacy  bool      // a version 2 or 3 signature, kept for marks alone (never handed to go-crypto)
	self    bool      // its issuer is the key itself, or cannot be read
	issuer  rawSig    // its issuer's fingerprint and key ID as read (keyBlock.designated)
	created time.Time // its creation time; zero when it cannot be read
	ok      bool
	why     string
}

// readKeyBlocks reads the keys of a joined keyring, key by key, so that a
// packet beside a key cannot make go-crypto drop it (the third re-review's
// V-R3C1). go-crypto's ReadEntity drops a whole key for one packet it does
// not expect where it stands: a document signature after a user ID or a
// subkey (a detached signature appended in its own SIGNATURE block,
// kr_asc_plus_sigblock, which every apt reads), a signature by the key
// that does not verify there, a subkey without a binding, a critical
// unknown packet. gpgv reads the key and passes over such a packet. So the
// keyring is first cut into keyBlocks (keyBlocksOf): only what go-crypto
// reads as part of a key is kept, a secret key's public part in its
// place. Each is read with the signatures that stand where they are, by
// their type; if go-crypto still refuses it, again with only the
// signatures it keeps there (keySig.ok: those it parses, and of the key's
// own on a user ID or subkey, or any on the primary key, only those that
// verify), a subkey kept only with a binding that does (keyBlock.packets);
// a key go-crypto refuses both ways is skipped. A keyring of which no key
// is read is refused. Whichever read succeeds, a signature the cache
// cannot use that gpgv would is marked for S2 (keyBlock.marks).
//
// AIDEV-NOTE: go-crypto's ReadEntity under this reader (v1.5.2). Three of
// v1.5.0's changes beneath ReadKeyRing (the third re-review's V-R3M3)
// change no real key measured (go-crypto's ReadKeyRing reads the same 933
// keys and 2244 subkeys of debian-keyring.gpg under v1.4.1 and v1.5.2; the
// 98 surveyed vendor keys alike):
//   - a self-signature is recognised by CheckKeyIdOrFingerprint, which now
//     requires an issuer fingerprint subpacket's (33) version to be the
//     key's (signature.go:752, keys.go:616): a user ID left without one is
//     no identity, and a key left with no identity is dropped;
//   - an ECDH key whose KDF hash is shorter than its KEK is a parse error,
//     for which go-crypto's ReadKeyRing dropped the whole key, signing
//     subkeys and all; keyBlocksOf drops that subkey alone;
//   - post-quantum (ML-KEM-768+X25519) v4 subkeys parse, which v1.4.1
//     skipped.
//
// Re-check these, and sigFits' types and positions (go-crypto's addUserID
// and addSubkey), when go-crypto is upgraded.
func readKeyBlocks(pkts []rawPacket) (*keyFile, error) {
	out := &keyFile{}
	var lastErr error
	for _, kb := range keyBlocksOf(pkts) {
		kb.classify()
		e, err := readEntity(kb.packets(false))
		if err != nil {
			e, err = readEntity(kb.packets(true))
		}
		if err != nil {
			lastErr = err
			continue
		}
		out.addEntity(e)
		out.addMarks(kb.marks())
	}
	if len(out.entities) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("no key go-crypto reads: %w", lastErr)
		}
		return nil, errors.New("no key")
	}
	return out, nil
}

// readEntity is go-crypto's ReadEntity of one keyBlock's packets.
func readEntity(b []byte) (*openpgp.Entity, error) {
	return openpgp.ReadEntity(packet.NewReader(bytes.NewReader(b)))
}

// keyBlocksOf cuts a joined keyring into its keys: a key opens at a primary
// key packet, public or secret, and runs to the next. Within it, a user ID
// or a subkey (public or secret) opens a component, and a signature stands
// on the last component opened, kept when its version is the key's and its
// type one go-crypto reads there (sigFits). Dropped: a key whose primary
// go-crypto does not read (an algorithm it lacks: gpgv cannot use it
// either) and a subkey likewise, with its signatures; a user attribute
// (go-crypto keeps none) with its signatures; and every other packet (a
// trust packet, a marker, literal data, a packet before the first key),
// none of them part of a key.
func keyBlocksOf(pkts []rawPacket) []*keyBlock {
	var blocks []*keyBlock
	var kb *keyBlock
	at := -1 // the component a signature stands on; -1: none, it is dropped
	for _, p := range pkts {
		switch p.tag {
		case tagPublicKey, tagSecretKey:
			kb, at = nil, -1
			raw, key, ok := publicKeyOf(p, false)
			if !ok {
				continue
			}
			kb = &keyBlock{primary: raw, key: key, components: []keyComponent{{kind: onPrimary}}}
			blocks = append(blocks, kb)
			at = 0
		case tagUserID:
			if kb == nil {
				continue
			}
			kb.components = append(kb.components, keyComponent{kind: onUserID, raw: p.raw, uid: string(p.body)})
			at = len(kb.components) - 1
		case tagPublicSubkey, tagSecretSubkey:
			if kb == nil {
				continue
			}
			at = -1
			raw, key, ok := publicKeyOf(p, true)
			if !ok {
				continue
			}
			kb.components = append(kb.components, keyComponent{kind: onSubkey, raw: raw, key: key})
			at = len(kb.components) - 1
		case tagSignature:
			if kb == nil || at < 0 {
				continue
			}
			if t, ok := sigTypeOf(p, kb.key.Version); ok && sigFits(kb.components[at].kind, t) {
				kb.components[at].sigs = append(kb.components[at].sigs, keySig{raw: p, typ: t})
			} else if t, ok := legacySigType(p); ok && legacyFits(kb.components[at].kind, t) {
				kb.components[at].sigs = append(kb.components[at].sigs, keySig{raw: p, typ: t, legacy: true})
			}
		case tagUserAttribute:
			at = -1
		}
	}
	return blocks
}

// publicKeyOf reads p, a public or secret (sub)key packet, as go-crypto
// reads a public one, and returns its public key packet and the key: a
// public packet's own octets; for a secret one (a secret-key export
// appended to a key file, whose public part apt-key's gpgv uses), its
// public part re-serialized: a secret key packet's body opens with its
// public key's (RFC 9580 §5.5.3), which go-crypto reads as a public key's
// body, its secret material behind. The cache keeps no secret.
func publicKeyOf(p rawPacket, subkey bool) ([]byte, *packet.PublicKey, bool) {
	src := p.raw
	secret := p.tag == tagSecretKey || p.tag == tagSecretSubkey
	if secret {
		tag := byte(tagPublicKey)
		if subkey {
			tag = tagPublicSubkey
		}
		src = newPacket(tag, p.body)
	}
	pk, err := packet.Read(bytes.NewReader(src))
	key, ok := pk.(*packet.PublicKey)
	if err != nil || !ok {
		return nil, nil, false
	}
	if !secret {
		return p.raw, key, true
	}
	var buf bytes.Buffer
	if err := key.Serialize(&buf); err != nil {
		return nil, nil, false
	}
	return buf.Bytes(), key, true
}

// newPacket frames body as a new-format packet of tag (RFC 9580 §4.2.1).
func newPacket(tag byte, body []byte) []byte {
	out := []byte{0xc0 | tag}
	switch n := len(body); {
	case n < 192:
		out = append(out, byte(n))
	case n < 8384:
		out = append(out, byte((n-192)>>8+192), byte(n-192))
	default:
		out = append(out, 0xff)
		out = binary.BigEndian.AppendUint32(out, uint32(n))
	}
	return append(out, body...)
}

// sigTypeOf returns the signature type of p, a signature packet whose
// version is the key's (go-crypto reads a v4 key's v4 signatures, a v6
// key's v6 ones; a version 2 or 3 signature it passes over: an
// UnsupportedError its packet reader skips; legacySigType keeps those the
// reader marks for).
func sigTypeOf(p rawPacket, version int) (byte, bool) {
	if len(p.body) < 2 || int(p.body[0]) != version {
		return 0, false
	}
	return p.body[1], true
}

// legacySigType returns the signature type of p when p is a version 2 or 3
// signature packet (RFC 4880 §5.2.2: the version, the hashed material's
// length (5), the type, a four-octet creation time, the issuer's eight-octet
// key ID, the algorithms, two octets of the digest and the signature; gpg
// reads version 2 as version 3) of a type the reader marks for: a subkey
// binding (0x18) or its back-signature (0x19), a direct-key signature (0x1f),
// a revocation of a key (0x20), of a subkey (0x28) or of a user ID (0x30).
// go-crypto passes every such packet over (an UnsupportedError its reader
// skips), so the reader keeps it for marks alone (keySig.legacy): it is never
// handed to go-crypto, and never counts as a binding or a self-signature. A
// legacy certification (0x10 to 0x13) is dropped: measured harmless
// (kr21_v3_cert_newer, ok in all six images).
//
// AIDEV-NOTE: the controller's design ruling for Task 9b's fix round 6
// (the sixth re-review's V-R6I1 and V-R6I2): fewer, broader fail-closed
// rules for version 2 and 3 signatures, not a closer copy of gpgv. Rounds
// 4, 5 and 6 each found another hand-built legacy shape gpgv honours and
// the reader passed over (a v3 self-revocation; a v2 one; a newer v3 or v2
// binding, which gpgv takes as the subkey's and refuses as not
// cross-certified, ERRSIG in the five gpgv-based apt). No real file holds
// a version 2 or 3 signature (V's v23scan over debian-keyring,
// debian-maintainers and the 70 surveyed vendor files), so over-strictness
// here costs nothing real: a legacy signature of these types marks the
// component it stands on whoever its issuer and whatever its age (marks),
// with no issuer or time parsing. The rows that makes over-strict, each
// Minor and Open under ruling R3: a third party's legacy 0x20 or 0x28 the
// key never designated (kr21_tp_v3rev_xin, ok ×6), and a legacy binding
// older than the one kept (unmeasured). A legacy 0x30 keeps round 5's rule
// (the key's own marks the key).
func legacySigType(p rawPacket) (byte, bool) {
	if len(p.body) < 19 || p.body[0] != 3 && p.body[0] != 2 || p.body[1] != 5 {
		return 0, false
	}
	switch t := packet.SignatureType(p.body[2]); t {
	case packet.SigTypeSubkeyBinding, packet.SigTypePrimaryKeyBinding, packet.SigTypeDirectSignature,
		packet.SigTypeKeyRevocation, packet.SigTypeSubkeyRevocation, packet.SigTypeCertificationRevocation:
		return byte(t), true
	}
	return 0, false
}

// legacyFits reports whether a legacy signature of type t (legacySigType)
// is kept on a component of kind: any but a user-ID revocation on the
// primary key or a subkey (it marks whatever stands there), a user-ID
// revocation on a user ID.
func legacyFits(kind int, t byte) bool {
	if t == byte(packet.SigTypeCertificationRevocation) {
		return kind == onUserID
	}
	return kind == onPrimary || kind == onSubkey
}

// sigFits reports whether go-crypto reads a signature of type t on a
// component of kind: on the primary key a key revocation or a direct-key
// signature (go-crypto passes over any other type there, so none is lost);
// on a user ID a certification or its revocation; on a subkey a binding or
// its revocation (go-crypto's ReadEntity, addUserID and addSubkey fail the
// key for any other type).
func sigFits(kind int, t byte) bool {
	switch kind {
	case onPrimary:
		return t == byte(packet.SigTypeKeyRevocation) || t == byte(packet.SigTypeDirectSignature)
	case onUserID:
		return t >= byte(packet.SigTypeGenericCert) && t <= byte(packet.SigTypePositiveCert) ||
			t == byte(packet.SigTypeCertificationRevocation)
	case onSubkey:
		return t == byte(packet.SigTypeSubkeyBinding) || t == byte(packet.SigTypeSubkeyRevocation)
	}
	return false
}

// packets returns kb's packets as go-crypto's ReadEntity reads them: its
// primary key, then each component with the signatures that stand on it, a
// subkey only with a binding among them. With strict, only the signatures
// go-crypto keeps there (keySig.ok).
func (kb *keyBlock) packets(strict bool) []byte {
	size := len(kb.primary)
	for _, c := range kb.components {
		size += len(c.raw)
		for _, s := range c.sigs {
			size += len(s.raw.raw)
		}
	}
	out := append(make([]byte, 0, size), kb.primary...)
	for _, c := range kb.components {
		var sigs []byte
		bound := false
		for _, s := range c.sigs {
			if s.legacy || strict && !s.ok {
				continue
			}
			bound = bound || s.typ == byte(packet.SigTypeSubkeyBinding)
			sigs = append(sigs, s.raw.raw...)
		}
		if c.kind == onSubkey && !bound {
			continue
		}
		out = append(out, c.raw...)
		out = append(out, sigs...)
	}
	return out
}

// classify reads every signature of kb's components (keySig.read).
func (kb *keyBlock) classify() {
	for i := range kb.components {
		c := &kb.components[i]
		for j := range c.sigs {
			s := &c.sigs[j]
			s.read(kb, c)
			// The key's designated revokers: named by a revocation-key
			// subpacket of any of its own direct-key signatures or
			// self-certifications, read or not (gpg reads them from the
			// hashed area; a self-signature whose subpackets cannot be
			// walked may name any key: anyRevoker).
			if s.self && !s.legacy && (c.kind == onPrimary && s.typ == byte(packet.SigTypeDirectSignature) ||
				c.kind == onUserID && supersedes(s.typ) == byte(packet.SigTypePositiveCert)) {
				r := rawSigInfo(s.raw.body)
				kb.revokers = append(kb.revokers, r.revokers...)
				kb.anyRevoker = kb.anyRevoker || !r.whole
			}
		}
	}
}

// designated reports whether s, a signature by another key, is a designated
// revoker's: either its issuer fingerprint (subpacket 33) or its issuer key
// ID (16, a fingerprint's last eight octets) names one of kb.revokers (the
// sixth re-review's V-R6I3: a disagreement between the two fails closed, as
// rawSig.issuerIs); or kb.anyRevoker.
func (kb *keyBlock) designated(s *keySig) bool {
	if kb.anyRevoker {
		return true
	}
	for _, r := range kb.revokers {
		if s.issuer.fpr != nil && bytes.Equal(r, s.issuer.fpr) ||
			s.issuer.keyID != nil && bytes.Equal(r[len(r)-8:], s.issuer.keyID) {
			return true
		}
	}
	return false
}

// read fills in what the reader makes of s on c: whether go-crypto parses
// it, whether it is the key's own (its issuer the key: go-crypto's
// CheckKeyIdOrFingerprint, or the raw subpackets' issuer where go-crypto
// does not parse it; one whose issuer cannot be read at all counts as the
// key's own), its creation time, and whether go-crypto keeps it on c
// without failing the key (ok): on the primary key only the key's own
// revocation or direct-key signature that verifies (ReadEntity fails the
// key for a revocation by another key, "signed by alternate key", and
// passes over a direct-key signature that does not verify); on a user ID
// another key's certification unverified, or the key's own that verifies
// (addUserID); on a subkey a binding or revocation that verifies by the
// primary key, whoever its issuer says (addSubkey).
func (s *keySig) read(kb *keyBlock, c *keyComponent) {
	if s.legacy {
		// go-crypto reads no version 2 or 3 signature; its issuer key ID
		// stands at body[7:15] (legacySigType), which only a user-ID
		// revocation's mark consults (marks).
		s.issuer = rawSig{keyID: s.raw.body[7:15]}
		s.self = s.issuer.issuerIs(kb.key)
		s.why = fmt.Sprintf("it is a version %d signature, which go-crypto does not read", s.raw.body[0])
		return
	}
	p, err := packet.Read(bytes.NewReader(s.raw.raw))
	sig, isSig := p.(*packet.Signature)
	if err == nil && !isSig {
		err = errors.New("not a signature packet")
	}
	if err != nil {
		s.issuer = rawSigInfo(s.raw.body)
		s.self, s.created = s.issuer.issuerIs(kb.key), s.issuer.created
		s.why = fmt.Sprintf("go-crypto does not read it (%v)", err)
		if len(s.raw.body) > 3 {
			s.why = fmt.Sprintf("go-crypto does not read it (hash algorithm %d: %v)", s.raw.body[3], err)
		}
		return
	}
	s.issuer, s.created = rawSig{fpr: sig.IssuerFingerprint}, sig.CreationTime
	if sig.IssuerKeyId != nil {
		s.issuer.keyID = binary.BigEndian.AppendUint64(nil, *sig.IssuerKeyId)
	}
	// go-crypto's own answer (the fingerprint first, where there is one);
	// a revocation is also the key's own when either issuer names the key
	// (the sixth re-review's V-R6I3, as rawSig.issuerIs), so one whose two
	// disagree fails closed as the key's own that does not verify.
	s.self = sig.CheckKeyIdOrFingerprint(kb.key)
	if t := packet.SignatureType(s.typ); t == packet.SigTypeKeyRevocation || t == packet.SigTypeSubkeyRevocation || t == packet.SigTypeCertificationRevocation {
		s.self = s.self || s.issuer.issuerIs(kb.key)
	}
	var verr error
	switch {
	case c.kind == onPrimary && !s.self:
		verr = fmt.Errorf("its issuer %016X is not the key", issuerOf(sig))
	case c.kind == onPrimary && s.typ == byte(packet.SigTypeKeyRevocation):
		verr = kb.key.VerifyRevocationSignature(sig)
	case c.kind == onPrimary:
		verr = kb.key.VerifyDirectKeySignature(sig)
	case c.kind == onUserID && s.self:
		verr = kb.key.VerifyUserIdSignature(c.uid, kb.key, sig)
	case c.kind == onSubkey:
		verr = kb.key.VerifyKeySignature(c.key, sig)
	}
	if verr != nil {
		s.why = fmt.Sprintf("it does not verify as the key's own (%v)", verr)
		return
	}
	s.ok = true
}

// issuerOf is sig's issuer key ID, 0 when it names none.
func issuerOf(sig *packet.Signature) uint64 {
	if sig.IssuerKeyId == nil {
		return 0
	}
	return *sig.IssuerKeyId
}

// rawSig is what rawSigInfo reads of a v4 (or v6) signature packet's
// subpackets without go-crypto: its issuer fingerprint (33) and key ID (16),
// nil when absent; its creation time (2, hashed; zero when not read); the
// fingerprints its revocation-key subpackets (12, hashed) name; and whether
// both areas were whole subpackets (whole; a short or misframed area stops
// the walk with what was read).
type rawSig struct {
	fpr, keyID []byte
	created    time.Time
	revokers   [][]byte
	whole      bool
}

// issuerIs reports whether the signature's issuer is key: when either its
// issuer fingerprint or its issuer key ID names the key (the sixth
// re-review's V-R6I3: gpgv 2.2 takes a revocation's issuer from its key ID,
// gpgv 2.4 from its fingerprint, so a signature whose two disagree is
// honoured by some apt as the key's own; the cache fails closed on either
// naming it); one naming neither counts as the key's own (it is not known to
// be another's).
func (r rawSig) issuerIs(key *packet.PublicKey) bool {
	if r.fpr == nil && r.keyID == nil {
		return true
	}
	return r.fpr != nil && bytes.Equal(r.fpr, key.Fingerprint) ||
		r.keyID != nil && binary.BigEndian.Uint64(r.keyID) == key.KeyId
}

// name is the issuer as a log names it: its key ID, else its fingerprint
// (a signature may name its issuer by fingerprint alone: the sixth
// re-review's V-R6M2), else "an unnamed key".
func (r rawSig) name() string {
	switch {
	case r.keyID != nil:
		return fmt.Sprintf("%X", r.keyID)
	case r.fpr != nil:
		return fmt.Sprintf("%X", r.fpr)
	}
	return "an unnamed key"
}

// rawSigInfo reads a v4 (or v6) signature packet's body for rawSig: the
// version octet, type, algorithms, then the hashed and unhashed subpacket
// areas, each behind its two-octet (v6: four-octet) length.
func rawSigInfo(body []byte) (r rawSig) {
	if len(body) < 6 {
		return r
	}
	lengthOctets := 2
	if body[0] == 6 {
		lengthOctets = 4
	}
	var areas [][]byte
	off := 4
	for range 2 {
		if len(body) < off+lengthOctets {
			return r
		}
		var n int
		for _, c := range body[off : off+lengthOctets] {
			n = n<<8 | int(c)
		}
		off += lengthOctets
		if n > len(body)-off {
			return r
		}
		areas = append(areas, body[off:off+n])
		off += n
	}
	for i, area := range areas {
		for len(area) > 0 {
			var size, hl int
			switch l0 := area[0]; {
			case l0 < 192:
				size, hl = int(l0), 1
			case l0 < 255 && len(area) >= 2:
				size, hl = (int(l0)-192)<<8+int(area[1])+192, 2
			case l0 == 255 && len(area) >= 5:
				size, hl = int(binary.BigEndian.Uint32(area[1:5])), 5
			}
			if size == 0 || hl == 0 || size > len(area)-hl {
				return r // the area is not whole subpackets
			}
			sub := area[hl : hl+size]
			area = area[hl+size:]
			switch typ, data := sub[0]&0x7f, sub[1:]; {
			case typ == 2 && i == 0 && len(data) == 4 && r.created.IsZero():
				r.created = time.Unix(int64(binary.BigEndian.Uint32(data)), 0)
			case typ == 16 && len(data) == 8 && r.keyID == nil:
				r.keyID = data
			case typ == 33 && len(data) == 21 && r.fpr == nil:
				r.fpr = data[1:]
			case typ == 12 && i == 0 && len(data) == 22: // class, algorithm, a v4 fingerprint
				r.revokers = append(r.revokers, data[2:])
			}
		}
	}
	r.whole = true
	return r
}

// Signature types a key's own signature supersedes by a newer one, and
// so can revoke or expire a component the cache has read as valid
// (keyBlock.marks): a subkey's binding, the key's direct-key signature,
// a user ID's self-certification (four types, one class).
func supersedes(typ byte) byte {
	switch t := packet.SignatureType(typ); {
	case t == packet.SigTypeSubkeyBinding, t == packet.SigTypeDirectSignature:
		return typ
	case t >= packet.SigTypeGenericCert && t <= packet.SigTypePositiveCert:
		return byte(packet.SigTypePositiveCert)
	}
	return 0
}

// marks returns S2's marks on kb's key and subkeys (keyFile.uncountable):
// each component on which stands a signature the cache cannot use (keySig
// not ok) and gpgv would, so that gpgv may treat the component as revoked,
// expired or unbound where the cache reads it valid — a revocation of the
// key (0x20) or of a subkey (0x28) that is the key's own but does not
// verify or cannot be read, or another key's when that key is a designated
// revoker (keyBlock.designated: named by a revocation-key subpacket of one
// of the key's own direct-key signatures or self-certifications, read or
// not; the fifth re-review's V-R5M1, the controller's ruling: a third party
// the key never designated is ignored, as every supported apt ignores it,
// kr17_tp_keyrev ok in all six images. Measured of a designated revoker
// (fix round 5's kr19, the sixth re-review's kr21): the five gpgv-based apt
// honour its revocation of the key, 0x20, REVKEYSIG, and only with the
// revoker's key in the keyring; they ignore its revocation of a subkey,
// 0x28; and gpg reads designations only from the key's own direct-key
// signatures that verify. The cache marks wider, to fail closed: a
// designated 0x20 without the revoker's key, a designated 0x28, a
// designation in a direct-key signature that does not verify or in a
// user-ID self-certification, and a forged designated revocation, which it
// never verifies; each ok in all six images, each over-strict, Minor and
// Open under ruling R3, the sixth re-review's V-R6M1, needing an unusual
// operator key file and touching no real one); the key's own revocation
// of a user ID (0x30); or the key's own binding, direct-key signature or
// self-certification made no earlier than the newest one the cache kept of
// that class on the component (gpgv takes the newest; it may carry an
// expiry), or whose time cannot be read. A revocation marks its component
// as revoked (pgperrors.ErrKeyRevoked), a superseding signature as expired
// (pgperrors.ErrKeyExpired): what S2 refuses a readable one with. A mark on
// a subkey is the subkey's alone; one on the key or a user ID is the whole
// key's (LoadKeyring extends it to the subkeys).
//
// AIDEV-NOTE: the controller's ruling R2 (Task 9b's fix round 4; the fourth
// re-review's V-R4I1): integrity fails closed. go-crypto's packet reader
// skips a signature it does not parse without a word (an UnsupportedError:
// a RIPEMD-160 hash, since main), and the strict re-read dropped one it
// fails on (a StructuralError: a malformed subpacket, since fix round 3), so
// a key or subkey every gpgv-based apt treats as revoked or expired was
// usable, S2 counting it (V's kr14 rows: K revoked by P, P revoked, a newer
// expired binding of K, each hashed with RIPEMD-160 or with an empty
// reason or a one-octet trust-signature subpacket; REVKEYSIG or EXPKEYSIG
// in the five gpgv-based apt, which debian:13's sqv does not read either).
// The component is marked rather than dropped, so that S1 still knows the
// key and a bad signature by it beside a good one is refused (gpgv's
// BADSIG); the mark is per component, as gpgv's verdict is (a key whose
// old subkey carries an unreadable revocation still counts by its current
// one). The cost: a real key with such an unreadable self-signature stops
// counting; measured over debian-keyring, debian-maintainers and the 70
// surveyed vendor files (Task 9b's fix round 5, after the v3 and
// designated-revoker rules), one mark: debian-keyring's subkey
// DF28DD6CA7AD27A9E3930C999EA0912EDF033203 (of A36878F464108681600CB648
// 44173FA13D058888), an ElGamal encryption subkey whose only binding is
// RIPEMD-160, which the reader drops anyway for want of a binding it reads,
// so no verdict changes; no loaded key or subkey is marked
// (TestVerifier_UnreadableRevocationsAndBindingsAreNotCounted,
// TestParseKeyring_MarksWhatItCannotRead). Not marked: a signature by
// another key that gpgv ignores too (its certification of a user ID, a
// binding it did not make), and a version 2 or 3 certification (sigTypeOf
// and legacySigType pass it over; kr21_v3_cert_newer, ok in all six
// images). A version 2 or 3 binding, back-signature, direct-key signature
// or revocation of the key or a subkey IS marked, whoever its issuer and
// whatever its age (legacySigType's note: the fifth re-review's V-R5I1 and
// the sixth's V-R6I1 and V-R6I2; every supported apt honours a key's own
// v3 or v2 revocation, kr17_v3_keyrev, kr17_v3_subrev, kr20_v2_keyrev,
// kr20_v2_subrev, REJ in the five gpgv-based apt, and gpgv takes a newer v3
// or v2 binding as the subkey's and refuses it as not cross-certified,
// kr21_v3_bind_newer, kr21_v2_bind_newer, ERRSIG); fix round 4 dropped every
// such signature before the marks were made, and round 5's version 3
// revocation rule, precise about the version, the type and the issuer, was
// still under-strict twice.
func (kb *keyBlock) marks() map[string]error {
	var out map[string]error
	mark := func(fp string, err error) {
		if out == nil {
			out = make(map[string]error)
		}
		if _, dup := out[fp]; !dup {
			out[fp] = err
		}
	}
	key := upperFP(kb.key.Fingerprint)
	// Revocations first, over every component, then superseding signatures:
	// a component both revoked and expired is reported revoked, as gpgv
	// reports REVKEYSIG before EXPKEYSIG (the first mark on a fingerprint
	// stands).
	for _, c := range kb.components {
		fp, what := key, "the key"
		if c.kind == onSubkey {
			fp, what = upperFP(c.key.Fingerprint), "the subkey"
		}
		for _, s := range c.sigs {
			if s.ok {
				continue
			}
			switch t := packet.SignatureType(s.typ); {
			case s.legacy && t == packet.SigTypeCertificationRevocation:
				if s.self {
					mark(key, fmt.Errorf("%w: the key's revocation of its user ID %q (signature type 0x30) stands on it that %s; gpgv honours it", pgperrors.ErrKeyRevoked, c.uid, s.why))
				}
			case s.legacy && (t == packet.SigTypeKeyRevocation || t == packet.SigTypeSubkeyRevocation):
				mark(fp, fmt.Errorf("%w: a revocation of %s (signature type 0x%02x) stands on it that %s; gpgv honours it whoever its issuer, so the cache fails closed", pgperrors.ErrKeyRevoked, what, s.typ, s.why))
			case s.legacy:
				mark(fp, fmt.Errorf("%w: a signature of type 0x%02x stands on %s that %s; gpgv may take it as the newest binding or direct-key signature and refuse the component, so the cache fails closed whoever its issuer and whatever its age", pgperrors.ErrKeyExpired, s.typ, what, s.why))
			case (t == packet.SigTypeKeyRevocation || t == packet.SigTypeSubkeyRevocation) && !s.self:
				if kb.designated(&s) {
					mark(fp, fmt.Errorf("%w: a revocation of %s (signature type 0x%02x) by a designated revoker, %s, stands on it, which the cache cannot verify; gpgv honours it when it holds that key", pgperrors.ErrKeyRevoked, what, s.typ, s.issuer.name()))
				}
			case t == packet.SigTypeKeyRevocation, t == packet.SigTypeSubkeyRevocation:
				mark(fp, fmt.Errorf("%w: a revocation of %s (signature type 0x%02x) stands on it that %s; gpgv honours it", pgperrors.ErrKeyRevoked, what, s.typ, s.why))
			case t == packet.SigTypeCertificationRevocation && s.self:
				mark(key, fmt.Errorf("%w: the key's revocation of its user ID %q (signature type 0x30) stands on it that %s; gpgv honours it", pgperrors.ErrKeyRevoked, c.uid, s.why))
			}
		}
	}
	for _, c := range kb.components {
		fp, what := key, "the key"
		if c.kind == onSubkey {
			fp, what = upperFP(c.key.Fingerprint), "the subkey"
		}
		newest := make(map[byte]time.Time)
		for _, s := range c.sigs {
			if cls := supersedes(s.typ); s.ok && cls != 0 && s.created.After(newest[cls]) {
				newest[cls] = s.created
			}
		}
		for _, s := range c.sigs {
			if cls := supersedes(s.typ); !s.ok && !s.legacy && cls != 0 && s.self && (s.created.IsZero() || !s.created.Before(newest[cls])) {
				mark(fp, fmt.Errorf("%w: a signature of type 0x%02x by the key on %s, newer than the one the cache kept, stands on it that %s; gpgv takes the newest, and it may expire or unbind the component", pgperrors.ErrKeyExpired, s.typ, what, s.why))
			}
		}
	}
	return out
}
