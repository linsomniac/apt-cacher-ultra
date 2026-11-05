package cache

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageMigrationPersistenceAndIsolation(t *testing.T) {
	for _, from := range []int{7, 8} {
		t.Run(fmt.Sprintf("schema_%d_to_9", from), func(t *testing.T) {
			ctx := context.Background()
			dir, dbDir := t.TempDir(), t.TempDir()
			db, err := openDB(filepath.Join(dbDir, "cache.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			for v := 0; v < from; v++ {
				if err := applyMigration(ctx, db, v); err != nil {
					t.Fatal(err)
				}
			}
			const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			const date = "Mon, 01 Jan 2024 00:00:00 GMT"
			for _, q := range []string{
				`INSERT INTO blob(hash,size,created_at,refcount) VALUES('` + hash + `',3,1,0)`,
				`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,upstream_lastmod) VALUES('http','h','/Packages','` + hash + `','http://h/Packages',1,'` + date + `')`,
				`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path) VALUES('http','h','/repo/dists/stable')`,
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if from == 8 {
				// Preserve existing v8 values, including a date distinct from the URL row.
				if _, err := db.Exec(`INSERT INTO representation_metadata(canonical_scheme,canonical_host,path,blob_hash,last_modified,observed_at) VALUES('http','h','/Packages',?,'Tue, 02 Jan 2024 00:00:00 GMT',123)`, hash); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE suite_freshness SET metadata_generation=42; UPDATE metadata_clock SET generation=42`); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			c, err := OpenWithDBDir(ctx, dir, dbDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if c != nil {
					_ = c.Close()
				}
			})
			u, err := usage.New(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			u.RecordFetch(100, time.Second)
			u.RecordServe(200, time.Second, true)
			if err := u.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c, err = OpenWithDBDir(ctx, dir, dbDir, nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err = usage.New(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			s := u.Snapshot()
			if s.Lifetime.FetchedBytes != 100 || s.Lifetime.ServedBytes != 200 || s.Session.Requests != 0 {
				t.Fatal(s)
			}
			wantDate, wantObserved, wantGeneration := date, int64(0), int64(0)
			if from == 8 {
				wantDate, wantObserved, wantGeneration = "Tue, 02 Jan 2024 00:00:00 GMT", 123, 42
			}
			gotDate, err := c.RepresentationLastModified(ctx, "http", "h", "/Packages", hash)
			if err != nil || gotDate != wantDate {
				t.Fatalf("origin date = %q, %v; want %q", gotDate, err, wantDate)
			}
			var observed, generation, clock, blobSize int64
			if err := c.db.QueryRow(`SELECT observed_at FROM representation_metadata WHERE blob_hash=?`, hash).Scan(&observed); err != nil || observed != wantObserved {
				t.Fatalf("observed_at = %d, %v; want %d", observed, err, wantObserved)
			}
			if err := c.db.QueryRow(`SELECT metadata_generation FROM suite_freshness`).Scan(&generation); err != nil || generation != wantGeneration {
				t.Fatalf("generation = %d, %v; want %d", generation, err, wantGeneration)
			}
			if err := c.db.QueryRow(`SELECT generation FROM metadata_clock WHERE id=1`).Scan(&clock); err != nil || clock != wantGeneration {
				t.Fatalf("clock = %d, %v; want %d", clock, err, wantGeneration)
			}
			if err := c.db.QueryRow(`SELECT size FROM blob WHERE hash=?`, hash).Scan(&blobSize); err != nil || blobSize != 3 {
				t.Fatalf("blob size = %d, %v", blobSize, err)
			}
			// Removing cached content must not reset the independent usage aggregate.
			if _, err := c.db.Exec(`DELETE FROM url_path; DELETE FROM blob`); err != nil {
				t.Fatal(err)
			}
			saved, err := c.LoadUsage(ctx)
			if err != nil || saved != s.Lifetime.Totals {
				t.Fatalf("usage after eviction: %+v, %v", saved, err)
			}
			if err := c.SaveUsage(ctx, usage.Totals{SinceUnix: 1, Requests: -1}); err == nil {
				t.Fatal("accepted invalid totals")
			}
			if _, err := c.db.Exec(`INSERT INTO usage_totals SELECT 2,since_unixtime,fetched_bytes,served_bytes,cache_hit_bytes,requests,cache_hit_requests,fetch_nanoseconds,serve_nanoseconds FROM usage_totals`); err == nil {
				t.Fatal("accepted second row")
			}
			if v, err := readSchemaVersion(ctx, c.db); err != nil || v != 9 {
				t.Fatalf("version %d, %v; want 9", v, err)
			}
		})
	}
}
