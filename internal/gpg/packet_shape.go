package gpg

// F6 of the apt-shape checks (armor_shape.go; DN-3 of
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md): the
// signature packets decoded from an InRelease's armor or a Release.gpg are
// signature packets only, each one parsed by go-crypto. apt's gpgv refuses
// a v3 garbage packet naming a known key, and some apt refuses an unknown
// hash, a marker, an unknown tag, a user ID, a literal-data packet or
// trailing bytes beside a good signature (DN-2); the cache's later walks
// over the signature could not see most of them.
//
// AIDEV-NOTE: packet.Reader.Next() silently skips v3/v5 signatures, unknown
// algorithms, markers and unknown tags (packet/reader.go), so every later
// walk over the signature (S1, S2) would not see them; a vendor adding a
// signature with an algorithm go-crypto lacks is refused until go-crypto
// supports it (xi_good_plus_unknownpk_A, a listed deviation). The checks
// back each other up: the framing walk alone refuses markers and unknown
// tags; NextWithUnsupported's refusal of an UnsupportedPacket and the
// per-packet count each refuse what Next() would skip, so dropping one of
// those two shows in no corpus row — keep both.
//
// AIDEV-NOTE: measured 2026-10-29, beyond the corpus: bytes inside a
// signature packet's declared length, after its fields, which gpgv and sqv
// skip, are read by go-crypto as the next packet's header. A marker or an
// unknown-tag packet there is then skipped (one packet framed, one parsed),
// and the anchor is adopted as all six apt adopt it; zero bytes there make
// NextWithUnsupported fail, so F6 refuses an anchor all six apt accept — a
// strict deviation the corpus does not hold.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// packetTagSignature is the signature packet's tag (RFC 9580 §5.2).
const packetTagSignature = 2

// requireSignaturePacketsOnly is F6: sigBytes, the binary signature the
// cache decoded, is a sequence of signature packets — the framing walk finds
// tag 2 only — and go-crypto parses each of them: NextWithUnsupported
// returns one *packet.Signature per framed packet, never an
// *packet.UnsupportedPacket or another packet type. Its errors are plain
// text; the callers wrap them in their form's malformed sentinel.
func requireSignaturePacketsOnly(sigBytes []byte) error {
	framed, err := countSignaturePackets(sigBytes)
	if err != nil {
		return err
	}
	r := packet.NewReader(bytes.NewReader(sigBytes))
	parsed := 0
	for {
		p, err := r.NextWithUnsupported()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("signature packet %d does not parse: %v", parsed+1, err)
		}
		if u, ok := p.(*packet.UnsupportedPacket); ok {
			return fmt.Errorf("signature packet %d is one go-crypto cannot parse: %v", parsed+1, u.Error)
		}
		if _, ok := p.(*packet.Signature); !ok {
			return fmt.Errorf("a %T packet among the signature packets", p)
		}
		parsed++
	}
	if parsed != framed {
		return fmt.Errorf("%d packets framed, %d parsed as signatures", framed, parsed)
	}
	return nil
}

// countSignaturePackets walks the OpenPGP packet framing of b (RFC 9580
// §4.2) and returns the number of packets, all of them signature packets:
// a byte that starts no packet header, a packet with another tag, a partial
// (new format) or indeterminate (old format) length, or a packet that runs
// past b's end is an error.
func countSignaturePackets(b []byte) (int, error) {
	n := 0
	for i := 0; i < len(b); {
		h := b[i]
		if h&0x80 == 0 {
			return 0, fmt.Errorf("byte %d starts no packet header", i)
		}
		newFormat := h&0x40 != 0
		tag := (h >> 2) & 0x0f // old format: the tag in bits 5-2
		if newFormat {
			tag = h & 0x3f
		}
		if tag != packetTagSignature {
			return 0, fmt.Errorf("packet %d: a packet of type %d among the signature packets", n+1, tag)
		}
		var hdr int     // the header's octets: the tag octet and the length octets
		var body uint64 // the packet body's length
		if newFormat {
			// A one-, two- or five-octet length; 224-254 is a partial
			// length (RFC 9580 §4.2.1).
			if i+1 >= len(b) {
				return 0, fmt.Errorf("packet %d: a truncated header", n+1)
			}
			switch l0 := b[i+1]; {
			case l0 < 192:
				hdr, body = 2, uint64(l0)
			case l0 < 224:
				if i+2 >= len(b) {
					return 0, fmt.Errorf("packet %d: a truncated header", n+1)
				}
				hdr, body = 3, (uint64(l0)-192)<<8+uint64(b[i+2])+192
			case l0 == 255:
				if i+6 > len(b) {
					return 0, fmt.Errorf("packet %d: a truncated header", n+1)
				}
				hdr, body = 6, uint64(binary.BigEndian.Uint32(b[i+2:i+6]))
			default:
				return 0, fmt.Errorf("packet %d: a partial body length", n+1)
			}
		} else {
			// The length type in bits 1-0: one, two or four length
			// octets, or 3, an indeterminate length (RFC 9580 §4.2.2).
			switch h & 0x03 {
			case 0:
				if i+2 > len(b) {
					return 0, fmt.Errorf("packet %d: a truncated header", n+1)
				}
				hdr, body = 2, uint64(b[i+1])
			case 1:
				if i+3 > len(b) {
					return 0, fmt.Errorf("packet %d: a truncated header", n+1)
				}
				hdr, body = 3, uint64(binary.BigEndian.Uint16(b[i+1:i+3]))
			case 2:
				if i+5 > len(b) {
					return 0, fmt.Errorf("packet %d: a truncated header", n+1)
				}
				hdr, body = 5, uint64(binary.BigEndian.Uint32(b[i+1:i+5]))
			default:
				return 0, fmt.Errorf("packet %d: an indeterminate length", n+1)
			}
		}
		if body > uint64(len(b)-i-hdr) {
			return 0, fmt.Errorf("packet %d: its body runs past the signature's end", n+1)
		}
		i += hdr + int(body)
		n++
	}
	return n, nil
}
