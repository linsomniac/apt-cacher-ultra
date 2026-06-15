# Snapshot Reconcile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Heal an already-published degraded snapshot in place by fetching the requestable IndexTargets its own signed Release declared but the snapshot lacks — auto on unchanged freshness ticks, and on demand via `POST /reconcile`.

**Architecture:** Re-parse the snapshot's GPG-verified `InRelease`/`Release` member blob (trust anchor, unchanged from adoption), diff declared-vs-present, fetch each missing member with `adoptMember` (validates against the re-parsed declared hash), and insert `snapshot_member` + by-hash alias + `package_hash` into the SAME snapshot via a transaction that re-checks it is still current. No new snapshot → no `idx_suite_snapshot_natural` collision, no serving flip. Reuses `ParseRelease`, `missingRequestableIndexGroups`, `adoptMember`, `byHashAliasPath`, `buildPackageHashes`.

**Tech Stack:** Go, modernc SQLite (`internal/cache`), existing freshness adoption machinery (`internal/freshness`), admin HTTP listener (`internal/admin`).

Design: `docs/superpowers/specs/2026-06-15-snapshot-reconcile-design.md`.

---

## File structure

- `internal/freshness/reconcile.go` (new) — `missingRequestableMembers` helper, `Adopter.ReconcileSnapshot`, the `reconciledSnapshots` gate.
- `internal/freshness/reconcile_test.go` (new) — unit + integration tests.
- `internal/cache/reconcile.go` (new) — `Cache.InsertReconciledMembers`.
- `internal/cache/reconcile_test.go` (new) — cache tx tests.
- `internal/freshness/adoption.go` (modify) — add `reconciledSnapshots sync.Map` to the `Adopter` struct.
- `internal/freshness/metrics.go` (modify) — `adoptionReconciledTotal` counter.
- `internal/freshness/freshness.go` (modify) — auto-heal hook in `check`; `Checker.Reconcile`.
- `internal/admin/server.go` (modify) — `Reconciler` interface, `Config.Reconciler`, `/reconcile` POST branch.
- `internal/admin/handlers.go` (modify) — `handleReconcile`.
- `internal/admin/admin_test.go` (modify) — endpoint tests.
- `cmd/apt-cacher-ultra/main.go` (modify) — wire `Reconciler: freshChecker`.
- `SPEC6_8.md` (modify) — §6 recovery now implemented.

---

## Task 1: `missingRequestableMembers` — select the declared members to heal

**Files:**
- Modify: `internal/freshness/reconcile.go` (create)
- Test: `internal/freshness/reconcile_test.go` (create)

`missingRequestableIndexGroups` (already in adoption.go) returns the missing GROUP keys (groups with zero present variants). Reconcile needs the actual declared MEMBERS in those groups to fetch.

- [ ] **Step 1: Write the failing test**

```go
package freshness

import (
	"reflect"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

func TestMissingRequestableMembers(t *testing.T) {
	rm := func(path, sha string) ReleaseMember { return ReleaseMember{Path: path, SHA256: sha} }
	declared := []ReleaseMember{
		rm("main/binary-amd64/Packages", "a"),
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
		rm("main/binary-arm64/Packages", "d"), // foreign, allowlisted-out
	}
	present := []cache.SnapshotMember{{Path: "main/binary-amd64/Packages"}}
	allow := map[string]struct{}{"amd64": {}}

	got := missingRequestableMembers(declared, present, allow)
	want := []ReleaseMember{
		rm("main/binary-all/Packages", "b"),
		rm("main/binary-all/Packages.gz", "c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missingRequestableMembers() = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestMissingRequestableMembers$'`
Expected: FAIL — build error, `undefined: missingRequestableMembers`.

- [ ] **Step 3: Write minimal implementation**

```go
package freshness

import "github.com/linsomniac/apt-cacher-ultra/internal/cache"

// missingRequestableMembers returns the declared ReleaseMembers belonging
// to a requestable IndexTarget GROUP that the snapshot does not currently
// serve. It reuses missingRequestableIndexGroups (the Layer-2 serve-contract
// predicate) for the group decision — "all" always required, allowlisted
// arches when filtered — then returns every declared variant of each missing
// group so apt can fetch whichever it asks for.
func missingRequestableMembers(declared []ReleaseMember, present []cache.SnapshotMember, allowlist map[string]struct{}) []ReleaseMember {
	presentMembers := make([]ReleaseMember, len(present))
	for i, p := range present {
		presentMembers[i] = ReleaseMember{Path: p.Path}
	}
	missingGroups := missingRequestableIndexGroups(declared, presentMembers, allowlist)
	if len(missingGroups) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(missingGroups))
	for _, g := range missingGroups {
		want[g] = struct{}{}
	}
	var out []ReleaseMember
	for _, m := range declared {
		if group, _, ok := indexTargetGroup(m.Path); ok {
			if _, missing := want[group]; missing {
				out = append(out, m)
			}
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/ -run '^TestMissingRequestableMembers$'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/reconcile.go internal/freshness/reconcile_test.go
git commit -m "feat(reconcile): select declared members of missing requestable index groups"
```

---

## Task 2: `cache.InsertReconciledMembers` — transactional in-place insert

**Files:**
- Create: `internal/cache/reconcile.go`
- Test: `internal/cache/reconcile_test.go`

Mirrors `RepairSkippedMember` (internal/cache/skipped_member.go:96) — same `submitWrite` + `BeginTx` + current-snapshot guard + per-member `(snapshot_id,path)` insert — but does NOT consume a `snapshot_skipped_member` row (these members were never recorded as skipped; the trust anchor is the re-parsed Release, validated by the caller). Inserts are idempotent: a byte-identical re-insert (`ON CONFLICT(snapshot_id,path) DO NOTHING` after a hash-equality check) is a no-op for safe retry; a conflicting hash on the same path is a loud error. Package hashes are upserted the same way `CommitAdoption` does (reuse the existing package_hash insert SQL from internal/cache/adoption.go's CommitAdoption — copy the INSERT, keyed on its PK).

- [ ] **Step 1: Write the failing test**

```go
package cache

import (
	"context"
	"strings"
	"testing"
)

func TestInsertReconciledMembers_InPlace(t *testing.T) {
	ctx := context.Background()
	c := openTestCache(t) // existing helper in this package's tests

	// Adopt a minimal current snapshot via the test helpers used elsewhere
	// in this package (see existing adoption_test.go: commitTestSnapshot or
	// equivalent). The snapshot has InRelease only.
	snapID := commitTestSnapshotInRelease(t, c) // returns the current snapshot id

	blob := strings.Repeat("e", 64)
	writeTestBlob(t, c, blob, []byte("binary-all Packages bytes"))
	members := []SnapshotMember{
		{SnapshotID: snapID, Path: "main/binary-all/Packages", BlobHash: blob, DeclaredSHA256: blob},
	}
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Fatalf("InsertReconciledMembers: %v", err)
	}
	if _, err := c.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("member not inserted: %v", err)
	}
	// Idempotent re-insert is a no-op.
	if err := c.InsertReconciledMembers(ctx, snapID, members, nil); err != nil {
		t.Errorf("re-insert should be idempotent, got %v", err)
	}
}

func TestInsertReconciledMembers_NotCurrent(t *testing.T) {
	ctx := context.Background()
	c := openTestCache(t)
	snapID := commitTestSnapshotInRelease(t, c)
	displaceCurrentSnapshot(t, c, snapID) // point suite_freshness.current_snapshot_id elsewhere

	blob := strings.Repeat("e", 64)
	writeTestBlob(t, c, blob, []byte("x"))
	members := []SnapshotMember{{SnapshotID: snapID, Path: "main/binary-all/Packages", BlobHash: blob, DeclaredSHA256: blob}}
	err := c.InsertReconciledMembers(ctx, snapID, members, nil)
	if !errorsIs(err, ErrSnapshotNotCurrent) {
		t.Fatalf("want ErrSnapshotNotCurrent, got %v", err)
	}
}
```

> NOTE for the implementer: the three `commitTestSnapshotInRelease` /
> `writeTestBlob` / `displaceCurrentSnapshot` helpers may not exist verbatim;
> reuse whatever the existing `internal/cache/*_test.go` files use to (a) open a
> cache, (b) write a blob, (c) create a current snapshot, (d) repoint the
> current_snapshot_id. Grep `internal/cache/adoption_test.go` and
> `skipped_member_test.go` first.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cache/ -run '^TestInsertReconciledMembers'`
Expected: FAIL — `undefined: (*Cache).InsertReconciledMembers`.

- [ ] **Step 3: Write minimal implementation**

```go
package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InsertReconciledMembers inserts snapshot_member (+ by-hash alias) and
// package_hash rows into an EXISTING current snapshot — the SPEC6_8 in-place
// reconcile path. Unlike RepairSkippedMember it consumes no
// snapshot_skipped_member row; the caller (Adopter.ReconcileSnapshot) has
// already validated each member's bytes against the re-parsed, GPG-verified
// Release declaration. The transaction re-checks the snapshot is still the
// suite's current snapshot, so a reconcile that races a re-adoption is a safe
// no-op (ErrSnapshotNotCurrent). Member inserts are idempotent on the PK; a
// byte-identical row is skipped, a conflicting blob_hash on the same path is a
// loud error (mirrors the by-hash-alias invariant in CommitAdoption).
func (c *Cache) InsertReconciledMembers(ctx context.Context, snapshotID int64, members []SnapshotMember, packageHashes []PackageHash) error {
	return c.submitWrite(ctx, func(ctx context.Context, conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("InsertReconciledMembers: begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		var one int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM suite_freshness WHERE current_snapshot_id = ?`, snapshotID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: snapshot_id=%d", ErrSnapshotNotCurrent, snapshotID)
		case err != nil:
			return fmt.Errorf("InsertReconciledMembers: current guard: %w", err)
		}

		for _, m := range members {
			if m.SnapshotID != snapshotID {
				return fmt.Errorf("InsertReconciledMembers: member %q references snapshot %d, reconciling %d",
					m.Path, m.SnapshotID, snapshotID)
			}
			// Idempotent: skip a byte-identical existing row; reject a
			// conflicting blob on the same path.
			var existing string
			switch err := tx.QueryRowContext(ctx,
				`SELECT blob_hash FROM snapshot_member WHERE snapshot_id = ? AND path = ?`,
				snapshotID, m.Path).Scan(&existing); {
			case err == nil:
				if existing != m.BlobHash {
					return fmt.Errorf("InsertReconciledMembers: path %q exists with blob %s, reconcile has %s",
						m.Path, existing, m.BlobHash)
				}
				continue
			case errors.Is(err, sql.ErrNoRows):
				// fall through to insert
			default:
				return fmt.Errorf("InsertReconciledMembers: probe %q: %w", m.Path, err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO snapshot_member (snapshot_id, path, blob_hash, declared_sha256) VALUES (?,?,?,?)`,
				m.SnapshotID, m.Path, m.BlobHash, m.DeclaredSHA256); err != nil {
				return fmt.Errorf("InsertReconciledMembers: insert member %q: %w", m.Path, err)
			}
		}

		// package_hash upserts — copy the exact INSERT CommitAdoption uses
		// (internal/cache/adoption.go). Reconcile passes only the hashes for
		// the healed indexes.
		for _, ph := range packageHashes {
			if err := insertPackageHashTx(ctx, tx, ph); err != nil {
				return fmt.Errorf("InsertReconciledMembers: package_hash %q: %w", ph.Path, err)
			}
		}
		return tx.Commit()
	})
}
```

> The implementer must factor the package_hash INSERT used inside
> `CommitAdoption` into a shared `insertPackageHashTx(ctx, tx, ph)` (or copy the
> exact column list + ON CONFLICT clause) so reconcile and adoption stay in
> lockstep. Grep `INSERT INTO package_hash` in internal/cache/.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cache/ -run '^TestInsertReconciledMembers'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cache/reconcile.go internal/cache/reconcile_test.go
git commit -m "feat(cache): InsertReconciledMembers — in-place member/hash insert with current guard"
```

---

## Task 3: `Adopter.reconciledSnapshots` field + `ReconcileSnapshot` core (members + alias)

**Files:**
- Modify: `internal/freshness/adoption.go` (Adopter struct: add `reconciledSnapshots sync.Map`)
- Modify: `internal/freshness/reconcile.go` (add `ReconcileSnapshot`)
- Test: `internal/freshness/reconcile_test.go`

- [ ] **Step 1: Write the failing test** (integration — the test the reverted force-readopt could not pass)

```go
func TestReconcileSnapshot_HealsBinaryAllInPlace(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t) // existing helper: env.cache, env.fetcher, env.suite
	ad := newReconcileAdopter(t, env, []string{"amd64"}) // see helper note below

	// Adopt a snapshot that OMITS binary-all (simulate the degraded state by
	// 404ing binary-all at adoption, with the guard disabled for setup, OR by
	// adopting then deleting the binary-all member). Simplest: declare
	// binary-amd64 + binary-all, fail404 binary-all, adopt WITHOUT the guard.
	pkgsAmd64 := fakePackagesStanzas(map[string]string{"pool/main/a/a/a_1_amd64.deb": strings.Repeat("a", 64)})
	pkgsAll := fakePackagesStanzas(map[string]string{"pool/main/c/c/c_1_all.deb": strings.Repeat("c", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgsAmd64,
		"main/binary-all/Packages":   pkgsAll,
	})
	base := "http://archive.ubuntu.com/ubuntu/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgsAmd64)
	env.fetcher.fail404(base + "main/binary-all/Packages") // degraded adoption
	if err := ad.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("setup Run: %v", err)
	}
	sf, _ := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	snapID := *sf.CurrentSnapshotID
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err == nil {
		t.Fatal("setup expected binary-all ABSENT")
	}

	// Now upstream serves binary-all (mirror caught up). Reconcile heals it.
	env.fetcher.put(base+"main/binary-all/Packages", pkgsAll)
	healed, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false)
	if err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	if healed == 0 {
		t.Fatal("healed 0 members, want >=1")
	}
	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("binary-all not healed: %v", err)
	}
	sf2, _ := env.cache.GetSuiteFreshness(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	if *sf2.CurrentSnapshotID != snapID {
		t.Errorf("snapshot id changed %d -> %d (reconcile must be IN PLACE)", snapID, *sf2.CurrentSnapshotID)
	}
}
```

> Helper note: `newReconcileAdopter` builds an Adopter with `Architectures:
> []string{"amd64"}`, `TolerateOptionalMemberFailures: true`, and (so the
> degraded setup adoption actually commits) the Layer-2 guard not tripping — at
> setup time binary-all 404s and is a `4xx_index_target` skip; if the guard now
> defers that, the test must build the degraded snapshot another way: adopt
> fully, then `env.cache` delete the binary-all member rows. Pick whichever the
> guard allows; the assertion is what matters.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot_HealsBinaryAllInPlace$'`
Expected: FAIL — `undefined: (*Adopter).ReconcileSnapshot`.

- [ ] **Step 3: Write minimal implementation**

Add to the `Adopter` struct in adoption.go (near `architectureAllowlist`):

```go
	// reconciledSnapshots memoizes snapshot IDs confirmed to serve every
	// requestable IndexTarget, so the per-tick reconcile parses a healthy
	// snapshot's Release at most once per process. force=true bypasses it.
	reconciledSnapshots sync.Map // map[int64]struct{}
```

Add to reconcile.go:

```go
// ReconcileSnapshot heals a degraded current snapshot in place: it re-parses
// the snapshot's GPG-verified Release (the trust anchor, pinned + refcounted),
// fetches every requestable IndexTarget the Release declared but the snapshot
// lacks, validates each against the re-parsed declaration, and inserts it into
// the SAME snapshot. Returns the number of members healed. force=false honors
// the reconciledSnapshots memo (the per-tick caller); force=true always
// re-checks (the on-demand caller).
func (a *Adopter) ReconcileSnapshot(ctx context.Context, suite SuiteRef, snapshotID int64, force bool) (int, error) {
	if !force {
		if _, done := a.reconciledSnapshots.Load(snapshotID); done {
			return 0, nil
		}
	}
	declared, err := a.reparseSnapshotRelease(ctx, snapshotID)
	if err != nil {
		return 0, err // logged by caller; do not memoize
	}
	present, err := a.cache.ListSnapshotMembers(ctx, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("reconcile: list members: %w", err)
	}
	missing := missingRequestableMembers(declared, present, a.architectureAllowlist)
	if len(missing) == 0 {
		a.reconciledSnapshots.Store(snapshotID, struct{}{})
		return 0, nil
	}

	if a.concurrencySem != nil {
		select {
		case a.concurrencySem <- struct{}{}:
			defer func() { <-a.concurrencySem }()
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	var rows []cache.SnapshotMember
	healedDecl := make([]ReleaseMember, 0, len(missing))
	for _, m := range missing {
		blobHash, ferr := a.adoptMember(ctx, suite, m, true)
		if ferr != nil {
			a.logger.Warn("adoption_reconcile_member_failed",
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
				"snapshot_id", snapshotID, "path", m.Path, "err", ferr)
			continue // leave uncompleted; next tick retries
		}
		rows = append(rows, cache.SnapshotMember{
			SnapshotID: snapshotID, Path: m.Path, BlobHash: blobHash, DeclaredSHA256: m.SHA256,
		})
		if alias := byHashAliasPath(m.Path, m.SHA256); alias != "" {
			rows = append(rows, cache.SnapshotMember{
				SnapshotID: snapshotID, Path: alias, BlobHash: blobHash, DeclaredSHA256: m.SHA256,
			})
		}
		healedDecl = append(healedDecl, m)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := a.cache.InsertReconciledMembers(ctx, snapshotID, rows, nil); err != nil {
		if errors.Is(err, cache.ErrSnapshotNotCurrent) {
			a.logger.Info("adoption_reconcile_snapshot_displaced",
				"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath, "snapshot_id", snapshotID)
			return 0, nil
		}
		return 0, fmt.Errorf("reconcile: insert: %w", err)
	}
	a.logger.Info("adoption_snapshot_reconciled",
		"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
		"snapshot_id", snapshotID, "healed", len(healedDecl))
	return len(healedDecl), nil
}

// reparseSnapshotRelease loads the snapshot's signed metadata member
// (InRelease or Release) and parses it into the declared member set.
func (a *Adopter) reparseSnapshotRelease(ctx context.Context, snapshotID int64) ([]ReleaseMember, error) {
	for _, name := range []string{"InRelease", "Release"} {
		mem, err := a.cache.GetSnapshotMember(ctx, snapshotID, name)
		if errors.Is(err, cache.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile: get %s member: %w", name, err)
		}
		bytes, rerr := os.ReadFile(a.cache.BlobPath(mem.BlobHash))
		if rerr != nil {
			return nil, fmt.Errorf("reconcile: read %s blob: %w", name, rerr)
		}
		return ParseRelease(bytes)
	}
	return nil, fmt.Errorf("reconcile: snapshot %d has no InRelease/Release member", snapshotID)
}
```

Add imports `os`, `sync`, `errors`, `fmt`, `cache` to reconcile.go as needed.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot_HealsBinaryAllInPlace$'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/adoption.go internal/freshness/reconcile.go internal/freshness/reconcile_test.go
git commit -m "feat(reconcile): ReconcileSnapshot heals missing requestable IndexTargets in place"
```

---

## Task 4: package_hash completeness in `ReconcileSnapshot`

**Files:**
- Modify: `internal/freshness/reconcile.go`
- Test: `internal/freshness/reconcile_test.go`

A healed `binary-all/Packages` should also yield `package_hash` rows for its arch:all `.deb`s, so they are snapshot-hash-validated (not just Phase-1 trust-upstream) — matching what a clean adoption produces.

- [ ] **Step 1: Write the failing test**

Add one assertion block to the END of `TestReconcileSnapshot_HealsBinaryAllInPlace`
(from Task 3) — same scenario, no duplicated setup. After the in-place-id check:

```go
	// The healed arch:all index also yields package_hash rows, so arch:all
	// .debs are snapshot-hash-validated, not just served trust-upstream.
	allDeb := "/ubuntu/pool/main/c/c/c_1_all.deb"
	if _, err := env.cache.GetPackageHash(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, allDeb, snapID); err != nil {
		t.Errorf("arch:all .deb has no package_hash after reconcile: %v", err)
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot_HealsBinaryAllInPlace$'`
Expected: FAIL — the new assertion fails; package_hash for the arch:all .deb is `ErrNotFound` (reconcile doesn't build hashes yet).

- [ ] **Step 3: Write minimal implementation**

In `ReconcileSnapshot`, after a successful member fetch loop and before `InsertReconciledMembers`, build package hashes for the healed index members and pass them through:

```go
	// Build package_hash rows for the healed indexes (declared set = full
	// re-parsed Release so coverage math matches adoption; fetched set =
	// the indexes we just healed).
	phRes, err := a.buildPackageHashes(suite, snapshotID, declared, healedDecl)
	if err != nil {
		return 0, fmt.Errorf("reconcile: build package hashes: %w", err)
	}
	if err := a.cache.InsertReconciledMembers(ctx, snapshotID, rows, phRes.rows); err != nil {
		// ... unchanged error handling ...
	}
```

(Replace the prior `InsertReconciledMembers(ctx, snapshotID, rows, nil)` call.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot'`
Expected: PASS (both reconcile tests)

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/reconcile.go internal/freshness/reconcile_test.go
git commit -m "feat(reconcile): build package_hash rows for healed indexes"
```

---

## Task 5: gate behavior + `force`

**Files:**
- Test: `internal/freshness/reconcile_test.go`

The memo is already implemented in Task 3; lock it with tests.

- [ ] **Step 1: Write the failing test**

```go
func TestReconcileSnapshot_GateAndForce(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})
	// Adopt a COMPLETE snapshot (binary-amd64 + binary-all both served).
	snapID := adoptCompleteSnapshot(t, env, ad) // helper: returns current snapshot id

	// First reconcile: no-op, marks complete.
	if n, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err != nil || n != 0 {
		t.Fatalf("first reconcile = (%d,%v), want (0,nil)", n, err)
	}
	if _, done := ad.reconciledSnapshots.Load(snapID); !done {
		t.Error("complete snapshot not memoized")
	}
	// force=true re-checks even though memoized (still 0 missing here).
	if n, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, true); err != nil || n != 0 {
		t.Errorf("forced reconcile = (%d,%v), want (0,nil)", n, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot_GateAndForce$'`
Expected: FAIL until `adoptCompleteSnapshot`/`newReconcileAdopter` helpers exist (add them to reconcile_test.go alongside the Task-3 helpers).

- [ ] **Step 3: Write minimal implementation**

No production change — implement the test helpers. Verify the memo logic from Task 3 satisfies the assertions.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/ -run '^TestReconcileSnapshot'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/reconcile_test.go
git commit -m "test(reconcile): gate memoization + force bypass"
```

---

## Task 6: auto-heal hook in `Checker.check`

**Files:**
- Modify: `internal/freshness/freshness.go` (the unchanged-tick handoff, ~line 315)
- Test: `internal/freshness/freshness_test.go`

- [ ] **Step 1: Write the failing test**

Reconcile reads the InRelease blob, so the Checker must run over a REAL
`*cache.Cache` (not `newFakeCache`). Build the degraded snapshot with the
adoption env (same real cache the Checker uses), seed the InRelease url_path so
the tick observes "unchanged", then drive `Check`.

```go
func TestCheck_UnchangedTick_ReconcilesDegraded(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"}) // from Task 3

	// 1. Adopt a degraded snapshot (binary-all absent) into env.cache.
	snapID, inReleaseBytes := adoptDegradedBinaryAll(t, env, ad) // helper below

	// 2. A Checker over the SAME real cache; its fetcher returns the
	//    byte-identical InRelease on the conditional GET (unchanged).
	cfetch := newTestFetcher(t) // serves whatever we seed; reuse freshness_test helper
	c, err := New(Config{
		Cache: env.cache, Fetcher: cfetch, HostLimiter: hostsem.New(8),
		Logger: discardLogger(), now: func() time.Time { return time.Unix(11000, 0) },
		Adopter: ad,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedUnchangedInRelease(t, env, cfetch, inReleaseBytes) // url_path baseline == upstream

	// 3. Once upstream serves binary-all, an unchanged tick heals it in place.
	env.fetcher.put(adoptionBase(env)+"main/binary-all/Packages", binaryAllBytes(t))
	c.Check(ctx, env.suite.CanonicalScheme, env.suite.CanonicalHost, env.suite.SuitePath)
	c.WaitForAdoptions()

	if _, err := env.cache.GetSnapshotMember(ctx, snapID, "main/binary-all/Packages"); err != nil {
		t.Errorf("unchanged tick did not reconcile binary-all: %v", err)
	}
}
```

> Implementer: add `adoptDegradedBinaryAll` (returns the current snapshot id +
> the adopted InRelease bytes — adopt with binary-all 404ing, or adopt then
> delete the binary-all member), `seedUnchangedInRelease` (put the InRelease
> url_path row with `BlobHash == sha256(inReleaseBytes)` and point the fetcher
> at it so the conditional GET 200s byte-identical → "unchanged"), and the small
> `binaryAllBytes`/`adoptionBase` accessors, in reconcile_test.go. These reuse
> `makeRelease`/`fakePackagesStanzas`/`hashOf` already in the package.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestCheck_UnchangedTick_ReconcilesDegraded$'`
Expected: FAIL — binary-all is still absent (the hook does not yet call `ReconcileSnapshot`).

- [ ] **Step 3: Write minimal implementation**

In `freshness.go`, extend the unchanged-tick handoff (the `if req == nil` block, currently running `RepairSkippedMembers`). Run reconcile in the same goroutine, gated by the same flag:

```go
		if c.adopter != nil && c.adopter.repairSkippedMembers && (repair != nil || c.hasCurrentSnapshot(ctx, scheme, host, suitePath)) {
			c.adoptionWg.Add(1)
			go func() {
				defer mu.Unlock()
				defer c.adoptionWg.Done()
				if repair != nil {
					c.adopter.RepairSkippedMembers(c.lifetimeCtx, repair.suite, repair.snapshotID)
				}
				if snapID, suite, ok := c.currentSnapshotRef(ctx, scheme, host, suitePath); ok {
					_, _ = c.adopter.ReconcileSnapshot(c.lifetimeCtx, suite, snapID, false)
				}
			}()
			return
		}
		mu.Unlock()
		return
```

Add small Checker helpers `hasCurrentSnapshot` / `currentSnapshotRef` (one `GetSuiteFreshness` lookup returning `(snapshotID, SuiteRef, ok)`). Keep the existing `repair == nil` fast path: if there's no current snapshot, skip.

> Keep this minimal and preserve the existing repair-only behavior when there
> is no current snapshot. Verify no existing `TestCheck_*` regresses.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/`
Expected: PASS (new test + all existing)

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/freshness.go internal/freshness/freshness_test.go
git commit -m "feat(reconcile): auto-heal current snapshot on unchanged freshness ticks"
```

---

## Task 7: `Checker.Reconcile` on-demand entry

**Files:**
- Modify: `internal/freshness/freshness.go`
- Test: `internal/freshness/freshness_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCheckerReconcile_UnknownSuiteAllocatesNoLock(t *testing.T) {
	c := newCheckerForReconcile(t) // real-cache Checker, no suites
	if c.Reconcile(context.Background(), "https", "nope.example", "/dists/x") {
		t.Error("Reconcile on unknown suite returned true")
	}
	// The per-suite lock map must not have grown.
	count := 0
	c.locks.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Errorf("lock map grew to %d for an unknown suite", count)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/freshness/ -run '^TestCheckerReconcile_'`
Expected: FAIL — `undefined: (*Checker).Reconcile`.

- [ ] **Step 3: Write minimal implementation**

```go
// Reconcile forces an in-place reconcile of one suite's current snapshot
// (SPEC6_8 on-demand recovery). Returns true if a reconcile was triggered;
// false if the suite is unknown / has no current snapshot (no lock allocated)
// or another check holds the suite lock. The reconcile runs the full
// trust-validated heal asynchronously — callers watch adoption_snapshot_reconciled.
func (c *Checker) Reconcile(ctx context.Context, scheme, host, suitePath string) bool {
	if c.adopter == nil {
		return false
	}
	sf, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	if err != nil || sf == nil || sf.CurrentSnapshotID == nil {
		return false // unknown / unadopted — no lock allocated
	}
	snapID := *sf.CurrentSnapshotID
	suite := SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}

	muVal, _ := c.locks.LoadOrStore(suiteKey(scheme, host, suitePath), &sync.Mutex{})
	mu := muVal.(*sync.Mutex)
	if !mu.TryLock() {
		return false
	}
	c.adoptionWg.Add(1)
	go func() {
		defer mu.Unlock()
		defer c.adoptionWg.Done()
		if _, err := c.adopter.ReconcileSnapshot(c.lifetimeCtx, suite, snapID, true); err != nil {
			c.logger.Warn("adoption_reconcile_failed",
				"canonical_host", host, "suite_path", suitePath, "snapshot_id", snapID, "err", err)
		}
	}()
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/freshness/ -run '^TestCheckerReconcile_'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/freshness.go internal/freshness/freshness_test.go
git commit -m "feat(reconcile): Checker.Reconcile on-demand entry, lock allocated only for real suites"
```

---

## Task 8: admin `POST /reconcile`

**Files:**
- Modify: `internal/admin/server.go` (Reconciler interface, Config field, dispatcher branch)
- Modify: `internal/admin/handlers.go` (handleReconcile)
- Test: `internal/admin/admin_test.go`

- [ ] **Step 1: Write the failing test**

```go
type stubReconciler struct {
	result bool
	calls  int
	scheme string
	host   string
	suite  string
}

func (s *stubReconciler) Reconcile(_ context.Context, scheme, host, suitePath string) bool {
	s.calls++
	s.scheme, s.host, s.suite = scheme, host, suitePath
	return s.result
}

func withReconciler(r Reconciler) adminOpt { return func(cfg *Config) { cfg.Reconciler = r } }

func TestEndpoint_Reconcile(t *testing.T) {
	const form = "application/x-www-form-urlencoded"
	t.Run("triggered 202 + params", func(t *testing.T) {
		stub := &stubReconciler{result: true}
		_, base, cleanup := startAdminServer(t, withReconciler(stub))
		defer cleanup()
		resp, err := http.Post(base+"/reconcile", form,
			strings.NewReader("scheme=https&host=packages.microsoft.com&suite=/ubuntu/24.04/prod/dists/noble"))
		if err != nil { t.Fatal(err) }
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusAccepted { t.Errorf("status=%d want 202", resp.StatusCode) }
		if stub.host != "packages.microsoft.com" || stub.suite != "/ubuntu/24.04/prod/dists/noble" || stub.scheme != "https" {
			t.Errorf("params scheme=%q host=%q suite=%q", stub.scheme, stub.host, stub.suite)
		}
	})
	t.Run("not triggered 409", func(t *testing.T) {
		stub := &stubReconciler{result: false}
		_, base, cleanup := startAdminServer(t, withReconciler(stub))
		defer cleanup()
		resp, _ := http.Post(base+"/reconcile", form, strings.NewReader("host=h&suite=/s"))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusConflict { t.Errorf("status=%d want 409", resp.StatusCode) }
	})
	t.Run("missing suite 400, no call", func(t *testing.T) {
		stub := &stubReconciler{result: true}
		_, base, cleanup := startAdminServer(t, withReconciler(stub))
		defer cleanup()
		resp, _ := http.Post(base+"/reconcile", form, strings.NewReader("host=h"))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest { t.Errorf("status=%d want 400", resp.StatusCode) }
		if stub.calls != 0 { t.Errorf("called %d want 0", stub.calls) }
	})
	t.Run("no reconciler 501", func(t *testing.T) {
		_, base, cleanup := startAdminServer(t)
		defer cleanup()
		resp, _ := http.Post(base+"/reconcile", form, strings.NewReader("host=h&suite=/s"))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotImplemented { t.Errorf("status=%d want 501", resp.StatusCode) }
	})
	t.Run("GET 405", func(t *testing.T) {
		stub := &stubReconciler{result: true}
		_, base, cleanup := startAdminServer(t, withReconciler(stub))
		defer cleanup()
		resp, _ := http.Get(base + "/reconcile")
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusMethodNotAllowed { t.Errorf("status=%d want 405", resp.StatusCode) }
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/admin/ -run '^TestEndpoint_Reconcile$'`
Expected: FAIL — `undefined: Reconciler` / `cfg.Reconciler`.

- [ ] **Step 3: Write minimal implementation**

In `server.go`, add the interface + Config field + dispatcher branch:

```go
// Reconciler forces an in-place reconcile of one suite's current snapshot
// (SPEC6_8 recovery). nil → POST /reconcile returns 501. cmd wires the
// freshness Checker; the interface keeps admin from importing freshness.
type Reconciler interface {
	Reconcile(ctx context.Context, scheme, host, suitePath string) bool
}
```

Add `Reconciler Reconciler` to `Config` (document OPTIONAL — New must not reject nil). In `buildHandler`'s dispatch, before the GET route table:

```go
		if r.URL.Path == "/reconcile" {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST, OPTIONS")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleReconcile(w, r)
			return
		}
```

In `handlers.go`:

```go
// handleReconcile serves POST /reconcile — the SPEC6_8 on-demand recovery tool.
// Forces an in-place reconcile of one suite's current snapshot (fetch declared-
// but-absent requestable IndexTargets into the existing snapshot). Strictly
// additive: it can only ADD hash-validated declared members to a current
// snapshot, never degrade serving. Params (form/query): host (required), suite
// (required suite_path), scheme (optional, default https). 202 triggered async
// / 409 not triggered (busy, unknown, or no current snapshot) / 400 bad params
// / 413 oversized / 501 not wired. Auth + method enforced by the dispatcher.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Reconciler == nil {
		http.Error(w, "reconcile not available\n", http.StatusNotImplemented)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request body too large\n", http.StatusRequestEntityTooLarge)
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	suite := strings.TrimSpace(r.FormValue("suite"))
	scheme := strings.TrimSpace(r.FormValue("scheme"))
	if scheme == "" {
		scheme = "https"
	}
	if host == "" || suite == "" {
		http.Error(w, "host and suite are required\n", http.StatusBadRequest)
		return
	}
	s.logger.Info("admin_reconcile_requested", "scheme", scheme, "canonical_host", host, "suite_path", suite)
	if !s.cfg.Reconciler.Reconcile(r.Context(), scheme, host, suite) {
		http.Error(w, "reconcile not triggered (busy, unknown, or no current snapshot)\n", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = io.WriteString(w, "reconcile triggered\n")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `gofmt -w internal/admin/*.go && go test ./internal/admin/ -run '^TestEndpoint_Reconcile$'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/admin/server.go internal/admin/handlers.go internal/admin/admin_test.go
git commit -m "feat(admin): POST /reconcile force in-place snapshot heal"
```

---

## Task 9: metric, wiring, docs

**Files:**
- Modify: `internal/freshness/metrics.go` (counter)
- Modify: `internal/freshness/reconcile.go` (Inc the counter)
- Modify: `cmd/apt-cacher-ultra/main.go` (wire Reconciler)
- Modify: `SPEC6_8.md` (§6)

- [ ] **Step 1: Write the failing test**

Render the default registry (the public API `/metrics` uses) and assert the
counter appears with a value after a heal. Counters are process-global, so
assert presence + nonzero, not an exact value.

```go
// internal/freshness/reconcile_test.go
func TestReconcileMetric_Increments(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	ad := newReconcileAdopter(t, env, []string{"amd64"})
	snapID := adoptDegradedBinaryAll_ID(t, env, ad)         // Task-3 helper (id only)
	env.fetcher.put(adoptionBase(env)+"main/binary-all/Packages", binaryAllBytes(t))
	if _, err := ad.ReconcileSnapshot(ctx, env.suite, snapID, false); err != nil {
		t.Fatalf("ReconcileSnapshot: %v", err)
	}
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	if !strings.Contains(buf.String(), "acu_adoption_reconciled_total") {
		t.Errorf("acu_adoption_reconciled_total not present after a heal:\n%s", buf.String())
	}
}
```

Imports: `bytes`, `strings`, `github.com/linsomniac/apt-cacher-ultra/internal/metrics`.

- [ ] **Step 2: Run test to verify it fails / Step 3: implement**

Add to `metrics.go`:

```go
	// adoptionReconciledTotal counts SPEC6_8 in-place reconcile heals — a
	// requestable IndexTarget fetched into an existing degraded snapshot —
	// by architecture (bounded enum). Pairs inversely with
	// acu_serve_snapshot_index_target_404_total, which falls to zero as a
	// suite heals.
	adoptionReconciledTotal = metrics.NewCounterWithCap(
		"acu_adoption_reconciled_total",
		"Requestable IndexTargets healed into an existing snapshot by in-place reconcile, by architecture (SPEC6_8).",
		metrics.DefaultMaxSeries,
		"architecture",
	)
```

In `ReconcileSnapshot`, after a member is added to `healedDecl`, `Inc` per arch (derive via `indexTargetGroup(m.Path)` → arch). In `main.go`, add `Reconciler: freshChecker,` to the `admin.Config{...}` literal. Update `SPEC6_8.md` §6 to state recovery is implemented (auto on unchanged ticks + `POST /reconcile`), replacing the "DEFERRED" wording and the DB-surgery interim.

- [ ] **Step 4: Run the full verification**

Run:
```bash
gofmt -l internal/ cmd/
go build ./... && go vet ./...
go test ./...
go test -race ./internal/freshness/ ./internal/handler/ ./internal/admin/ ./internal/cache/
```
Expected: gofmt empty, build/vet clean, all tests pass, race clean.

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/metrics.go internal/freshness/reconcile.go cmd/apt-cacher-ultra/main.go SPEC6_8.md internal/freshness/metrics_test.go
git commit -m "feat(reconcile): acu_adoption_reconciled_total metric; wire endpoint; SPEC6_8 §6 recovery implemented"
```

---

## Post-implementation

- Run `/codex-review` over the stack and address findings (especially the cache transaction in Task 2 and any trust-anchor concern in `reparseSnapshotRelease`).
- Manually verify against a dev/stg degraded Microsoft suite: `POST /reconcile` then confirm `apt update` succeeds and `acu_serve_snapshot_index_target_404_total` stops climbing.
