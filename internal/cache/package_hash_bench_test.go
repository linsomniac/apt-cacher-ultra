package cache

// Issue #14 package_hash benchmark (env-gated; excluded from CI by its gate):
// where does a snapshot's package_hash delete (and insert) spend its time?
//
// Run:  ACU_BENCH=1 ACU_BENCH_K=1,4 ACU_BENCH_VARIANTS=base \
//       go test ./internal/cache -run TestPackageHashBench -v -timeout 60m
// Knobs: ACU_BENCH_K, ACU_BENCH_REPS, ACU_BENCH_VARIANTS.
//
// AIDEV-NOTE: before/after measurements live in
// docs/superpowers/plans/2026-11-09-issue-14-gc-followup-evidence.md.
//
// Variants of the package_hash table, each in its own fresh DB opened with
// production's DSN (openDB):
//   base      the live schema (rowid table, PK autoindex on
//             scheme,host,path,snapshot_id, idx_snapshot, idx_pkg_arch,
//             the partial flat index)
//   nopk      base without the PK autoindex (attribution only)
//   nopkarch  base without idx_package_hash_pkg_arch (attribution only)
//   cache256  base, PRAGMA cache_size = 256 MiB on the bench connection
//   wr        WITHOUT ROWID, PRIMARY KEY (snapshot_id, scheme, host, path),
//             plus a lookup index (scheme, host, path, snapshot_id)
//
// Data: 12 suites on one host, 70k packages a suite, K snapshots a suite;
// 1% of a suite's packages change version per snapshot, so neighbouring
// snapshots share 99% of their paths (as real -updates suites do).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	b14Suites = 12
	b14Pkgs   = 70000
)

func b14Env(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func b14Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// liveDDL returns the CREATE statements of package_hash and its indexes in
// a freshly migrated cache.
func liveDDL(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rows, err := c.db.Query(`SELECT type, sql FROM sqlite_master WHERE tbl_name = 'package_hash' AND sql IS NOT NULL ORDER BY type DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var typ, s string
		if err := rows.Scan(&typ, &s); err != nil {
			t.Fatal(err)
		}
		// Drop the FK clause: the bench DB has no suite_snapshot.
		s = strings.Replace(s, "REFERENCES suite_snapshot(snapshot_id)", "", 1)
		out = append(out, s)
	}
	return out
}

func variantDDL(t *testing.T, live []string, variant string) []string {
	t.Helper()
	switch variant {
	case "base", "cache256":
		return live
	case "nopk", "nopkarch":
		var out []string
		for _, s := range live {
			if strings.HasPrefix(s, "CREATE TABLE") && variant == "nopk" {
				s = strings.Replace(s, "PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id)", "CHECK (1)", 1)
				if !strings.Contains(s, "CHECK (1)") {
					t.Fatalf("PK clause not found in %q", s)
				}
			}
			if variant == "nopkarch" && strings.Contains(s, "idx_package_hash_pkg_arch") {
				continue
			}
			out = append(out, s)
		}
		return out
	case "wr":
		var out []string
		for _, s := range live {
			if strings.HasPrefix(s, "CREATE TABLE") {
				s = strings.Replace(s, "PRIMARY KEY (canonical_scheme, canonical_host, path, snapshot_id)", "PRIMARY KEY (snapshot_id, canonical_scheme, canonical_host, path)", 1)
				s = strings.TrimSpace(s) + " WITHOUT ROWID"
			}
			if strings.Contains(s, "idx_package_hash_snapshot") {
				// The clustered PK leads with snapshot_id: the lookup index
				// replaces it.
				s = `CREATE INDEX idx_package_hash_lookup ON package_hash(canonical_scheme, canonical_host, path, snapshot_id)`
			}
			out = append(out, s)
		}
		return out
	}
	t.Fatalf("unknown variant %q", variant)
	return nil
}

func ver(i, j int) int { return (j + i%100) / 100 }

func insertSnapshot(ctx context.Context, db *sql.DB, suite, snap, seq int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	st, err := tx.Prepare(`INSERT INTO package_hash (canonical_scheme, canonical_host, path, declared_sha256, snapshot_id, package_name, architecture, version) VALUES ('http','archive.ubuntu.com',?,?,?,?,'amd64',?)`)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	for i := 0; i < b14Pkgs; i++ {
		v := ver(i, seq)
		name := fmt.Sprintf("s%dpkg%d", suite, i)
		path := fmt.Sprintf("/ubuntu/pool/main/%c/%s/%s_1.%d_amd64.deb", 'a'+rune(i%26), name, name, v)
		if _, err := st.Exec(path, b14Hash(path), snap, name, "1."+strconv.Itoa(v)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func deleteChunked(ctx context.Context, db *sql.DB, variant string, snap int) (int, error) {
	total := 0
	for {
		var q string
		if variant == "wr" {
			q = `DELETE FROM package_hash WHERE snapshot_id = ?1 AND path IN (SELECT path FROM package_hash WHERE snapshot_id = ?1 LIMIT ?2)`
		} else {
			q = `DELETE FROM package_hash WHERE rowid IN (SELECT rowid FROM package_hash WHERE snapshot_id = ?1 LIMIT ?2)`
		}
		res, err := db.ExecContext(ctx, q, snap, retiredChunkRows)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n == 0 {
			return total, nil
		}
	}
}

func TestPackageHashBench(t *testing.T) {
	if os.Getenv("ACU_BENCH") == "" {
		t.Skip("set ACU_BENCH=1")
	}
	ctx := context.Background()
	live := liveDDL(t)
	for _, s := range live {
		t.Logf("live DDL: %s", strings.Join(strings.Fields(s), " "))
	}
	variants := strings.Split(os.Getenv("ACU_BENCH_VARIANTS"), ",")
	if variants[0] == "" {
		variants = []string{"base", "nopk", "nopkarch", "cache256", "wr"}
	}
	ks := strings.Split(os.Getenv("ACU_BENCH_K"), ",")
	if ks[0] == "" {
		ks = []string{"1", "4", "12"}
	}
	reps := b14Env("ACU_BENCH_REPS", 3)
	for _, kStr := range ks {
		k, _ := strconv.Atoi(kStr)
		for _, variant := range variants {
			dir := t.TempDir()
			db, err := openDB(filepath.Join(dir, "bench.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			if variant == "cache256" {
				if _, err := db.Exec(`PRAGMA cache_size = -262144`); err != nil {
					t.Fatal(err)
				}
			}
			for _, s := range variantDDL(t, live, variant) {
				if _, err := db.Exec(s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			// Seed: suite s, snapshots j = 0..k-1; snapshot id = s*1000+j.
			t0 := time.Now()
			for j := 0; j < k; j++ {
				for s := 0; s < b14Suites; s++ {
					if err := insertSnapshot(ctx, db, s, s*1000+j, j); err != nil {
						t.Fatal(err)
					}
				}
			}
			seed := time.Since(t0)
			var n int
			_ = db.QueryRow(`SELECT count(*) FROM package_hash`).Scan(&n)
			// Measure: for reps suites, insert snapshot k (the newest), then
			// delete snapshot 0 (the oldest), keeping the size steady.
			var ins, del, delWhole time.Duration
			for r := 0; r < reps; r++ {
				s := r % b14Suites
				t1 := time.Now()
				if err := insertSnapshot(ctx, db, s, s*1000+k+r, k+r); err != nil {
					t.Fatal(err)
				}
				ins += time.Since(t1)
				t2 := time.Now()
				got, err := deleteChunked(ctx, db, variant, s*1000)
				if err != nil {
					t.Fatal(err)
				}
				if got != b14Pkgs {
					t.Fatalf("deleted %d rows, want %d", got, b14Pkgs)
				}
				del += time.Since(t2)
			}
			// One whole-snapshot delete in a single statement (startup B's shape).
			{
				s := (reps) % b14Suites
				t3 := time.Now()
				res, err := db.Exec(`DELETE FROM package_hash WHERE snapshot_id = ?`, s*1000)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := res.RowsAffected()
				delWhole = time.Since(t3)
				if got != b14Pkgs {
					t.Fatalf("whole delete: %d rows", got)
				}
			}
			var pages, pageSize int
			_ = db.QueryRow(`PRAGMA page_count`).Scan(&pages)
			_ = db.QueryRow(`PRAGMA page_size`).Scan(&pageSize)
			t.Logf("RESULT k=%-2d rows=%-8d variant=%-9s seed=%-8s insert70k=%-8s delete70k_chunked=%-8s per10k=%-7s delete70k_whole=%-8s db=%dMiB",
				k, n, variant, seed.Round(time.Second),
				(ins / time.Duration(reps)).Round(time.Millisecond),
				(del / time.Duration(reps)).Round(time.Millisecond),
				(del / time.Duration(reps*7)).Round(time.Millisecond),
				delWhole.Round(time.Millisecond),
				pages*pageSize>>20)
			_ = db.Close()
			_ = os.RemoveAll(dir)
		}
	}
}
