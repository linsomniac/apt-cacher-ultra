package config

import "testing"

// adoption.write_package_list_differences is on by default, and an
// explicit false survives Load (the bool pre-populate pattern: a zero
// value cannot tell "absent" from "false").
func TestLoad_WritePackageListDifferencesDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeTOML(t, dir, "config.toml", `
[cache]
dir = "`+dir+`"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Adoption.WritePackageListDifferences {
		t.Error("write_package_list_differences default = false, want true")
	}

	off, err := Load(writeTOML(t, dir, "off.toml", `
[cache]
dir = "`+dir+`"
[adoption]
write_package_list_differences = false
`))
	if err != nil {
		t.Fatalf("Load with an explicit false: %v", err)
	}
	if off.Adoption.WritePackageListDifferences {
		t.Error("an explicit write_package_list_differences = false was clobbered to true")
	}
}
