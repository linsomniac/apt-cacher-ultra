package gpg

// The apt-shape checks (spec: docs/open-decisions.md, "The inline verifier
// accepts three shapes apt rejects"; rulings HD-2, HD-8). Each rule is what
// apt 2.2–3.2 enforce on an InRelease or Release.gpg, measured on the corpus
// in testdata/aptshapes (`make e2e-apt-oracle` re-measures it): DN-3 of
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md.
//
// AIDEV-NOTE: go-crypto's decoders are lenient where apt is not — the armor
// reader skips a header line without a colon to the next BEGIN line, ends the
// body at any line starting "-----END ", takes any line with a colon as a
// header, and ignores the CRC (RFC 9580); clearsign.Decode cuts the armor at
// the first substring "\n-----END PGP SIGNATURE-----". These checks run on the
// raw bytes beside the decode, so what the decoder skipped cannot reach the
// pool. Re-check them when go-crypto is upgraded (the table test and the
// oracle pin them).
//
// This file holds F0 and F2–F5, both forms; F1 is the callers'
// (requireBareClearsigned inline, isArmoredSignature detached) and F6 is
// packet_shape.go's. Their errors are plain text: VerifyInline wraps them in
// ErrMalformedClearsign, VerifyDetached in ErrMalformedSignature.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
)

// The signature armor's marker lines (RFC 9580 §6.2): the BEGIN line opens
// the armor of a Release.gpg and the armor after an InRelease's signed text;
// the END line closes either.
const (
	sigArmorBegin = "-----BEGIN PGP SIGNATURE-----"
	sigArmorEnd   = "-----END PGP SIGNATURE-----"
)

// lineSpan is one line of a body: b[start:end], without its LF. A CR before
// the LF belongs to the line; the marker and blank tests ignore it.
type lineSpan struct{ start, end int }

// splitLines splits b into lines on LF. A final LF ends the last line and
// starts no empty one, so "a\n" is one line and "a\n\n" two (the second
// empty), and an empty b has none.
//
// AIDEV-NOTE: F0 compares the first END marker's offset with the start of
// the last line: were a final LF to start an empty last line, F0 would refuse
// every file that ends in a newline (PE/proto/main.go's splitLines does the
// same).
func splitLines(b []byte) []lineSpan {
	var out []lineSpan
	for i := 0; i < len(b); {
		j := bytes.IndexByte(b[i:], '\n')
		if j < 0 {
			out = append(out, lineSpan{i, len(b)})
			break
		}
		out = append(out, lineSpan{i, i + j})
		i += j + 1
	}
	return out
}

// lineAt returns the bytes of lines[i], its LF excluded.
func lineAt(b []byte, lines []lineSpan, i int) []byte {
	return b[lines[i].start:lines[i].end]
}

// isMarkerLine reports whether l is marker followed only by spaces, tabs and
// CRs, as apt reads a marker line (RFC 9580 §6.2 allows trailing whitespace;
// any number of CRs: x7d_begin_double_cr, xi_end_double_cr). Never a prefix
// test: text after the marker is not the marker line (x7*_end_trailing_*).
func isMarkerLine(l []byte, marker string) bool {
	rest, ok := bytes.CutPrefix(l, []byte(marker))
	return ok && isBlankLine(rest)
}

// isBlankLine reports whether l holds only spaces, tabs and CRs.
func isBlankLine(l []byte) bool {
	return len(bytes.Trim(l, " \t\r")) == 0
}

// requireEndMarkerBeginsLastLine is F0: the first occurrence of
// "-----END PGP SIGNATURE-----" anywhere in b begins b's last line. Its
// substring search sees what no line test does — an END marker after text
// on its line (a dash-escaped END line in the signed text,
// i_dash_escaped_end) or with text after it before the real END line
// ("-----END PGP SIGNATURE-----x", xd_endsuffix_nocrc) — and it refuses
// every line after the END line (so F2's last-line clause never fires
// after it). b with no END marker fails too; requireBareClearsigned does
// not call it then, so that the decoder reports a missing END line as
// before.
func requireEndMarkerBeginsLastLine(b []byte, lines []lineSpan) error {
	first := bytes.Index(b, []byte(sigArmorEnd))
	switch {
	case first < 0 || len(lines) == 0:
		return errors.New("no END marker")
	case first < lines[len(lines)-1].start:
		return errors.New("extraneous bytes after the END marker")
	case first > lines[len(lines)-1].start:
		return errors.New("the END marker does not begin its line")
	}
	return nil
}

// checkSignatureArmor applies F2–F5 to the signature armor of b whose BEGIN
// line is lines[begin]; decoded is the signature packets the cache decoded
// from that armor, which F3's checksum must match.
//
//   - F2: the first line after the BEGIN line that is the END marker followed
//     only by spaces, tabs and CRs is the END line, and it is the last line.
//   - F3: the first line after the headers whose trimmed form (spaces, tabs
//     and CRs trimmed) starts with "=" is the CRC line: "=" and four base64
//     characters, the CRC-24 of decoded; only blank lines follow it before
//     the END line. No CRC line passes (RFC 9580 makes it optional).
//   - F4: no line strictly between the BEGIN line and the END line starts
//     with "-".
//   - F5: every non-blank line between the BEGIN line and the first blank
//     line (the armor headers) contains ": ".
//
// AIDEV-NOTE: F4 refuses the lines inside the armor at which go-crypto's
// armor reader would end the body early ("-----END X-----",
// "-----END PGP SIGNATURE-----x": xi_endx_nocrc, the endsuffix shapes) or
// a second BEGIN line; F3 constrains what follows the CRC line, where the
// reader stops too. F5 refuses the colon-less header that makes
// armor.Decode jump (goto TryNextBlock) and the colon-only one (Junk:junk)
// gpgv refuses; and with no blank line every base64 line counts as a header,
// so x3*_armor_no_blank is refused here too, whatever the decoder does.
//
// AIDEV-NOTE: known gap, open (found and measured 2026-10-29, beyond the
// corpus): go-crypto trims a line with bytes.TrimSpace before its END and
// CRC tests, these checks (as DN-3 words F3 and F4) do not. A line inside
// the armor such as " -----END X-----" (leading blank) or "\v=AAAA" ends
// go-crypto's body, so the lines after it go unread and unchecked: the
// cache adopts such an anchor, inline or detached, and all six measured apt
// refuse it (NODATA; sqv: "Invalid ASCII Armor footer"). The fix proposed:
// F4 on the line as bytes.TrimSpace leaves it, and the CRC line found and
// read the same way — pending a ruling and a corpus row.
func checkSignatureArmor(b []byte, lines []lineSpan, begin int, decoded []byte) error {
	end := -1
	for i := begin + 1; i < len(lines); i++ {
		if isMarkerLine(lineAt(b, lines, i), sigArmorEnd) {
			end = i
			break
		}
	}
	if end < 0 {
		if last := lineAt(b, lines, len(lines)-1); bytes.HasPrefix(last, []byte(sigArmorEnd)) {
			// The last line is the END marker with text after it: say
			// so, as requireBareClearsigned always has.
			return errors.New("extraneous bytes after the END marker")
		}
		return errors.New("no END line")
	}
	if end != len(lines)-1 {
		return errors.New("extraneous bytes after the END marker")
	}
	inHeaders := true
	crc := -1
	for i := begin + 1; i < end; i++ {
		l := lineAt(b, lines, i)
		if bytes.HasPrefix(l, []byte("-")) {
			return fmt.Errorf("line %d, inside the armor, starts with \"-\"", i+1)
		}
		if inHeaders {
			if isBlankLine(l) {
				inHeaders = false
				continue
			}
			if !bytes.Contains(l, []byte(": ")) {
				return fmt.Errorf("armor header line %d has no \": \"", i+1)
			}
			continue
		}
		t := bytes.Trim(l, " \t\r")
		if crc >= 0 && len(t) > 0 {
			return fmt.Errorf("line %d follows the CRC line (line %d) before the END line", i+1, crc+1)
		}
		if bytes.HasPrefix(t, []byte("=")) {
			crc = i
		}
	}
	if crc >= 0 {
		t := bytes.Trim(lineAt(b, lines, crc), " \t\r")
		sum, err := base64.StdEncoding.DecodeString(string(t[1:]))
		if len(t) != 5 || err != nil || len(sum) != 3 {
			return fmt.Errorf("the CRC line (line %d) is not \"=\" and four base64 characters", crc+1)
		}
		if uint32(sum[0])<<16|uint32(sum[1])<<8|uint32(sum[2]) != crc24(decoded) {
			return fmt.Errorf("the armor checksum (line %d) does not match the signature", crc+1)
		}
	}
	return nil
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
