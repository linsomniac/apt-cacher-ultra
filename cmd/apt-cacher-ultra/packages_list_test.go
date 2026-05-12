package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// writePackagesConfig writes a minimal config TOML pointing at a fresh
// cache dir under t.TempDir(), and returns (configPath, cacheDir). The
// cache dir is created but NOT pre-populated with cache.db; the caller
// is responsible for opening, seeding, and closing the cache before
// invoking the subcommand under test.
func writePackagesConfig(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	cfg := filepath.Join(root, "config.toml")
	body := "[cache]\ndir = \"" + cacheDir + "\"\nlisten = \"127.0.0.1:3142\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg, cacheDir
}

// seedCacheDebs opens a fresh cache at dir, runs seedFn (which uses the
// returned *cache.Cache to plant blobs and url_path rows), then closes
// it. After the function returns, the cache is on disk and ready for a
// subcommand to reopen it. Returns the sha256 hex of each blob written
// via the seedFn's helper closures so tests can assert on hashes.
func seedCacheDebs(t *testing.T, dir string, seedFn func(c *cache.Cache, put func(scheme, host, path, content string) string)) {
	t.Helper()
	discard := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	c, err := cache.Open(context.Background(), dir, discard)
	if err != nil {
		t.Fatalf("seed: cache.Open: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Fatalf("seed: cache.Close: %v", err)
		}
	}()

	put := func(scheme, host, path, content string) string {
		t.Helper()
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		hash, err := w.Finalize(int64(len(content)))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if err := c.PutBlob(context.Background(), hash, int64(len(content))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
		u := cache.URLPath{
			CanonicalScheme: scheme,
			CanonicalHost:   host,
			Path:            path,
			BlobHash:        &hash,
			UpstreamURL:     scheme + "://" + host + path,
		}
		if err := c.PutURLPath(context.Background(), u); err != nil {
			t.Fatalf("PutURLPath: %v", err)
		}
		return hash
	}

	seedFn(c, put)
}

func TestPackagesList_EmptyCache(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		// no rows
	})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	// Header should be present, no data rows.
	if !strings.Contains(stdout.String(), "NAME") {
		t.Errorf("stdout missing header: %q", stdout.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected only header line, got %d lines: %q", len(lines), stdout.String())
	}
}

func TestPackagesList_ShowsCachedDebs(t *testing.T) {
	// Freeze AGE rendering so the test does not race the wall clock.
	prev := nowForPackagesCmd
	nowForPackagesCmd = func() time.Time { return time.Unix(2_000_000_000, 0) }
	defer func() { nowForPackagesCmd = prev }()

	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "archive.ubuntu.com", "/pool/main/n/nginx/nginx_1.18.0-1_amd64.deb", "nginx body")
		put("http", "archive.ubuntu.com", "/pool/main/v/vim/vim_9.0_amd64.deb", "vim body")
		// non-deb — should NOT appear
		put("http", "archive.ubuntu.com", "/dists/noble/InRelease", "release body")
	})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nginx_1.18.0-1_amd64.deb") {
		t.Errorf("stdout missing nginx: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "vim_9.0_amd64.deb") {
		t.Errorf("stdout missing vim: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "InRelease") {
		t.Errorf("stdout should not list metadata file: %q", stdout.String())
	}
}

func TestPackagesList_SubstringFilter(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "h", "/pool/nginx_1.0_amd64.deb", "n")
		put("http", "h", "/pool/libnginx-mod_1.0_amd64.deb", "ln")
		put("http", "h", "/pool/vim_9.0_amd64.deb", "v")
	})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "nginx"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "vim_9.0") {
		t.Errorf("substring 'nginx' should exclude vim: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "nginx_1.0_amd64.deb") {
		t.Errorf("stdout missing nginx: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "libnginx-mod_1.0_amd64.deb") {
		t.Errorf("stdout missing libnginx-mod: %q", stdout.String())
	}
}

func TestPackagesList_PlainFormat(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		put("http", "h", "/pool/foo_1.0_amd64.deb", "foo")
		put("http", "h", "/pool/bar_2.0_amd64.deb", "bar")
	})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "-format", "plain"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := "bar_2.0_amd64.deb\nfoo_1.0_amd64.deb\n"
	if stdout.String() != want {
		t.Errorf("plain format mismatch:\ngot:  %q\nwant: %q", stdout.String(), want)
	}
}

func TestPackagesList_JSONFormat(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	var hashA string
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		hashA = put("http", "archive.ubuntu.com", "/pool/foo_1.0_amd64.deb", "foo body")
	})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "-format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var rows []packageJSONRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, stdout.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Filename != "foo_1.0_amd64.deb" {
		t.Errorf("filename = %q", rows[0].Filename)
	}
	if rows[0].BlobHash != hashA {
		t.Errorf("blob_hash = %q, want %q", rows[0].BlobHash, hashA)
	}
	if rows[0].Size != int64(len("foo body")) {
		t.Errorf("size = %d, want %d", rows[0].Size, len("foo body"))
	}
	if len(rows[0].Hosts) != 1 || rows[0].Hosts[0] != "archive.ubuntu.com" {
		t.Errorf("hosts = %v", rows[0].Hosts)
	}
}

func TestPackagesList_DedupesMirrorsByContent(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {
		// Same content under same filename via two hosts — same blob, one row.
		w, err := c.NewTempBlob()
		if err != nil {
			t.Fatalf("NewTempBlob: %v", err)
		}
		if _, err := w.Write([]byte("shared")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		hash, err := w.Finalize(int64(len("shared")))
		if err != nil {
			t.Fatalf("Finalize: %v", err)
		}
		if err := c.PutBlob(context.Background(), hash, int64(len("shared"))); err != nil {
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

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "-format", "plain"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.String() != "shared_1.0_amd64.deb\n" {
		t.Errorf("plain output should have one row, got %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = runPackagesList([]string{"-config", cfg, "-format", "json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("json exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	var rows []packageJSONRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if len(rows[0].Hosts) != 2 {
		t.Errorf("want 2 hosts in collapsed row, got %v", rows[0].Hosts)
	}
}

func TestPackagesList_InvalidFormatReturns1(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "-format", "yaml"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("invalid -format: exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid -format") {
		t.Errorf("stderr should explain the rejection: %q", stderr.String())
	}
}

func TestPackagesList_ConfigMissingReturns3(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", "/no/such/path.toml"}, &stdout, &stderr)
	if code != 3 {
		t.Errorf("missing config: exit code = %d, want 3; stderr=%s", code, stderr.String())
	}
}

func TestPackagesList_TooManyPositionalArgsReturns1(t *testing.T) {
	cfg, cacheDir := writePackagesConfig(t)
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(scheme, host, path, content string) string) {})

	var stdout, stderr bytes.Buffer
	code := runPackagesList([]string{"-config", cfg, "one", "two"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("extra args: exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
}

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1024 * 1024, "1.0M"},
		{1024 * 1024 * 1024, "1.0G"},
	} {
		if got := humanSize(tc.n); got != tc.want {
			t.Errorf("humanSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-1 * time.Second, "0s"},
		{5 * time.Second, "5s"},
		{2 * time.Minute, "2m"},
		{3 * time.Hour, "3h"},
		{2 * 24 * time.Hour, "2d"},
		{2 * 7 * 24 * time.Hour, "2w"},
	} {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
