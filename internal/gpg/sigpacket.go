package gpg

// Layer P of the hybrid verifier (the hardening plan's DN-11, ruling HD-16):
// the signature packets the armor decodes to, read by the cache's own v4
// parser with exact consumption. apt hands them to gpgv or sqv, which parse
// them differently, so the cache accepts only packets both parse alike and
// real signers emit — v4 data signatures, RSA, DSA, ECDSA or EdDSA, SHA-1 to
// SHA-512, one issuer — and refuses the rest, even where every apt accepts
// it (the deviation families "non-canonical packet layout" and
// "unverifiable algorithm"). go-crypto reads a packet only after P1–P4
// passed it, and only that packet's bytes as they were framed (P5): it never
// sees a v5 or v6 packet, an embedded signature or a length it would
// allocate before checking.
//
// AIDEV-NOTE: history moved here from the deleted packet_shape.go (F6) and
// verifier.go's issuer helpers (s1Issuer, hashedIssuer, firstIssuerKeyID,
// firstKeyID, hashedArea, unhashedArea, nextSubpacket; rulings HD-8, HD-13,
// HD-14). F6 framed the packets and had go-crypto parse them, comparing
// counts: packet.Reader.Next() skips v3/v5 signatures, unknown algorithms,
// markers and non-critical unknown tags, and reads bytes left inside a
// packet's declared length as the next packet's header, so a packet hidden
// in a re-framed packet's tail reached neither S1 nor S2 (the final
// review's V-C1), a marker or an unknown-tag packet there was skipped and
// the anchor adopted as all six apt adopt it (gi_/gd_p1_inner_marker,
// p2_inner_unknowntag), zero bytes there made it refuse what every apt
// accepts (gi_/gd_p3_inner_zero_bytes), and a v6 length or an embedded v6
// signature made it allocate 4 GiB before any check (V-C2, a crash). P3
// refuses any byte after a packet's fields now: the six are strict
// deviations, as the straddle's controls are (sx_*_straddle_unknownB).
//
// S1 then read the issuer as gpgv reads it (HD-14), out of a re-serialized
// packet (strict: Serialize re-framed lengths), because go-crypto takes the
// last issuer subpacket it parses, unhashed after hashed, and gpgv looks the
// key up by the first hashed issuer fingerprint (33) in its version's form,
// else by the first issuer key ID (16), hashed then unhashed — gpgv 2.4 when
// there is no such fingerprint, gpgv 2.2 also when the fingerprint names a
// key it lacks (t7g6, t7g6b: debian:11, ubuntu:22.04 and debian:12 refuse
// them) — reading the first matching subpacket at each step (a later 33 is
// not read, t7g7; nor a second 16, t7g3); the first 16 of an area counted,
// so a forged packet putting a known key's ID before an unknown one stayed
// known (t7g3; the security review's prototype kept the last). P4 makes the
// question moot: a packet names exactly one issuer key ID, and an issuer
// fingerprint only if it agrees, so every reader takes the same key; the
// shapes that told the readers apart are refused, a strict deviation where
// every apt accepted one (t7g3b_hashed16B_16A, t7g7b_33junk_33A).

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// packetTagSignature is the signature packet's tag (RFC 9580 §5.2).
const packetTagSignature = 2

// Layer P's limits: at most 8 packets an anchor (the survey's most is 3;
// the cap also bounds S1's hashing, the final review's V-M4) and 10,000
// octets a subpacket area (gpgv's parse-packet.c refuses a longer one as an
// invalid packet and reads no packet after it: A-C8).
const (
	sigMaxPackets  = 8
	sigMaxAreaSize = 10000
)

// sigPacket is one signature packet as layer P read it.
type sigPacket struct {
	n   int    // its place among the anchor's packets, from 1
	raw []byte // the packet as framed, header and body: what go-crypto reads and verifies

	sigType  byte   // 0x00 binary or 0x01 text
	pkAlgo   byte   // the public-key algorithm
	hashAlgo byte   // the hash algorithm
	issuer   uint64 // its one issuer key ID (subpacket 16)
	fpr      []byte // its issuer fingerprint (subpacket 33's 20 octets), nil when it has none

	// sqvFault names the packet's first subpacket, in either area, that
	// Sequoia's parser refuses (sqvSubpacketFault), "" when it has none.
	// Layer P does not refuse such a packet: debian:13's sqv reads it as
	// an unknown packet and ignores it, as it ignores an unknown key's, and
	// gpgv reads it (the unhashed subpackets but 16 and 33 not at all), so
	// it only keeps S2 from counting it (discount; the re-review's A-RC1).
	sqvFault string
}

// The algorithms P2 accepts (RFC 9580 §9.1, §9.5), each public-key
// algorithm with the signature fields it implies: the number of MPIs, or
// the octets of a native signature (Ed25519 and Ed448, RFC 9580 §5.2.3).
var (
	sigPublicKeyFields = map[byte]struct{ mpis, octets int }{
		1:  {mpis: 1}, // RSA
		3:  {mpis: 1}, // RSA sign-only
		17: {mpis: 2}, // DSA
		19: {mpis: 2}, // ECDSA
		22: {mpis: 2}, // EdDSA (legacy)
		27: {octets: 64},
		28: {octets: 114},
	}
	sigHashes = map[byte]bool{2: true, 8: true, 9: true, 10: true} // SHA-1, SHA-256, SHA-384, SHA-512
)

// sigCriticalOK reports whether gpgv 2.2's can_handle_critical
// (g10/parse-packet.c, gnupg 2.2.27) accepts a critical subpacket of typ
// with data, less 32 (refused anywhere) and 20 (a notation: refused, gpgv's
// known notations aside). gpgv 2.4 handles 34 and 37 too; 2.2 is the
// strictest supported gpgv.
func sigCriticalOK(typ byte, data []byte) bool {
	switch typ {
	case 2, 3, 4, 5, 6, 7, 9, 11, 12, 16, 21, 22, 24, 25, 26, 27, 29, 30, 33:
		return true
	case 38: // a key block, version 0
		return len(data) > 0 && data[0] == 0
	}
	return false
}

// parseSigPackets applies P1–P5 to b, the bytes the armor decodes to:
//
//   - P1: 1 to 8 packets, each framed old-format (tag 2, a one-, two- or
//     four-octet length) or new-format (tag 2, a one-, two- or five-octet
//     length; a partial or indeterminate length refused), its body inside b.
//   - P2: the body is a v4 signature (v3, v5 and v6 refused: go-crypto never
//     sees a non-v4 octet), of type 0x00 or 0x01, with a public-key
//     algorithm of sigPublicKeyFields and a hash of sigHashes (11, 12 and
//     14 refused until measured otherwise: debian:13 refuses SHA-224, every
//     gpgv SHA3-256).
//   - P3: each subpacket area at most 10,000 octets and a run of whole
//     subpackets (one-, two- or five-octet lengths, each at least 1); then
//     the hash's left 16 bits; then the algorithm's fields, each MPI
//     minimal (its first octet non-zero, its bit count exact); then the
//     body ends: not one byte after.
//   - P4: no subpacket 32 (an embedded signature) in either area; a
//     critical subpacket only of a type gpgv 2.2 handles (sigCriticalOK);
//     exactly one 2 (creation time, 4 octets) in the hashed area; exactly
//     one 16 (issuer key ID, 8 octets) in the packet; at most one 33
//     (issuer fingerprint: 21 octets, version 4, its key ID the 16's).
//   - P5: go-crypto's packet.Read of the packet's bytes as framed gives a
//     *packet.Signature, version 4, with P4's issuer.
func parseSigPackets(b []byte) ([]sigPacket, error) {
	var out []sigPacket
	for i := 0; i < len(b); {
		n := len(out) + 1
		if n > sigMaxPackets {
			return nil, refuse("P1", "more than %d signature packets", sigMaxPackets)
		}
		hdr, bodyLen, err := sigPacketHeader(b[i:], n)
		if err != nil {
			return nil, err
		}
		if bodyLen > uint64(len(b)-i-hdr) {
			return nil, refuse("P1", "packet %d: its body runs past the signature's end", n)
		}
		raw := b[i : i+hdr+int(bodyLen)]
		p, err := parseSigBody(raw[hdr:], n)
		if err != nil {
			return nil, err
		}
		p.raw = raw
		if err := readBack(p); err != nil {
			return nil, err
		}
		out = append(out, p)
		i += len(raw)
	}
	if len(out) == 0 {
		return nil, refuse("P1", "no signature packet")
	}
	return out, nil
}

// sigPacketHeader reads the packet header at b's start (RFC 9580 §4.2): its
// octets and the body's length. The n-th packet; P1.
func sigPacketHeader(b []byte, n int) (int, uint64, error) {
	h := b[0]
	if h&0x80 == 0 {
		return 0, 0, refuse("P1", "packet %d: byte 0x%02x starts no packet header", n, h)
	}
	if h&0x40 != 0 { // new format: the tag in bits 5-0
		if h&0x3f != packetTagSignature {
			return 0, 0, refuse("P1", "packet %d: a packet of type %d among the signature packets", n, h&0x3f)
		}
		if len(b) < 2 {
			return 0, 0, refuse("P1", "packet %d: a truncated header", n)
		}
		switch l0 := b[1]; {
		case l0 < 192:
			return 2, uint64(l0), nil
		case l0 < 224:
			if len(b) < 3 {
				return 0, 0, refuse("P1", "packet %d: a truncated header", n)
			}
			return 3, (uint64(l0)-192)<<8 + uint64(b[2]) + 192, nil
		case l0 == 255:
			if len(b) < 6 {
				return 0, 0, refuse("P1", "packet %d: a truncated header", n)
			}
			return 6, uint64(binary.BigEndian.Uint32(b[2:6])), nil
		}
		return 0, 0, refuse("P1", "packet %d: a partial body length", n)
	}
	if tag := (h >> 2) & 0x0f; tag != packetTagSignature { // old format: the tag in bits 5-2
		return 0, 0, refuse("P1", "packet %d: a packet of type %d among the signature packets", n, tag)
	}
	octets := [4]int{1, 2, 4, 0}[h&0x03] // the length type in bits 1-0
	switch {
	case octets == 0:
		return 0, 0, refuse("P1", "packet %d: an indeterminate length", n)
	case len(b) < 1+octets:
		return 0, 0, refuse("P1", "packet %d: a truncated header", n)
	}
	var l uint64
	for _, c := range b[1 : 1+octets] {
		l = l<<8 | uint64(c)
	}
	return 1 + octets, l, nil
}

// parseSigBody applies P2–P4 to the n-th packet's body.
func parseSigBody(body []byte, n int) (sigPacket, error) {
	p := sigPacket{n: n}
	if len(body) < 6 {
		return p, refuse("P2", "packet %d: %d octets, too short for a v4 signature", n, len(body))
	}
	if body[0] != 4 {
		return p, refuse("P2", "packet %d: a version %d signature (the cache reads version 4 only)", n, body[0])
	}
	p.sigType, p.pkAlgo, p.hashAlgo = body[1], body[2], body[3]
	fields, ok := sigPublicKeyFields[p.pkAlgo]
	switch {
	case p.sigType > 0x01:
		return p, refuse("P2", "packet %d: signature type 0x%02x (a data signature is 0x00 or 0x01)", n, p.sigType)
	case !ok:
		return p, refuse("P2", "packet %d: public-key algorithm %d, which the cache does not read", n, p.pkAlgo)
	case !sigHashes[p.hashAlgo]:
		return p, refuse("P2", "packet %d: hash algorithm %d, which the cache does not read", n, p.hashAlgo)
	}
	rest := body[4:]
	subs := subpacketCounts{hashAlgo: p.hashAlgo}
	for _, hashed := range []bool{true, false} {
		area := "unhashed"
		if hashed {
			area = "hashed"
		}
		if len(rest) < 2 {
			return p, refuse("P3", "packet %d: it ends before its %s subpacket area", n, area)
		}
		size := int(binary.BigEndian.Uint16(rest))
		switch {
		case size > sigMaxAreaSize:
			return p, refuse("P3", "packet %d: %s subpacket area of %d octets, gpgv's limit is %d", n, area, size, sigMaxAreaSize)
		case size > len(rest)-2:
			return p, refuse("P3", "packet %d: its %s subpacket area runs past its body", n, area)
		}
		if err := subs.walk(rest[2:2+size], hashed, n); err != nil {
			return p, err
		}
		rest = rest[2+size:]
	}
	if err := subs.finish(&p); err != nil {
		return p, err
	}
	if len(rest) < 2 {
		return p, refuse("P3", "packet %d: it ends before the hash's left 16 bits", n)
	}
	rest = rest[2:]
	if fields.octets > 0 {
		if len(rest) < fields.octets {
			return p, refuse("P3", "packet %d: a %d-octet signature runs past its body", n, fields.octets)
		}
		rest = rest[fields.octets:]
	}
	for k := 1; k <= fields.mpis; k++ {
		var why string
		if rest, why = minimalMPI(rest); why != "" {
			return p, refuse("P3", "packet %d: MPI %d %s", n, k, why)
		}
	}
	if len(rest) > 0 {
		return p, refuse("P3", "packet %d: %d octets after the signature's fields", n, len(rest))
	}
	return p, nil
}

// minimalMPI cuts one MPI (RFC 9580 §3.2) off b, and says why it is not a
// minimal one when it is not: no octet, or a bit count other than its
// value's — which a leading zero octet always makes it (sqv refuses both:
// A-C9) — or it runs past b.
func minimalMPI(b []byte) ([]byte, string) {
	if len(b) < 2 {
		return nil, "runs past the body"
	}
	declared := int(binary.BigEndian.Uint16(b))
	octets := (declared + 7) / 8
	switch {
	case octets > len(b)-2:
		return nil, "runs past the body"
	case octets == 0:
		return nil, "holds no octet"
	case declared != 8*(octets-1)+bits.Len8(b[2]):
		return nil, "is not minimal: a leading zero octet, or a bit count other than its value's"
	}
	return b[2+octets:], ""
}

// subpacketCounts gathers what P4 counts over a packet's two areas.
type subpacketCounts struct {
	created   int    // hashed 2s
	issuers   int    // 16s, both areas
	issuer    []byte // the last 16's data
	fprs      int    // 33s, both areas
	fpr       []byte // the last 33's data
	badLength error  // the first hashed 2, or 16 or 33, of a wrong length
	hashAlgo  byte   // the packet's hash (sqvSubpacketFault's type 37)
	sqvFault  string // the first subpacket Sequoia's parser refuses (sigPacket.sqvFault)
}

// sqvDigestSizes are the digest sizes, in octets, of the hashes P2 reads:
// an approved certifications subpacket (37) holds whole digests of its
// packet's hash.
var sqvDigestSizes = map[byte]int{2: 20, 8: 32, 9: 48, 10: 64}

// sqvSubpacketFault says why Sequoia's subpacket parser refuses a
// subpacket of type typ whose data (its octets after the type octet) are
// data, in a packet whose hash is hashAlgo, or "" when it reads it: the
// parser debian:13's sqv 1.3.0 is built with (sequoia-openpgp 2.0.0,
// src/parse.rs, Subpacket::parse), rule by rule.
//
// AIDEV-NOTE: the re-review's A-RC1. Sequoia parses every subpacket of a
// signature packet, in both areas, by type, and a type it knows must hold
// exactly its fields (it fails a subpacket whose body its fields do not
// consume exactly: "body length is N bytes, but read M"); any failure
// makes the whole packet unknown to sqv, which then ignores it. The table
// below is the parser's, type by type: a fixed-size field (2, 3, 9: four
// octets; 5: two), a boolean (4, 7, 25: one octet, 0 or 1), a
// NUL-terminated regular expression (6), a revocation key of 22 octets or
// more whose class has bit 0x80 (12: RevocationKey::from_bits), a notation
// whose two lengths are its size and whose name is UTF-8 (20), a reason
// code (29), two algorithm octets (31; the digest's length is not
// checked), a fingerprint of a version octet and, for v4, 20 octets, for
// v6, 32 (33, 35; another version any length), whole digests of the
// packet's hash (37), octet pairs (39). Every other type it reads whatever
// its size: preferences, flags, URIs, user IDs, features, an issuer key ID
// of any length (KeyID::Invalid: P4 asks 8), and the reserved, private and
// unknown types. An embedded signature (32) it parses as a signature: P4
// refuses 32 first. TestSqvSubpacketFault_FollowsSequoia pins each rule
// both sides of its boundary, and the corpus' batch fx15 measures the
// re-review's shapes (u_good_unh_*, z_good_unh_*, x_good_unh_*) on sqv:
// re-derive the table when debian's sqv moves to another sequoia-openpgp.
func sqvSubpacketFault(typ byte, data []byte, hashAlgo byte) string {
	n := len(data)
	switch typ {
	case 2, 3, 9: // signature creation, signature expiration, key expiration time
		if n != 4 {
			return fmt.Sprintf("of %d octets, not 4", n)
		}
	case 4, 7, 25: // exportable certification, revocable, primary user ID
		switch {
		case n != 1:
			return fmt.Sprintf("of %d octets, not a one-octet boolean", n)
		case data[0] > 1:
			return fmt.Sprintf("a boolean of value %d, not 0 or 1", data[0])
		}
	case 5: // trust signature: a level and an amount
		if n != 2 {
			return fmt.Sprintf("of %d octets, not 2", n)
		}
	case 6: // regular expression
		if n == 0 || data[n-1] != 0 {
			return "not NUL-terminated"
		}
	case 12: // revocation key: a class, an algorithm, a fingerprint
		switch {
		case n < 22:
			return fmt.Sprintf("of %d octets, under 22", n)
		case data[0]&0x80 == 0:
			return fmt.Sprintf("its class 0x%02x without bit 0x80", data[0])
		}
	case 20: // notation: four flag octets, the name's and the value's lengths, the name, the value
		if n < 8 {
			return fmt.Sprintf("of %d octets, under 8", n)
		}
		name := int(binary.BigEndian.Uint16(data[4:6]))
		if n != 8+name+int(binary.BigEndian.Uint16(data[6:8])) {
			return fmt.Sprintf("of %d octets, which its name's and value's lengths do not make", n)
		}
		if !utf8.Valid(data[8 : 8+name]) {
			return "its name not UTF-8"
		}
	case 29: // reason for revocation: a code, then text
		if n == 0 {
			return "empty, without a code"
		}
	case 31: // signature target: two algorithms, then a digest
		if n < 2 {
			return fmt.Sprintf("of %d octets, under 2", n)
		}
	case 33, 35: // issuer fingerprint, intended recipient: a version, a fingerprint
		switch {
		case n == 0:
			return "empty, without a version"
		case data[0] == 4 && n != 21, data[0] == 6 && n != 33:
			return fmt.Sprintf("of version %d and %d octets", data[0], n)
		}
	case 37: // approved certifications: digests of the packet's hash
		if d := sqvDigestSizes[hashAlgo]; d > 0 && n%d != 0 {
			return fmt.Sprintf("of %d octets, not whole %d-octet digests", n, d)
		}
	case 39: // preferred AEAD ciphersuites: pairs
		if n%2 != 0 {
			return fmt.Sprintf("of %d octets, not pairs", n)
		}
	}
	return ""
}

// walk applies P3's framing and P4's per-subpacket rules to one area of the
// n-th packet, counting its 2s, 16s and 33s.
func (s *subpacketCounts) walk(area []byte, hashed bool, n int) error {
	where := "unhashed"
	if hashed {
		where = "hashed"
	}
	for len(area) > 0 {
		var size uint64
		var hl int
		switch l0 := area[0]; {
		case l0 < 192:
			size, hl = uint64(l0), 1
		case l0 < 255:
			if len(area) < 2 {
				return refuse("P3", "packet %d: a %s subpacket's length runs past its area", n, where)
			}
			size, hl = (uint64(l0)-192)<<8+uint64(area[1])+192, 2
		default:
			if len(area) < 5 {
				return refuse("P3", "packet %d: a %s subpacket's length runs past its area", n, where)
			}
			size, hl = uint64(binary.BigEndian.Uint32(area[1:5])), 5
		}
		switch {
		case size == 0:
			return refuse("P3", "packet %d: a %s subpacket of length 0", n, where)
		case size > uint64(len(area)-hl):
			return refuse("P3", "packet %d: a %s subpacket runs past its area", n, where)
		}
		sub := area[hl : hl+int(size)]
		area = area[hl+int(size):]
		typ, critical, data := sub[0]&0x7f, sub[0]&0x80 != 0, sub[1:]
		switch {
		case typ == 32:
			return refuse("P4", "packet %d: an embedded signature (subpacket 32) in its %s area", n, where)
		case critical && !sigCriticalOK(typ, data):
			return refuse("P4", "packet %d: a critical subpacket %d in its %s area, which gpgv 2.2 does not handle", n, typ, where)
		case typ == 4 && hashed && len(data) == 0:
			return refuse("P4", "packet %d: an exportable certification (subpacket 4) without its value octet in its hashed area, which go-crypto before v1.5.0 reads past", n)
		}
		if s.sqvFault == "" {
			if why := sqvSubpacketFault(typ, data, s.hashAlgo); why != "" {
				s.sqvFault = fmt.Sprintf("its %s subpacket %d %s", where, typ, why)
			}
		}
		switch typ {
		case 2:
			if hashed {
				s.created++
				s.length(typ, data, 4, n)
			}
		case 16:
			s.issuers++
			s.issuer = data
			s.length(typ, data, 8, n)
		case 33:
			s.fprs++
			s.fpr = data
			s.length(typ, data, 21, n)
		}
	}
	return nil
}

// length records the first subpacket of typ whose data is not want octets.
func (s *subpacketCounts) length(typ byte, data []byte, want, n int) {
	if len(data) != want && s.badLength == nil {
		s.badLength = refuse("P4", "packet %d: subpacket %d of %d octets, not %d", n, typ, len(data), want)
	}
}

// finish applies P4's counts to p, setting its issuer and fingerprint.
func (s *subpacketCounts) finish(p *sigPacket) error {
	switch {
	case s.badLength != nil:
		return s.badLength
	case s.created != 1:
		return refuse("P4", "packet %d: %d creation times (subpacket 2) in its hashed area, not one", p.n, s.created)
	case s.issuers != 1:
		return refuse("P4", "packet %d: %d issuer key IDs (subpacket 16), not one", p.n, s.issuers)
	case s.fprs > 1:
		return refuse("P4", "packet %d: %d issuer fingerprints (subpacket 33), not one at most", p.n, s.fprs)
	}
	p.issuer = binary.BigEndian.Uint64(s.issuer)
	p.sqvFault = s.sqvFault
	if s.fprs == 1 {
		switch {
		case s.fpr[0] != 4:
			return refuse("P4", "packet %d: an issuer fingerprint of version %d in a v4 signature", p.n, s.fpr[0])
		case !bytes.Equal(s.fpr[13:21], s.issuer):
			return refuse("P4", "packet %d: its issuer fingerprint's key ID %X is not its issuer key ID %016X", p.n, s.fpr[13:21], p.issuer)
		}
		p.fpr = s.fpr[1:]
	}
	return nil
}

// readBack is P5: go-crypto's own parse of p's bytes, as framed, is a
// version 4 *packet.Signature naming p's issuer; any disagreement refuses.
//
// AIDEV-NOTE: go-crypto (v1.4.1 to v1.5.2, packet/signature.go) refuses a
// critical hashed subpacket of a type it does not parse — any but 2 to 6, 9, 11, 16,
// 20 to 30, 32, 33, 35 and 39. Where gpgv 2.2's can_handle_critical refuses
// the type too, P4 has refused it first, and the two agree (gpgv's BADSIG).
// Not for 7, 12 and 38 (version 0): gpgv 2.2 handles them and P4 lets them
// through, so P5's refusal is strict (the re-review's V-RM4; batch fx15's
// k_good_plus_unknownB_hashed_crit7 and _crit12, an unknown key's packet,
// every apt accepts, the cache refuses: the deviation family "non-canonical
// packet layout"). go-crypto also refuses, critical or not, a hashed
// subpacket it parses and finds malformed (a primary user ID of two
// octets, a trust signature of one, an odd AEAD list, a regular expression
// without its NUL) and a hashed exportable certification of 0: strict too
// where gpgv and sqv read the packet (k_*, v_*_hashed_regexp_nonul_go).
//
// AIDEV-NOTE: the re-review's A-RC3. go-crypto v1.4.1's
// parseSignatureSubpacket read a hashed exportable certification (4) as
// subpacket[0] after a guard that counted the type octet, so one without
// its value octet made packet.Read panic (fixed in v1.5.0; go.mod pins
// v1.5.2 since Task 9b's fix round 2) — key-free: a
// packet beside the good one carried it — and the adoption goroutine's
// recover then failed every attempt on the suite alike. Audited type by
// type (v1.4.1 and v1.5.2), every other subpacket go-crypto reads checks
// its length before it indexes (2, 3, 5, 9, 16, 25: exact sizes; 6, 20,
// 29, 33, 35: minimums; 23, 27, 30: an empty body skipped; 39: even; the
// rest are copied whole), the subpacket length parse included; P4
// refuses the empty hashed 4 first (in the unhashed area go-crypto parses
// no 4) and 32 anywhere, whose nested parse lacked a recursion guard until
// v1.5.0. And the parse runs under recover(): any go-crypto parse panic,
// this one or a later one, is a P5 refusal
// (TestReadBack_TurnsAPanicIntoARefusal).

// readPacket is go-crypto's packet reader, a test seam: tests swap it for
// one that panics.
var readPacket = packet.Read

func readBack(p sigPacket) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = refuse("P5", "packet %d: go-crypto's parse of it panicked: %v", p.n, r)
		}
	}()
	pkt, err := readPacket(bytes.NewReader(p.raw))
	if err != nil {
		return refuse("P5", "packet %d: go-crypto does not parse it: %v", p.n, err)
	}
	sig, ok := pkt.(*packet.Signature)
	switch {
	case !ok:
		return refuse("P5", "packet %d: go-crypto parses it as a %T", p.n, pkt)
	case sig.Version != 4 || sig.IssuerKeyId == nil || *sig.IssuerKeyId != p.issuer:
		return refuse("P5", "packet %d: go-crypto reads another version or issuer", p.n)
	}
	return nil
}
