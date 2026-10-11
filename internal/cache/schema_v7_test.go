package cache

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

const v7Now = int64(1_800_000_000)

const (
	hA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func discardSlog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// buildV6 creates a database at schema v6 with a production-shaped
// fixture and returns it open.
func buildV6(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for v := 0; v < 6; v++ {
		if err := applyMigration(ctx, db, v); err != nil {
			t.Fatalf("migration %d: %v", v, err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, h := range []string{hA, hB, hC, hD} {
		exec(`INSERT INTO blob(hash,size,created_at,refcount) VALUES(?,1,1,1)`, h)
	}
	// S1: inline, adopted, current; plus an orphan inline candidate.
	exec(`INSERT INTO suite_snapshot(snapshot_id,canonical_scheme,canonical_host,suite_path,inrelease_hash,created_at,adopted_at)
	      VALUES(1,'http','h','/u/dists/s1',?,1,100)`, hA)
	exec(`INSERT INTO suite_snapshot(snapshot_id,canonical_scheme,canonical_host,suite_path,inrelease_hash,created_at)
	      VALUES(3,'http','h','/u/dists/s1',?,1)`, hD)
	// S2: detached, adopted, current.
	exec(`INSERT INTO suite_snapshot(snapshot_id,canonical_scheme,canonical_host,suite_path,release_hash,release_gpg_hash,created_at,adopted_at)
	      VALUES(2,'http','h','/u/dists/s2',?,?,1,200)`, hB, hC)
	exec(`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path,current_snapshot_id) VALUES('http','h','/u/dists/s1',1)`)
	exec(`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path,current_snapshot_id) VALUES('http','h','/u/dists/s2',2)`)
	// S3: never adopted, has a client-requested anchor row.
	exec(`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path) VALUES('http','h','/u/dists/s3')`)
	// S4: never adopted, dormant (no anchor row).
	exec(`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path) VALUES('http','h','/u/dists/s4')`)
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('http','h','/u/dists/s1/InRelease',?, 'http://h/u/dists/s1/InRelease',1,1799999000,5)`, hA)
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('http','h','/u/dists/s3/InRelease',?, 'http://h/u/dists/s3/InRelease',1,1799990000,2)`, hD)
	return db
}

func TestSchemaV7_MigratesPopulatedV6(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	if err := migrate(context.Background(), db, discardSlog()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 7 {
		t.Fatalf("version = %d (%v), want 7", v, err)
	}
	forms := map[int64]string{}
	rows, err := db.Query(`SELECT snapshot_id, anchor_form, release_date IS NULL, retired_at IS NULL FROM suite_snapshot`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var f string
		var rdNull, retNull bool
		if err := rows.Scan(&id, &f, &rdNull, &retNull); err != nil {
			t.Fatal(err)
		}
		forms[id] = f
		if !rdNull || !retNull {
			t.Errorf("snapshot %d: release_date/retired_at must be NULL after migration", id)
		}
	}
	_ = rows.Close()
	if forms[1] != "inline" || forms[2] != "detached" || forms[3] != "inline" {
		t.Errorf("anchor_form backfill = %v", forms)
	}
	lr := func(suite string) int64 {
		var x sql.NullInt64
		if err := db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE suite_path=?`, suite).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x.Int64
	}
	if got := lr("/u/dists/s1"); got != 1799999000 {
		t.Errorf("s1 last_requested_at = %d, want the anchor's stamp", got)
	}
	if got := lr("/u/dists/s3"); got != 1799990000 {
		t.Errorf("s3 last_requested_at = %d, want the anchor's stamp", got)
	}
	for _, s := range []string{"/u/dists/s2", "/u/dists/s4"} {
		if got := lr(s); got != v7Now-604800 {
			t.Errorf("%s last_requested_at = %d, want now-7d", s, got)
		}
	}
	// The migration writes no signed-seen markers ("adopted" is not
	// "signed"; the evidence is in pool files): every ADOPTED pre-v7
	// snapshot is queued for the startup legacy trust audit instead (spec
	// §6.1). Snapshot 3 is a never-adopted candidate: inert, not queued.
	var markers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen`).Scan(&markers); err != nil || markers != 0 {
		t.Errorf("signed-seen markers = %d (%v), want 0 after migration", markers, err)
	}
	if got := auditQueue(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("legacy_trust_audit = %v, want [1 2] (the adopted pre-v7 snapshots)", got)
	}
	var failCount, checkFails int
	if err := db.QueryRow(`SELECT adoption_fail_count, check_fail_count FROM suite_freshness WHERE suite_path='/u/dists/s1'`).Scan(&failCount, &checkFails); err != nil || failCount != 0 || checkFails != 0 {
		t.Errorf("adoption_fail_count = %d, check_fail_count = %d (%v), want 0, 0", failCount, checkFails, err)
	}
}

// Review Focus 1: the interest backfill must correlate on scheme AND host,
// consider both anchor names and take the newest stamp; only adopted
// pre-v7 snapshots are queued for the trust audit. Each assertion kills one
// plausible wrong query.
func TestSchemaV7_BackfillCorrelatesAndFilters(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// s1: an OLDER Release anchor beside the newer InRelease anchor -> MAX wins.
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('http','h','/u/dists/s1/Release',?, 'http://h/u/dists/s1/Release',1,1799000000,1)`, hB)
	// s2 (detached): only a Release anchor carries its interest.
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('http','h','/u/dists/s2/Release',?, 'http://h/u/dists/s2/Release',1,1799998000,3)`, hB)
	// The same suite path on another host and on another scheme, recently requested.
	exec(`INSERT INTO suite_freshness(canonical_scheme,canonical_host,suite_path) VALUES('http','h2','/u/dists/s4')`)
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('http','h2','/u/dists/s4/InRelease',?, 'http://h2/u/dists/s4/InRelease',1,1799999500,9)`, hA)
	exec(`INSERT INTO url_path(canonical_scheme,canonical_host,path,blob_hash,upstream_url,is_metadata,last_requested_at,request_count)
	      VALUES('https','h','/u/dists/s4/InRelease',?, 'https://h/u/dists/s4/InRelease',1,1799999600,9)`, hA)
	// s3: a never-adopted candidate snapshot -> not queued for the audit.
	exec(`INSERT INTO suite_snapshot(snapshot_id,canonical_scheme,canonical_host,suite_path,inrelease_hash,created_at)
	      VALUES(4,'http','h','/u/dists/s3',?,1)`, hB)
	if err := migrate(context.Background(), db, discardSlog()); err != nil {
		t.Fatal(err)
	}
	lr := func(host, suite string) int64 {
		var x sql.NullInt64
		if err := db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE canonical_host=? AND suite_path=?`, host, suite).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x.Int64
	}
	if got := lr("h", "/u/dists/s1"); got != 1799999000 {
		t.Errorf("s1 = %d, want MAX of its anchors", got)
	}
	if got := lr("h", "/u/dists/s2"); got != 1799998000 {
		t.Errorf("s2 = %d, want its Release anchor stamp", got)
	}
	if got := lr("h", "/u/dists/s4"); got != v7Now-604800 {
		t.Errorf("s4@h = %d, want now-7d (another host/scheme leaked)", got)
	}
	if got := lr("h2", "/u/dists/s4"); got != 1799999500 {
		t.Errorf("s4@h2 = %d, want 1799999500", got)
	}
	if got := auditQueue(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("legacy_trust_audit = %v, want [1 2]: candidates are not queued", got)
	}
}

// auditQueue lists the legacy_trust_audit queue in order.
func auditQueue(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT snapshot_id FROM legacy_trust_audit ORDER BY snapshot_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// The Go step runs inside the migration transaction: if it fails, the
// whole v6 -> v7 step rolls back (otherwise last_requested_at would stay
// NULL forever and phase F would never reap those suites).
func TestSchemaV7_GoStepFailureRollsBackMigration(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	orig := migrationGoSteps[6]
	migrationGoSteps[6] = func(context.Context, *sql.Tx, int64) error { return errors.New("injected") }
	defer func() { migrationGoSteps[6] = orig }()
	if err := migrate(context.Background(), db, discardSlog()); err == nil {
		t.Fatal("migrate succeeded despite a failing Go step")
	}
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil || v != 6 {
		t.Fatalf("version = %d (%v), want 6 after rollback", v, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('suite_snapshot') WHERE name = 'anchor_form'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("anchor_form column present after rollback (n=%d, %v)", n, err)
	}
}

func TestSchemaV7_TriggerRejectsEmptyFormAndUniqueIndexIsPartial(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	if err := migrate(context.Background(), db, discardSlog()); err != nil {
		t.Fatal(err)
	}
	// Spec §6: an insert without anchor_form is rejected, also under
	// OR IGNORE (RAISE(ABORT) is not a conflict the IGNORE policy swallows).
	for _, verb := range []string{"INSERT", "INSERT OR IGNORE"} {
		_, err := db.Exec(verb+` INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,release_hash,release_gpg_hash,created_at)
		                      VALUES('http','h','/u/dists/s9',?,?,1)`, hB, hC)
		if err == nil || !strings.Contains(err.Error(), "anchor_form required") {
			t.Fatalf("%s without anchor_form: err = %v, want the trigger's abort", verb, err)
		}
	}
	var empty int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suite_snapshot WHERE anchor_form = ''`).Scan(&empty); err != nil || empty != 0 {
		t.Fatalf("rows with empty anchor_form = %d (%v), want 0", empty, err)
	}
	// Duplicate inline content of S1 is rejected by the unique index ...
	if _, err := db.Exec(`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,anchor_form,created_at)
	                      VALUES('http','h','/u/dists/s1',?,'inline',1)`, hA); err == nil {
		t.Fatal("duplicate natural key accepted")
	}
	// ... but accepted once the old incarnation is retired.
	if _, err := db.Exec(`UPDATE suite_snapshot SET retired_at = 1 WHERE snapshot_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,anchor_form,created_at)
	                      VALUES('http','h','/u/dists/s1',?,'inline',1)`, hA); err != nil {
		t.Fatalf("insert after retirement: %v", err)
	}
	// Unsigned rows share the single-anchor slot but are a distinct form.
	if _, err := db.Exec(`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,anchor_form,created_at)
	                      VALUES('http','h','/u/dists/s1',?,'unsigned',1)`, hA); err != nil {
		t.Fatalf("unsigned insert: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,anchor_form,created_at)
	                      VALUES('http','h','/u/dists/s1',?,'bogus',1)`, hB); err == nil {
		t.Fatal("bogus anchor_form accepted")
	}
	// The flat_filename partial index exists.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_package_hash_flat_filename'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("flat_filename index missing (%v)", err)
	}
}
