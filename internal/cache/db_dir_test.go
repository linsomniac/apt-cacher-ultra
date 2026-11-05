package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenWithDBDir(t *testing.T) {
	for _, mode := range []string{"default", "same", "separate"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir, dbDir := t.TempDir(), ""
			switch mode {
			case "same":
				dbDir = dir
			case "separate":
				dbDir = filepath.Join(t.TempDir(), "database ?#%")
			}
			c, err := OpenWithDBDir(ctx, dir, dbDir, discardSlog())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if dbDir == "" {
				dbDir = dir
			}
			if c.Dir() != dir || c.DBDir() != dbDir {
				t.Fatalf("directories = %q, %q", c.Dir(), c.DBDir())
			}
			for _, name := range []string{"cache.db", "cache.db-wal", "cache.db-shm"} {
				if _, err := os.Stat(filepath.Join(dbDir, name)); err != nil {
					t.Fatal(err)
				}
				if mode == "separate" {
					if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
						t.Fatalf("unexpected %s in cache root: %v", name, err)
					}
				}
			}
			for _, sub := range []string{"pool", "tmp", "staging"} {
				if st, err := os.Stat(filepath.Join(dir, sub)); err != nil || !st.IsDir() {
					t.Fatalf("missing %s: %v", sub, err)
				}
				if mode == "separate" {
					if _, err := os.Stat(filepath.Join(dbDir, sub)); !os.IsNotExist(err) {
						t.Fatalf("unexpected %s in database directory: %v", sub, err)
					}
				}
			}
			w, err := c.NewTempBlob()
			if err != nil {
				t.Fatal(err)
			}
			const body = "cached package"
			if _, err := w.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			hash, err := w.Finalize(int64(len(body)))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.PutBlob(ctx, hash, int64(len(body))); err != nil {
				t.Fatal(err)
			}
			if err := c.PutURLPath(ctx, URLPath{CanonicalScheme: "http", CanonicalHost: "mirror.example", Path: "/pool/example.deb", BlobHash: &hash}); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenWithDBDir(ctx, dir, dbDir, discardSlog())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			debs, err := reopened.ListCachedDebs(ctx, "example")
			if err != nil || len(debs) != 1 || debs[0].BlobHash != hash {
				t.Fatalf("reopened packages = %v, err = %v", debs, err)
			}
			if got, err := os.ReadFile(reopened.BlobPath(hash)); err != nil || string(got) != body {
				t.Fatalf("reopened blob = %q, err = %v", got, err)
			}
		})
	}
}

func TestOpenWithDBDirFailureDoesNotFallBack(t *testing.T) {
	dir, dbDir := t.TempDir(), filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dbDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenWithDBDir(context.Background(), dir, dbDir, discardSlog())
	if err == nil {
		_ = c.Close()
		t.Fatal("opened cache with a file as the database directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !os.IsNotExist(err) {
		t.Fatalf("unexpected fallback database: %v", err)
	}
}
