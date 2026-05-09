package tlsmitm

import (
	"errors"
	"reflect"
	"testing"
)

func TestTranslateRegex_AcceptedShapes(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			"shape 1: literal hostname",
			`^foo\.example\.com$`,
			[]string{"foo.example.com"},
		},
		{
			"shape 1: single-label hostname",
			`^localhost$`,
			[]string{"localhost"},
		},
		{
			"shape 2: positive single-label prefix",
			`^[a-z0-9-]+\.foo\.com$`,
			[]string{"foo.com"},
		},
		{
			"shape 2: negated-dot single-label prefix",
			`^[^.]+\.foo\.com$`,
			[]string{"foo.com"},
		},
		{
			"shape 3: optional 2-letter region prefix",
			`^([a-z]{2}\.)?archive\.ubuntu\.com$`,
			[]string{"archive.ubuntu.com"},
		},
		{
			"shape 4: alternation of literals",
			`^(foo\.example\.com|bar\.example\.com)$`,
			[]string{"foo.example.com", "bar.example.com"},
		},
		{
			"shape 4: alternation deduplicated",
			`^(foo\.example\.com|foo\.example\.com|bar\.example\.com)$`,
			[]string{"foo.example.com", "bar.example.com"},
		},
		// SPEC6 §5.1.1.1: anchors are optional. The unanchored
		// variants of every accepted shape translate identically.
		{
			"shape 1 unanchored: literal hostname",
			`foo\.example\.com`,
			[]string{"foo.example.com"},
		},
		{
			"shape 1 unanchored: single-label hostname",
			`localhost`,
			[]string{"localhost"},
		},
		{
			"shape 2 unanchored: positive single-label prefix",
			`[a-z0-9-]+\.foo\.com`,
			[]string{"foo.com"},
		},
		{
			"shape 2 unanchored: negated-dot single-label prefix",
			`[^.]+\.foo\.com`,
			[]string{"foo.com"},
		},
		{
			"shape 3 unanchored: optional 2-letter region prefix",
			`([a-z]{2}\.)?archive\.ubuntu\.com`,
			[]string{"archive.ubuntu.com"},
		},
		{
			"shape 4 unanchored: alternation of literals",
			`(foo\.example\.com|bar\.example\.com)`,
			[]string{"foo.example.com", "bar.example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TranslateRegex(tc.pattern)
			if err != nil {
				t.Fatalf("TranslateRegex(%q) error: %v", tc.pattern, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TranslateRegex(%q) = %v, want %v", tc.pattern, got, tc.want)
			}
		})
	}
}

func TestTranslateRegex_RejectedShapes(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
	}{
		{"empty regex", ``},
		{"just .*", `.*`},
		// Asymmetric anchors are rejected because their semantics
		// diverge from the bare-literal form: `^foo\.com` matches
		// any string starting with foo.com (e.g. `foo.com.evil.org`)
		// and `foo\.com$` matches any string ending with it. SPEC6
		// §5.1.1.1's "anchors optional" rule is both-or-neither.
		{"missing leading anchor (only $)", `foo\.example\.com$`},
		{"missing trailing anchor (only ^)", `^foo\.example\.com`},
		{"charclass admitting dot", `^[a-z0-9.-]+\.foo\.com$`},
		{"unbounded multi-label suffix path", `^.*\.foo\.com$`},
		{"label with non-LDH character", `^foo_bar\.com$`},
		{"label too long", `^aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\.com$`},
		{"hostname starts with dot", `^\.foo\.com$`},
		{"hostname ends with dot", `^foo\.com\.$`},
		{"alternation with non-literal alt", `^(foo\.com|.*)$`},

		// SPEC6 §5.1.1.1 enumerates the accepted grammar exhaustively;
		// other shapes that "look safe" are still rejected so the
		// translator's accepted set tracks the spec exactly. If the spec
		// is broadened later, these cases move to TestTranslateRegex_AcceptedShapes.
		{"shape 2: narrower than spec — [a-z]+", `^[a-z]+\.foo\.com$`},
		{"shape 2: narrower than spec — [A-Z]+", `^[A-Z]+\.foo\.com$`},
		{"shape 2: broader than spec — [a-zA-Z0-9-]+", `^[a-zA-Z0-9-]+\.foo\.com$`},
		{"shape 2: negated class other than [^.]", `^[^.x]+\.foo\.com$`},
		{"shape 3: three-letter prefix not blessed by spec", `^([a-z]{3}\.)?archive\.ubuntu\.com$`},
		{"shape 3: one-letter prefix not blessed by spec", `^([a-z]\.)?archive\.ubuntu\.com$`},
		{"shape 3: unbounded prefix", `^([a-z]+\.)?archive\.ubuntu\.com$`},
		{"shape 3: uppercase region prefix", `^([A-Z]{2}\.)?archive\.ubuntu\.com$`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TranslateRegex(tc.pattern)
			if err == nil {
				t.Fatalf("TranslateRegex(%q) = %v, want error", tc.pattern, got)
			}
			if !errors.Is(err, ErrNameConstraintsUnsupported) {
				t.Errorf("TranslateRegex(%q) error not wrapping ErrNameConstraintsUnsupported: %v", tc.pattern, err)
			}
		})
	}
}

func TestTranslateRegex_ParseFailure(t *testing.T) {
	_, err := TranslateRegex(`^[a-z$`) // unclosed character class
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !errors.Is(err, ErrNameConstraintsUnsupported) {
		t.Errorf("got %v, want wrap of ErrNameConstraintsUnsupported", err)
	}
}

func TestValidateHostname(t *testing.T) {
	good := []string{
		"foo.example.com",
		"x",
		"localhost",
		"a-b.c-d.e",
		"aaa-bbb-ccc.tld",
	}
	for _, s := range good {
		if err := validateHostname(s); err != nil {
			t.Errorf("validateHostname(%q): %v", s, err)
		}
	}
	bad := []string{
		"",
		"-foo.com",
		"foo-.com",
		"foo..com",
		".foo.com",
		"foo.com.",
		"foo_bar.com",
		"foo bar.com",
	}
	for _, s := range bad {
		if err := validateHostname(s); err == nil {
			t.Errorf("validateHostname(%q): expected error", s)
		}
	}
}
