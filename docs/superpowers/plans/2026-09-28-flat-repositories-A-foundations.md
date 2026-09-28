# Flat repositories — Phase A: Foundations — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the infrastructure every later phase needs — the v7 schema, the wall-clock and path packages, apt-exact upstream URLs, the tightened `dists/` grammar, precise error wrapping, the R-1 and I-1 security fixes, the clock-step rule, a handler fetch seam, and a virtual-clock test harness — without changing behaviour for ordinary traffic except the documented upstream encoding of `+`/`~`.

**Architecture:** Two new leaf packages (`internal/wallclock`, `internal/aptpath`) centralise wall-clock time and path logic. The v7 migration adds all columns later phases use, with a Go backfill step inside the migration transaction. Existing code is routed through the new packages; readers of the snapshot anchor switch to an explicit `Form`.

**Tech Stack:** Go 1.25 (toolchain 1.26.7), modernc SQLite, `github.com/ProtonMail/go-crypto` v1.4.1, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 3) — sections "Shared infrastructure", C1, C2 (`dists/` grammar only), §6, C5 (R-1 and panic recovery), C4 (clock-step rule). Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (Global Constraints and deviations apply to every task).

## Global Constraints

See the overview. In particular: never edit existing `migrations` entries; no SQLite clock functions in production SQL; persisted stamps from `wallclock`, durations from `time.Now`/`time.Since`; `internal/cache` never imports `internal/proxy`; adoption-disabled behaviour unchanged; `go test -race ./...` and `golangci-lint run ./...` clean and no new `gofmt -l .` entries after each task; tests that call `wallclock.SetOffsetForTest` never use `t.Parallel()`; commit trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

Conventions in this plan:

- Each task's "Interfaces" block lists the helpers it uses from other tasks and from existing test files. Never redefine one: a second definition in the same package does not compile.
- Each commit step starts with `gofmt -l <files>`; it must print nothing. Never run `gofmt -w` on the five drift files listed in Task 13.
- A "Run to verify failure" step whose expected result is a compile failure is complete when the compiler names the undefined identifiers listed.

## Review Focus (phase A share)

1. Paths with `+ ~ % space | { }` and UTF-8 must reach upstream in apt's encoding and decode back to the same cache key — Task 3 (exhaustive byte table, real-world shapes, round trip), Task 4 (no ad-hoc composer), Task 11 (end to end through the handler).
2. A populated v6 cache must migrate atomically with correctly correlated backfills, and no snapshot row may ever lack an anchor form — Task 5.
3. A panic anywhere in the freshness/adoption goroutines must not kill the daemon, leak the suite mutex, or bypass the cooldown — Task 9.
4. A backward or forward wall-clock step must not freeze or flood checks — Tasks 10 and 12.

## File map

| File | Change |
|---|---|
| `internal/wallclock/wallclock.go` (+ `_test.go`) | Create: wall-clock seam. |
| `internal/aptpath/suite.go`, `encode.go` (+ tests, `imports_test.go`, `composers_test.go`, `fixture_test.go`) | Create: suite grammar, canonicality, `AptEncodePath`, `BuildUpstreamURL`; package-boundary and composer guards. |
| `internal/proxy/classify.go`, `proxy.go`, `proxy_test.go` | Delegate `SuitePath`; reject control bytes; build upstream URL via `aptpath`. |
| `internal/freshness/adoption.go`, `freshness.go`, `hot_set.go`, `reconcile.go`, `scheduler.go`, `metrics.go` (+ tests) | URL composers, explicit candidate form, form readers, `%w`, guard sentinel, R-1, panic recovery, clock rule, stale gauge. |
| `internal/cache/schema.go`, `types.go`, `adoption.go`, `queries.go`, `cache.go` (+ tests) | v7 migration + Go step, `AnchorForm`, candidate insert and lookup, snapshot fields, interest stamp on new suite rows, seed URL, `nowUnix`. |
| 11 raw `INSERT INTO suite_snapshot` test fixtures (Task 5 Step 8) | Name their anchor form. |
| `internal/gpg/verifier.go` (+ test) | Panic-safe clearsign decoding (I-1). |
| `internal/handler/handler.go` (+ tests) | `Fetcher` interface; wall-clock stamps; T1 panic recovery. |
| `internal/gc/gc.go` | Wall-clock summary stamps. |
| `internal/simtest/simtest.go`, `internal/freshness/sim_smoke_test.go` | Create: virtual-clock helpers; smoke and clock-step simulations. |
| `docs/log-fields.md`, `docs/configuration.md` | New events; encoding note. |

---

### Task 1: `internal/wallclock` and persisted-stamp wiring

**Files:**
- Create: `internal/wallclock/wallclock.go`, `internal/wallclock/wallclock_test.go`, `internal/cache/wallclock_wiring_test.go`, `internal/freshness/wallclock_wiring_test.go`
- Modify: `internal/cache/cache.go:228-229`, `internal/freshness/freshness.go:219-222` (default `now`) and `:362,370` (adoption duration), `internal/freshness/adoption.go:588-591`, `internal/handler/handler.go:679,867,1522,1792`, `internal/gc/gc.go:275,361`

**Interfaces:**
- Produces: `wallclock.Now() time.Time`, `wallclock.Unix() int64`, `wallclock.SetOffsetForTest(d time.Duration) (restore func())`.

- [ ] **Step 1: Write the failing test**

`internal/wallclock/wallclock_test.go`:

```go
package wallclock

import (
	"testing"
	"time"
)

func TestNow_TracksRealTimeWithZeroOffset(t *testing.T) {
	before := time.Now().Add(-time.Second)
	got := Now()
	after := time.Now().Add(time.Second)
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, want between %v and %v", got, before, after)
	}
}

func TestSetOffsetForTest_ShiftsAndRestores(t *testing.T) {
	restore := SetOffsetForTest(-2 * time.Hour)
	shifted := Now()
	if d := time.Since(shifted); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Fatalf("offset not applied: time.Since(Now()) = %v, want ~2h", d)
	}
	n, u := Now().Unix(), Unix() // read once each: a second boundary may fall between them
	if u != n && u != n+1 {
		t.Fatalf("Unix() = %d disagrees with Now().Unix() = %d", u, n)
	}
	restore()
	if d := time.Since(Now()); d > time.Minute || d < -time.Minute {
		t.Fatalf("offset not restored: %v", d)
	}
}

func TestNow_HasNoMonotonicReading(t *testing.T) {
	// A wall-clock value must compare by wall time only, so test offsets
	// behave like real clock steps.
	if s := Now().String(); containsMonotonic(s) {
		t.Fatalf("Now() carries a monotonic reading: %s", s)
	}
}

func containsMonotonic(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i:i+3] == "m=+" || s[i:i+3] == "m=-" {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wallclock/`
Expected: FAIL — package has no non-test Go files / undefined: `Now`.

- [ ] **Step 3: Write the implementation**

`internal/wallclock/wallclock.go`:

```go
// Package wallclock is the single source of wall-clock time for values
// that are persisted, or compared against persisted values (Unix stamps
// in SQLite). Durations, deadlines and in-memory timing keep using
// time.Now / time.Since, which carry a monotonic reading.
//
// AIDEV-NOTE: tests step the wall clock backwards and forwards through
// SetOffsetForTest. testing/synctest alone can only move time forward,
// and clock-step handling (spec C4) must be provable. Never read the
// wall clock for a persisted stamp any other way.
package wallclock

import (
	"sync/atomic"
	"time"
)

var offsetNanos atomic.Int64

// Now returns the current wall-clock time, shifted by the test offset
// (always zero in production). The monotonic reading is stripped so
// comparisons use wall time, exactly like persisted Unix stamps.
func Now() time.Time {
	return time.Now().Round(0).Add(time.Duration(offsetNanos.Load()))
}

// Unix returns Now().Unix().
func Unix() int64 { return Now().Unix() }

// SetOffsetForTest shifts Now by d and returns a function that restores
// the previous offset. Only tests call it.
func SetOffsetForTest(d time.Duration) (restore func()) {
	prev := offsetNanos.Swap(int64(d))
	return func() { offsetNanos.Store(prev) }
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wallclock/`
Expected: PASS

- [ ] **Step 5: Write the failing wiring tests**

The offset is process-global. No test in `cache`, `freshness`, `handler` or `gc` uses `t.Parallel()`; keep it that way in any test that calls `SetOffsetForTest`.

`internal/cache/wallclock_wiring_test.go`:

```go
package cache

import (
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// Persisted cache stamps follow the shared wall clock, so simulations
// can step it.
func TestNowUnix_FollowsWallclock(t *testing.T) {
	defer wallclock.SetOffsetForTest(-48 * time.Hour)()
	if d := time.Now().Unix() - nowUnix(); d < 48*3600-5 || d > 48*3600+5 {
		t.Fatalf("nowUnix is %ds behind real time, want ~172800", d)
	}
}
```

`internal/freshness/wallclock_wiring_test.go`:

```go
package freshness

import (
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

func TestNew_DefaultNowFollowsWallclock(t *testing.T) {
	defer wallclock.SetOffsetForTest(-48 * time.Hour)()
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8)})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(c.now()); d < 48*time.Hour-time.Minute || d > 48*time.Hour+time.Minute {
		t.Fatalf("Checker.now is %v behind real time, want ~48h", d)
	}
}
```

- [ ] **Step 6: Run to verify they fail**

Run: `go test ./internal/cache/ -run FollowsWallclock ; go test ./internal/freshness/ -run FollowsWallclock`
Expected: both FAIL (about 0 s behind real time).

- [ ] **Step 7: Route persisted stamps through `wallclock`**

In `internal/cache/cache.go` replace lines 228-229:

```go
// nowUnix is the test seam for "what time is it". Production reads the
// shared wall clock (internal/wallclock) so simulations can step it.
var nowUnix = func() int64 { return wallclock.Unix() }
```

(add import `"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"`; drop `"time"` only if it becomes unused).

In `internal/freshness/freshness.go` (`New`) replace `now = time.Now` with `now = wallclock.Now`; same in `internal/freshness/adoption.go` (`NewAdopter`). Update the two "production uses time.Now" comments to "production uses wallclock.Now", and add to the `now` field comment in both `Config` structs:

```go
	// AIDEV-NOTE: wall clock, for persisted stamps and comparisons with
	// them only. Never measure a duration or a deadline with it (it has no
	// monotonic reading and tests step it); use time.Now / time.Since.
```

`wallclock.Now` has no monotonic reading, so the adoption goroutine in `Check` (`freshness.go` ~362 and ~370) must stop measuring its duration with `c.now()`: change `start := c.now()` to `start := time.Now()` and `duration := c.now().Sub(start)` to `duration := time.Since(start)`. Keep `CompletedUnixSec: c.now().Unix()` (a persisted-style stamp). Otherwise a clock step would feed negative durations into `acu_adoption_duration_seconds`.

In `internal/handler/handler.go`: replace `time.Now().Unix()` with `wallclock.Unix()` at the `recordByHashURLPath` stamp (line ~679), the `X-Cache-Age` computation (~867), the `runFetch` stamp (~1522) and the stale-serve age (~1792). Leave `start := time.Now()` (durations) and the passthrough-log hour bucket (~2176, in-memory) unchanged.

In `internal/gc/gc.go`: replace `endUnix := time.Now().Unix()` (lines ~275 and ~361) with `endUnix := wallclock.Unix()`. Leave deadlines and `start := time.Now()` unchanged.

- [ ] **Step 8: Run the affected packages**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./internal/gc/ ./internal/wallclock/`
Expected: PASS (the wiring tests now pass; everything else is unchanged because the offset is zero).

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/wallclock internal/cache/cache.go internal/cache/wallclock_wiring_test.go internal/freshness/freshness.go internal/freshness/adoption.go internal/freshness/wallclock_wiring_test.go internal/handler/handler.go internal/gc/gc.go
git add internal/wallclock internal/cache/cache.go internal/cache/wallclock_wiring_test.go internal/freshness/freshness.go internal/freshness/adoption.go internal/freshness/wallclock_wiring_test.go internal/handler/handler.go internal/gc/gc.go
git commit -m "$(cat <<'EOF'
Add wallclock seam for persisted timestamps

Every persisted Unix stamp now comes from internal/wallclock, which is
time.Now plus a test-only offset, so simulations can step the wall clock
backwards and forwards. Durations and deadlines keep monotonic time.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `internal/aptpath` suite grammar (tightened `dists/`)

**Files:**
- Create: `internal/aptpath/suite.go`, `internal/aptpath/suite_test.go`, `internal/aptpath/imports_test.go`
- Modify: `internal/proxy/classify.go:64-95` (remove `suiteRegex`, delegate), `internal/proxy/proxy_test.go:459-482`

**Interfaces:**
- Produces: `aptpath.SuitePath(p string) string`, `aptpath.IsCanonicalSuitePath(s string) bool`, `aptpath.ForbiddenPathBytes` (string constant `"#?%\\"`); test constant `hex64` in `suite_test.go` (Task 3 uses it).
- Phase G extends `SuitePath` with the flat grammar in this same file.

- [ ] **Step 1: Write the failing test**

`internal/aptpath/suite_test.go`:

```go
package aptpath

import "testing"

func TestSuitePath_Dists(t *testing.T) {
	cases := []struct{ path, want string }{
		// unchanged behaviour
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/Packages.gz", "/ubuntu/dists/noble"},
		{"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex64, "/ubuntu/dists/noble"},
		{"/dists/stable/Release", "/dists/stable"},
		{"/debian/dists/bookworm-updates/InRelease", "/debian/dists/bookworm-updates"},
		{"/some/deep/repo/dists/sid/InRelease", "/some/deep/repo/dists/sid"},
		{"/a/dists/b/dists/c/InRelease", "/a/dists/b/dists/c"},
		{"/dépôt/dists/x/InRelease", "/dépôt/dists/x"}, // UTF-8 is not a forbidden byte
		{"/ubuntu/pool/main/h/hello/hello.deb", ""},
		{"/", ""},
		{"", ""},
		{"relative/dists/noble/InRelease", ""},
		// tightened (spec C2): a suite must have a member
		{"/ubuntu/dists/noble", ""},
		{"/dists/stable", ""},
		{"/foo/dists/InRelease", ""},
		// tightened: non-canonical segments
		{"/ubuntu//dists/noble/InRelease", ""},
		{"/ubuntu/./dists/noble/InRelease", ""},
		{"/ubuntu/pool/../dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/main/../InRelease", ""},
		{"/ubuntu/dists/noble//InRelease", ""},
		{"/ubuntu/dists/./InRelease", ""},
		// tightened: forbidden decoded bytes (S-3)
		{"/ubuntu#x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble?x/InRelease", ""},
		{"/ubuntu/dists/noble/In%52elease", ""},
		{"/ubuntu\\x/dists/noble/InRelease", ""},
		{"/ubuntu/dists/noble/In\x00Release", ""},
		{"/ubuntu/dists/noble/In\x7fRelease", ""},
	}
	for _, c := range cases {
		if got := SuitePath(c.path); got != c.want {
			t.Errorf("SuitePath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestIsCanonicalSuitePath(t *testing.T) {
	cases := []struct {
		suite string
		want  bool
	}{
		{"/ubuntu/dists/noble", true},
		{"/dists/stable", true},
		// "InRelease" is a legal codename under the spec C2 grammar
		// (SuitePath("/foo/dists/InRelease/x") = "/foo/dists/InRelease").
		// apt never requests such a suite, so phase F reaps any legacy row
		// by idleness, not as non-canonical.
		{"/foo/dists/InRelease", true},
		{"/ubuntu/dists/noble/", false},
		{"/ubuntu//dists/noble", false},
		{"/ubuntu/./dists/noble", false},
		{"/ubuntu#x/dists/noble", false},
		{"/foo/dists", false},               // no codename
		{"/ubuntu/dists/noble/main", false}, // deeper than a suite
		{"", false},
	}
	for _, c := range cases {
		if got := IsCanonicalSuitePath(c.suite); got != c.want {
			t.Errorf("IsCanonicalSuitePath(%q) = %v, want %v", c.suite, got, c.want)
		}
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
```

Note: `/foo/dists/InRelease` gets no suite in phase A; phase G's flat grammar still refuses it because a path containing a `dists` segment is never flat.

`internal/aptpath/imports_test.go` pins the package boundary from the overview's Global Constraints (the compiler cannot: `internal/proxy` does not import `internal/cache`, so a `cache → proxy` import would compile):

```go
package aptpath

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nonTestImports returns the import paths of dir's non-test Go files.
func nonTestImports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			out[name] = append(out[name], p)
		}
	}
	return out
}

// aptpath is a leaf: standard library only (a stdlib path has no dot in
// its first element).
func TestAptpathImportsOnlyStdlib(t *testing.T) {
	for file, imps := range nonTestImports(t, ".") {
		for _, p := range imps {
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") {
				t.Errorf("%s imports non-stdlib %q", file, p)
			}
		}
	}
}

// internal/cache must not import internal/proxy; shared path logic lives here.
func TestCacheDoesNotImportProxy(t *testing.T) {
	for file, imps := range nonTestImports(t, filepath.Join("..", "cache")) {
		for _, p := range imps {
			if strings.HasSuffix(p, "/internal/proxy") {
				t.Errorf("internal/cache/%s imports %q", file, p)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/aptpath/`
Expected: FAIL — undefined: `SuitePath`.

- [ ] **Step 3: Write the implementation**

`internal/aptpath/suite.go`:

```go
// Package aptpath holds the path grammar shared by the proxy, cache,
// freshness and handler packages: suite identity, canonicality, and the
// apt-compatible upstream URL encoding. It depends only on the standard
// library so the storage layer can use it without importing internal/proxy.
package aptpath

import "strings"

// ForbiddenPathBytes are decoded bytes that disqualify a path from suite
// identity. A decoded '#' or '?' means the client sent %23/%3F, '%' means
// it sent %25, and '\' is never part of an apt path (spec C2, S-3).
const ForbiddenPathBytes = "#?%\\"

// SuitePath returns the canonical suite path for decoded request path p,
// or "" when p belongs to no suite.
//
// dists/ grammar (spec C2): /<repo>/dists/<codename>/<member...> with a
// non-empty member; the suite is /<repo>/dists/<codename>. The LAST
// "dists" segment that is followed by a codename and at least one member
// segment wins, matching the old greedy regex.
//
// AIDEV-NOTE: canonicality is strict on purpose. Every segment must be
// non-empty and not "." or "..", and ForbiddenPathBytes / control bytes
// are refused. A path that fails gets NO suite (Phase 1 url_path serving
// continues); it never gets a "cleaned" suite, because the handler maps a
// request onto its snapshot by stripping suite+"/" from the raw path.
// Phase G adds the flat grammar here (one trailing "/." allowed there).
func SuitePath(p string) string {
	if !strings.HasPrefix(p, "/") || !cleanBytes(p) {
		return ""
	}
	segs := strings.Split(p[1:], "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return ""
		}
	}
	for i := len(segs) - 3; i >= 0; i-- {
		if segs[i] == "dists" {
			return "/" + strings.Join(segs[:i+2], "/")
		}
	}
	return ""
}

// IsCanonicalSuitePath reports whether s is a suite path SuitePath could
// have produced. Used to recognise legacy suite rows that the tightened
// grammar can never reach again (phase F reaps them). A codename equal to
// an anchor name ("/foo/dists/InRelease") is canonical: the grammar allows
// it, and idle reaping handles such rows.
func IsCanonicalSuitePath(s string) bool {
	return s != "" && SuitePath(s+"/InRelease") == s
}

func cleanBytes(p string) bool {
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(ForbiddenPathBytes, c) >= 0 {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/aptpath/`
Expected: PASS

- [ ] **Step 5: Delegate `proxy.SuitePath`**

In `internal/proxy/classify.go` delete `suiteRegex` and replace the body of `SuitePath` with a delegation, keeping the doc comment updated:

```go
// SuitePath returns the canonical suite path for p (see
// aptpath.SuitePath, which owns the grammar). Kept as a proxy-level name
// because callers throughout the package use it.
func SuitePath(p string) string { return aptpath.SuitePath(p) }
```

Remove the now-unused `regexp` import if nothing else uses it. In `internal/proxy/proxy_test.go` `TestSuitePath`, change the two bare-suite cases to the tightened expectation:

```go
		{"/ubuntu/dists/noble", ""}, // spec C2: a suite needs a member
		{"/dists/stable", ""},       // spec C2: a suite needs a member
```

(then `gofmt -w internal/proxy/proxy_test.go`; comment alignment depends on the neighbouring lines).

- [ ] **Step 6: Run the proxy and handler suites**

Run: `go test -race ./internal/aptpath/ ./internal/proxy/ ./internal/handler/`
Expected: PASS. If a handler test asserted a suite for a bare `.../dists/<codename>` path, change its expectation and note it in the commit message (none are expected: apt never requests a bare suite path).

- [ ] **Step 7: Commit**

```bash
gofmt -l internal/aptpath internal/proxy/classify.go internal/proxy/proxy_test.go
git add internal/aptpath/suite.go internal/aptpath/suite_test.go internal/aptpath/imports_test.go internal/proxy/classify.go internal/proxy/proxy_test.go
git commit -m "$(cat <<'EOF'
Tighten dists suite identity into internal/aptpath

Suite identity moves to the stdlib-only aptpath package. A dists suite
now needs a member segment, canonical segments, and no decoded #, ?, %,
backslash or control bytes, so aliased or fabricated paths no longer mint
suites (security finding S-3). Such paths keep Phase 1 serving.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: apt-exact upstream URLs and control-byte rejection

**Files:**
- Create: `internal/aptpath/encode.go`, `internal/aptpath/encode_test.go`
- Modify: `internal/proxy/proxy.go:162-186`, `internal/proxy/proxy_test.go`

**Interfaces:**
- Consumes: test constant `hex64` (Task 2, `internal/aptpath/suite_test.go`).
- Produces: `aptpath.AptEncodePath(p string) string`, `aptpath.BuildUpstreamURL(scheme, authority, path string) string`.

- [ ] **Step 1: Write the failing test**

`internal/aptpath/encode_test.go`:

```go
package aptpath

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestAptEncodePath_Table(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/ubuntu/dists/noble/InRelease", "/ubuntu/dists/noble/InRelease"},
		{"/core:/stable:/v1.34/deb/InRelease", "/core:/stable:/v1.34/deb/InRelease"},
		{"/repo/./x.deb", "/repo/./x.deb"},
		{"/x/a@b/c", "/x/a@b/c"},
		{"/pool/libstdc++-11-dev_11.4.0-1ubuntu1~22.04.3_amd64.deb",
			"/pool/libstdc%2b%2b-11-dev_11.4.0-1ubuntu1%7e22.04.3_amd64.deb"},
		{"/p/foo_1%3a2.deb", "/p/foo_1%253a2.deb"},
		{"/a b/c", "/a%20b/c"},
		{"/a#b?c", "/a%23b%3fc"},
		{"/x/pkg+x|y.deb", "/x/pkg%2bx%7cy.deb"},
		{"/x/{}^`\"<>\\", "/x/%7b%7d%5e%60%22%3c%3e%5c"},
		{"/é", "/%c3%a9"},
	}
	for _, c := range cases {
		if got := AptEncodePath(c.in); got != c.want {
			t.Errorf("AptEncodePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Every single byte, in every position that matters, must survive
// BuildUpstreamURL -> url.Parse as the same decoded path, and Go must use
// our encoding verbatim (EscapedPath == RawPath).
func TestBuildUpstreamURL_AllBytesRoundTrip(t *testing.T) {
	for b := 1; b < 256; b++ {
		if b == '/' {
			continue
		}
		path := "/a" + string([]byte{byte(b)}) + "z/" + string([]byte{byte(b)})
		raw := BuildUpstreamURL("http", "h.example:8080", path)
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("byte %#x: url.Parse(%q): %v", b, raw, err)
		}
		if u.Path != path {
			t.Fatalf("byte %#x: round trip %q -> %q", b, path, u.Path)
		}
		if u.EscapedPath() != AptEncodePath(path) {
			t.Fatalf("byte %#x: Go re-escaped: %q vs ours %q", b, u.EscapedPath(), AptEncodePath(path))
		}
		if u.Host != "h.example:8080" || u.Scheme != "http" {
			t.Fatalf("byte %#x: authority changed: %s", b, raw)
		}
	}
}

// Real-world public path shapes (spec C1.4; deviation: synthetic table,
// see the overview plan). Plain paths are byte-identical to the old
// scheme://host+path concatenation.
func TestBuildUpstreamURL_PlainPathsUnchanged(t *testing.T) {
	plain := []string{
		"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/" + hex64,
		"/debian/pool/main/c/curl/curl_8.5.0-2_amd64.deb",
		"/core:/stable:/v1.34/deb/amd64/kubectl_1.34.0-1.1_amd64.deb",
		"/compute/cuda/repos/ubuntu2204/x86_64/./cuda-keyring_1.1-1_all.deb",
		"/debian-stable/binary/Release.gpg",
	}
	for _, p := range plain {
		if got, want := BuildUpstreamURL("https", "example.org", p), "https://example.org"+p; got != want {
			t.Errorf("BuildUpstreamURL(%q) = %q, want %q", p, got, want)
		}
	}
}

// Real-world public path shapes that DO change: each expected URL is what
// apt itself sends for that Filename.
func TestBuildUpstreamURL_RealWorldSpecialShapes(t *testing.T) {
	cases := []struct{ path, want string }{
		// '+' and '~' (Ubuntu toolchain backports)
		{"/ubuntu/pool/main/g/gcc-14/libstdc++6_14.2.0-4ubuntu2~24.04_amd64.deb",
			"https://example.org/ubuntu/pool/main/g/gcc-14/libstdc%2b%2b6_14.2.0-4ubuntu2%7e24.04_amd64.deb"},
		// '~' in a Debian stable update
		{"/debian/pool/main/s/systemd/systemd_252.33-1~deb12u1_amd64.deb",
			"https://example.org/debian/pool/main/s/systemd/systemd_252.33-1%7edeb12u1_amd64.deb"},
		// a repository that wrote the epoch colon pre-encoded into Filename:
		// apt quotes the literal '%'
		{"/p/foo_1%3a2.0_amd64.deb", "https://example.org/p/foo_1%253a2.0_amd64.deb"},
		// flat repository ("./" kept literally, as apt sends it) with '+'
		{"/compute/cuda/repos/ubuntu2204/x86_64/./libnvjitlink-dev-12-6_12.6.85-1+cuda12.6_amd64.deb",
			"https://example.org/compute/cuda/repos/ubuntu2204/x86_64/./libnvjitlink-dev-12-6_12.6.85-1%2bcuda12.6_amd64.deb"},
		// ':' is not escaped (pkgs.k8s.io paths)
		{"/core:/stable:/v1.34/deb/./kubectl_1.34.0-1.1_amd64.deb",
			"https://example.org/core:/stable:/v1.34/deb/./kubectl_1.34.0-1.1_amd64.deb"},
	}
	for _, c := range cases {
		if got := BuildUpstreamURL("https", "example.org", c.path); got != c.want {
			t.Errorf("BuildUpstreamURL(%q)\n got %q\nwant %q", c.path, got, c.want)
		}
	}
}

// Opt-in: walk a real cache database's url_path paths.
func TestBuildUpstreamURL_FixtureDB(t *testing.T) {
	dbPath := os.Getenv("ACU_URLPATH_FIXTURE_DB")
	if dbPath == "" {
		t.Skip("set ACU_URLPATH_FIXTURE_DB to a cache.db to run")
	}
	paths := readURLPaths(t, dbPath)
	for _, p := range paths {
		u, err := url.Parse(BuildUpstreamURL("http", "h", p))
		if err != nil || u.Path != p {
			t.Errorf("round trip failed for %q: %v", p, err)
		}
		if strings.ContainsAny(p, "+~") && !strings.Contains(BuildUpstreamURL("http", "h", p), "%") {
			t.Errorf("expected apt encoding for %q", p)
		}
	}
}
```

`readURLPaths` lives in `internal/aptpath/fixture_test.go` (test-only, so importing the SQLite driver there keeps the non-test package stdlib-only):

```go
package aptpath

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func readURLPaths(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT path FROM url_path`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}
```

(Confirm the driver import path with `grep -rn '"modernc.org/sqlite"' internal/cache/cache.go`; use the same one.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/aptpath/ -run 'Encode|Upstream'`
Expected: FAIL — undefined: `AptEncodePath`, `BuildUpstreamURL`.

- [ ] **Step 3: Write the implementation**

`internal/aptpath/encode.go`:

```go
package aptpath

import (
	"net/url"
	"strings"
)

const hexLower = "0123456789abcdef"

// AptEncodePath percent-encodes decoded path p the way apt's
// QuoteString(part, "+~ ") does (plus '%', controls and non-ASCII, in
// lowercase hex), extended with every byte Go's net/url refuses inside a
// RawPath (" < > \ ^ ` { | } # ?). The upstream therefore receives what
// apt would send it directly (spec C1).
//
// AIDEV-NOTE: the extension set is what makes url.URL.EscapedPath() use
// this encoding verbatim; TestBuildUpstreamURL_AllBytesRoundTrip proves
// it for every byte. Do not remove bytes from needsEscape.
func AptEncodePath(p string) string {
	n := 0
	for i := 0; i < len(p); i++ {
		if needsEscape(p[i]) {
			n++
		}
	}
	if n == 0 {
		return p
	}
	var b strings.Builder
	b.Grow(len(p) + 2*n)
	for i := 0; i < len(p); i++ {
		c := p[i]
		if needsEscape(c) {
			b.WriteByte('%')
			b.WriteByte(hexLower[c>>4])
			b.WriteByte(hexLower[c&0x0f])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func needsEscape(c byte) bool {
	if c <= 0x20 || c >= 0x7f {
		return true
	}
	switch c {
	case '+', '~', '%', '"', '<', '>', '\\', '^', '`', '{', '|', '}', '#', '?':
		return true
	}
	return false
}

// BuildUpstreamURL composes scheme://authority + path with path encoded
// by AptEncodePath. It is the only way upstream URLs are built from a
// decoded canonical path (spec C1.3).
func BuildUpstreamURL(scheme, authority, path string) string {
	u := url.URL{Scheme: scheme, Host: authority, Path: path, RawPath: AptEncodePath(path)}
	if u.EscapedPath() != u.RawPath {
		// Unreachable: escaping is byte-local and the all-bytes test
		// covers every byte on every toolchain CI builds with, so a Go
		// change to net/url fails that test before it ships. Fall back to
		// Go's own (still correct, not apt-identical) escaping rather than
		// panic. No counter (plan deviation 2): aptpath is stdlib-only.
		u.RawPath = ""
	}
	return u.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/aptpath/`
Expected: PASS (fixture-DB test SKIPs).

- [ ] **Step 5: Write the failing proxy test**

Append to `internal/proxy/proxy_test.go`:

```go
func TestParse_ControlBytesRejected(t *testing.T) {
	p, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"http://archive.ubuntu.com/ubuntu/dists/noble/In%00Release",
		"http://archive.ubuntu.com/ubuntu/dists/noble/In%0aRelease",
		"http://archive.ubuntu.com/ubuntu/dists/noble/In%7fRelease",
	} {
		if _, err := p.Parse(uri, ""); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Parse(%q) err = %v, want ErrInvalidPath", uri, err)
		}
	}
}

// Spec C1 / S-3: a decoded '#' must not truncate the upstream URL nor
// mint a suite.
func TestParse_EncodedHashKeepsKeyAndURLInSync(t *testing.T) {
	p, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := p.Parse("http://archive.ubuntu.com/ubuntu%23x/dists/noble/InRelease", "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/ubuntu#x/dists/noble/InRelease" {
		t.Fatalf("Path = %q", req.Path)
	}
	if req.SuitePath != "" {
		t.Errorf("SuitePath = %q, want none", req.SuitePath)
	}
	if want := "http://archive.ubuntu.com/ubuntu%23x/dists/noble/InRelease"; req.UpstreamURL != want {
		t.Errorf("UpstreamURL = %q, want %q", req.UpstreamURL, want)
	}
}

func TestParse_UpstreamURLUsesAptEncoding(t *testing.T) {
	p, err := New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err := p.Parse("http://archive.ubuntu.com/ubuntu/pool/main/g/gcc/libstdc%2b%2b6_14.2.0-4ubuntu2~24.04_amd64.deb", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "http://archive.ubuntu.com/ubuntu/pool/main/g/gcc/libstdc%2b%2b6_14.2.0-4ubuntu2%7e24.04_amd64.deb"
	if req.UpstreamURL != want {
		t.Errorf("UpstreamURL = %q, want %q", req.UpstreamURL, want)
	}
}

// Mirror mode (abs_path request line) goes through the same checks.
func TestParse_MirrorModeControlBytesAndEncoding(t *testing.T) {
	p, err := New(nil, []config.MirrorRule{
		{Prefix: "/ubuntu", Upstream: "http://archive.ubuntu.com/ubuntu/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Parse("/ubuntu/dists/noble/In%00Release", "cache.example.com:3142"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("mirror control byte: err = %v, want ErrInvalidPath", err)
	}
	req, err := p.Parse("/ubuntu/pool/main/g/gcc/libstdc%2b%2b6_14.2.0-4ubuntu2~24.04_amd64.deb", "cache.example.com:3142")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://archive.ubuntu.com/ubuntu/pool/main/g/gcc/libstdc%2b%2b6_14.2.0-4ubuntu2%7e24.04_amd64.deb"; req.UpstreamURL != want {
		t.Errorf("mirror UpstreamURL = %q, want %q", req.UpstreamURL, want)
	}
}
```

(Add `"errors"` to the test imports if missing; `config` is already imported by the existing mirror tests.)

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/proxy/ -run 'ControlBytes|EncodedHash|AptEncoding|MirrorModeControl'`
Expected: FAIL — control bytes accepted (both modes); `UpstreamURL` truncated at `#`; `+`/`~` unencoded.

- [ ] **Step 7: Implement in `proxy.Parse`**

In `internal/proxy/proxy.go`, after the existing `if path == "" || path[0] != '/'` check add:

```go
	// Spec C1: a decoded control byte is never a legitimate apt path
	// (apt percent-encodes them) and must not reach the cache key.
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 || path[i] == 0x7f {
			return nil, fmt.Errorf("%w: control byte in path", ErrInvalidPath)
		}
	}
```

and replace `upstream := scheme + "://" + upstreamAuthority + path` with:

```go
	// AIDEV-NOTE: never concatenate a decoded path into a URL string — a
	// decoded '#', '?' or '%' would be re-parsed (S-3). BuildUpstreamURL
	// re-encodes exactly as apt would.
	upstream := aptpath.BuildUpstreamURL(scheme, upstreamAuthority, path)
```

- [ ] **Step 8: Run the full affected suites**

Run: `go test -race ./internal/aptpath/ ./internal/proxy/ ./internal/handler/ ./cmd/...`
Expected: PASS. If an existing test asserts an `UpstreamURL` containing a raw `+` or `~`, update the expectation to apt's `%2b`/`%7e` form (the new behaviour matches apt).

- [ ] **Step 9: Commit**

```bash
gofmt -l internal/aptpath internal/proxy/proxy.go internal/proxy/proxy_test.go
git add internal/aptpath/encode.go internal/aptpath/encode_test.go internal/aptpath/fixture_test.go internal/proxy/proxy.go internal/proxy/proxy_test.go
git commit -m "$(cat <<'EOF'
Build upstream URLs with apt's path encoding

Upstream URLs are now composed with aptpath.BuildUpstreamURL, which
encodes the decoded path exactly as apt does (lowercase hex, +, ~, %,
controls, non-ASCII) plus the bytes Go refuses in a RawPath. This fixes
the cache-key/upstream-URL desync for decoded # and ? (S-3), the double
decoding of literal %, and + read as a space by S3 origins. Paths with
control bytes are rejected with 400.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Convert the remaining upstream-URL composers

**Files:**
- Modify: `internal/freshness/adoption.go:2619-2625` (`buildMemberURL`), `internal/freshness/freshness.go:227-232` (`urlReconstruct` default), `internal/freshness/hot_set.go:62`, `internal/cache/adoption.go:~469` (`CommitAdoption` anchor seed `upstream_url`)
- Test: `internal/freshness/adoption_url_test.go` (create), `internal/cache/adoption_anchor_test.go` (extend), `internal/aptpath/composers_test.go` (create)

**Interfaces:**
- Consumes: `aptpath.BuildUpstreamURL`; existing freshness test helpers `newFakeCache`, `newTestFetcher`; cache test helpers `openCache`, `stubNow`, `seedBlob`.
- Produces: test constant `testHex64` and helper `testHostSem() *hostsem.Sem` in `internal/freshness/adoption_url_test.go` (Tasks 7–10 and 12 use `testHex64`).

- [ ] **Step 1: Write the failing tests**

`internal/freshness/adoption_url_test.go`:

```go
package freshness

import "testing"

func TestBuildMemberURL_UsesAptEncoding(t *testing.T) {
	s := SuiteRef{CanonicalScheme: "https", CanonicalHost: "example.org", SuitePath: "/repo/dists/x"}
	cases := []struct{ rel, want string }{
		{"main/binary-amd64/Packages.gz", "https://example.org/repo/dists/x/main/binary-amd64/Packages.gz"},
		{"main/by-hash/SHA256/" + testHex64, "https://example.org/repo/dists/x/main/by-hash/SHA256/" + testHex64},
		{"weird/a+b~c", "https://example.org/repo/dists/x/weird/a%2bb%7ec"},
	}
	for _, c := range cases {
		if got := buildMemberURL(s, c.rel); got != c.want {
			t.Errorf("buildMemberURL(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
}

func TestDefaultURLReconstruct_UsesAptEncoding(t *testing.T) {
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: testHostSem()})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.urlReconstruct("http", "h", "/a+b/InRelease"), "http://h/a%2bb/InRelease"; got != want {
		t.Errorf("urlReconstruct = %q, want %q", got, want)
	}
}

const testHex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
```

Add to the test file a tiny helper if the package has none: `func testHostSem() *hostsem.Sem { return hostsem.New(8) }` (with the `hostsem` import).

Append to `internal/cache/adoption_anchor_test.go`:

```go
// Spec C1.3: a reconstructed anchor URL uses apt's path encoding.
func TestCommitAdoption_CreatesAnchorWithAptEncodedURL(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_700_000_000)()

	adopted := seedBlob(t, c, "adopted inrelease bytes (encoded path)")
	const host = "archive.test"
	const suite = "/a+b/dists/x"

	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: host, SuitePath: suite, InReleaseHash: &adopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	u, err := c.LookupURL(ctx, "http", host, suite+"/InRelease")
	if err != nil {
		t.Fatal(err)
	}
	if want := "http://archive.test/a%2bb/dists/x/InRelease"; u.UpstreamURL != want {
		t.Errorf("anchor upstream_url = %q, want %q", u.UpstreamURL, want)
	}
}
```

`internal/aptpath/composers_test.go` keeps a sixth ad-hoc composer from appearing later (it also covers the hot-prefetch composer, which has no cheap unit test of its own):

```go
package aptpath

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Spec C1.3: every upstream URL built from a decoded path goes through
// BuildUpstreamURL. Concatenating "://" with a path re-parses decoded
// '#', '?' and '%' (S-3).
func TestNoAdHocUpstreamURLConcatenation(t *testing.T) {
	concat := regexp.MustCompile(`"://"\s*\+`)
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if concat.Match(b) {
				t.Errorf("%s: builds a URL by concatenation; use aptpath.BuildUpstreamURL", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'BuildMemberURL|URLReconstruct' ; go test ./internal/cache/ -run Anchor ; go test ./internal/aptpath/ -run AdHoc`
Expected: FAIL on the `+`/`~` cases, and the ad-hoc test names `internal/cache/adoption.go`, `internal/freshness/adoption.go`, `internal/freshness/freshness.go` and `internal/freshness/hot_set.go` (Task 3 already converted `internal/proxy/proxy.go`).

- [ ] **Step 3: Implement**

`buildMemberURL` becomes:

```go
func buildMemberURL(suite SuiteRef, relPath string) string {
	p := strings.TrimSuffix(suite.SuitePath, "/") + "/" + relPath
	return aptpath.BuildUpstreamURL(suite.CanonicalScheme, suite.CanonicalHost, p)
}
```

(update its `AIDEV-NOTE` to say the URL is built by `aptpath.BuildUpstreamURL`; keep the port-correctness note.)

In `freshness.New` the default reconstruct becomes `reconstruct = func(scheme, host, path string) string { return aptpath.BuildUpstreamURL(scheme, host, path) }`.

In `hot_set.go` replace `suite.CanonicalScheme + "://" + suite.CanonicalHost + r.Path` with `aptpath.BuildUpstreamURL(suite.CanonicalScheme, suite.CanonicalHost, r.Path)`.

In `cache/adoption.go` `seedAnchor`, replace `scheme+"://"+host+anchorPath` with `aptpath.BuildUpstreamURL(scheme, host, anchorPath)`.

Update the three anchor comments that still describe the old composition, keeping their port-correctness content:

- `internal/cache/adoption.go` ~439 (AIDEV-NOTE above the anchor seed): "reconstructs upstream_url as scheme://host+path" → "reconstructs upstream_url with aptpath.BuildUpstreamURL(scheme, host, path)".
- `internal/freshness/freshness.go` ~145 (`urlReconstruct` field comment): "production defaults to scheme://host + path" → "production defaults to aptpath.BuildUpstreamURL(scheme, host, path)".
- `internal/freshness/freshness.go` ~1036 (AIDEV-TODO(port-correctness)): "(test-seam) policy scheme://host + path" → "(test-seam) policy aptpath.BuildUpstreamURL(scheme, host, path)".

- [ ] **Step 4: Run the suites**

Run: `go test -race ./internal/freshness/ ./internal/cache/ ./internal/aptpath/`
Expected: PASS. Existing fake-fetcher URLs have no `+`/`~`; if one does, update its key to the encoded form.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/adoption.go internal/freshness/freshness.go internal/freshness/hot_set.go internal/freshness/adoption_url_test.go internal/cache/adoption.go internal/cache/adoption_anchor_test.go internal/aptpath
git add internal/freshness/adoption.go internal/freshness/freshness.go internal/freshness/hot_set.go internal/freshness/adoption_url_test.go internal/cache/adoption.go internal/cache/adoption_anchor_test.go internal/aptpath/composers_test.go
git commit -m "$(cat <<'EOF'
Use apt path encoding in every upstream URL composer

Member URLs, reconstructed anchors, hot-prefetch URLs and seeded anchor
rows now go through aptpath.BuildUpstreamURL, so all five composers
encode paths the same way apt does.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: v7 migration, anchor-form write path, and suite interest stamp

The v7 trigger rejects snapshot rows without an `anchor_form` (spec §6), so the migration, the candidate write path that sets the form, and the test fixtures that insert raw rows land in one commit. Readers switch to the form in Task 6.

**Files:**
- Modify: `internal/cache/schema.go` (constant, new `migrations[6]`, `migrationGoSteps`, `applyMigration`), `internal/cache/types.go` (`AnchorForm`, `SnapshotCandidate.Form`, `ErrInvalidAnchorForm`), `internal/cache/adoption.go` (`InsertCandidateSnapshot`; `CommitAdoption` Step 6 suite insert), `internal/cache/queries.go` (`PutSuiteFreshness`), `internal/freshness/adoption.go` (candidate construction in `runShared`, ~967-978)
- Modify (raw fixture inserts, Step 8): `internal/cache/admin_queries_test.go:51`, `internal/cache/admin_queries_benchmark_test.go:35`, `internal/cache/chaos_blob_gc_race_test.go:406`, `internal/cache/coverage_group_benchmark_test.go:181,226`, `internal/cache/gc_test.go:853,870`, `internal/cache/gc_urlpath_test.go:106,322,624`, `internal/gc/gc_test.go:643`
- Create: `internal/cache/schema_v7_test.go`, `internal/cache/sqlclock_test.go`, `internal/cache/anchor_form_test.go`, `internal/cache/suite_interest_test.go`

**Interfaces:**
- Consumes: existing cache test helpers `openCache(t) *Cache`, `stubNow(t, int64) (restore func())`, `openDB(path) (*sql.DB, error)`, `applyMigration(ctx, db, from int) error`, `migrate(ctx, db, logger) error`; `(*Cache).PutBlob(ctx, hash string, size int64) error`; the unexported `c.db`.
- Produces:
  - schema v7 exactly as spec §6, including the rejecting trigger `suite_snapshot_anchor_form_required`;
  - `migrationGoSteps map[int]func(ctx context.Context, tx *sql.Tx, now int64) error` (a package variable, so a test can inject a failing step) and `interestWindowSeconds`;
  - `type AnchorForm string` with `FormInline`, `FormDetached`, `FormUnsigned`; `SnapshotCandidate.Form AnchorForm`; `var ErrInvalidAnchorForm`;
  - test constants `hA`, `hB`, `hC`, `hD`, `v7Now` and helpers `buildV6(t) *sql.DB`, `formOf(t, c, id) string`, `putBlobs(t, c, hashes...)` (Task 6 uses `hA`–`hC`, `formOf` and `putBlobs`).

- [ ] **Step 1: Write the failing migration tests**

`internal/cache/schema_v7_test.go`:

```go
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
	var markers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen WHERE last_seen_at = ?`, v7Now).Scan(&markers); err != nil || markers != 2 {
		t.Errorf("signed-seen markers = %d (%v), want 2 (s1, s2)", markers, err)
	}
	var failCount int
	if err := db.QueryRow(`SELECT adoption_fail_count FROM suite_freshness WHERE suite_path='/u/dists/s1'`).Scan(&failCount); err != nil || failCount != 0 {
		t.Errorf("adoption_fail_count = %d (%v), want 0", failCount, err)
	}
}

// Review Focus 1: the interest backfill must correlate on scheme AND host,
// consider both anchor names, take the newest stamp, and mark only
// adopted suites as signed-seen. Each assertion kills one plausible
// wrong query.
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
	// s3: a never-adopted candidate snapshot -> no signed-seen marker.
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
	var markers int
	if err := db.QueryRow(`SELECT COUNT(*) FROM suite_signed_seen`).Scan(&markers); err != nil || markers != 2 {
		t.Errorf("markers = %d (%v), want 2: an unadopted candidate is not signed-seen", markers, err)
	}
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
```

`internal/cache/sqlclock_test.go`:

```go
package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Spec §6 / overview Global Constraints: production SQL never reads
// SQLite's clock, because the Go wall clock (internal/wallclock) is the
// only clock simulations can control.
func TestNoSQLiteClockFunctionsInProductionCode(t *testing.T) {
	forbidden := []string{
		"strftime(", "julianday(", "unixepoch(", "datetime(",
		"CURRENT_TIMESTAMP", "CURRENT_TIME", "CURRENT_DATE", "'now'",
	}
	for _, root := range []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, f := range forbidden {
				if strings.Contains(string(b), f) {
					t.Errorf("%s uses SQLite clock function %q", path, f)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Write the failing write-path tests**

`internal/cache/anchor_form_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"testing"
)

// formOf reads the stored anchor_form of a snapshot row.
func formOf(t *testing.T, c *Cache, id int64) string {
	t.Helper()
	var f string
	if err := c.db.QueryRow(`SELECT anchor_form FROM suite_snapshot WHERE snapshot_id = ?`, id).Scan(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func putBlobs(t *testing.T, c *Cache, hashes ...string) {
	t.Helper()
	for _, h := range hashes {
		if err := c.PutBlob(context.Background(), h, 1); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertCandidateSnapshot_DerivesAndStoresForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA, hB, hC)
	inl := hA
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &inl})
	if err != nil {
		t.Fatal(err)
	}
	if got := formOf(t, c, id); got != "inline" {
		t.Fatalf("derived form = %q, want inline", got)
	}

	rel, sig := hB, hC
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/y", ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	if got := formOf(t, c, id2); got != "detached" {
		t.Fatalf("derived form = %q, want detached", got)
	}

	// Unsigned must be explicit and uses the single-anchor slot; the form
	// is part of the natural key, so it does not collide with the inline row.
	un := hA
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &un, Form: FormUnsigned})
	if err != nil {
		t.Fatalf("unsigned insert: %v", err)
	}
	if id3 == id || formOf(t, c, id3) != "unsigned" {
		t.Fatalf("unsigned candidate: id=%d (inline id=%d) form=%q", id3, id, formOf(t, c, id3))
	}

	// Inconsistent shapes are rejected before touching SQLite.
	bad := []SnapshotCandidate{
		{ReleaseHash: &rel, ReleaseGPGHash: &sig, Form: FormUnsigned},
		{InReleaseHash: &inl, Form: FormDetached},
		{InReleaseHash: &inl, ReleaseHash: &rel, ReleaseGPGHash: &sig},
		{ReleaseHash: &rel},
		{},
		{InReleaseHash: &inl, Form: "bogus"},
	}
	for i, sc := range bad {
		sc.CanonicalScheme, sc.CanonicalHost, sc.SuitePath = "http", "h", "/r/dists/z"
		if _, _, err := c.InsertCandidateSnapshot(ctx, sc); !errors.Is(err, ErrInvalidAnchorForm) {
			t.Errorf("bad[%d]: err = %v, want ErrInvalidAnchorForm", i, err)
		}
	}
}

// The collision lookup must include the form: re-inserting an unsigned
// candidate must reuse the unsigned row, never the inline row with the
// same hash (an inline row vouches .debs; an unsigned one must not).
func TestInsertCandidateSnapshot_LookupIncludesForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA)
	h := hA
	inl := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h}
	idInl, _, err := c.InsertCandidateSnapshot(ctx, inl)
	if err != nil {
		t.Fatal(err)
	}
	un := inl
	un.Form = FormUnsigned
	idUn, _, err := c.InsertCandidateSnapshot(ctx, un)
	if err != nil {
		t.Fatal(err)
	}
	idUn2, reused, err := c.InsertCandidateSnapshot(ctx, un)
	if err != nil || !reused || idUn2 != idUn || idUn2 == idInl {
		t.Fatalf("unsigned re-insert: id=%d reused=%v err=%v, want reuse of %d (inline row %d)", idUn2, reused, err, idUn, idInl)
	}
	if got := formOf(t, c, idUn2); got != "unsigned" {
		t.Fatalf("reused row form = %q, want unsigned", got)
	}
}

// The collision lookup must skip retired incarnations: with a retired
// and a live row for the same content, a re-insert reuses the live one.
func TestInsertCandidateSnapshot_LookupSkipsRetiredIncarnation(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA)
	h := hA
	cand := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h}
	id, _, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1, retired_at = 2 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	id2, reused, err := c.InsertCandidateSnapshot(ctx, cand) // live candidate beside the retired row
	if err != nil || reused || id2 == id {
		t.Fatalf("insert beside retired row: id2=%d reused=%v err=%v (want a fresh candidate, not ErrSnapshotNaturalKeyAdopted)", id2, reused, err)
	}
	id3, reused, err := c.InsertCandidateSnapshot(ctx, cand) // collides: the lookup must skip the retired row
	if err != nil || !reused || id3 != id2 {
		t.Fatalf("id3=%d reused=%v err=%v; want reuse of live candidate %d", id3, reused, err, id2)
	}
}
```

`internal/cache/suite_interest_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"testing"
)

// A suite row created after v7 starts with a client-interest stamp
// (otherwise phase F could never reap it); later writes keep it.
func TestPutSuiteFreshness_StampsInterestOnInsertOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	s := SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	restore := stubNow(t, 1_800_000_000)
	if err := c.PutSuiteFreshness(ctx, s); err != nil {
		t.Fatal(err)
	}
	restore()
	defer stubNow(t, 1_800_009_999)()
	later := int64(1_800_009_999)
	s.LastCheckAt = &later
	if err := c.PutSuiteFreshness(ctx, s); err != nil {
		t.Fatal(err)
	}
	var lr sql.NullInt64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE suite_path = '/r/dists/x'`).Scan(&lr); err != nil {
		t.Fatal(err)
	}
	if !lr.Valid || lr.Int64 != 1_800_000_000 {
		t.Fatalf("last_requested_at = %v, want 1800000000 (set on insert, kept on update)", lr)
	}
}

// CommitAdoption's Step 6 can create the suite row too; it gets the stamp.
func TestCommitAdoption_NewSuiteRowGetsInterestStamp(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	defer stubNow(t, 1_800_000_000)()
	adopted := seedBlob(t, c, "inrelease bytes (interest stamp)")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/new", InReleaseHash: &adopted})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: adopted, DeclaredSHA256: adopted},
	}, nil, nil, nil, false); err != nil {
		t.Fatalf("CommitAdoption: %v", err)
	}
	var lr sql.NullInt64
	if err := c.db.QueryRow(`SELECT last_requested_at FROM suite_freshness WHERE suite_path = '/r/dists/new'`).Scan(&lr); err != nil {
		t.Fatal(err)
	}
	if !lr.Valid || lr.Int64 != 1_800_000_000 {
		t.Fatalf("last_requested_at = %v, want 1800000000", lr)
	}
}
```

(`seedBlob(t, c, content) string` is the existing helper used by `adoption_anchor_test.go`.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/cache/ -run 'SchemaV7|SQLiteClock|InsertCandidateSnapshot_|StampsInterest|InterestStamp'`
Expected: compile failure (`migrationGoSteps`, `FormUnsigned`, `ErrInvalidAnchorForm`, `SnapshotCandidate.Form` undefined). The SQL-clock test alone passes already; it guards the new migration.

- [ ] **Step 4: Implement the migration**

In `internal/cache/schema.go`:

1. `const CurrentSchemaVersion = 7` (add "flat-repositories design §6" to the comment's SPEC list).
2. Append this entry to `migrations` (do not touch earlier entries):

```go
	// v6 → v7: flat repositories and suite lifecycle (design spec
	// 2026-09-27 §6). Additive DDL; backfills that need "now" run in the
	// Go step (migrationGoSteps[6]) so no SQL reads SQLite's clock.
	//
	// AIDEV-NOTE: suite_freshness.last_requested_at is the SUITE's
	// client-interest stamp, distinct from url_path.last_requested_at.
	// suite_snapshot.inrelease_hash is the single-anchor blob for both the
	// inline and the unsigned form; anchor_form says which file it is.
	// The trigger REJECTS a row without anchor_form (fail closed: a
	// guessed form could turn an unsigned snapshot into a vouching one),
	// and RAISE(ABORT) also defeats INSERT OR IGNORE. Every insert —
	// production and test fixtures — names its form.
	`
ALTER TABLE suite_freshness ADD COLUMN last_requested_at      INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_hash     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_reason   TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_first_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_last_at  INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_next_retry_at INTEGER;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_etag     TEXT;
ALTER TABLE suite_freshness ADD COLUMN adoption_fail_lastmod  TEXT;

ALTER TABLE suite_snapshot ADD COLUMN anchor_form TEXT NOT NULL DEFAULT ''
  CHECK (anchor_form IN ('', 'inline', 'detached', 'unsigned'));
ALTER TABLE suite_snapshot ADD COLUMN release_date INTEGER;
ALTER TABLE suite_snapshot ADD COLUMN retired_at   INTEGER;
UPDATE suite_snapshot
   SET anchor_form = CASE WHEN inrelease_hash IS NOT NULL THEN 'inline' ELSE 'detached' END;
CREATE TRIGGER suite_snapshot_anchor_form_required
BEFORE INSERT ON suite_snapshot
WHEN NEW.anchor_form = ''
BEGIN
  SELECT RAISE(ABORT, 'suite_snapshot.anchor_form required');
END;

DROP INDEX idx_suite_snapshot_natural;
CREATE UNIQUE INDEX idx_suite_snapshot_natural_v7
  ON suite_snapshot(canonical_scheme, canonical_host, suite_path,
                    COALESCE(inrelease_hash, release_hash), anchor_form)
  WHERE retired_at IS NULL;

ALTER TABLE package_hash ADD COLUMN flat_filename TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_package_hash_flat_filename
  ON package_hash(canonical_scheme, canonical_host, flat_filename)
  WHERE flat_filename <> '';

CREATE TABLE suite_signed_seen (
  canonical_scheme TEXT NOT NULL,
  canonical_host   TEXT NOT NULL,
  base             TEXT NOT NULL,
  last_seen_at     INTEGER NOT NULL,
  PRIMARY KEY (canonical_scheme, canonical_host, base)
);
`,
```

3. Below `migrations` add:

```go
// migrationGoSteps holds optional Go code run inside migrations[from]'s
// transaction, after its SQL and before the version bump, with
// now = nowUnix(). Used for backfills that need the current time
// (production SQL must not read SQLite's clock — see
// TestNoSQLiteClockFunctionsInProductionCode). A failing step rolls the
// whole migration back.
var migrationGoSteps = map[int]func(ctx context.Context, tx *sql.Tx, now int64) error{
	6: migrateV7Backfill,
}

// interestWindowSeconds is the spec C4 eager-adoption interest window
// (7 days); the v7 backfill uses the same constant for suites with no
// client anchor stamp.
const interestWindowSeconds = 7 * 24 * 60 * 60

func migrateV7Backfill(ctx context.Context, tx *sql.Tx, now int64) error {
	if _, err := tx.ExecContext(ctx, `
UPDATE suite_freshness SET last_requested_at = COALESCE(
  (SELECT MAX(u.last_requested_at) FROM url_path u
    WHERE u.canonical_scheme = suite_freshness.canonical_scheme
      AND u.canonical_host   = suite_freshness.canonical_host
      AND u.path IN (suite_freshness.suite_path || '/InRelease',
                     suite_freshness.suite_path || '/Release')),
  ?)`, now-interestWindowSeconds); err != nil {
		return fmt.Errorf("backfill last_requested_at: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO suite_signed_seen (canonical_scheme, canonical_host, base, last_seen_at)
SELECT DISTINCT canonical_scheme, canonical_host, suite_path, ?
  FROM suite_snapshot WHERE adopted_at IS NOT NULL`, now); err != nil {
		return fmt.Errorf("backfill suite_signed_seen: %w", err)
	}
	return nil
}
```

4. In `applyMigration`, directly after `tx.ExecContext(ctx, migrations[from])` succeeds (before the `UPDATE schema_version`), add:

```go
	if step, ok := migrationGoSteps[from]; ok {
		if err := step(ctx, tx, nowUnix()); err != nil {
			return fmt.Errorf("migration %d go step: %w", from, err)
		}
	}
```

- [ ] **Step 5: Implement the form types and the candidate write path**

In `internal/cache/types.go`, above `SnapshotCandidate`, add:

```go
// AnchorForm is how a snapshot's Release metadata was anchored (spec C5).
// It is stored in suite_snapshot.anchor_form and never empty.
type AnchorForm string

const (
	FormInline   AnchorForm = "inline"   // InRelease, verified clearsigned
	FormDetached AnchorForm = "detached" // Release + Release.gpg, verified
	FormUnsigned AnchorForm = "unsigned" // Release only, unverified (phase E)
)
```

and add to `SnapshotCandidate`:

```go
	// Form of the candidate. Empty is accepted only for the unambiguous
	// inline and detached hash shapes and is derived from them;
	// FormUnsigned is never derived and must be set explicitly.
	Form AnchorForm
```

Next to the other cache error variables add `var ErrInvalidAnchorForm = errors.New("cache: invalid anchor form for snapshot candidate")`.

In `internal/cache/adoption.go`, add above `InsertCandidateSnapshot`:

```go
// candidateForm derives (or validates) the anchor form of a candidate.
// Inline and unsigned use the single-anchor slot (InReleaseHash);
// detached uses Release + Release.gpg.
//
// AIDEV-NOTE: deriving an empty Form exists for the unambiguous shapes
// only (test fixtures and legacy callers). FormUnsigned is NEVER derived:
// an unsigned candidate that forgot its Form would be stored as inline
// and vouch .debs. Production candidate construction (runShared) always
// sets Form, and phase E's tests assert that an unsigned adoption stores
// "unsigned".
func candidateForm(sc SnapshotCandidate) (AnchorForm, error) {
	single := sc.InReleaseHash != nil && sc.ReleaseHash == nil && sc.ReleaseGPGHash == nil
	detached := sc.InReleaseHash == nil && sc.ReleaseHash != nil && sc.ReleaseGPGHash != nil
	switch sc.Form {
	case "":
		if single {
			return FormInline, nil
		}
		if detached {
			return FormDetached, nil
		}
	case FormInline, FormUnsigned:
		if single {
			return sc.Form, nil
		}
	case FormDetached:
		if detached {
			return FormDetached, nil
		}
	}
	return "", fmt.Errorf("%w: form=%q", ErrInvalidAnchorForm, sc.Form)
}
```

In `InsertCandidateSnapshot`:

- At the top of the function body add:

```go
	form, err := candidateForm(sc)
	if err != nil {
		return 0, false, err
	}
```

- Replace `insertQ` and `lookupQ` with:

```go
	const insertQ = `
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, inrelease_etag, inrelease_lastmod,
   release_hash, release_gpg_hash, anchor_form, created_at, adopted_at,
   package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`
	const lookupQ = `
SELECT snapshot_id, adopted_at FROM suite_snapshot
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
   AND COALESCE(inrelease_hash, release_hash) = ?
   AND anchor_form = ?
   AND retired_at IS NULL`
```

- In the INSERT `ExecContext` arguments insert `string(form)` between `sc.ReleaseGPGHash` and the first `now`; in the lookup `QueryRowContext` arguments append `string(form)` after `coalesceHash`.
- Update the function's doc comment: the natural key is (scheme, host, suite, anchor hash, form) among non-retired rows (spec §6).

In `CommitAdoption` Step 6 (`internal/cache/adoption.go` ~521), give a newly created suite row its interest stamp (the transaction already has `now`):

```go
INSERT INTO suite_freshness
  (canonical_scheme, canonical_host, suite_path, current_snapshot_id, last_requested_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(canonical_scheme, canonical_host, suite_path) DO UPDATE SET
  current_snapshot_id = excluded.current_snapshot_id
```

with arguments `scheme, host, suite, snapshotID, now`.

In `internal/cache/queries.go` `PutSuiteFreshness`, add `last_requested_at` to the INSERT column list and `VALUES` (bound to `nowUnix()`, captured before `submitWrite`), and leave it out of `DO UPDATE SET`; add above the query:

```go
	// AIDEV-NOTE: last_requested_at (v7) is the suite's client-interest
	// stamp. A new row starts "requested now"; this writer never updates
	// it (phase D's TouchSuite owns updates), so a scheduler write cannot
	// keep an idle suite alive.
```

In `internal/freshness/adoption.go` `runShared` (~972-978), set the form explicitly in both arms:

```go
	switch p.form {
	case adoptionFormInline:
		cand.InReleaseHash = &p.inlineHash
		cand.Form = cache.FormInline
	case adoptionFormDetached:
		cand.ReleaseHash = &p.releaseHash
		cand.ReleaseGPGHash = &p.releaseGPGHash
		cand.Form = cache.FormDetached
	}
```

- [ ] **Step 6: Run the new tests**

Run: `go test -race ./internal/cache/ -run 'SchemaV|SQLiteClock|InsertCandidateSnapshot_|StampsInterest|InterestStamp'`
Expected: PASS (including the existing `TestSchemaV6_*` and `TestMigration_*`).

- [ ] **Step 7: Find the fixtures the trigger now rejects**

Run: `go test ./internal/cache/ ./internal/gc/ 2>&1 | grep -c 'anchor_form required'`
Expected: a non-zero count. These are the raw fixture inserts that run against a migrated (v7) database.

- [ ] **Step 8: Name the form in every raw fixture insert that runs at v7**

Add the column `anchor_form` to the column list and the literal `'inline'` to `VALUES` (same position) in each of these single-row inserts, and `'detached'` in the one detached insert:

| File:line | Form |
|---|---|
| `internal/cache/admin_queries_test.go:51` | `'inline'` |
| `internal/cache/admin_queries_benchmark_test.go:35` | `'inline'` |
| `internal/cache/chaos_blob_gc_race_test.go:406` | `'inline'` |
| `internal/cache/coverage_group_benchmark_test.go:181` and `:226` | `'inline'` |
| `internal/cache/gc_test.go:853` (`adoptCandidate`) and `:870` (`orphanCandidate`) | `'inline'` |
| `internal/cache/gc_urlpath_test.go:106` (`seedCurrentSnapshot`) and `:322` | `'inline'` |
| `internal/cache/gc_urlpath_test.go:624` | `'detached'` |
| `internal/gc/gc_test.go:643` | `'inline'` |

Example (`internal/cache/gc_test.go:853`):

```go
INSERT INTO suite_snapshot
  (canonical_scheme, canonical_host, suite_path,
   inrelease_hash, anchor_form, created_at, adopted_at, package_coverage_complete, heartbeat_at)
VALUES (?, ?, ?, ?, 'inline', ?, ?, 0, ?)`,
```

Do **not** change the raw inserts in `internal/cache/cache_test.go` or `internal/cache/gc_test.go:1456`: they run inside `TestMigration_V1ToV2_*`, `TestMigration_V2ToV3_*` and `TestMigration_V3ToV4_*` against a database deliberately built at an older version, where the column does not exist. `cache_test.go` and `gc_urlpath_test.go` are two of the five gofmt-drift files: edit them by hand and do not run `gofmt -w` on them (the edits are inside raw SQL strings).

- [ ] **Step 9: Run the whole repository**

Run: `go test -race ./... ; go test -run '^$' -bench 'AdminCacheSummaries|CoverageGroup' -benchtime 1x ./internal/cache/`
Expected: PASS, and both benchmarks complete one iteration (they seed with the edited inserts; `go test` alone never runs them).

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/cache/schema.go internal/cache/types.go internal/cache/adoption.go internal/cache/queries.go internal/cache/schema_v7_test.go internal/cache/sqlclock_test.go internal/cache/anchor_form_test.go internal/cache/suite_interest_test.go internal/freshness/adoption.go
git add internal/cache internal/gc/gc_test.go internal/freshness/adoption.go
git commit -m "$(cat <<'EOF'
Add schema v7 and store an explicit snapshot anchor form

Schema v7 adds the suite client-interest stamp and adoption failure
record, snapshot anchor form, release date and retirement, the flat
filename column with a partial index, and the signed-seen marker table.
Time-dependent backfills run in a Go step inside the migration
transaction, so no SQL reads SQLite's clock and a failed step rolls the
migration back.

A trigger rejects snapshot rows without an anchor form. Candidate
inserts store the form, and the natural-key lookup includes it and skips
retired incarnations. New suite rows start with an interest stamp.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

(The `gofmt -l` line must print nothing.)

---

### Task 6: Snapshot fields and form-aware readers

**Files:**
- Modify: `internal/cache/types.go` (`SuiteSnapshot`), `internal/cache/adoption.go` (`GetSuiteSnapshot`; AIDEV-TODO at `CommitAdoption`'s anchor seed ~443-469), `internal/cache/gc_urlpath.go` (AIDEV-TODO at ~269-274), `internal/freshness/freshness.go` (`detectForm`, inline `cachedHash`, `snapshotAnchorHash`), `internal/freshness/adoption.go` (`priorAdoptionForm`), `internal/freshness/freshness_test.go` (`fakeCache.putSnapshot` and its four fixtures), every test that reads `SuiteSnapshot.InReleaseHash`
- Test: `internal/cache/anchor_form_test.go` (extend)

**Interfaces:**
- Consumes (Task 5): `AnchorForm`, `FormInline`, `FormDetached`, `FormUnsigned`; test helpers `putBlobs`, `formOf`; constants `hA`, `hB`, `hC`.
- Produces: `SuiteSnapshot` fields `SingleAnchorHash *string` (renamed from `InReleaseHash`), `Form AnchorForm`, `ReleaseDate *int64`, `RetiredAt *int64`. `SnapshotCandidate.InReleaseHash` keeps its name.

- [ ] **Step 1: Write the failing test**

Append to `internal/cache/anchor_form_test.go`:

```go
func TestGetSuiteSnapshot_ReturnsFormAndLifecycleFields(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putBlobs(t, c, hA, hB, hC)
	h := hA
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h, Form: FormUnsigned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET release_date = 1700000000, retired_at = 1700000500 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	s, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Form != FormUnsigned || s.SingleAnchorHash == nil || *s.SingleAnchorHash != hA {
		t.Fatalf("unsigned snapshot = %+v", s)
	}
	if s.ReleaseDate == nil || *s.ReleaseDate != 1700000000 || s.RetiredAt == nil || *s.RetiredAt != 1700000500 {
		t.Fatalf("release_date/retired_at not scanned: %+v", s)
	}

	rel, sig := hB, hC
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/y", ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := c.GetSuiteSnapshot(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Form != FormDetached || s2.SingleAnchorHash != nil || s2.ReleaseDate != nil || s2.RetiredAt != nil {
		t.Fatalf("detached snapshot = %+v", s2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run GetSuiteSnapshot_ReturnsForm`
Expected: compile failure (`Form`, `SingleAnchorHash`, `ReleaseDate`, `RetiredAt` are not fields of `SuiteSnapshot`).

- [ ] **Step 3: Change `SuiteSnapshot` and `GetSuiteSnapshot`**

In `internal/cache/types.go` change `SuiteSnapshot` to:

```go
type SuiteSnapshot struct {
	SnapshotID      int64
	CanonicalScheme string
	CanonicalHost   string
	SuitePath       string
	// SingleAnchorHash is the suite_snapshot.inrelease_hash column: the
	// single anchor blob for the inline form (InRelease) AND the unsigned
	// form (Release). Read Form to know which file it is.
	//
	// AIDEV-NOTE: renamed from InReleaseHash so the compiler lists every
	// reader when forms change (spec C5). Never assume "set ⇒ inline".
	SingleAnchorHash *string
	InReleaseETag    *string
	InReleaseLastMod *string
	ReleaseHash      *string
	ReleaseGPGHash   *string
	CreatedAt        int64
	AdoptedAt        *int64 // NULL while candidate; set on flip
	Form             AnchorForm
	ReleaseDate      *int64 // parsed Release Date (phase B); NULL until then
	RetiredAt        *int64 // set when the suite incarnation is reaped/forgotten (phase F)

	PackageCoverageComplete bool
}
```

(keep the existing `PackageCoverageComplete` comment). In `GetSuiteSnapshot`, add `anchor_form, release_date, retired_at` to the SELECT, scan them into a local `var form string`, `&s.ReleaseDate`, `&s.RetiredAt`, set `s.Form = AnchorForm(form)`, and scan `inrelease_hash` into `&s.SingleAnchorHash`.

- [ ] **Step 4: Update the readers**

- `freshness.detectForm`: `if snap.Form == cache.FormDetached { return adoptionFormDetached }; return adoptionFormInline`, with `// AIDEV-TODO(phase E): unsigned form dispatch`.
- inline `cachedHash` in `checkLockedInline`: `case serr == nil && snap.Form == cache.FormInline && snap.SingleAnchorHash != nil: cachedHash = *snap.SingleAnchorHash`.
- `snapshotAnchorHash`: `case inReleaseFilename: if snap.Form == cache.FormInline { h = snap.SingleAnchorHash }`.
- `priorAdoptionForm`: `switch snap.Form { case cache.FormInline: return adoptionFormInline, true; case cache.FormDetached: return adoptionFormDetached, true }` (anything else falls through to the existing "unknown" return).
- Two raw-SQL readers still assume "`inrelease_hash` set ⇒ InRelease". Phase A creates no unsigned rows, so their behaviour is unchanged; mark both for phase E:
  - `internal/cache/gc_urlpath.go` (~269-274, the `ss.inrelease_hash IS NOT NULL AND ? = sf.suite_path || '/InRelease'` guard): add `// AIDEV-TODO(phase E): an unsigned snapshot stores its Release hash in inrelease_hash; guard by anchor_form.`
  - `internal/cache/adoption.go` `CommitAdoption` anchor seed (~443-469, `/InRelease` seeded from `inrelease_hash`): add the same AIDEV-TODO.

- [ ] **Step 5: Update the tests**

- Run `go vet ./...`. For every error `….InReleaseHash undefined (type …SuiteSnapshot…)` or `unknown field InReleaseHash in struct literal of type cache.SuiteSnapshot`, rename to `SingleAnchorHash`. Do **not** rename `SnapshotCandidate.InReleaseHash`.
- The freshness fake cache stores test-built snapshots as written, while production `GetSuiteSnapshot` always returns a form (the v7 trigger rejects form-less rows). Make form-less fixtures impossible, in `internal/freshness/freshness_test.go`:

```go
func (f *fakeCache) putSnapshot(s cache.SuiteSnapshot) {
	// Production snapshots always carry a Form (the v7 trigger rejects
	// form-less rows), so a fixture without one models an impossible row.
	if s.Form == "" {
		panic("fakeCache.putSnapshot: fixture must set Form")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[s.SnapshotID] = s
}
```

  and add the form to its four fixtures: `Form: cache.FormInline` at `freshness_test.go:879` and `:931` (they set `SingleAnchorHash`), `Form: cache.FormDetached` at `:1012` and `:1229` (they set `ReleaseHash` and `ReleaseGPGHash`). Do not change the production readers to make a test pass: these four tests (`TestCheck_UsesSnapshotHashForComparison`, `TestCheck_RecoversWhenAnchorMissingButSnapshotPresent`, `TestCheck_DetachedRecoversWhenAnchorsMissing`, `TestCheck_DetachedKnownSuite_AdoptsOnReleaseChange`) are the freeze-trap regression tests.
- Run `gofmt -w` on every test file you edited except the five drift files (`internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go` among the ones this step may touch); in those, align the renamed lines by hand.

- [ ] **Step 6: Run to verify it passes**

Run: `go vet ./... && go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./internal/gc/ ./internal/admin/ ./internal/integrity/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A internal/
git commit -m "$(cat <<'EOF'
Read the snapshot anchor form instead of guessing it

SuiteSnapshot.InReleaseHash is renamed SingleAnchorHash, since the
column also holds an unsigned snapshot's Release hash, and gains Form,
ReleaseDate and RetiredAt. Every reader that loads a SuiteSnapshot now
dispatches on Form. Two raw-SQL readers that still equate inrelease_hash
with InRelease are marked for phase E; phase A creates no unsigned rows.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Precise error wrapping and the guard sentinel

**Files:**
- Modify: `internal/freshness/adoption.go` (sentinels block ~77-100; wrap sites ~1008, ~1422, ~2019; guard sites ~1225, ~1241, ~1271)
- Test: `internal/freshness/adoption_errors_test.go` (create)

**Interfaces:**
- Consumes: `testHex64` (Task 4); existing freshness test helpers `newAdoptionTestEnv`, `fakePackagesStanzas`, `makeRelease`, `newMemberErr`, `classifyAdoptionOutcome`.
- Produces: `freshness.ErrAdoptionGuardFailed` (wraps `ErrAdoptionMemberFetchFailed`, so the `member_fetch_failed` outcome label is unchanged). Phase B, the first consumer of the wrapped causes, adds tests for each cause it classifies (commit and Packages-read wraps included).

- [ ] **Step 1: Write the failing test**

`internal/freshness/adoption_errors_test.go`:

```go
package freshness

import (
	"context"
	"errors"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// Re-adopting the current snapshot's exact content collides with the
// adopted natural key; the cause must be visible through the chain.
func TestRun_NaturalKeyCollisionIsWrapped(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": testHex64})
	release, _ := makeRelease(map[string][]byte{"main/binary-amd64/Packages": pkgs})
	env.fetcher.put("http://archive.ubuntu.com/ubuntu/dists/noble/main/binary-amd64/Packages", pkgs)
	if err := env.adopter.Run(ctx, env.suite, release, "", ""); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	err := env.adopter.Run(ctx, env.suite, release, "", "")
	if !errors.Is(err, ErrAdoptionDBFailed) || !errors.Is(err, cache.ErrSnapshotNaturalKeyAdopted) {
		t.Fatalf("err = %v, want both ErrAdoptionDBFailed and ErrSnapshotNaturalKeyAdopted in the chain", err)
	}
}

func TestErrAdoptionGuardFailed_KeepsMemberFetchOutcome(t *testing.T) {
	err := newMemberErr(ErrAdoptionGuardFailed, "main/binary-all/Packages", "absent")
	if !errors.Is(err, ErrAdoptionGuardFailed) || !errors.Is(err, ErrAdoptionMemberFetchFailed) {
		t.Fatalf("chain = %v", err)
	}
	if got := classifyAdoptionOutcome(err); got != "member_fetch_failed" {
		t.Fatalf("outcome = %q, want member_fetch_failed", got)
	}
}
```

Then make the three existing guard tests assert the new sentinel, so an unconverted guard site fails. In each, change the `errors.Is(err, ErrAdoptionMemberFetchFailed)` check that follows the guarded `Run` call to `errors.Is(err, ErrAdoptionGuardFailed)` (and its message to "want ErrAdoptionGuardFailed …"):

- `internal/freshness/phase6_7_skip_repair_test.go` `TestAdopter_RequiredArchGroupAllMissingDefersAdoption` (~line 722; required-arch guard);
- `internal/freshness/adoption_test.go` `TestAdopter_MemberSkipped_AllMembers404Fails` (~line 1999; all-4xx guard — only this first check, not the later "403 must not skip" check, which is an ordinary member failure);
- `internal/freshness/adoption_test.go` `TestAdopter_Allowlist_BlocksPublishWhenBinaryAllUnavailable` (~line 2650; serve-contract guard).

If one of them still fails after Step 3 because its path is not one of the three guards, stop and report it; do not weaken the assertion back.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'NaturalKeyCollisionIsWrapped|GuardFailed|RequiredArchGroupAllMissing|AllMembers404Fails|BlocksPublishWhenBinaryAll'`
Expected: compile failure (`undefined: ErrAdoptionGuardFailed`). After adding only the sentinel (Step 3's first block), the natural-key test and the three guard tests FAIL: the cause is not in the chain yet.

- [ ] **Step 3: Implement**

Add to the sentinel `var (...)` block:

```go
	// ErrAdoptionGuardFailed marks a pre-commit guard refusal (required
	// architecture group missing, every member 4xx, serve-contract target
	// absent). It wraps ErrAdoptionMemberFetchFailed so the outcome label
	// stays member_fetch_failed; retry logic can tell guards apart.
	ErrAdoptionGuardFailed = fmt.Errorf("adoption_guard_failed: %w", ErrAdoptionMemberFetchFailed)
```

Change the three wraps to preserve the cause:
- `return fmt.Errorf("%w: insert candidate: %w", ErrAdoptionDBFailed, err)`
- `return fmt.Errorf("%w: commit: %w", ErrAdoptionDBFailed, err)`
- `return packageHashBuildResult{}, fmt.Errorf("%w: read %q: %w", ErrAdoptionParseFailed, m.Path, err)`

Change the guard returns: the required-arch guard and the serve-contract guard call `newMemberErr(ErrAdoptionGuardFailed, …)`; the all-4xx guard returns `fmt.Errorf("%w: all %d declared members returned 4xx", ErrAdoptionGuardFailed, skippedCount)`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/freshness/`
Expected: PASS (existing tests assert `ErrAdoptionMemberFetchFailed`, still satisfied).

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/adoption.go internal/freshness/adoption_errors_test.go internal/freshness/adoption_test.go internal/freshness/phase6_7_skip_repair_test.go
git add internal/freshness/adoption.go internal/freshness/adoption_errors_test.go internal/freshness/adoption_test.go internal/freshness/phase6_7_skip_repair_test.go
git commit -m "$(cat <<'EOF'
Preserve adoption error causes and mark guard failures

Candidate-insert, commit and Packages-read failures now wrap their cause
with %w, and pre-commit guard refusals carry ErrAdoptionGuardFailed.
Outcome labels are unchanged; later retry logic can tell causes apart.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: R-1 — reconcile parses only verified plaintext

**Files:**
- Modify: `internal/freshness/reconcile.go` (`reparseSnapshotRelease` and its `AIDEV-NOTE`)
- Test: `internal/freshness/reconcile_r1_test.go` (create)

**Interfaces:**
- Consumes: existing freshness test helper `newAdoptionTestEnv` (fields `adopter`, `cache`, `suite`) and the unexported `(*Adopter).writeBlobBytes`, `(*Adopter).reparseSnapshotRelease`.
- Produces: test helpers `clearsignForTest(t, text) []byte` and `storeInlineSnapshot(t, env, blob) int64`.

- [ ] **Step 1: Write the failing test**

`internal/freshness/reconcile_r1_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// R-1 (security review 2026-09): lines placed after a forged "=CRC" line
// inside the signature armor are unauthenticated. Reconcile must parse
// only the clearsigned plaintext.
func TestReparseSnapshotRelease_IgnoresArmorInjection(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()

	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte{}, signed[:idx]...), evil...), signed[idx:]...)

	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(ctx, id)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, want only the signed member", got)
	}
}

func TestReparseSnapshotRelease_PlainTextInReleaseStillParses(t *testing.T) {
	// require_signature = false stores a non-clearsigned InRelease; its
	// whole body is the verified text.
	env := newAdoptionTestEnv(t)
	body := "Origin: Test\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	id := storeInlineSnapshot(t, env, []byte(body))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// R-1 fail-closed: an envelope whose clearsign frame no longer decodes
// (END marker gone) but whose RAW text still carries a parseable SHA256
// block must be an error — never a fallback parse of the raw envelope.
func TestReparseSnapshotRelease_UndecodableEnvelopeFailsClosed(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	id := storeInlineSnapshot(t, env, append([]byte{}, signed[:idx]...))
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err == nil || len(got) != 0 {
		t.Fatalf("got %d members, err=%v; want a decode error and no members", len(got), err)
	}
}

// R-1: the verifier accepts whitespace before the BEGIN marker, so the
// clearsign detection must find a marker anywhere (never HasPrefix), and
// the injection must be ignored for a whitespace-prefixed envelope too.
func TestReparseSnapshotRelease_IgnoresInjectionWithLeadingWhitespace(t *testing.T) {
	env := newAdoptionTestEnv(t)
	genuine := "Origin: Test\nSuite: noble\nSHA256:\n " + strings.Repeat("a", 64) + " 10 main/binary-amd64/Packages\n"
	signed := clearsignForTest(t, genuine)
	evil := "=AAAA\nSHA256:\n " + strings.Repeat("b", 64) + " 10 main/binary-evilarch/Packages\n"
	idx := bytes.LastIndex(signed, []byte("-----END PGP SIGNATURE-----"))
	tampered := append(append(append([]byte("\n \n"), signed[:idx]...), evil...), signed[idx:]...)
	id := storeInlineSnapshot(t, env, tampered)
	got, err := env.adopter.reparseSnapshotRelease(context.Background(), id)
	if err != nil || len(got) != 1 || got[0].Path != "main/binary-amd64/Packages" {
		t.Fatalf("declared = %+v, err=%v; want only the signed member", got, err)
	}
}

func clearsignForTest(t *testing.T, text string) []byte {
	t.Helper()
	ent, err := openpgp.NewEntity("t", "", "t@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, ent.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storeInlineSnapshot writes blob to the pool and records an adopted
// inline snapshot whose InRelease member points at it.
func storeInlineSnapshot(t *testing.T, env *adoptionTestEnv, blob []byte) int64 {
	t.Helper()
	ctx := context.Background()
	h, err := env.adopter.writeBlobBytes(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := env.cache.InsertCandidateSnapshot(ctx, cache.SnapshotCandidate{
		CanonicalScheme: env.suite.CanonicalScheme, CanonicalHost: env.suite.CanonicalHost,
		SuitePath: env.suite.SuitePath, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.cache.CommitAdoption(ctx, id,
		[]cache.SnapshotMember{{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}},
		nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	return id
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run ReparseSnapshotRelease_`
Expected: FAIL — both injection tests return 2 members (`main/binary-evilarch/Packages` included), and the undecodable-envelope test returns 1 member and no error (main parses the raw envelope).

- [ ] **Step 3: Implement**

In `reparseSnapshotRelease`, replace `return ParseRelease(blob)` with:

```go
		if name == "InRelease" {
			text, err := inReleasePlaintext(blob)
			if err != nil {
				return nil, err
			}
			return ParseRelease(text)
		}
		return ParseRelease(blob)
```

and add:

```go
// inReleasePlaintext returns the signed plaintext of a stored InRelease
// without re-verifying the signature (the bytes were verified at adoption
// and are content-addressed; re-verification would also fail after a
// legitimate keyring change).
//
// AIDEV-NOTE: R-1. Never ParseRelease the raw envelope: lines placed
// after a forged "=CRC" line inside the signature armor are part of the
// stored bytes but were never signed. A body with no clearsign marker at
// all is the require_signature=false "unsigned inline" case, whose whole
// body is the verified text. The marker test is bytes.Contains, never
// HasPrefix (the verifier accepts leading whitespace), and a decode
// failure is an error, never a raw parse. The decode runs under
// recover(): a stored blob cannot trigger I-1, but reconcile must never
// crash the daemon.
func inReleasePlaintext(blob []byte) (text []byte, err error) {
	if !bytes.Contains(blob, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
		return blob, nil
	}
	defer func() {
		if r := recover(); r != nil {
			text, err = nil, fmt.Errorf("reconcile: clearsign decode panicked: %v", r)
		}
	}()
	block, _ := clearsign.Decode(blob)
	if block == nil {
		return nil, errors.New("reconcile: stored InRelease is not a valid clearsigned message")
	}
	return block.Plaintext, nil
}
```

(imports: `bytes`, `github.com/ProtonMail/go-crypto/openpgp/clearsign`). Rewrite the function's `AIDEV-NOTE` "trust anchor" paragraph to say the hash check proves the envelope is intact at rest and `inReleasePlaintext` limits the parse to the signed region.

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/reconcile.go internal/freshness/reconcile_r1_test.go
git add internal/freshness/reconcile.go internal/freshness/reconcile_r1_test.go
git commit -m "$(cat <<'EOF'
Reconcile parses only the signed InRelease plaintext (R-1)

reparseSnapshotRelease parsed the whole stored InRelease envelope, so
member declarations injected after a forged armor CRC line reached the
snapshot and package_hash. It now parses the clearsigned plaintext only,
under recover().

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: I-1 — panic-safe clearsign decoding and panic recovery in freshness, adoption and T1 goroutines

**Files:**
- Modify: `internal/gpg/verifier.go` (`VerifyInline` decode, new `decodeClearsigned`), `internal/freshness/freshness.go` (`Check`, the goroutines at ~339, ~356 and ~1193, new `recoverGoroutine`, `reportPanic`, `checkLockedSafe`, `bumpFailedCheck`), `internal/freshness/adoption.go` (heartbeat ticker goroutine ~1051), `internal/freshness/scheduler.go` (`tick`), `internal/freshness/metrics.go` (outcome inventory comment), `internal/handler/handler.go` (`maybeFireFreshness`)
- Test: `internal/gpg/clearsign_panic_test.go`, `internal/freshness/panic_recovery_test.go`, `internal/handler/panic_recovery_test.go` (create)

**Interfaces:**
- Consumes: `testHex64` (Task 4, `internal/freshness/adoption_url_test.go`); existing freshness test helpers `newAdoptionTestEnv`, `newFakeCache`, `(*fakeCache).putURL`, `(*fakeCache).suite`, `discardLogger`, `newTestFetcher`, `passThroughVerifier`, `fakePackagesStanzas`, `makeRelease`, `currentSnapshotID`; handler helper `newTestHandlerWithFreshness`; gpg test helpers `newTestEntity`, `newKeyring`, `newSuite`, `silentLogger`.
- Produces: `freshness.ErrAdoptionPanicked`; `(*Checker).recoverGoroutine(where string, suite SuiteRef)`; test types in `internal/freshness/panic_recovery_test.go` — `panickingVerifier`, `changedUpstream` (fields `body []byte`, `calls atomic.Int32`, `panic atomic.Bool`) and `lockedBuffer` — which Task 10 reuses; log events `adoption_panicked` and `freshness_check_panicked` (ERROR; `stack` only on the first occurrence per goroutine kind per process).

- [ ] **Step 1: Write the failing gpg test**

`internal/gpg/clearsign_panic_test.go`:

```go
package gpg

import (
	"context"
	"testing"
)

// I-1 (security review 2026-09): go-crypto v1.4.1's clearsign.Decode
// panics when a clearsigned message has no plaintext lines. VerifyInline
// must return an ordinary error in both signature modes, never panic and
// never hand the envelope back as "unsigned" text.
func TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic(t *testing.T) {
	signer := newTestEntity(t, "Signer", "signer@example.com")
	bodies := []string{
		"-----BEGIN PGP SIGNED MESSAGE-----\n\n-----BEGIN PGP SIGNATURE-----",
		"-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n-----BEGIN PGP SIGNATURE-----\n\nwsBc\n-----END PGP SIGNATURE-----\n",
	}
	for _, require := range []bool{true, false} {
		v, err := NewVerifier(VerifierConfig{Keyring: newKeyring(t, signer), RequireSignature: require, Logger: silentLogger()})
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range bodies {
			out, err := v.VerifyInline(context.Background(), newSuite(), []byte(body))
			if err == nil || out != nil {
				t.Errorf("require_signature=%v body=%q: out=%q err=%v; want an error", require, body, out, err)
			}
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gpg/ -run ZeroPlaintext`
Expected: the test binary crashes with `panic: runtime error: slice bounds out of range [:-1]`.

- [ ] **Step 3: Implement `decodeClearsigned`**

In `internal/gpg/verifier.go` add:

```go
// errClearsignPanicked reports a clearsigned body that made the go-crypto
// decoder panic. It is always an error, whatever require_signature says:
// the body claims to be clearsigned and is not a valid clearsigned message.
var errClearsignPanicked = errors.New("clearsigned message is malformed (decoder panicked)")

// decodeClearsigned is clearsign.Decode with a panic turned into
// errClearsignPanicked.
//
// AIDEV-NOTE: I-1. go-crypto v1.4.1 panics (slice bounds [:-1]) when the
// signed text has no lines. Every clearsign.Decode call on untrusted bytes
// in this package goes through this wrapper; the freshness goroutines
// also recover, as defence in depth.
func decodeClearsigned(b []byte) (block *clearsign.Block, err error) {
	defer func() {
		if r := recover(); r != nil {
			block, err = nil, fmt.Errorf("%w: %v", errClearsignPanicked, r)
		}
	}()
	block, _ = clearsign.Decode(b)
	return block, nil
}
```

and in `VerifyInline` Step 3 replace `block, _ := clearsign.Decode(inRelease)` with:

```go
	block, err := decodeClearsigned(inRelease)
	if err != nil {
		return nil, err
	}
```

(the existing `if block == nil { … }` branch follows unchanged).

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/gpg/`
Expected: PASS.

- [ ] **Step 5: Write the failing freshness and handler tests**

`internal/freshness/panic_recovery_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

type panickingVerifier struct{}

func (panickingVerifier) VerifyInline(context.Context, SuiteRef, []byte) ([]byte, error) {
	panic("clearsign.Decode: zero-plaintext frame")
}

func (panickingVerifier) VerifyDetached(context.Context, SuiteRef, []byte, []byte) ([]byte, error) {
	panic("unexpected")
}

type panickingAdoptionFetcher struct{}

func (panickingAdoptionFetcher) Fetch(context.Context, *fetch.Target, fetch.FetchDst) (*fetch.FetchResult, error) {
	panic("member fetch bug")
}

// changedUpstream is a freshness Fetcher that always answers 200 with
// body, counting calls; with panic set it panics instead.
type changedUpstream struct {
	body  []byte
	calls atomic.Int32
	panic atomic.Bool
}

func (u *changedUpstream) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	u.calls.Add(1)
	if u.panic.Load() {
		panic("fetcher bug")
	}
	return &fetch.ConditionalResult{Status: 200, Body: u.body}, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// seedStaleAnchor gives suite /r/dists/x on host h an InRelease anchor
// whose hash differs from anything upstream serves, so a check sees
// "changed".
func seedStaleAnchor(t *testing.T, c *cache.Cache) {
	t.Helper()
	ctx := context.Background()
	stale := testHex64
	if err := c.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// I-1: a panic inside the adoption goroutine must not kill the process,
// must be logged, and must release the suite mutex.
func TestCheck_AdoptionPanicIsRecovered(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	seedStaleAnchor(t, env.cache)
	ad, err := NewAdopter(AdoptionConfig{
		Cache: env.cache, Fetcher: env.fetcher, Verifier: panickingVerifier{}, HostLimiter: hostsem.New(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	up := &changedUpstream{body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")}
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: env.cache, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad,
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if !strings.Contains(logs.String(), "adoption_panicked") {
		t.Fatalf("no adoption_panicked log; logs:\n%s", logs.String())
	}
	// Cooldown is 0, so a second Check proceeds only if the mutex was released.
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (suite mutex leaked?)", got)
	}
}

// mutexProbeHandler records, when adoption_panicked is logged, whether
// the suite mutex is still held. Phase B records the failure at that
// point, which spec C5 requires to happen before the mutex is released.
type mutexProbeHandler struct {
	slog.Handler
	c       *Checker
	key     string
	logged  atomic.Bool
	heldNow atomic.Bool
}

func (p *mutexProbeHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "adoption_panicked" {
		return nil
	}
	p.logged.Store(true)
	if v, ok := p.c.locks.Load(p.key); ok {
		mu := v.(*sync.Mutex)
		if mu.TryLock() {
			mu.Unlock()
		} else {
			p.heldNow.Store(true)
		}
	}
	return nil
}

func (p *mutexProbeHandler) Enabled(context.Context, slog.Level) bool { return true }
func (p *mutexProbeHandler) WithAttrs([]slog.Attr) slog.Handler      { return p }
func (p *mutexProbeHandler) WithGroup(string) slog.Handler           { return p }

// The recover must be the last-registered defer: it runs while the suite
// mutex is still held, and WaitForAdoptions returns only after both.
func TestCheck_AdoptionPanicRecoveredBeforeMutexRelease(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	seedStaleAnchor(t, env.cache)
	ad, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: panickingVerifier{}, HostLimiter: hostsem.New(8)})
	if err != nil {
		t.Fatal(err)
	}
	up := &changedUpstream{body: []byte("-----BEGIN PGP SIGNED MESSAGE-----\n")}
	ph := &mutexProbeHandler{key: suiteKey("http", "h", "/r/dists/x")}
	c, err := New(Config{Cache: env.cache, Fetcher: up, HostLimiter: hostsem.New(8), Adopter: ad, Logger: slog.New(ph)})
	if err != nil {
		t.Fatal(err)
	}
	ph.c = c
	c.Check(ctx, "http", "h", "/r/dists/x")
	c.WaitForAdoptions()
	if !ph.logged.Load() {
		t.Fatal("adoption_panicked not logged before WaitForAdoptions returned")
	}
	if !ph.heldNow.Load() {
		t.Fatal("adoption_panicked handled after the suite mutex was released")
	}
	v, _ := c.locks.Load(ph.key)
	if mu := v.(*sync.Mutex); !mu.TryLock() {
		t.Fatal("suite mutex still held after WaitForAdoptions returned")
	} else {
		mu.Unlock()
	}
}

// I-1: a panic in the repair goroutine is recovered and logged, and the
// suite mutex is released. Setup follows
// TestChecker_FreshTickRepairsSkippedMembers (unchanged upstream, one
// repairable skipped member).
func TestCheck_RepairPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	env := newAdoptionTestEnv(t)
	env.suite = SuiteRef{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: "/dists/noble"}
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	releaseText, _ := makeRelease(map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64.gz":          []byte(strings.Repeat("C", 100)),
	})
	base := "http://127.0.0.1/dists/noble/"
	env.fetcher.put(base+"main/binary-amd64/Packages", pkgs)
	env.fetcher.put(base+"Contents-amd64.gz", []byte(strings.Repeat("c", 90))) // stale: skipped at adoption
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dists/noble/InRelease" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(releaseText)
	}))
	defer srv.Close()
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "127.0.0.1", Path: "/dists/noble/InRelease",
		UpstreamURL: srv.URL + "/dists/noble/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
	good, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: env.fetcher, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Run(ctx, env.suite, releaseText, "", ""); err != nil {
		t.Fatalf("adoption: %v", err)
	}
	if rows, _ := env.cache.ListRepairableSkippedMembers(ctx, currentSnapshotID(t, env)); len(rows) != 1 {
		t.Fatalf("fixture: want 1 repairable row, got %d", len(rows))
	}
	panicky, err := NewAdopter(AdoptionConfig{Cache: env.cache, Fetcher: panickingAdoptionFetcher{}, Verifier: passThroughVerifier{},
		HostLimiter: hostsem.New(8), TolerateOptionalMemberFailures: true, RepairSkippedMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	checker, err := New(Config{Cache: env.cache, Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Adopter: panicky, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	checker.Check(ctx, "http", "127.0.0.1", "/dists/noble")
	checker.WaitForAdoptions()
	if !strings.Contains(logs.String(), "adoption_panicked") || !strings.Contains(logs.String(), "where=repair") {
		t.Fatalf("no repair panic logged:\n%s", logs.String())
	}
	v, ok := checker.locks.Load(suiteKey("http", "127.0.0.1", "/dists/noble"))
	if !ok {
		t.Fatal("no suite mutex allocated")
	}
	if mu := v.(*sync.Mutex); !mu.TryLock() {
		t.Fatal("suite mutex leaked by the panicking repair goroutine")
	}
}

// recoverGoroutine is the single recover used by the adoption, repair and
// on-demand reconcile goroutines.
func TestRecoverGoroutine_LogsAndSwallows(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c.recoverGoroutine("reconcile", SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"})
		panic("boom")
	}()
	<-done
	out := logs.String()
	for _, want := range []string{"adoption_panicked", "where=reconcile", "panic=boom", "suite_path=/r/dists/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// A panic in the synchronous part of Check releases the mutex too.
func TestCheck_SynchronousPanicReleasesMutex(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	up.panic.Store(false)
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if !strings.Contains(logs.String(), "freshness_check_panicked") {
		t.Fatalf("no freshness_check_panicked log:\n%s", logs.String())
	}
}

// Spec C4: a recovered panic counts as a failed check, so it bumps
// last_check_at and the cooldown gates the next trigger (otherwise every
// client metadata request would re-run it: one upstream GET and one
// ERROR line each).
func TestCheck_PanickingCheckIsCooldownGated(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
	up := &changedUpstream{body: []byte("x")}
	up.panic.Store(true)
	now := time.Unix(1_800_000_000, 0)
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		c.Check(context.Background(), "http", "h", "/r/dists/x")
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a panic must bump last_check_at)", got)
	}
	got, ok := fc.suite("http", "h", "/r/dists/x")
	if !ok || got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() || got.LastSuccessAt != nil {
		t.Fatalf("suite after panic = %+v, want last_check_at=now and no last_success_at", got)
	}
}

// Spec C5: a deterministic panic logs its stack once per goroutine kind
// per process; later occurrences log one line without the stack.
func TestReportPanic_LogsStackOncePerKind(t *testing.T) {
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: newFakeCache(), Fetcher: newTestFetcher(t), HostLimiter: hostsem.New(8),
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	s := SuiteRef{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}
	const kind = "test-kind-stack-once"
	c.reportPanic("adoption_panicked", kind, s, "first")
	c.reportPanic("adoption_panicked", kind, s, "second")
	if n := strings.Count(logs.String(), "stack="); n != 1 {
		t.Fatalf("stack attributes = %d, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "adoption_panicked"); n != 2 {
		t.Fatalf("adoption_panicked lines = %d, want 2", n)
	}
}
```

`internal/handler/panic_recovery_test.go`:

```go
package handler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

type panickingChecker struct{}

func (panickingChecker) Check(context.Context, string, string, string) { panic("checker bug") }

// I-1: a panicking T1 check must not kill the process, must be logged,
// and must still release the handler's drain WaitGroup.
func TestMaybeFireFreshness_RecoversPanic(t *testing.T) {
	h := newTestHandlerWithFreshness(t, panickingChecker{})
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.maybeFireFreshness(&proxy.Request{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", IsMetadata: true, SuitePath: "/r/dists/x"})
	h.Close() // blocks until the goroutine finishes; returns only if Done ran
	out := logs.String()
	if !strings.Contains(out, "freshness_check_panicked") || !strings.Contains(out, "where=t1") {
		t.Fatalf("T1 panic not logged:\n%s", out)
	}
}
```

(`logs` is read only after `h.Close()` returns, which waits for the goroutine, so a plain `bytes.Buffer` is race-free here.)

- [ ] **Step 6: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'PanicIsRecovered|PanicRecoveredBefore|RecoverGoroutine|SynchronousPanic|PanickingCheck|ReportPanic' ; go test ./internal/handler/ -run RecoversPanic`
Expected: the freshness package fails to compile (`recoverGoroutine`, `reportPanic` undefined); the handler test binary crashes with `panic: checker bug`.

- [ ] **Step 7: Implement in the freshness package**

Add to `freshness.go` (imports `runtime/debug`, `sync`):

```go
// ErrAdoptionPanicked is recorded (phase B) when an adoption, repair or
// reconcile goroutine panics (security finding I-1).
var ErrAdoptionPanicked = errors.New("adoption_panicked")

// panicStackLogged holds the goroutine kinds ("adoption", "repair",
// "reconcile", "check", "scheduler") whose stack was already logged.
var panicStackLogged sync.Map

// reportPanic logs a recovered panic. The stack is attached to the first
// occurrence per goroutine kind per process only (spec C5 "log the stack
// once"): a deterministic panic on every attempt must not flood the log.
func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
	attrs := []any{
		"where", where,
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"panic", fmt.Sprint(r),
	}
	if _, seen := panicStackLogged.LoadOrStore(where, struct{}{}); !seen {
		attrs = append(attrs, "stack", string(debug.Stack()))
	}
	c.logger.Error(event, attrs...)
}

// recoverGoroutine is deferred by the adoption, repair and on-demand
// reconcile goroutines as their LAST defer, so it runs first: the panic
// is logged while the suite mutex is still held, then the goroutine's
// other defers release the mutex and the WaitGroup.
//
// AIDEV-NOTE: recover() works only because this method IS the deferred
// function (`defer c.recoverGoroutine(...)`). Wrapping it in a closure
// (`defer func() { c.recoverGoroutine(...) }()`) makes recover() return
// nil and the panic kills the daemon. Phase B records ErrAdoptionPanicked
// as an adoption failure here, before the mutex is released (spec C5).
func (c *Checker) recoverGoroutine(where string, suite SuiteRef) {
	r := recover()
	if r == nil {
		return
	}
	c.reportPanic("adoption_panicked", where, suite, r)
	if where == "adoption" {
		adoptionTotal.Inc("panicked", suite.CanonicalHost)
	}
}
```

In each of the three goroutines, register the defers in this order (the WaitGroup first so it is released last, and `WaitForAdoptions` returning implies the mutex is free):

- adoption goroutine (~356):

```go
	go func() {
		defer c.adoptionWg.Done()
		defer mu.Unlock()
		defer c.recoverGoroutine("adoption", req.suite)
```

- repair goroutine (~339): the same three lines with `c.recoverGoroutine("repair", repair.suite)`;
- on-demand reconcile goroutine (~1193): the same three lines with `c.recoverGoroutine("reconcile", suite)`.

Above the adoption goroutine add `// AIDEV-NOTE: defer order is load-bearing: Done (last), Unlock, recover (first). See recoverGoroutine.`

The adoption also starts one goroutine of its own: the heartbeat ticker in `runShared` (`internal/freshness/adoption.go` ~1051). A panic there would not reach the adoption goroutine's recover (recover only sees its own goroutine), so give it its own, after `defer tickerWG.Done()`:

```go
		go func() {
			defer tickerWG.Done()
			// AIDEV-NOTE: I-1. A panic in this goroutine is invisible to
			// the adoption goroutine's recover. Recover here, log, and stop
			// heartbeating; a candidate whose heartbeat lapses is reaped by
			// snapshot GC and the adoption fails at commit, which is safe.
			defer func() {
				if r := recover(); r != nil {
					a.logger.Error("adoption_panicked",
						"where", "heartbeat",
						"canonical_host", suite.CanonicalHost,
						"suite_path", suite.SuitePath,
						"panic", fmt.Sprint(r))
				}
			}()
			a.runHeartbeatTicker(tickerCtx, suite.CanonicalHost, snapshotID, tracker)
		}()
```

(This site has no test: the ticker only writes hashes the adoption computed itself, so no untrusted input reaches it; the recover is defence in depth. Any other `go func` added under `internal/freshness` later needs the same treatment — `grep -n 'go func' internal/freshness/*.go` lists them.)

In `Check`, replace `req, repair := c.checkLocked(ctx, scheme, host, suitePath)` with:

```go
	req, repair, ok := c.checkLockedSafe(ctx, scheme, host, suitePath)
	if !ok {
		mu.Unlock()
		return
	}
```

and add:

```go
// checkLockedSafe runs checkLocked and converts a panic into a FAILED
// check (spec C4): logged, and last_check_at bumped like any failed check
// so the cooldown gates the next T1/T2 trigger. The caller releases the
// suite mutex.
//
// AIDEV-NOTE: nothing in Check between TryLock and this call, or between
// it and the goroutine hand-off, may panic: checkRecovered (scheduler)
// and the T1 recover cannot release a mutex they do not know about.
func (c *Checker) checkLockedSafe(ctx context.Context, scheme, host, suitePath string) (req *adoptionRequest, repair *repairRequest, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "check",
				SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, r)
			c.bumpFailedCheck(ctx, scheme, host, suitePath)
			freshnessCheckTotal.Inc("failed", host)
			req, repair, ok = nil, nil, false
		}
	}()
	req, repair = c.checkLocked(ctx, scheme, host, suitePath)
	return req, repair, true
}

// bumpFailedCheck mirrors checkLocked's failure bookkeeping
// (last_check_at = now, last_success_at untouched). Best effort; it
// never panics its caller.
// AIDEV-NOTE: phase D turns the Put into the UPDATE-only writer.
func (c *Checker) bumpFailedCheck(ctx context.Context, scheme, host, suitePath string) {
	defer func() { _ = recover() }()
	cur, err := c.cache.GetSuiteFreshness(ctx, scheme, host, suitePath)
	switch {
	case errors.Is(err, cache.ErrNotFound):
		cur = &cache.SuiteFreshness{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}
	case err != nil:
		return
	}
	now := c.now().Unix()
	cur.LastCheckAt = &now
	_ = c.cache.PutSuiteFreshness(ctx, *cur)
}
```

In `scheduler.go` `tick`, replace the per-suite `c.Check(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)` with `c.checkRecovered(ctx, s)` and add:

```go
// checkRecovered keeps one suite's panic from ending the scheduler loop.
// Check already converts a checkLocked panic into a failed check; this
// catches anything outside it.
func (c *Checker) checkRecovered(ctx context.Context, s cache.SuiteFreshness) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "scheduler",
				SuiteRef{CanonicalScheme: s.CanonicalScheme, CanonicalHost: s.CanonicalHost, SuitePath: s.SuitePath}, r)
		}
	}()
	c.Check(ctx, s.CanonicalScheme, s.CanonicalHost, s.SuitePath)
}
```

In `metrics.go`, add `panicked` to the `acu_adoption_total` outcome inventory comment (the AIDEV-NOTE listing the SPEC5 §10.4.3 outcomes).

- [ ] **Step 8: Implement in the handler**

In `maybeFireFreshness`'s goroutine, after `defer h.activeWG.Done()` add (imports `runtime/debug`, `sync/atomic` if missing):

```go
		defer func() {
			if r := recover(); r != nil {
				attrs := []any{
					"where", "t1",
					"canonical_host", req.CanonicalHost,
					"suite_path", req.SuitePath,
					"panic", fmt.Sprint(r),
				}
				if t1PanicStackLogged.CompareAndSwap(false, true) {
					attrs = append(attrs, "stack", string(debug.Stack()))
				}
				h.logger.Error("freshness_check_panicked", attrs...)
			}
		}()
```

with, at package level, `// t1PanicStackLogged limits the T1 panic stack to one per process (spec C5).` and `var t1PanicStackLogged atomic.Bool`.

- [ ] **Step 9: Run to verify it passes**

Run: `go test -race ./internal/gpg/ ./internal/freshness/ ./internal/handler/ && go test -race -count=20 ./internal/freshness/ -run 'PanicRecoveredBefore|PanicIsRecovered'`
Expected: PASS (the repeated run shows the defer ordering is deterministic).

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/gpg internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/panic_recovery_test.go internal/handler/handler.go internal/handler/panic_recovery_test.go
git add internal/gpg/verifier.go internal/gpg/clearsign_panic_test.go internal/freshness/freshness.go internal/freshness/adoption.go internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/panic_recovery_test.go internal/handler/handler.go internal/handler/panic_recovery_test.go
git commit -m "$(cat <<'EOF'
Stop malformed InRelease files from crashing the daemon (I-1)

A clearsigned InRelease with no signed text makes go-crypto's decoder
panic, and no freshness or adoption goroutine recovered, so the daemon
crash-looped. The verifier now turns that panic into an ordinary error.
As defence in depth, every freshness, adoption, repair, reconcile and T1
goroutine recovers, logs (with the stack once per kind), and releases
the suite mutex; a panic during a check counts as a failed check, so the
cooldown still applies.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

(The `gofmt -l` line must print nothing.)

---

### Task 10: Clock-step rule in the cooldown, scheduler and stale-gauge gates

**Files:**
- Modify: `internal/freshness/freshness.go` (cooldown gate ~476-482, new `clockStep`, `clockStepTolerance`, `Checker.lastClockStepLog`), `internal/freshness/scheduler.go` (`staleSuiteCounts`, T2 threshold ~98-104), `internal/freshness/metrics.go` (`clockStepsTotal`), `internal/freshness/freshness_test.go` (`TestStaleSuiteCounts`)
- Test: `internal/freshness/clockstep_test.go` (create)

**Interfaces:**
- Consumes: `testHex64` (Task 4); `changedUpstream` and `lockedBuffer` (Task 9, `internal/freshness/panic_recovery_test.go`); existing helpers `newFakeCache`, `(*fakeCache).putURL`, `(*fakeCache).suite`, `discardLogger`.
- Produces: WARN event `clock_step_detected` (fields `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime`), rate-limited to one per minute per checker; unlabelled counter `acu_clock_steps_detected_total`; `clockStepTolerance` (2 s).

Spec C4: a *past-event* stamp found in the future is treated as infinitely old by gates that delay work. Stamps are whole seconds, so a sub-second NTP step back could make a just-written stamp look 1 s in the future; `clockStepTolerance` (2 s) keeps that from counting as a step.

- [ ] **Step 1: Write the failing tests**

`internal/freshness/clockstep_test.go`:

```go
package freshness

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

func seedFakeAnchor(fc *fakeCache) {
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
}

// I-7 / spec C4: a last_check_at stamp in the future (backward clock
// step) must not freeze checks; the check heals the stamp, after which
// the cooldown works again; the WARN is rate-limited.
func TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(2 * time.Hour).Unix()
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &future, LastSuccessAt: &future})
	up := &changedUpstream{body: []byte("new")}
	logs := &lockedBuffer{}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	c.Check(context.Background(), "http", "h", "/r/dists/x") // within the cooldown of the healed stamp
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (frozen, or cooldown not restored after healing)", got)
	}
	got, _ := fc.suite("http", "h", "/r/dists/x")
	if got.LastCheckAt == nil || *got.LastCheckAt != now.Unix() {
		t.Fatalf("last_check_at not healed to now: %v", got.LastCheckAt)
	}
	// A second detection within a minute must not log again.
	c.clockStep("last_check_at", "http", "h", "/r/dists/y", future, now)
	if n := strings.Count(logs.String(), "clock_step_detected"); n != 1 {
		t.Fatalf("clock_step_detected lines = %d, want 1 (rate-limited)", n)
	}
}

// A stamp within the tolerance is "just now", not a clock step: the
// cooldown still applies.
func TestCheck_StampWithinToleranceKeepsCooldown(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	justAhead := now.Unix() + 1
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &justAhead})
	up := &changedUpstream{body: []byte("new")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if got := up.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 (1 s ahead is within tolerance: cooldown applies)", got)
	}
}

func TestTick_FutureLastSuccessAtIsDue(t *testing.T) {
	fc := newFakeCache()
	seedFakeAnchor(fc)
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(30 * 24 * time.Hour).Unix()
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastSuccessAt: &future})
	up := &changedUpstream{body: []byte("new")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Refresh: 15 * time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.tick(context.Background())
	if up.calls.Load() != 1 {
		t.Fatalf("scheduler skipped a suite whose last_success_at is in the future")
	}
}
```

In `internal/freshness/freshness_test.go`, replace the body of `TestStaleSuiteCounts` from `suites := …` to the end with:

```go
	const now = int64(3000)
	suites := []cache.SuiteFreshness{
		mk(i(2000), i(7)), // fresh adopted        -> not stale
		mk(i(500), i(7)),  // stale adopted        -> count adopted
		mk(i(999), i(7)),  // just-stale adopted   -> count adopted
		mk(i(400), nil),   // stale unadopted      -> count unadopted
		mk(nil, i(7)),     // never-succeeded      -> not "went stale", skip
		mk(i(9000), i(7)), // in the future (clock step) -> infinitely old: count adopted
		mk(i(3001), i(7)), // 1 s ahead, within tolerance -> fresh
	}
	adopted, unadopted := staleSuiteCounts(suites, staleBefore, now)
	if adopted != 3 {
		t.Errorf("adopted stale = %d, want 3", adopted)
	}
	if unadopted != 1 {
		t.Errorf("unadopted stale = %d, want 1", unadopted)
	}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'FutureLast|WithinTolerance|StaleSuiteCounts'`
Expected: compile failure (`clockStep` undefined; `staleSuiteCounts` takes 2 arguments). With stubs, the future-stamp tests see 0 upstream calls.

- [ ] **Step 3: Implement**

Add to `freshness.go`:

```go
// clockStepTolerance: stamps are whole seconds, so a sub-second backward
// NTP step can make a just-written stamp look up to 1 s in the future.
// Only a stamp further ahead than this counts as a clock step.
const clockStepTolerance = 2 * time.Second

// clockStepLogEvery rate-limits clock_step_detected to one line per
// minute per checker (monotonic).
const clockStepLogEvery = time.Minute

var processStart = time.Now()

// clockStep records a past-event stamp found in the future (spec C4).
//
// AIDEV-NOTE: only PAST-EVENT stamps (last_check_at, last_success_at,
// last_requested_at, adoption_fail_first_at/last_at, adopted_at) are
// "infinitely old" when in the future. adoption_next_retry_at is a
// FUTURE-event stamp: phase B only clamps it to now+1h; never route it
// through this rule, or a backward step would disable backoff.
func (c *Checker) clockStep(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	clockStepsTotal.Inc()
	last := c.lastClockStepLog.Load()
	mono := int64(time.Since(processStart))
	if last != 0 && time.Duration(mono-last) < clockStepLogEvery {
		return
	}
	if !c.lastClockStepLog.CompareAndSwap(last, mono) {
		return
	}
	c.logger.Warn("clock_step_detected",
		"stamp", stamp,
		"canonical_scheme", scheme,
		"canonical_host", host,
		"suite_path", suitePath,
		"stored_unixtime", stored,
		"now_unixtime", now.Unix(),
	)
}
```

Add `lastClockStepLog atomic.Int64 // monotonic ns since processStart of the last clock_step_detected line` to `Checker` (import `sync/atomic`).

Cooldown gate (replace the existing `if cur.LastCheckAt != nil && c.cooldown > 0 { … }` block):

```go
	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
	// the future (backward wall-clock step) is treated as infinitely old,
	// never as "just checked" — otherwise the suite freezes for the size
	// of the step (security finding I-7).
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		last := time.Unix(*cur.LastCheckAt, 0)
		if last.After(now.Add(clockStepTolerance)) {
			c.clockStep("last_check_at", scheme, host, suitePath, *cur.LastCheckAt, now)
		} else if now.Sub(last) < c.cooldown {
			return nil, nil
		}
	}
```

(keep whatever the existing block returns on "in cooldown"; only the condition changes).

In `scheduler.go` `tick`, compute the time once at the top (`nowT := c.now()`; use it for `staleBefore` and `threshold`), pass `nowT.Unix()` to `staleSuiteCounts`, and change the T2 skip to:

```go
		if s.LastSuccessAt != nil && *s.LastSuccessAt > threshold {
			if *s.LastSuccessAt <= nowT.Add(clockStepTolerance).Unix() {
				continue
			}
			c.clockStep("last_success_at", s.CanonicalScheme, s.CanonicalHost, s.SuitePath, *s.LastSuccessAt, nowT)
		}
```

and `staleSuiteCounts` becomes:

```go
func staleSuiteCounts(suites []cache.SuiteFreshness, staleBefore, now int64) (adopted, unadopted int) {
	futureAfter := now + int64(clockStepTolerance/time.Second)
	for _, s := range suites {
		if s.LastSuccessAt == nil {
			continue
		}
		// Spec C4: a success stamp in the future is infinitely old, so a
		// suite frozen across a backward clock step still raises the gauge.
		if *s.LastSuccessAt >= staleBefore && *s.LastSuccessAt <= futureAfter {
			continue
		}
		if s.CurrentSnapshotID != nil {
			adopted++
		} else {
			unadopted++
		}
	}
	return adopted, unadopted
}
```

In `metrics.go` add (unlabelled: spec C11 allows host-labelled or unlabelled series; the stamp is in the log line):

```go
	// clockStepsTotal counts past-event stamps found in the future
	// (backward wall-clock steps, spec C4).
	clockStepsTotal = metrics.NewCounter(
		"acu_clock_steps_detected_total",
		"Past-event timestamps found in the future (backward wall-clock step).",
	)
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/clockstep_test.go internal/freshness/freshness_test.go
git add internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/clockstep_test.go internal/freshness/freshness_test.go
git commit -m "$(cat <<'EOF'
Stop backward clock steps from freezing freshness checks (I-7)

A last_check_at or last_success_at stamp in the future made the cooldown
and scheduler gates skip the suite until the clock caught up, and hid it
from the stale-suite gauge. Such stamps (beyond a 2 s tolerance) are now
treated as infinitely old, counted, and logged as clock_step_detected
(rate-limited).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

(The `gofmt -l` line must print nothing.)

---

### Task 11: Handler fetch seam

**Files:**
- Modify: `internal/handler/handler.go` (`Config.Fetch`, `Handler.fetch`, new `Fetcher`)
- Test: `internal/handler/fetch_seam_test.go` (create)

**Interfaces:**
- Consumes: existing handler test helper `silentLogger()`.
- Produces:

```go
type Fetcher interface {
	HostAllowed(host string) bool
	Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error)
}
```

- [ ] **Step 1: Write the failing tests**

`internal/handler/fetch_seam_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// recFetcher is an in-memory upstream that records the URLs it is asked
// for and answers every request with body.
type recFetcher struct {
	body []byte
	mu   sync.Mutex
	urls []string
}

func (*recFetcher) HostAllowed(string) bool { return true }

func (r *recFetcher) Fetch(_ context.Context, tg *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	r.mu.Lock()
	r.urls = append(r.urls, tg.URL)
	r.mu.Unlock()
	if _, err := dst.Write(r.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(r.body))}, nil
}

func (r *recFetcher) fetched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

func newSeamHandler(t *testing.T, f Fetcher) *Handler {
	t.Helper()
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h, err := New(Config{Parser: parser, Cache: c, Fetch: f, Logger: silentLogger(), HostLimiter: hostsem.New(4)})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The seam exists so virtual-clock simulations can drive the handler:
// the real cache and an in-memory upstream run inside a synctest bubble.
func TestHandler_InMemoryFetcherInsideBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up := &recFetcher{body: []byte("deb")}
		h := newSeamHandler(t, up)
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://up.example/pool/x.deb", nil))
			if rec.Code != 200 || rec.Body.String() != "deb" {
				t.Fatalf("request %d: status=%d body=%q", i, rec.Code, rec.Body.String())
			}
			time.Sleep(time.Hour)
		}
		h.Close()
		if got := up.fetched(); len(got) != 1 {
			t.Fatalf("upstream fetches = %v, want 1 (later requests are cache hits)", got)
		}
	})
}

// Review Focus 2 end to end: apt's request line reaches upstream in apt's
// encoding, and a differently-encoded request for the same file hits the
// same cache key.
func TestHandler_EncodingEndToEnd(t *testing.T) {
	up := &recFetcher{body: []byte("deb bytes")}
	h := newSeamHandler(t, up)
	defer h.Close()
	get := func(target string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code
	}
	if code := get("http://up.example/pool/libstdc%2b%2b6_1%253a2~24.04_amd64.deb"); code != 200 {
		t.Fatalf("first GET = %d", code)
	}
	if code := get("http://up.example/pool/libstdc++6_1%253a2%7e24.04_amd64.deb"); code != 200 {
		t.Fatalf("second GET = %d", code)
	}
	urls := up.fetched()
	if len(urls) != 1 {
		t.Fatalf("upstream fetches = %v, want exactly 1 (same cache key)", urls)
	}
	if want := "http://up.example/pool/libstdc%2b%2b6_1%253a2%7e24.04_amd64.deb"; urls[0] != want {
		t.Fatalf("upstream URL = %q, want %q", urls[0], want)
	}
	u, err := url.Parse(urls[0])
	if err != nil || u.Path != "/pool/libstdc++6_1%3a2~24.04_amd64.deb" {
		t.Fatalf("upstream URL decodes to %q (%v)", u.Path, err)
	}
}
```

(`handler.New` requires exactly `Parser`, `Cache`, `Fetch` and `HostLimiter`; `httptest.NewRequest` sets `RequestURI` to the absolute target, which is the proxy-mode form the parser expects. The default allowlist is irrelevant because `recFetcher.HostAllowed` returns true.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run 'InMemoryFetcherInsideBubble|EncodingEndToEnd'`
Expected: compile failure — `Fetcher` undefined, and `*recFetcher` is not a `*fetch.Client`.

- [ ] **Step 3: Implement**

Add the `Fetcher` interface (with a doc comment: "the subset of *fetch.Client the handler uses; an interface so virtual-clock simulations can drive the handler with an in-memory upstream — the real client does not advance testing/synctest time") and the assertion `var _ Fetcher = (*fetch.Client)(nil)` next to the existing `fetch.FetchDst` assertion. Change `Config.Fetch` and the `Handler.fetch` field types from `*fetch.Client` to `Fetcher`. `cmd/apt-cacher-ultra/main.go` keeps passing `fetchClient` unchanged.

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./... && go test -race ./internal/handler/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/handler/handler.go internal/handler/fetch_seam_test.go
git add internal/handler/handler.go internal/handler/fetch_seam_test.go
git commit -m "$(cat <<'EOF'
Add a fetcher interface seam to the handler

The handler depends on a small Fetcher interface that *fetch.Client
satisfies, so virtual-clock simulations can drive it with an in-memory
upstream. An end-to-end test pins apt's upstream encoding and the shared
cache key for differently-encoded requests.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Virtual-clock harness, smoke and clock-step simulations

**Files:**
- Create: `internal/simtest/simtest.go`, `internal/freshness/sim_smoke_test.go`

**Interfaces:**
- Consumes: `testHex64` (Task 4); `wallclock.SetOffsetForTest` (Task 1).
- Produces: `simtest.OpenCache(t *testing.T) *cache.Cache` (closes itself before the bubble ends), `simtest.DiscardLogger() *slog.Logger`, `simtest.StaticConditional` (a `freshness.Fetcher` returning a fixed status and counting calls, via `Calls() int32`).

Measured cost under `-race` on the dev box: about 3.5 ms per check and 1 ms per scheduler tick (≈0.3 ms and 0.1 ms without `-race`). Each simulation below stays under 5 s under `-race`; size later simulations the same way (overview Global Constraints).

- [ ] **Step 1: Write the failing tests**

`internal/freshness/sim_smoke_test.go`:

```go
package freshness

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

// startSimChecker seeds one suite (/r/dists/x on host h) in a real cache
// and runs a checker's scheduler against a 304-answering upstream. The
// returned stop function cancels the scheduler and drains it.
func startSimChecker(t *testing.T, refresh time.Duration) (up *simtest.StaticConditional, stop func()) {
	t.Helper()
	c := simtest.OpenCache(t)
	ctx := context.Background()
	h := testHex64
	if err := c.PutBlob(ctx, h, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.PutURLPath(ctx, cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", BlobHash: &h, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutSuiteFreshness(ctx, cache.SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x"}); err != nil {
		t.Fatal(err)
	}
	up = &simtest.StaticConditional{Status: 304}
	chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
		Cooldown: time.Minute, Refresh: refresh, Logger: simtest.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { chk.Run(runCtx); close(done) }()
	return up, func() {
		cancel()
		<-done
		chk.WaitForAdoptions()
	}
}

// Phase A DoD: the real cache, checker and scheduler run for 30 virtual
// days inside a synctest bubble. Asserts liveness (checks keep happening)
// and a bound (no runaway checking).
func TestSimSmoke_30DaysPeriodicRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up, stop := startSimChecker(t, 2*time.Hour)
		time.Sleep(30 * 24 * time.Hour)
		stop()
		// Ticks run every refresh/4 = 30m. The first tick (30m) checks the
		// never-succeeded suite; after that a check lands whenever
		// last_success_at is 2h old: 30m, 2h30m, ... = 360 checks in 30
		// days. synctest makes this exact; the slack only absorbs an
		// off-by-one at the window edges.
		if got := up.Calls(); got < 358 || got > 362 {
			t.Fatalf("checks in 30 virtual days = %d, want 360 (358..362)", got)
		}
	})
}

// Spec §7 C4: wall-clock steps of +30d, -30d and -2h must not stop
// periodic checks (liveness) nor cause runaway checking (bound).
func TestSimClockSteps_KeepChecking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		up, stop := startSimChecker(t, 15*time.Minute)
		defer stop()
		phase := func(name string, offset time.Duration) {
			restore := wallclock.SetOffsetForTest(offset)
			defer restore()
			before := up.Calls()
			time.Sleep(12 * time.Hour)
			got := up.Calls() - before
			// One check per 15 min; the first tick after a step may be
			// lost to rounding at the step or gained by healing.
			if want := int32(12 * 4); got < want-2 || got > want+2 {
				t.Errorf("%s: %d checks in 12h, want %d±2", name, got, want)
			}
		}
		phase("baseline", 0)
		phase("forward +30d", 30*24*time.Hour)
		phase("back to 0 (-30d step)", 0)
		phase("backward -2h", -2*time.Hour)
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'SimSmoke|SimClockSteps'`
Expected: FAIL — package `internal/simtest` does not exist.

- [ ] **Step 3: Implement the harness**

`internal/simtest/simtest.go`:

```go
// Package simtest holds helpers for virtual-clock simulations run inside
// testing/synctest bubbles with the real cache (spec §7). Only tests
// import it.
//
// AIDEV-NOTE: rules for code under simulation and for simulation authors:
//   - Code under simulation must block only on channels and timers, never
//     on a sync.Mutex held across a virtual sleep, and must use in-memory
//     fakes for network I/O (real sockets do not advance bubble time).
//   - Backward clock steps use wallclock.SetOffsetForTest. The offset is
//     process-global: never set it from a t.Parallel test. Nested calls
//     restore the previous offset.
//   - Bubble time starts at 2000-01-01 UTC, but pool file mtimes (used by
//     ServeContent's Last-Modified and by SweepTmp) are real host times, so
//     inside a bubble the pool mtime always looks newer than any stamp.
//   - Cost under -race (dev box): ~3.5 ms per freshness check and ~1 ms
//     per scheduler tick. Keep each simulation under ~5 s under -race by
//     choosing coarser intervals, not by dropping assertions.
package simtest

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// OpenCache opens a real cache in a temp dir and closes it at cleanup,
// which runs inside the bubble before it ends.
func OpenCache(t *testing.T) *cache.Cache {
	t.Helper()
	c, err := cache.Open(context.Background(), t.TempDir(), DiscardLogger())
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// DiscardLogger drops all log output.
func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// StaticConditional is a freshness.Fetcher answering every conditional
// GET with Status (and Body when Status is 200), counting calls.
type StaticConditional struct {
	Status int
	Body   []byte
	calls  atomic.Int32
}

// Conditional implements freshness.Fetcher.
func (s *StaticConditional) Conditional(context.Context, *fetch.Target, string, string, int64) (*fetch.ConditionalResult, error) {
	s.calls.Add(1)
	return &fetch.ConditionalResult{Status: s.Status, Body: s.Body}, nil
}

// Calls returns how many conditional GETs were made.
func (s *StaticConditional) Calls() int32 { return s.calls.Load() }
```

- [ ] **Step 4: Run to verify it passes, and time it**

Run: `go test ./internal/freshness/ -run 'SimSmoke|SimClockSteps' -count=1 -v`, then `time go test -race ./internal/freshness/ -run 'SimSmoke|SimClockSteps' -count=1 -v`.
Expected: PASS; the smoke simulation reports exactly 360 checks; each test takes under 5 s under `-race` (the review measured about 2.2 s for the smoke run). If either hangs, a goroutine in the checker or cache is blocked on something other than a channel or timer: find it with `GOTRACEBACK=all` and fix it before continuing (that is exactly the class of bug the harness must catch). If the clock-step simulation fails only in the "back to 0" or "-2h" phase, Task 10's rule is not applied at some gate.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/simtest internal/freshness/sim_smoke_test.go
git add internal/simtest internal/freshness/sim_smoke_test.go
git commit -m "$(cat <<'EOF'
Add virtual-clock simulation harness

internal/simtest runs the real cache inside testing/synctest bubbles.
A 30-day smoke simulation drives the freshness scheduler and asserts
liveness and an exact check count; a clock-step simulation steps the
wall clock by +30d, -30d and -2h and asserts checks keep happening.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 13: Phase A documentation, anchors, and final verification

**Files:**
- Modify: `docs/log-fields.md`, `docs/configuration.md`, `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (phase table)

- [ ] **Step 1: Document new events**

In `docs/log-fields.md` add an "Operational events" subsection (create it if the file only documents the request line) listing:

| Event | Level | Fields | Meaning |
|---|---|---|---|
| `adoption_panicked` | ERROR | `where` (`adoption`, `repair`, `reconcile`, `heartbeat`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic inside an adoption, repair, reconcile or heartbeat goroutine was recovered; the prior snapshot keeps serving. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` (first occurrence per `where` per process only) | A panic during a freshness check was recovered; the check counts as failed (`last_check_at` bumped, so the cooldown applies). |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is more than 2 s in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. |

- [ ] **Step 2: Document the upstream encoding change**

In `docs/configuration.md`, in the `[upstream]` section's prose, add one paragraph: "Upstream request paths are encoded exactly as apt encodes them (lowercase percent-encoding of `+`, `~`, space, `%`, control and non-ASCII bytes, plus `#` and `?`). Paths containing `+` or `~` therefore reach upstream as `%2b`/`%7e`, as they do when apt fetches directly. Request paths containing a decoded control byte are rejected with 400."

The repository has no metrics reference document; the metric inventory lives in the AIDEV-NOTEs of `internal/freshness/metrics.go`, which Tasks 9 and 10 already updated (`panicked` outcome, `acu_clock_steps_detected_total`). Check with `grep -n 'panicked\|clock_steps' internal/freshness/metrics.go`.

- [ ] **Step 3: Full verification**

Run:

```bash
gofmt -l . ; go vet ./... ; golangci-lint run ./... ; go test -race -count=1 ./...
```

Expected: `gofmt -l .` prints exactly the five files it already prints on `main` (formatter-version drift, not ours: `cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) and no others — do not reformat those five; every file this phase creates or edits must be gofmt-clean. vet and lint clean; all packages PASS. Note the wall time of `go test -race ./internal/freshness/` in the commit message; it must stay well under the 10-minute CI timeout.

- [ ] **Step 4: Mark the phase in the overview and commit**

Set phase A's status in the overview table to "done (commit <sha of this commit's parent>)".

```bash
git add docs/
git commit -m "$(cat <<'EOF'
Document phase A events, metric and upstream encoding

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 5: Phase review gate**

Dispatch the max-effort review of `git diff main...HEAD` for phase A (correctness, security, robustness lenses) per the overview's execution protocol; fix findings in follow-up commits before writing the phase B plan.

## Phase A Definition of Done

- Schema: `TestSchemaV7_MigratesPopulatedV6`, `TestSchemaV7_BackfillCorrelatesAndFilters`, `TestSchemaV7_GoStepFailureRollsBackMigration`, `TestSchemaV7_TriggerRejectsEmptyFormAndUniqueIndexIsPartial`, `TestNoSQLiteClockFunctionsInProductionCode`, `TestPutSuiteFreshness_StampsInterestOnInsertOnly`, `TestCommitAdoption_NewSuiteRowGetsInterestStamp` pass.
- Wall clock: `internal/wallclock` tests, `TestNowUnix_FollowsWallclock`, `TestNew_DefaultNowFollowsWallclock` pass.
- Paths: `TestAptEncodePath_Table`, `TestBuildUpstreamURL_AllBytesRoundTrip`, `TestBuildUpstreamURL_PlainPathsUnchanged`, `TestBuildUpstreamURL_RealWorldSpecialShapes`, `TestParse_*` (control bytes in both modes, encoded hash, apt encoding), `TestNoAdHocUpstreamURLConcatenation`, `TestAptpathImportsOnlyStdlib`, `TestCacheDoesNotImportProxy`, `TestHandler_EncodingEndToEnd` pass.
- Suites: `TestSuitePath_Dists`, `TestIsCanonicalSuitePath` pass; the only changed existing expectations are the two bare-suite cases in `proxy_test.go`, the form-less fixtures (Task 5 Step 8, Task 6 Step 5) and the three guard-test sentinels (Task 7).
- Forms: `TestInsertCandidateSnapshot_DerivesAndStoresForm`, `_LookupIncludesForm`, `_LookupSkipsRetiredIncarnation`, `TestGetSuiteSnapshot_ReturnsFormAndLifecycleFields` pass, and the four freeze-trap tests pass unchanged apart from their explicit `Form`.
- Errors: `TestRun_NaturalKeyCollisionIsWrapped`, `TestErrAdoptionGuardFailed_KeepsMemberFetchOutcome` and the three guard tests pass.
- R-1: `TestReparseSnapshotRelease_IgnoresArmorInjection`, `_IgnoresInjectionWithLeadingWhitespace` and `_UndecodableEnvelopeFailsClosed` fail on `main` and pass here.
- I-1: `TestVerifyInline_ZeroPlaintextIsAnErrorNotAPanic`, `TestCheck_AdoptionPanicIsRecovered` and `TestMaybeFireFreshness_RecoversPanic` fail (crash) on `main` and pass here; `TestCheck_AdoptionPanicRecoveredBeforeMutexRelease`, `TestCheck_RepairPanicIsRecovered`, `TestRecoverGoroutine_LogsAndSwallows`, `TestCheck_SynchronousPanicReleasesMutex`, `TestCheck_PanickingCheckIsCooldownGated`, `TestReportPanic_LogsStackOncePerKind` pass.
- Clock: `TestCheck_FutureLastCheckAtDoesNotFreezeAndHeals`, `TestCheck_StampWithinToleranceKeepsCooldown`, `TestTick_FutureLastSuccessAtIsDue`, `TestStaleSuiteCounts` pass.
- Simulation: `TestSimSmoke_30DaysPeriodicRefresh` (exactly 360 checks) and `TestSimClockSteps_KeepChecking` pass, each under 5 s under `-race`; `TestHandler_InMemoryFetcherInsideBubble` passes.
- `go test -race ./...`, `go vet ./...`, `golangci-lint run ./...` clean; no new `gofmt -l .` entries; docs updated; phase review findings resolved.
