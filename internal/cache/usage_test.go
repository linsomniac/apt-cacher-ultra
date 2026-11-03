package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageMigrationPersistenceAndIsolation(t *testing.T) {
	ctx := context.Background()
	dir, dbDir := t.TempDir(), t.TempDir()
	db, err := openDB(filepath.Join(dbDir, "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 7; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,3,1,0)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := OpenWithDBDir(ctx, dir, dbDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := usage.New(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	u.RecordFetch(100, time.Second)
	u.RecordServe(200, time.Second, true)
	if err := u.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Cache maintenance must not reset lifetime usage.
	if _, err := c.db.Exec(`DELETE FROM blob`); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = OpenWithDBDir(ctx, dir, dbDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	u, err = usage.New(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	s := u.Snapshot()
	if s.Lifetime.FetchedBytes != 100 || s.Lifetime.ServedBytes != 200 || s.Session.Requests != 0 {
		t.Fatal(s)
	}
	if err := c.SaveUsage(ctx, usage.Totals{SinceUnix: 1, Requests: -1}); err == nil {
		t.Fatal("accepted invalid totals")
	}
	if _, err := c.db.Exec(`INSERT INTO usage_totals SELECT 2,since_unixtime,fetched_bytes,served_bytes,cache_hit_bytes,requests,cache_hit_requests,fetch_nanoseconds,serve_nanoseconds FROM usage_totals`); err == nil {
		t.Fatal("accepted second row")
	}
	if v, err := readSchemaVersion(ctx, c.db); err != nil || v != 8 {
		t.Fatalf("version %d, %v", v, err)
	}
}
