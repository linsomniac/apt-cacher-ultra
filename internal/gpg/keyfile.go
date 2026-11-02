package gpg

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// parseKeyringFile reads one keyring file (readKeyFile, under
// keyringMaxFile) and its keys: parseKeyring.
func parseKeyringFile(path string) (openpgp.EntityList, error) {
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
func parseKeyringBytes(b []byte) (openpgp.EntityList, error) {
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
func parseKeyringReader(r io.Reader) (openpgp.EntityList, error) {
	return parseKeyringReaderMax(r, keyringMaxFile)
}

// parseKeyringReaderMax is parseKeyringReader with the cap given (tests
// read past a small one).
func parseKeyringReaderMax(r io.Reader, maxFile int) (openpgp.EntityList, error) {
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
// has its top bit set), and dearmorKeyring joins its blocks into one
// keyring; any other file is a binary keyring. Either way the keyring must
// be whole OpenPGP packets (packetsOf), and its keys are read key by key
// (readKeyBlocks). An empty file holds no key and is no error, as apt reads
// it.
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
// block (apt-key and sqv read them) — each a key S1 did not know. A file is
// refused only where every apt fails it, each measured REJ in all six
// images: a line after a block's checksum line (kr6_after_crc: the awk and
// apt 3 fold it into the base64, and every apt then fails the keyring;
// dearmorAs), bytes after a binary keyring's packets (kr_gpg_trailing_lf,
// kr_gpg_trailing_junk) and binary packets followed by armor
// (kr8_asc_bin_A_then_armor_A, kr8_asc_bin_E_then_armor_A,
// kr8_gpg_bin_A_then_armor_A); with them a keyring whose packets are not
// whole (what such bytes make of it) and a file holding no key at all.
// LoadKeyring logs a refusal at WARN and skips the file. All 70 surveyed
// vendor files load.
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
// not): a key go-crypto does not read (an algorithm it lacks, a primary key
// that cannot sign, a key revocation it cannot verify, a designated
// revoker's; no user ID with a self-signature it verifies), and a file both
// of apt's text readings fail but whose blocks before the failure apt-key's
// base64 -d decoded (it stops at the first bad character). A key beside
// packets that are no part of it, which go-crypto alone drops, is read
// (readKeyBlocks): of the real debian-keyring.pgp (2025.07.26) the loader
// reads 937 keys where go-crypto's ReadKeyRing reads 933, of
// debian-maintainers.pgp 249 where it reads 247 (it dropped each for a
// subkey it cannot read, whose binding it then took for the previous
// subkey's; for a subkey whose one binding uses RIPEMD-160; for a
// certification after a subkey).
func parseKeyring(b []byte) (openpgp.EntityList, error) {
	if len(b) == 0 {
		return nil, nil
	}
	kind, data := "binary", b
	if b[0]&0x80 == 0 {
		kind = "armored"
		var err error
		if data, err = dearmorKeyring(b); err != nil {
			return nil, fmt.Errorf("armored: %w", err)
		}
	}
	pkts, err := packetsOf(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	entities, err := readKeyBlocks(pkts)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	return entities, nil
}

// The armor lines an armored key file's blocks open and close at (a line,
// stripped of its blanks, starting with one), and the one apt 3 opens a
// block at.
var (
	armorBeginLine = []byte("-----BEGIN")
	armorEndLine   = []byte("-----END")
	keyBlockBegin  = []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----")
)

// dearmorKeyring joins the blocks of an armored key file into one keyring,
// as apt-key's awk reads them (dearmorAs: every block, of any type), and
// where that reading fails, as apt 3's dearmorKeyOrCheckFormat reads them
// (public key blocks only): a block apt 3 skips (a clearsigned message's
// text, a block whose base64 is no packets) cannot cost the file the keys
// apt 3 reads. Where both readings fail, so does the file, with the first's
// reason.
func dearmorKeyring(b []byte) ([]byte, error) {
	data, err := dearmorAs(b, false)
	if err == nil {
		return data, nil
	}
	if data, err3 := dearmorAs(b, true); err3 == nil {
		return data, nil
	}
	return nil, err
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
// quantum (base64Quanta), and must be whole packets. A line after a block's
// checksum line is refused: both readers fold it into the base64, and every
// apt then fails the keyring (measured REJ in all six images:
// kr6_after_crc).
func dearmorAs(b []byte, apt3 bool) ([]byte, error) {
	const (
		outside = iota // no block open (apt 3: before the first)
		header         // a block's header lines, to the first empty line
		body           // a block's base64 lines
		closed         // apt 3: after an END line
	)
	var b64 []byte
	state, blocks, crc := outside, 0, false
	for off, n := 0, 1; off < len(b); n++ {
		next := len(b)
		if i := bytes.IndexByte(b[off:], '\n'); i >= 0 {
			next = off + i + 1
		}
		l := bytes.Trim(b[off:next], " \t\n\v\f\r") // apt's String::Strip: isspace
		off = next
		begin := bytes.HasPrefix(l, armorBeginLine)
		if apt3 {
			begin = bytes.HasPrefix(l, keyBlockBegin)
		}
		switch {
		case begin:
			state, crc = header, false
			blocks++
		case state == header && len(l) == 0:
			state = body
		case state == body && len(l) > 0 && l[0] != '=' && l[0] != '-':
			if crc {
				return nil, fmt.Errorf("line %d: a line after a block's checksum line", n)
			}
			b64 = append(b64, l...)
		case state == body && len(l) > 0 && l[0] == '=':
			crc = true
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
	}
	data := base64Quanta(b64)
	if _, err := packetsOf(data); err != nil {
		return nil, fmt.Errorf("its blocks' packets: %w", err)
	}
	return data, nil
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

// packetsOf splits b into its OpenPGP packets, which must be whole and of
// definite lengths (RFC 9580 §4.2: old-format one-, two- or four-octet
// lengths, new-format one-, two- or five-octet ones), any tag: a keyring as
// gpgv reads it to its end. The framing is layer P1's (sigpacket.go,
// sigPacketHeader), for every tag.
func packetsOf(b []byte) ([]rawPacket, error) {
	var out []rawPacket
	for off := 0; off < len(b); {
		h := b[off]
		if h&0x80 == 0 {
			return nil, fmt.Errorf("offset %d: an octet (0x%02x) that starts no OpenPGP packet", off, h)
		}
		var tag byte
		var hl int
		var n uint64
		if h&0x40 != 0 {
			tag = h & 0x3f
			if len(b)-off < 2 {
				return nil, fmt.Errorf("offset %d: a truncated packet header", off)
			}
			switch l0 := b[off+1]; {
			case l0 < 192:
				hl, n = 2, uint64(l0)
			case l0 < 224:
				if len(b)-off < 3 {
					return nil, fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 3, (uint64(l0)-192)<<8+uint64(b[off+2])+192
			case l0 == 255:
				if len(b)-off < 6 {
					return nil, fmt.Errorf("offset %d: a truncated packet header", off)
				}
				hl, n = 6, uint64(binary.BigEndian.Uint32(b[off+2:off+6]))
			default:
				return nil, fmt.Errorf("offset %d: a partial packet length", off)
			}
		} else {
			tag = (h >> 2) & 0x0f
			octets := [4]int{1, 2, 4, 0}[h&0x03]
			switch {
			case octets == 0:
				return nil, fmt.Errorf("offset %d: an indeterminate packet length", off)
			case len(b)-off < 1+octets:
				return nil, fmt.Errorf("offset %d: a truncated packet header", off)
			}
			for _, c := range b[off+1 : off+1+octets] {
				n = n<<8 | uint64(c)
			}
			hl = 1 + octets
		}
		if n > uint64(len(b)-off-hl) {
			return nil, fmt.Errorf("offset %d: a packet running past the end", off)
		}
		end := off + hl + int(n)
		out = append(out, rawPacket{tag: tag, raw: b[off:end], body: b[off+hl : end]})
		off = end
	}
	return out, nil
}

// keyBlock is one key of a joined keyring, as readKeyBlocks hands it to
// go-crypto: its public primary key packet, the key, and its components in
// the keyring's order, each with the signatures that may stand on it.
type keyBlock struct {
	primary    []byte
	key        *packet.PublicKey
	components []keyComponent
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
	sigs []rawPacket
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
// signatures it parses, and of the key's own on a user ID or subkey only
// those that verify, a subkey kept only with a binding that does
// (keyBlock.packets); a key go-crypto refuses both ways is skipped. A
// keyring of which no key is read is refused.
func readKeyBlocks(pkts []rawPacket) (openpgp.EntityList, error) {
	var out openpgp.EntityList
	var lastErr error
	for _, kb := range keyBlocksOf(pkts) {
		e, err := readEntity(kb.packets(false))
		if err != nil {
			e, err = readEntity(kb.packets(true))
		}
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
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
				kb.components[at].sigs = append(kb.components[at].sigs, p)
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
// key's v6 ones; a v3 signature it passes over).
func sigTypeOf(p rawPacket, version int) (byte, bool) {
	if len(p.body) < 2 || int(p.body[0]) != version {
		return 0, false
	}
	return p.body[1], true
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
// subkey only with a binding among them. With strict, a signature only if
// go-crypto parses it, and the key's own signature on a user ID, or any on
// a subkey, only if it verifies there (go-crypto verifies those and fails
// the key on one that does not; it keeps another key's certification
// unverified, and verifies a key revocation itself).
func (kb *keyBlock) packets(strict bool) []byte {
	size := len(kb.primary)
	for _, c := range kb.components {
		size += len(c.raw)
		for _, s := range c.sigs {
			size += len(s.raw)
		}
	}
	out := append(make([]byte, 0, size), kb.primary...)
	for _, c := range kb.components {
		var sigs []byte
		bound := false
		for _, s := range c.sigs {
			if strict && !kb.sigOK(c, s) {
				continue
			}
			bound = bound || s.body[1] == byte(packet.SigTypeSubkeyBinding)
			sigs = append(sigs, s.raw...)
		}
		if c.kind == onSubkey && !bound {
			continue
		}
		out = append(out, c.raw...)
		out = append(out, sigs...)
	}
	return out
}

// sigOK reports whether go-crypto keeps s on c without failing the key: s
// parses, and on a user ID is another key's certification or the key's own
// verifying there, on a subkey verifies there.
func (kb *keyBlock) sigOK(c keyComponent, s rawPacket) bool {
	p, err := packet.Read(bytes.NewReader(s.raw))
	sig, ok := p.(*packet.Signature)
	if err != nil || !ok {
		return false
	}
	switch c.kind {
	case onUserID:
		return !sig.CheckKeyIdOrFingerprint(kb.key) || kb.key.VerifyUserIdSignature(c.uid, kb.key, sig) == nil
	case onSubkey:
		return kb.key.VerifyKeySignature(c.key, sig) == nil
	}
	return true
}
