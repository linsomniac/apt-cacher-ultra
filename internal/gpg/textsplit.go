package gpg

// Layer T of the hybrid verifier (the hardening plan's DN-11, ruling HD-16,
// docs/superpowers/plans/2026-10-28-gc-and-upstream-hardening.md): an exact
// port of apt's text layer, apt-pkg/contrib/gpgv.cc — LineBuffer::readFrom
// (gpgv.cc:82-100 at apt 2.2.4), SplitClearSignedFile (:418-536) and the
// detached check (:250-316, VerifyDetachedSignatureFile from apt 3.0),
// identical in the six measured apt versions 2.2.4 to 3.2.0 but for type
// names (PE/final-review/A/tools/diffsplit.sh). The cache does not watch
// apt's splitter from beside another parser: its output, apt's message M and
// apt's signature lines S, is the cache's own input — S goes to the armor
// layer (armor_canon.go), M is what the signatures are checked against and
// what VerifyInline returns, exactly what the client's apt hands gpgv or sqv
// and then its Release parser.
//
// AIDEV-NOTE: keep splitClearsigned and splitDetached byte-exact ports;
// TestAptTextLayer_RefusesExactlyTheSplitRows holds them to apt's own
// refusals (classes.txt's "split" rows) on every corpus row. What the cache
// adds on top — no NUL (T5), no END marker in M (F0), one armor block
// (T4/one-block) — lives in inlineTextLayer and detachedTextLayer, never in
// the ports.
//
// AIDEV-NOTE: history moved here from the deleted requireBareClearsigned
// and isArmoredSignature (rulings R-D2, HD-8). The first-line rule: apt
// refuses an InRelease whose first line is not the clearsign marker,
// leading whitespace, a blank line or a BOM included, and so does the
// cache (T1; the handler's sniff, aptpath.SniffInRelease, is apt's
// StartsWithGPGClearTextSignature and stays looser than the splitter: the
// fail-safe direction). The 2026-09 security review's VC-2 (a line after
// the END line, an empty one included; bytes around a Release.gpg's armor; a
// binary Release.gpg) and C-4 (a forged CRC line, armor_canon.go's A5) were
// adopted until the 2026-10-28 hardening plan's F-rules refused them beside
// go-crypto's decoders; the hybrid refuses them by apt's own grammar (T1,
// T4) before any other parser runs. Until then the detached form enforced no
// bare-armor rule at all, because armor.Decode strips bytes around the
// armor: every client of the suite then failed `apt update`.

import (
	"bytes"
	"errors"
	"fmt"
)

// The marker lines (RFC 9580 §6.2, §7): the clearsign marker opens an
// InRelease; the signature armor's BEGIN and END lines open and close a
// Release.gpg and the armor after an InRelease's signed text.
const (
	clearsignBeginMarker = "-----BEGIN PGP SIGNED MESSAGE-----"
	sigArmorBegin        = "-----BEGIN PGP SIGNATURE-----"
	sigArmorEnd          = "-----END PGP SIGNATURE-----"
)

// shapeError is a refusal by one of DN-11's structural rules: the rule
// (T1–T5, F0, T4/one-block for the text layer; A1–A6 for the armor; P1–P5
// for the packets) and what it saw. VerifyInline wraps it in
// ErrMalformedClearsign, VerifyDetached in ErrMalformedSignature, so the
// operator's detail names the layer and the rule ("armor A4: line 7: 80
// base64 characters, the limit is 76").
type shapeError struct {
	rule   string
	detail string
}

func (e *shapeError) Error() string {
	layer := "text"
	switch e.rule[0] {
	case 'A':
		layer = "armor"
	case 'P':
		layer = "packet"
	}
	return layer + " " + e.rule + ": " + e.detail
}

// refuse is a shapeError for rule, its detail formatted.
func refuse(rule, format string, args ...any) error {
	return &shapeError{rule: rule, detail: fmt.Sprintf(format, args...)}
}

// errNotClearsigned is the port's "not a clearsigned file": apt's
// SplitClearSignedFile returning false without an error — no line of the
// body is the clearsign marker (or the body is empty: apt's error then,
// "doesn't contain all expected parts", a refusal either way).
// VerifyInline turns it into ErrMissingSignature.
var errNotClearsigned = errors.New("not a clearsigned file")

// aptLine is a line as apt's LineBuffer::readFrom holds it (gpgv.cc:94-98 at
// 2.2.4): its trailing LFs and CRs removed, in any order, then its
// trailing spaces and tabs, in that order — so "x\r " keeps its CR and
// "x \r" loses both (the final review's A-C1).
func aptLine(l []byte) []byte {
	return bytes.TrimRight(bytes.TrimRight(l, "\r\n"), " \t")
}

// aptLines splits b as getline reads it — on LF, a final line without LF a
// line too, none in an empty b — each line as aptLine leaves it. lines[i] is
// the file's line i+1.
//
// AIDEV-NOTE: history moved here from the deleted splitLines (F0's helper):
// a final LF ends the last line and starts no empty one, so "a\n" is one
// line and "a\n\n" two (the second empty), as getline reads them. Were a
// final LF to start an empty last line, F0 refused every file that ends in
// a newline then, and T4 would now ("unsigned lines"): TestAptLine_StripsAsApt.
func aptLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		l := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			l, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		out = append(out, aptLine(l))
	}
	return out
}

// clearsigned is what apt's SplitClearSignedFile makes of an InRelease.
type clearsigned struct {
	// message is M: the signed text's lines, each as apt reads it with a
	// dash-escape's "- " removed, joined with LF, no LF after the last.
	// Never nil.
	message []byte
	// armor is S's lines: from the signature's BEGIN line to the last END
	// line, each as aptLine leaves it (apt writes each with an LF).
	armor [][]byte
	// armorLine is armor[0]'s line number in the InRelease, from 1.
	armorLine int
	// blocks is the number of armor blocks in S: apt reads every one.
	blocks int
}

// splitClearsigned is apt's SplitClearSignedFile (gpgv.cc:418-536 at
// 2.2.4), ported line for line. It returns errNotClearsigned where apt
// returns false without an error, and a shapeError where apt reports one:
//
//   - T1: line 1 is the clearsign marker; else, when a later line is,
//     refused ("does not start with a signed message block"); else not
//     clearsigned.
//   - T2: the armor headers run from line 2 to the first empty line; the
//     end of the file before it, or a header line starting with "-",
//     refuses. Their content is ignored: apt keeps "Hash:" lines and never
//     uses them (gpgv gets M and S, a detached verification).
//   - T3: the signed text runs to the signature's BEGIN line; the end of the
//     file before it refuses. A line starting with "-" is that BEGIN line,
//     or dash-escaped ("- ", removed), or refused. M is the text's lines
//     joined with LF.
//   - T4: the signature lines: inside an armor block, the END line closes
//     it and any other line starting with "-" refuses; outside one, a
//     BEGIN line opens another and any other line refuses ("unsigned
//     lines"); the end of the file inside a block refuses.
func splitClearsigned(b []byte) (clearsigned, error) {
	lines := aptLines(b)
	if len(lines) == 0 {
		return clearsigned{}, errNotClearsigned
	}
	if !bytes.Equal(lines[0], []byte(clearsignBeginMarker)) {
		for i, l := range lines[1:] {
			if bytes.Equal(l, []byte(clearsignBeginMarker)) {
				return clearsigned{}, refuse("T1", "line %d is %q, line 1 is not (apt: does not start with a signed message block)", i+2, clearsignBeginMarker)
			}
		}
		return clearsigned{}, errNotClearsigned
	}
	i := 1
	for ; ; i++ {
		if i == len(lines) {
			return clearsigned{}, refuse("T2", "the file ends inside the armor headers (no empty line after them)")
		}
		if len(lines[i]) == 0 {
			i++
			break
		}
		if lines[i][0] == '-' {
			return clearsigned{}, refuse("T2", "armor header line %d starts with \"-\"", i+1)
		}
	}
	var m []byte
	first := true
	for ; ; i++ {
		if i == len(lines) {
			return clearsigned{}, refuse("T3", "the file ends inside the signed text (no %q line)", sigArmorBegin)
		}
		l := lines[i]
		if len(l) > 0 && l[0] == '-' {
			if bytes.Equal(l, []byte(sigArmorBegin)) {
				break
			}
			if !bytes.HasPrefix(l, []byte("- ")) {
				return clearsigned{}, refuse("T3", "line %d of the signed text starts with \"-\" and is neither dash-escaped (\"- \") nor %q", i+1, sigArmorBegin)
			}
			l = l[2:]
		}
		if !first {
			m = append(m, '\n')
		}
		m = append(m, l...)
		first = false
	}
	out := clearsigned{message: m, armorLine: i + 1, blocks: 1}
	if out.message == nil {
		out.message = []byte{}
	}
	start := i
	open := true
	for i++; i < len(lines); i++ {
		l := lines[i]
		switch {
		case open && bytes.Equal(l, []byte(sigArmorEnd)):
			open = false
		case open && len(l) > 0 && l[0] == '-':
			return clearsigned{}, refuse("T4", "line %d, inside the signature armor, starts with \"-\"", i+1)
		case !open && bytes.Equal(l, []byte(sigArmorBegin)):
			open = true
			out.blocks++
		case !open:
			return clearsigned{}, refuse("T4", "line %d follows the signature's END line (apt: unsigned lines)", i+1)
		}
	}
	if open {
		return clearsigned{}, refuse("T4", "the file ends inside the signature armor (no %q line)", sigArmorEnd)
	}
	out.armor = lines[start:]
	return out, nil
}

// splitDetached is apt's check of a Release.gpg (gpgv.cc:250-316 at 2.2.4,
// VerifyDetachedSignatureFile from 3.0), ported line for line, on lines as
// aptLine leaves them (apt then hands gpgv or sqv the raw file). It returns
// the lines and the number of armor blocks:
//
//   - T1: a line before the first BEGIN line, or no BEGIN line at all
//     (apt's NODATA, its "unsupported binary format" for a binary
//     signature), refuses.
//   - T4: inside an armor block, the END line closes it and any other line
//     starting with "-" refuses; outside one, a BEGIN line opens another
//     and any other line refuses ("lines not belonging to a signature");
//     the end of the file inside a block refuses ("unclosed signatures").
func splitDetached(b []byte) ([][]byte, int, error) {
	lines := aptLines(b)
	open := false
	found := 0
	stray := -1 // the first line outside every armor block
	for i, l := range lines {
		if open {
			switch {
			case bytes.Equal(l, []byte(sigArmorEnd)):
				open = false
			case len(l) > 0 && l[0] == '-':
				return nil, 0, refuse("T4", "line %d, inside the signature armor, starts with \"-\"", i+1)
			}
			continue
		}
		if bytes.Equal(l, []byte(sigArmorBegin)) {
			open = true
			found++
			if stray >= 0 {
				break
			}
			continue
		}
		if stray < 0 {
			stray = i
		}
		if found != 0 {
			break
		}
	}
	switch {
	case found == 0:
		return nil, 0, refuse("T1", "no line is %q (apt: NODATA; a binary signature is refused too)", sigArmorBegin)
	case stray == 0:
		return nil, 0, refuse("T1", "line 1 is not %q", sigArmorBegin)
	case stray > 0:
		return nil, 0, refuse("T4", "line %d is outside the signature armor (apt: lines not belonging to a signature)", stray+1)
	case open:
		return nil, 0, refuse("T4", "the file ends inside the signature armor (no %q line)", sigArmorEnd)
	}
	return lines, found, nil
}

// inlineTextLayer is layer T for an InRelease: apt's split, then what the
// cache adds to it (DN-11):
//
//   - T5: no NUL byte anywhere: apt's split is length-aware, but gpgv
//     parses the armor headers as C strings and sqv refuses NUL in the
//     signed text (a_textline_nul, debian:13); no Release holds one.
//   - F0: M holds no "-----END PGP SIGNATURE-----": apt's split accepts a
//     dash-escaped END line in the signed text, its Release parser then
//     refuses the file (i_dash_escaped_end, every apt), and the cache's
//     ParseRelease would ignore the line.
//   - T4/one-block: one armor block (the deviation family "one armor
//     block": apt reads every block; no surveyed vendor publishes two).
//
// AIDEV-NOTE: F0 was "the first END marker in the body begins its last
// line" (requireEndMarkerBeginsLastLine, deleted), there for go-crypto's
// clearsign decoder, which cut the armor at the first "\n-----END PGP
// SIGNATURE-----" substring, and for i_dash_escaped_end. The cache's own
// armor parser has no such cut, so F0 now reads M only (Task 9b's ruling):
// an END marker in an armor header's value (vm3_i/vm3_d_end_marker_in_header,
// accepted by every apt) is A2's business, not F0's.
func inlineTextLayer(b []byte) (clearsigned, error) {
	cs, err := splitClearsigned(b)
	if err != nil {
		return clearsigned{}, err
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return clearsigned{}, refuse("T5", "a NUL byte at offset %d", i)
	}
	if i := bytes.Index(cs.message, []byte(sigArmorEnd)); i >= 0 {
		return clearsigned{}, refuse("F0", "the signed text holds %q (line %d of it), which apt's Release parser refuses", sigArmorEnd, bytes.Count(cs.message[:i], []byte("\n"))+1)
	}
	if cs.blocks > 1 {
		return clearsigned{}, refuse("T4/one-block", "%d armor blocks: the cache reads one", cs.blocks)
	}
	return cs, nil
}

// detachedTextLayer is layer T for a Release.gpg: apt's check, then one
// armor block (T4/one-block, as inline). It returns the armor's lines, the
// file's every line (T1 and T4 left none outside the block).
func detachedTextLayer(b []byte) ([][]byte, error) {
	lines, blocks, err := splitDetached(b)
	if err != nil {
		return nil, err
	}
	if blocks > 1 {
		return nil, refuse("T4/one-block", "%d armor blocks: the cache reads one", blocks)
	}
	return lines, nil
}
