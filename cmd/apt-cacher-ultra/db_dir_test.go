package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
)

func TestSeparateDatabaseDaemonAndPackageCommands(t *testing.T) {
	const body = "package bytes on bulk storage"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	cacheDir, dbDir := t.TempDir(), t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	text := fmt.Sprintf(`[cache]
dir = %q
db_dir = %q
[tls_mitm]
enabled = false
[admin]
enabled = false
[[mirror]]
prefix = "/repo"
upstream = %q
`, cacheDir, dbDir, upstream.URL+"/")
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := serveUntilCleanup(t, ctx, cancel, func(ctx context.Context) error {
		return serveListeners(ctx, cfg, newTestLogger(), ln, nil, nil, nil)
	})
	if err := waitForDaemonReady(t, ln.Addr().String(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MISS", "HIT"} {
		resp, got := getNoFollow(t, "http://"+ln.Addr().String()+"/repo/pool/example.deb")
		if resp.StatusCode != 200 || got != body || resp.Header.Get("X-Cache") != want {
			t.Fatalf("request: status=%d body=%q cache=%q, want %s", resp.StatusCode, got, resp.Header.Get("X-Cache"), want)
		}
	}
	for _, name := range []string{"cache.db", "cache.db-wal", "cache.db-shm"} {
		if _, err := os.Stat(filepath.Join(dbDir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cacheDir, name)); !os.IsNotExist(err) {
			t.Fatalf("database in cache root: %v", err)
		}
	}
	checkPackageCommands(t, configPath, body)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not stop")
	}
	checkPackageCommands(t, configPath, body)
}

func checkPackageCommands(t *testing.T, configPath, body string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runPackagesList([]string{"-config", configPath, "-format", "plain"}, &stdout, &stderr); code != 0 || stdout.String() != "example.deb\n" {
		t.Fatalf("packages list: code=%d stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	destination := filepath.Join(t.TempDir(), "example.deb")
	stdout.Reset()
	stderr.Reset()
	if code := runPackagesCopy([]string{"-config", configPath, "example.deb", destination}, &stdout, &stderr); code != 0 {
		t.Fatalf("packages copy: code=%d stderr=%s", code, stderr.String())
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != body {
		t.Fatalf("copied package = %q, err = %v", got, err)
	}
}

func TestPackageCommandsAfterMovingDatabase(t *testing.T) {
	configPath, cacheDir := writePackagesConfig(t)
	const body = "existing cached package"
	seedCacheDebs(t, cacheDir, func(c *cache.Cache, put func(string, string, string, string) string) {
		put("http", "mirror.example", "/pool/example.deb", body)
	})
	dbDir := t.TempDir()
	// A closed database has checkpointed its WAL. Moving just the database
	// leaves its pool in place, as documented for an existing installation.
	if err := os.Rename(filepath.Join(cacheDir, "cache.db"), filepath.Join(dbDir, "cache.db")); err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprintf("[cache]\ndir = %q\ndb_dir = %q\n", cacheDir, dbDir)
	if err := os.WriteFile(configPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	checkPackageCommands(t, configPath, body)
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.db")); !os.IsNotExist(err) {
		t.Fatalf("commands recreated the old database: %v", err)
	}
}

func TestDaemonStorageLocks(t *testing.T) {
	dir, dbDir, otherDir := t.TempDir(), t.TempDir(), t.TempDir()
	unlock, err := lockDaemonStorage(dir, dbDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, dirs := range [][2]string{{dir, t.TempDir()}, {otherDir, dbDir}} {
		if release, err := lockDaemonStorage(dirs[0], dirs[1]); err == nil {
			release()
			t.Fatal("second daemon acquired shared storage")
		} else if !strings.Contains(err.Error(), "another daemon") {
			t.Fatal(err)
		}
	}
	// Failure to acquire the database must release the pool's lock.
	if release, err := lockDaemonStorage(otherDir, ""); err != nil {
		t.Fatalf("failed acquisition leaked cache lock: %v", err)
	} else {
		release()
	}
	unlock()
	if release, err := lockDaemonStorage(dir, dbDir); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	for _, sameDir := range []string{"", dir, alias} {
		if release, err := lockDaemonStorage(dir, sameDir); err != nil {
			t.Fatalf("same directory %q: %v", sameDir, err)
		} else {
			release()
		}
	}
}
