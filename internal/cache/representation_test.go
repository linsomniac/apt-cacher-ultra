package cache

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRepresentation_MetadataSurvivesRestartAndFollowsBlobGC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c, err := Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	hash := seedBlob(t, c, "shared content")
	a := "Mon, 01 Jan 2024 00:00:00 GMT"
	b := "Tue, 02 Jan 2024 00:00:00 GMT"
	for host, date := range map[string]string{"a": a, "b": b} {
		if err := c.PutRepresentation(ctx, "http", host, "/Packages", hash, date); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, dir, discardSlog())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	for host, want := range map[string]string{"a": a, "b": b, "unknown": ""} {
		got, err := c.RepresentationLastModified(ctx, "http", host, "/Packages", hash)
		if err != nil || got != want {
			t.Fatal(host, got, err)
		}
	}
	if err := c.PutRepresentation(ctx, "http", "a", "/Packages", hash, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.RepresentationLastModified(ctx, "http", "a", "/Packages", hash); got != "" {
		t.Fatal("stale date retained")
	}
	if _, err := c.db.Exec(`DELETE FROM blob WHERE hash = ?`, hash); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM representation_metadata`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestSchemaV8_BackfillsDatesForExactRepresentations(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for v := 0; v < 7; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO blob(hash,size,created_at) VALUES ('` + hA + `',1,1),('` + hB + `',1,1)`,
		`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,upstream_lastmod) VALUES ('http','h','/r/dists/x/InRelease','` + hA + `','http://h/r/dists/x/InRelease',1,'Mon, 01 Jan 2024 00:00:00 GMT')`,
		`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,inrelease_lastmod,created_at,anchor_form) VALUES ('http','h','/r/dists/x','` + hB + `','Tue, 02 Jan 2024 00:00:00 GMT',1,'inline')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, db, discardSlog()); err != nil {
		t.Fatal(err)
	}
	for hash, want := range map[string]string{hA: "Mon, 01 Jan 2024 00:00:00 GMT", hB: "Tue, 02 Jan 2024 00:00:00 GMT"} {
		var got string
		if err := db.QueryRow(`SELECT last_modified FROM representation_metadata WHERE blob_hash = ?`, hash).Scan(&got); err != nil || got != want {
			t.Fatal(got, err)
		}
	}
}

func TestRepresentation_GCPrunesUnownedDatesForSharedBlobs(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	hash := seedBlob(t, c, "shared")
	const date = "Mon, 01 Jan 2024 00:00:00 GMT"
	for _, path := range []string{"/live", "/orphan", "/staging"} {
		if err := c.PutRepresentation(ctx, "http", "h", path, hash, date); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/live", BlobHash: &hash, UpstreamURL: "http://h/live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE representation_metadata SET observed_at = 1 WHERE path <> '/staging'`); err != nil {
		t.Fatal(err)
	}
	// No blobs are reclaimable: the date cleanup must still run.
	if reaped, err := c.RunBlobGCBatch(ctx, 100, 1); err != nil || len(reaped) != 0 {
		t.Fatal(reaped, err)
	}
	for path, want := range map[string]string{"/live": date, "/orphan": "", "/staging": date} {
		got, err := c.RepresentationLastModified(ctx, "http", "h", path, hash)
		if err != nil || got != want {
			t.Fatal(path, got, err)
		}
	}
}
