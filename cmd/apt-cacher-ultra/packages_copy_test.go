package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func TestPackagesCopy_HappyPath(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	wantBody := "this is the nginx .deb body"
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "archive.ubuntu.com", "/pool/nginx_1.18.0-1_amd64.deb", wantBody)
	})

	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "nginx_1.18.0-1_amd64.deb", dest}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("destination contents = %q, want %q", got, wantBody)
	}
}

func TestPackagesCopy_DestinationAsDirectory(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	body := "deb body"
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "h", "/pool/foo_1.0_amd64.deb", body)
	})

	dstDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "foo_1.0_amd64.deb", dstDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	wantPath := filepath.Join(dstDir, "foo_1.0_amd64.deb")
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read %s: %v", wantPath, err)
	}
	if string(got) != body {
		t.Errorf("contents = %q, want %q", got, body)
	}
}

func TestPackagesCopy_NotFoundReturns1(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "h", "/pool/foo_1.0_amd64.deb", "x")
	})

	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "missing_pkg.deb", dest}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "no cached .deb") {
		t.Errorf("stderr should explain not-found: %q", stderr.String())
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("destination should not exist on not-found")
	}
}

func TestPackagesCopy_AmbiguousReturns2(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		// Same filename, different content per host → different blob hashes.
		put("http", "archive.ubuntu.com", "/pool/foo_1.0_amd64.deb", "variant A")
		put("http", "mirror.example.org", "/pool/foo_1.0_amd64.deb", "variant B different")
	})

	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "foo_1.0_amd64.deb", dest}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ambiguous") {
		t.Errorf("stderr should explain ambiguity: %q", stderr.String())
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("destination should not exist on ambiguous match")
	}
}

func TestPackagesCopy_OverwritesSilently(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	body := "fresh body"
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "h", "/pool/foo_1.0_amd64.deb", body)
	})

	dest := filepath.Join(t.TempDir(), "out.deb")
	if err := os.WriteFile(dest, []byte("stale data that should be replaced"), 0o644); err != nil {
		t.Fatalf("seed existing dest: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "foo_1.0_amd64.deb", dest}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != body {
		t.Errorf("contents = %q, want %q (silent overwrite expected)", got, body)
	}
}

func TestPackagesCopy_SameBlobMultiHostIsNotAmbiguous(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	body := "shared body across mirrors"
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		// Same content via two hosts → same blob hash; should be unambiguous.
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		hash, err := w.Finalize(int64(len(body)))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if err := c.PutBlob(context.Background(), hash, int64(len(body))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		for _, host := range []string{"archive.ubuntu.com", "mirror.example.org"} {
			u := cache.URLPath{
				CanonicalScheme: "http",
				CanonicalHost:   host,
				Path:            "/pool/shared_1.0_amd64.deb",
				BlobHash:        &hash,
				UpstreamURL:     "http://" + host + "/pool/shared_1.0_amd64.deb",
			}
			if err := c.PutURLPath(context.Background(), u); err != nil {
				t.Fatalf("PutURLPath: %v", err)
			}
		}
	})

	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "shared_1.0_amd64.deb", dest}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != body {
		t.Errorf("contents = %q, want %q", got, body)
	}
}

func TestPackagesCopy_MissingBlobOnDiskReturns2(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	var poolPath string
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		hash := put("http", "h", "/pool/foo_1.0_amd64.deb", "body")
		poolPath = c.BlobPath(hash)
	})
	// Simulate disk inconsistency: DB still references the blob, but the
	// pool file has been deleted (e.g. mid-GC or manual cleanup).
	if err := os.Remove(poolPath); err != nil {
		t.Fatalf("rm pool file: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "foo_1.0_amd64.deb", dest}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "blob missing") {
		t.Errorf("stderr should explain missing blob: %q", stderr.String())
	}
}

func TestPackagesCopy_MissingArgsReturns1(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {})

	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", cfg, "foo.deb"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("only one positional: exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
}

func TestPackagesCopy_ConfigMissingReturns3(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.deb")
	var stdout, stderr bytes.Buffer
	code := runPackagesCopy([]string{"-config", "/no/such/path.toml", "foo.deb", dest}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("missing config: exit code = %d, want 3; stderr=%s", code, stderr.String())
	}
}
