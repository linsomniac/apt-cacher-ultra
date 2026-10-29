package gpg

// Layer A of the hybrid verifier (the hardening plan's DN-11, ruling HD-16):
// the signature armor, read by the cache's own parser as a canonical
// whitelist. apt hands the armor to gpgv or sqv, which read it differently,
// so the cache accepts only a grammar both accept and real signers emit —
// GnuPG, Sequoia, BouncyCastle, BSN, go-crypto: the vendor survey's 158
// anchors all lie inside it — and refuses the rest, even where every apt
// accepts it (a deviation of the family "non-canonical armor layout";
// widening the grammar is a code change with a measured corpus row, ruling
// HD-17). Both forms, on lines as apt's LineBuffer leaves them
// (textsplit.go's aptLine: what an InRelease's S holds; a Release.gpg's raw
// lines reach gpgv and sqv, which both tolerate the trailing blanks and CRs
// aptLine strips, measured). The cache's base64 decode is the decode: no
// other parser reads the armor.
//
// AIDEV-NOTE: history moved here from the deleted armor_shape.go (F0, F2–F5;
// rulings HD-8, HD-13). go-crypto's armor reader trims every line with
// bytes.TrimSpace (Unicode-aware), ends the body at a trimmed "-----END "
// line or a trimmed five-character "=" line, takes any line with a colon as
// a header, skips a colon-less header to the next BEGIN line (goto
// TryNextBlock) and never checks the CRC; it base64-decodes line by line, so
// padding mid-stream is read past. The F-rules watched it from beside: F3
// and F4 trimmed as it trims, so that " -----END X-----" or "\v=AAAA"
// inside the armor could not end its body unseen (HD-13: gi_/gd_g1, g3, g4,
// which every apt refuses); F5 refused the colon-less header that made
// armor.Decode jump and the colon-only one gpgv refuses (Junk:junk), and,
// with no blank line, every base64 line as a header without ": "
// (x3*_armor_no_blank: A2 now, the same way). Each round found more (the
// final review's A-C4 to A-C6: armor headers gpgv parses at the first colon
// as a C string, \v and Unicode spaces sqv refuses, padding mid-stream gpgv
// stops at), which is why the armor is now parsed here, once, by rules
// A1–A6. A1 and A6 cannot fail behind layer T (its block starts at a BEGIN
// line and ends at an END line); parseArmor checks them anyway, and
// TestParseArmor_Rules pins them.

import (
	"bytes"
	"encoding/base64"
)

// Layer A's limits: RFC 4880 §6.3 and RFC 9580 §6.2.3 cap a base64 line
// at 76 characters (every surveyed signer writes 64); a data signature
// is well under 1 KiB, the survey's largest anchor three packets.
const (
	armorMaxHeaders     = 16
	armorMaxKey         = 64
	armorMaxValue       = 1024
	armorMaxLine        = 76
	armorMaxPacketBytes = 64 << 10
)

// parseArmor applies A1–A6 to one armor block, lines from its BEGIN line to
// its END line as aptLine leaves them (first is lines[0]'s line number in
// the file, for the details), and returns the bytes its base64 encodes:
//
//   - A1: the first line is exactly "-----BEGIN PGP SIGNATURE-----".
//   - A2: 0 to 16 header lines "KEY: VALUE": KEY [A-Za-z][A-Za-z0-9-]{0,63},
//     the separator exactly ": " at the first colon, VALUE 1 to 1024 bytes
//     of [\x20-\x7E\x80-\xFF] (never empty on an aptLine'd line:
//     armorHeaderFault).
//   - A3: exactly one empty line ends the headers (a second is no base64
//     line: A4).
//   - A4: one or more base64 lines, each 1 to 76 characters of
//     [A-Za-z0-9+/=]; together they decode, strictly (padding only at the
//     very end, the unused bits of the last character zero), to 1 byte to
//     64 KiB.
//   - A5: an optional CRC line, "=" and four base64 characters, once, right
//     after the last base64 line, equal to the CRC-24 of the decoded bytes.
//   - A6: the END line "-----END PGP SIGNATURE-----" is the next line, and
//     the last.
//
// AIDEV-NOTE: A4 decodes strictly (base64.StdEncoding.Strict), past DN-11's
// letter: debian:13's sqv refuses a last base64 character whose unused bits
// are set and gpgv drops them (n_i/n_d_b64_pad_bits, Task 9b's batch fx14),
// so the lenient decode would adopt a key-free edit some apt refuses.
func parseArmor(lines [][]byte, first int) ([]byte, error) {
	if len(lines) < 2 || !bytes.Equal(lines[0], []byte(sigArmorBegin)) {
		return nil, refuse("A1", "line %d is not %q", first, sigArmorBegin)
	}
	end := len(lines) - 1
	if !bytes.Equal(lines[end], []byte(sigArmorEnd)) {
		return nil, refuse("A6", "line %d, the armor's last, is not %q", first+end, sigArmorEnd)
	}
	i := 1
	for headers := 0; ; headers++ {
		l := lines[i]
		if len(l) == 0 {
			i++
			break
		}
		if i == end {
			return nil, refuse("A3", "line %d ends the armor before an empty line ends its headers", first+i)
		}
		if headers == armorMaxHeaders {
			return nil, refuse("A2", "line %d: more than %d armor headers", first+i, armorMaxHeaders)
		}
		if why := armorHeaderFault(l); why != "" {
			return nil, refuse("A2", "line %d is not an armor header KEY: VALUE: %s", first+i, why)
		}
		i++
	}
	var b64 []byte
	body, crc := i, -1
	for ; i < end; i++ {
		l := lines[i]
		if isCRCLine(l) {
			crc = i
			break
		}
		switch {
		case len(l) == 0:
			return nil, refuse("A4", "line %d: an empty line among the base64 lines", first+i)
		case len(l) > armorMaxLine:
			return nil, refuse("A4", "line %d: %d base64 characters, the limit is %d", first+i, len(l), armorMaxLine)
		}
		for _, c := range l {
			if !isBase64Char(c) && c != '=' {
				return nil, refuse("A4", "line %d: byte 0x%02x is not a base64 character", first+i, c)
			}
		}
		b64 = append(b64, l...)
	}
	switch {
	case i == body:
		return nil, refuse("A4", "line %d: no base64 line after the armor headers", first+i)
	case crc >= 0 && crc+1 != end:
		return nil, refuse("A5", "line %d follows the CRC line (line %d); the END line must", first+crc+1, first+crc)
	case base64.StdEncoding.DecodedLen(len(b64)) > armorMaxPacketBytes:
		return nil, refuse("A4", "the base64 encodes over %d bytes", armorMaxPacketBytes)
	}
	packets := make([]byte, base64.StdEncoding.DecodedLen(len(b64)))
	n, err := base64.StdEncoding.Strict().Decode(packets, b64)
	if err != nil {
		return nil, refuse("A4", "the base64 (lines %d to %d) is not canonical base64: %v", first+body, first+i-1, err)
	}
	packets = packets[:n] // one byte at least: one base64 line at least, and it decoded
	if crc >= 0 {
		sum, err := base64.StdEncoding.Strict().DecodeString(string(lines[crc][1:]))
		if err != nil || len(sum) != 3 {
			return nil, refuse("A5", "line %d: the CRC line is not \"=\" and four base64 characters", first+crc)
		}
		if uint32(sum[0])<<16|uint32(sum[1])<<8|uint32(sum[2]) != crc24(packets) {
			return nil, refuse("A5", "line %d: the armor checksum does not match the signature", first+crc)
		}
	}
	return packets, nil
}

// armorHeaderFault says why l, a line as aptLine leaves it, is not A2's
// KEY: VALUE, or "" when it is. Its value is never empty: aptLine has
// stripped the space a "KEY: " line would end in, so that line fails the
// separator rule ("Comment: " in an InRelease, which sqv reads as
// "Comment:" and refuses: a_hdr_trailing_space).
func armorHeaderFault(l []byte) string {
	c := bytes.IndexByte(l, ':')
	switch {
	case c < 0:
		return "no colon"
	case c == 0 || c > armorMaxKey:
		return "the key is not 1 to 64 characters"
	case c+1 == len(l) || l[c+1] != ' ':
		return "no space after the first colon (or an empty value)"
	case len(l)-c-2 > armorMaxValue:
		return "a value over 1024 bytes"
	}
	for k, b := range l[:c] {
		letter := b|0x20 >= 'a' && b|0x20 <= 'z'
		if !letter && (k == 0 || b != '-' && (b < '0' || b > '9')) {
			return "the key is not [A-Za-z][A-Za-z0-9-]*"
		}
	}
	for _, b := range l[c+2:] {
		if b < 0x20 || b == 0x7f {
			return "a control byte in the value"
		}
	}
	return ""
}

// isBase64Char reports whether c is one of base64's 64 characters (RFC
// 4648 §4), "=" not included.
func isBase64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/'
}

// isCRCLine reports whether l is a CRC line: "=" and four base64
// characters. No base64 line can be one: canonical base64 has its "=" only
// at its very end, two at most.
func isCRCLine(l []byte) bool {
	if len(l) != 5 || l[0] != '=' {
		return false
	}
	for _, c := range l[1:] {
		if !isBase64Char(c) {
			return false
		}
	}
	return true
}

// crc24 is the armor checksum of RFC 4880 §6.1 (RFC 9580 §6.1.1): CRC-24,
// initial value 0xB704CE, generator 0x1864CFB. go-crypto computes it when
// it writes an armor and never checks it when it reads one.
func crc24(b []byte) uint32 {
	const (
		crc24Init = 0xB704CE
		crc24Poly = 0x1864CFB
	)
	crc := uint32(crc24Init)
	for _, c := range b {
		crc ^= uint32(c) << 16
		for range 8 {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= crc24Poly
			}
		}
	}
	return crc & 0xFFFFFF
}
