package cache

import "testing"

// TestSchemaV6_AddsVersionAndDroppedAt verifies the version-aware-retention
// columns the v6 migration added are there at the current schema: the
// package rows' version (the Debian version the mirror rule ranks on; on
// package_hash then, carried to package_list_entry by schema 11) and
// url_path.dropped_at (the hold-grace clock).
func TestSchemaV6_AddsVersionAndDroppedAt(t *testing.T) {
	if CurrentSchemaVersion < 6 {
		t.Fatalf("CurrentSchemaVersion = %d, want >= 6", CurrentSchemaVersion)
	}
	c := openCache(t)
	for _, q := range []string{
		`SELECT version FROM package_list_entry LIMIT 0`,
		`SELECT dropped_at FROM url_path LIMIT 0`,
	} {
		if _, err := c.db.Exec(q); err != nil {
			t.Errorf("%q: column missing after migration: %v", q, err)
		}
	}
	// version is NOT NULL DEFAULT '' so an insert without it is allowed and
	// reads back as the empty string (the non-binary / pre-migration marker).
	v, err := readSchemaVersion(t.Context(), c.db)
	if err != nil {
		t.Fatalf("readSchemaVersion: %v", err)
	}
	if v != CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", v, CurrentSchemaVersion)
	}
}
