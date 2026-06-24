package cache

import "testing"

// TestSchemaV6_AddsVersionAndDroppedAt verifies the v6 migration adds the
// version-aware-retention columns: package_hash.version (the Debian version
// the mirror rule ranks on) and url_path.dropped_at (the hold-grace clock).
func TestSchemaV6_AddsVersionAndDroppedAt(t *testing.T) {
	if CurrentSchemaVersion < 6 {
		t.Fatalf("CurrentSchemaVersion = %d, want >= 6", CurrentSchemaVersion)
	}
	c := openCache(t)
	for _, q := range []string{
		`SELECT version FROM package_hash LIMIT 0`,
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
