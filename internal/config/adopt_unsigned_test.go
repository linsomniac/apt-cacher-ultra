package config

import "testing"

// Spec C5 safeguard 1, C11: adoption.adopt_unsigned defaults to true, and
// an explicit false survives Load (the bool pre-populate pattern).
func TestLoad_AdoptUnsignedDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Adoption.AdoptUnsigned {
		t.Error("adopt_unsigned default = false, want true")
	}
	cfg, err = Load(writeTOML(t, dir, "config2.toml", `
[cache]
dir = "`+dir+`"
[adoption]
adopt_unsigned = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adoption.AdoptUnsigned {
		t.Error("explicit adopt_unsigned = false was clobbered to true")
	}
}
