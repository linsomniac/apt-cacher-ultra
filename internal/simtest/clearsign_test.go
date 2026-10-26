package simtest

import (
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/linsomniac/apt-cacher-ultra/internal/aptpath"
)

// Deferred minor D-7: text with a line that begins with "-" panics — its
// decoded plaintext would differ from it.
func TestClearsignedFixture_PanicsOnADashLine(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal(`no panic for a line beginning with "-"`)
		}
	}()
	ClearsignedFixture("Origin: x\n- y\n")
}

// The fixture is what spec C3's sniff and reconcile's decoder take for a
// clearsigned InRelease, and its plaintext is the text it wraps.
func TestClearsignedFixture(t *testing.T) {
	cases := []struct{ text, plain string }{
		{"Origin: x\nSHA256:\n 0123 1 main/Packages\n", "Origin: x\nSHA256:\n 0123 1 main/Packages"},
		{"Origin: x\nSuite: y", "Origin: x\nSuite: y"},
		// Each line's trailing spaces and tabs are trimmed, as the doc
		// says (review M-3).
		{"Origin: x \t\nSuite: y\t \n", "Origin: x\nSuite: y"},
	}
	for _, c := range cases {
		f := ClearsignedFixture(c.text)
		if !aptpath.SniffInRelease(f) {
			t.Fatalf("%q: fails the InRelease sniff", f)
		}
		b, _ := clearsign.Decode(f)
		if b == nil {
			t.Fatalf("%q: does not decode", f)
		}
		if string(b.Plaintext) != c.plain {
			t.Fatalf("plaintext = %q, want %q", b.Plaintext, c.plain)
		}
	}
}
