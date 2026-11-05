package apttext

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// ruleOf names the rule that refused err (a ShapeError's), "" for none.
func ruleOf(err error) string {
	var se *ShapeError
	if errors.As(err, &se) {
		return se.Rule
	}
	if err != nil {
		return "?" + err.Error()
	}
	return ""
}

// textRule names what layer T made of a body: "" accepted, "not" not
// clearsigned (ErrNotClearsigned), else the refusing rule.
func textRule(err error) string {
	if errors.Is(err, ErrNotClearsigned) {
		return "not"
	}
	return ruleOf(err)
}

// The armor block the text-layer tests put after a signed text: layer T
// never reads inside it but for its marker and dash lines.
const textTestArmor = "-----BEGIN PGP SIGNATURE-----\n\nAAAA\n-----END PGP SIGNATURE-----\n"

// TestSplitClearsigned_PortsApt pins the port of apt's SplitClearSignedFile
// on shapes the corpus lacks (TestAptTextLayer_RefusesExactlyTheSplitRows
// holds it to apt on the corpus): each body, and the rule apt's split
// refuses it by, or the M and the number of armor blocks it makes of it.
func TestSplitClearsigned_PortsApt(t *testing.T) {
	const begin = ClearsignBeginMarker
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	for _, tc := range []struct {
		name, body string
		rule       string // "" accepted, "not" not clearsigned, else the rule
		m          string // M, when accepted
		blocks     int
	}{
		{"empty", "", "not", "", 0},
		{"plain text", "Origin: x\n", "not", "", 0},
		{"the marker on line 2", "x\n" + begin + "\n\nx\n" + textTestArmor, "T1", "", 0},
		{"the marker after spaces", "  " + begin + "\n\nx\n" + textTestArmor, "not", "", 0},
		{"the marker, then blanks and CRs", begin + " \t\r\r\n\nx\n" + textTestArmor, "", "x", 1},
		{"the marker, then a CR and a space", begin + "\r \n\nx\n" + textTestArmor, "not", "", 0},
		{"the file ends inside the headers", begin + "\nHash: SHA256\n", "T2", "", 0},
		{"a header line starting with a dash", begin + "\n-Hash: SHA256\n\nx\n" + textTestArmor, "T2", "", 0},
		{"headers ended by blanks and a CR", begin + "\nHash: SHA256\n \t\r\nx\n" + textTestArmor, "", "x", 1},
		{"no header", begin + "\n\nx\n" + textTestArmor, "", "x", 1},
		{"headers of any content", begin + "\nComment: x\n\v\nhash: sha1\n\nx\n" + textTestArmor, "", "x", 1},
		{"the file ends inside the text", begin + "\n\nx\n", "T3", "", 0},
		{"an undashed dash line", begin + "\n\n-x\n" + textTestArmor, "T3", "", 0},
		{"a dash and a space alone", begin + "\n\n- \n" + textTestArmor, "T3", "", 0},
		{"dash-escaped lines", begin + "\n\n- -x\n- y\n" + textTestArmor, "", "-x\ny", 1},
		{"CRLF, trailing blanks and CRs", crlf(begin+"\n\na \t\nb\r\n\n") + crlf(textTestArmor), "", "a\nb\n", 1},
		{"a CR before a trailing blank stays", begin + "\n\na\r \n" + textTestArmor, "", "a\r", 1},
		{"no text line", begin + "\n\n" + textTestArmor, "", "", 1},
		{"the signature's BEGIN line with blanks", begin + "\n\nx\n-----BEGIN PGP SIGNATURE----- \t\r\n\nAAAA\n-----END PGP SIGNATURE-----\n", "", "x", 1},
		{"a dash line inside the armor", begin + "\n\nx\n-----BEGIN PGP SIGNATURE-----\n\n-x\n-----END PGP SIGNATURE-----\n", "T4", "", 0},
		{"an empty line after the END line", begin + "\n\nx\n" + textTestArmor + "\n", "T4", "", 0},
		{"the file ends inside the armor", begin + "\n\nx\n-----BEGIN PGP SIGNATURE-----\n\nAAAA\n", "T4", "", 0},
		{"two armor blocks", begin + "\n\nx\n" + textTestArmor + textTestArmor, "", "x", 2},
		{"the END line with blanks, no final LF", begin + "\n\nx\n-----BEGIN PGP SIGNATURE-----\n\nAAAA\n-----END PGP SIGNATURE----- \t\r", "", "x", 1},
	} {
		cs, err := SplitClearsigned([]byte(tc.body))
		if got := textRule(err); got != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, got, tc.rule)
			continue
		}
		if err != nil {
			continue
		}
		if string(cs.Message) != tc.m || cs.Message == nil || cs.Blocks != tc.blocks {
			t.Errorf("%s: M %q (nil %v), %d blocks; want %q, %d", tc.name, cs.Message, cs.Message == nil, cs.Blocks, tc.m, tc.blocks)
		}
		if !bytes.Equal(cs.Armor[0], []byte(SigArmorBegin)) || !bytes.Equal(cs.Armor[len(cs.Armor)-1], []byte(SigArmorEnd)) {
			t.Errorf("%s: the armor runs from %q to %q; want the BEGIN line to the END line", tc.name, cs.Armor[0], cs.Armor[len(cs.Armor)-1])
		}
	}
}

// TestSplitDetached_PortsApt pins the port of apt's check of a Release.gpg
// (VerifyDetachedSignatureFile) on shapes the corpus lacks: each file, and
// the rule apt's check refuses it by, or the number of armor blocks.
func TestSplitDetached_PortsApt(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		rule       string
		blocks     int
	}{
		{"empty", "", "T1", 0},
		{"a binary signature", "\x89\x01\xb3\x04\x00\x01\x08", "T1", 0},
		{"a blank line first", "\n" + textTestArmor, "T1", 0},
		{"text first", "x\n" + textTestArmor, "T1", 0},
		{"no BEGIN line at all", "x\n\n-----END PGP SIGNATURE-----\n", "T1", 0},
		{"an empty line after the END line", textTestArmor + "\n", "T4", 0},
		{"text after the END line", textTestArmor + "x\n", "T4", 0},
		{"the file ends inside the armor", "-----BEGIN PGP SIGNATURE-----\n\nAAAA\n", "T4", 0},
		{"a dash line inside the armor", "-----BEGIN PGP SIGNATURE-----\n\n-x\n-----END PGP SIGNATURE-----\n", "T4", 0},
		{"a second BEGIN line inside the armor", "-----BEGIN PGP SIGNATURE-----\n-----BEGIN PGP SIGNATURE-----\n-----END PGP SIGNATURE-----\n", "T4", 0},
		{"blanks and CRs on the marker lines", "-----BEGIN PGP SIGNATURE----- \r\n\nAAAA\n-----END PGP SIGNATURE-----\t\r\n", "", 1},
		{"one block, no final LF", strings.TrimSuffix(textTestArmor, "\n"), "", 1},
		{"two blocks", textTestArmor + textTestArmor, "", 2},
	} {
		lines, blocks, err := SplitDetached([]byte(tc.body))
		if got := ruleOf(err); got != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, got, tc.rule)
			continue
		}
		if err == nil && (blocks != tc.blocks || !bytes.Equal(lines[0], []byte(SigArmorBegin))) {
			t.Errorf("%s: %d blocks, first line %q; want %d and the BEGIN line", tc.name, blocks, lines[0], tc.blocks)
		}
	}
}

// TestAptLine_StripsAsApt: apt's LineBuffer strips trailing LFs and CRs,
// then trailing spaces and tabs (gpgv.cc:94-98 at apt 2.2.4), and a body
// splits on LF with a last line without one (the final review's A-C1); a
// final LF starts no empty line (AptLines' AIDEV-NOTE).
func TestAptLine_StripsAsApt(t *testing.T) {
	for in, want := range map[string]string{
		"x":         "x",
		"x \t":      "x",
		"x\r":       "x",
		"x\r\r\n":   "x",
		"x \r":      "x",
		"x\r ":      "x\r",
		"x \t\r \t": "x \t\r",
		"\r":        "",
		" \t\r":     "",
		"x\v":       "x\v",
		"x\u00a0":   "x\u00a0",
	} {
		if got := string(AptLine([]byte(in))); got != want {
			t.Errorf("AptLine(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][]string{
		"":        nil,
		"a":       {"a"},
		"a\n":     {"a"},
		"a\n\n":   {"a", ""},
		"a\r\nb":  {"a", "b"},
		"\n":      {""},
		"a\nb \n": {"a", "b"},
	} {
		got := AptLines([]byte(in))
		var gs []string
		for _, l := range got {
			gs = append(gs, string(l))
		}
		if strings.Join(gs, "|") != strings.Join(want, "|") || len(gs) != len(want) {
			t.Errorf("AptLines(%q) = %q, want %q", in, gs, want)
		}
	}
}

// TestInline_TheCachesRules pins what layer T adds to apt's split:
// no NUL (T5), no blank after the clearsign marker (T6: CRs may follow it),
// no END marker in M (F0, which reads M only: an END marker in an armor
// header's value is the armor layer's), one armor block; apt's own refusals
// come first.
func TestInline_TheCachesRules(t *testing.T) {
	const begin = ClearsignBeginMarker
	for _, tc := range []struct{ name, body, rule string }{
		{"a NUL in the text", begin + "\n\nx\x00y\n" + textTestArmor, "T5"},
		{"a NUL in a cleartext header", begin + "\nHash: SHA\x00256\n\nx\n" + textTestArmor, "T5"},
		{"a dash-escaped END line", begin + "\n\n- " + SigArmorEnd + "\n" + textTestArmor, "F0"},
		{"the END marker inside a text line", begin + "\n\nDescription: x " + SigArmorEnd + " y\n" + textTestArmor, "F0"},
		{"the END marker in an armor header", begin + "\n\nx\n-----BEGIN PGP SIGNATURE-----\nComment: " + SigArmorEnd + "\n\nAAAA\n-----END PGP SIGNATURE-----\n", ""},
		{"a space after the marker", begin + " \n\nx\n" + textTestArmor, "T6"},
		{"a tab after the marker", begin + "\t\n\nx\n" + textTestArmor, "T6"},
		{"a space, then a CR, after the marker", begin + " \r\n\nx\n" + textTestArmor, "T6"},
		{"a CR after the marker", begin + "\r\n\nx\n" + textTestArmor, ""},
		{"CRs after the marker", begin + "\r\r\n\nx\n" + textTestArmor, ""},
		{"a blank on line 2, not line 1", begin + "\n \nx\n" + textTestArmor, ""},
		{"two armor blocks", begin + "\n\nx\n" + textTestArmor + textTestArmor, "T4/one-block"},
		{"apt's refusal first", "\x00\n" + begin + "\n\nx\n" + textTestArmor, "T1"},
		{"accepted", begin + "\n\nx\n" + textTestArmor, ""},
	} {
		if _, err := Inline([]byte(tc.body)); ruleOf(err) != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, ruleOf(err), tc.rule)
		}
	}
	if _, err := Detached([]byte(textTestArmor + textTestArmor)); ruleOf(err) != "T4/one-block" {
		t.Errorf("a Release.gpg of two blocks: %v; want T4/one-block", err)
	}
}
