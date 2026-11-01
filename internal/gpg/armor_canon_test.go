package gpg

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/apttext"
)

// TestParseArmor_Rules pins A1–A6 on armors the corpus lacks, the edges of
// each limit included: each armor, and the rule that refuses it or the
// bytes it decodes to.
func TestParseArmor_Rules(t *testing.T) {
	packets := []byte("the bytes the CRC line covers; never parsed here!!") // 50 octets: one "=" of padding
	b64 := base64.StdEncoding.EncodeToString(packets)
	crc := func(b []byte) string {
		c := crc24(b)
		return "=" + base64.StdEncoding.EncodeToString([]byte{byte(c >> 16), byte(c >> 8), byte(c)})
	}
	good := crc(packets)
	if !strings.HasSuffix(b64, "=") || strings.HasSuffix(b64, "==") {
		t.Fatalf("test premise: %q does not end in one padding character", b64)
	}
	// armorOf joins lines between the BEGIN and END lines.
	armorOf := func(lines ...string) string {
		return apttext.SigArmorBegin + "\n" + strings.Join(lines, "\n") + "\n" + apttext.SigArmorEnd + "\n"
	}
	big := bytes.Repeat([]byte{0xa5}, armorMaxPacketBytes+1)
	var bigLines []string
	for s := base64.StdEncoding.EncodeToString(big); len(s) > 0; {
		n := min(64, len(s))
		bigLines = append(bigLines, s[:n])
		s = s[n:]
	}
	padBits := b64[:len(b64)-2] + string(b64[len(b64)-2]+1) + "="
	headers := func(n int) []string {
		var h []string
		for i := range n {
			h = append(h, fmt.Sprintf("Comment: %d", i))
		}
		return h
	}
	for _, tc := range []struct {
		name, armor, rule string
	}{
		{"the canonical form", armorOf("", b64, good), ""},
		{"no CRC line", armorOf("", b64), ""},
		{"64-character lines", armorOf("", b64[:20], b64[20:], good), ""},
		{"a 76-character line", armorOf("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 57)), crc(bytes.Repeat([]byte{1}, 57))), ""},
		{"a 77-character line", armorOf("", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 57))+"A", crc(bytes.Repeat([]byte{1}, 57))), "A4"},
		{"the padding alone on the last line", armorOf("", b64[:len(b64)-1], "=", good), ""},
		{"real signers' headers", armorOf("Version: GnuPG v1", "Comment: BSN Pgp v1.0.0.0 (caf\xc3\xa9)", "X-Foo-1: a: b", "", b64, good), ""},
		{"16 headers", armorOf(append(headers(16), "", b64, good)...), ""},
		{"17 headers", armorOf(append(headers(17), "", b64, good)...), "A2"},
		{"a 64-character key", armorOf("K"+strings.Repeat("a", 63)+": x", "", b64, good), ""},
		{"a 65-character key", armorOf("K"+strings.Repeat("a", 64)+": x", "", b64, good), "A2"},
		{"a 1024-byte value", armorOf("Comment: "+strings.Repeat("v", 1024), "", b64, good), ""},
		{"a 1025-byte value", armorOf("Comment: "+strings.Repeat("v", 1025), "", b64, good), "A2"},
		{"no colon", armorOf("Comment x", "", b64, good), "A2"},
		{"no space after the colon", armorOf("Comment:x", "", b64, good), "A2"},
		{"an empty value", armorOf("Comment: ", "", b64, good), "A2"},
		{"an empty key", armorOf(": x", "", b64, good), "A2"},
		{"a key starting with a digit", armorOf("1x: y", "", b64, good), "A2"},
		{"a space in the key", armorOf("Com ment: x", "", b64, good), "A2"},
		{"a control byte in the value", armorOf("Comment: a\x01b", "", b64, good), "A2"},
		{"DEL in the value", armorOf("Comment: a\x7fb", "", b64, good), "A2"},
		{"the END line before an empty line", apttext.SigArmorBegin + "\n" + apttext.SigArmorEnd + "\n", "A3"},
		{"a header, then the END line", armorOf("Comment: x"), "A3"},
		{"no base64 line", armorOf(""), "A4"},
		{"a CRC line and no base64 line", armorOf("", good), "A4"},
		{"two empty lines", armorOf("", "", b64, good), "A4"},
		{"an empty line among the base64 lines", armorOf("", b64[:20], "", b64[20:], good), "A4"},
		{"a space in a base64 line", armorOf("", b64[:20]+" "+b64[20:], good), "A4"},
		{"padding mid-stream", armorOf("", b64, b64, good), "A4"},
		{"the unused bits of the last character set", armorOf("", padBits, good), "A4"},
		{"over 64 KiB", armorOf(append([]string{""}, bigLines...)...), "A4"},
		{"a forged CRC", armorOf("", b64, "=AAAA"), "A5"},
		{"a line after the CRC line", armorOf("", b64, good, b64), "A5"},
		{"an empty line after the CRC line", armorOf("", b64, good, ""), "A5"},
		{"two CRC lines", armorOf("", b64, good, good), "A5"},
		{"a CRC line holding padding", armorOf("", b64, "=AB=C"), "A4"},
	} {
		lines := apttext.AptLines([]byte(tc.armor))
		got, err := parseArmor(lines, 1)
		if ruleOf(err) != tc.rule {
			t.Errorf("%s: %v (rule %q); want rule %q", tc.name, err, ruleOf(err), tc.rule)
			continue
		}
		if err == nil && len(got) == 0 {
			t.Errorf("%s: no bytes decoded", tc.name)
		}
	}
	// A1 and A6 hold of every block layer T hands over (its BEGIN line
	// first, its END line last); parseArmor checks them anyway.
	lines := apttext.AptLines([]byte(armorOf("", b64, good)))
	if _, err := parseArmor(lines[1:], 2); ruleOf(err) != "A1" {
		t.Errorf("no BEGIN line first: %v; want A1", err)
	}
	if _, err := parseArmor(lines[:len(lines)-1], 1); ruleOf(err) != "A6" {
		t.Errorf("no END line last: %v; want A6", err)
	}
	if got, err := parseArmor(lines, 1); err != nil || !bytes.Equal(got, packets) {
		t.Errorf("the canonical form decodes to %q, %v; want %q", got, err, packets)
	}
}
