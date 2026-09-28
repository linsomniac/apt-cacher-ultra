# Flat repositories — Phase A: Foundations — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the infrastructure every later phase needs — the v7 schema, the wall-clock and path packages, apt-exact upstream URLs, the tightened `dists/` grammar, precise error wrapping, the R-1 and I-1 security fixes, the clock-step rule, a handler fetch seam, and a virtual-clock test harness — without changing behaviour for ordinary traffic except the documented upstream encoding of `+`/`~`.

**Architecture:** Two new leaf packages (`internal/wallclock`, `internal/aptpath`) centralise wall-clock time and path logic. The v7 migration adds all columns later phases use, with a Go backfill step inside the migration transaction. Existing code is routed through the new packages; readers of the snapshot anchor switch to an explicit `Form`.

**Tech Stack:** Go 1.25 (toolchain 1.26.7), modernc SQLite, `github.com/ProtonMail/go-crypto` v1.4.1, `testing/synctest`.

**Spec:** `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (revision 3) — sections "Shared infrastructure", C1, C2 (`dists/` grammar only), §6, C5 (R-1 and panic recovery), C4 (clock-step rule). Overview: `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (Global Constraints and deviations apply to every task).

## Global Constraints

See the overview. In particular: never edit existing `migrations` entries; no SQLite clock functions in production SQL; persisted stamps from `wallclock`; `internal/cache` never imports `internal/proxy`; adoption-disabled behaviour unchanged; `go test -race ./...` and `golangci-lint run ./...` clean and no new `gofmt -l .` entries after each task; commit trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus (phase A share)

1. Paths with `+ ~ % space | { }` and UTF-8 must reach upstream in apt's encoding and decode back to the cache key — Task 3 (exhaustive byte table + round trip).
2. A populated v6 cache must migrate with correct backfills and keep accepting legacy-shaped inserts — Task 5.
3. A panic anywhere in the freshness/adoption goroutines must not kill the daemon or leak the suite mutex — Task 9.

## File map

| File | Change |
|---|---|
| `internal/wallclock/wallclock.go` (+ `_test.go`) | Create: wall-clock seam. |
| `internal/aptpath/suite.go` (+ `_test.go`) | Create: canonicality predicate, tightened `SuitePath`, `IsCanonicalSuitePath`. |
| `internal/aptpath/encode.go` (+ `_test.go`) | Create: `AptEncodePath`, `BuildUpstreamURL`. |
| `internal/proxy/classify.go`, `proxy.go`, `proxy_test.go`, `url_test.go` | Delegate `SuitePath`; reject control bytes; build upstream URL via `aptpath`. |
| `internal/freshness/adoption.go`, `freshness.go`, `hot_set.go`, `reconcile.go`, `scheduler.go`, `metrics.go` | URL composers, form readers, `%w`, guard sentinel, R-1, panic recovery, clock rule. |
| `internal/cache/schema.go`, `types.go`, `adoption.go`, `cache.go` | v7 migration + Go step, `AnchorForm`, snapshot fields, candidate insert, seed URL, `nowUnix`. |
| `internal/cache/schema_v7_test.go`, `sqlclock_test.go` | Create: migration and SQL-clock tests. |
| `internal/handler/handler.go` (+ tests) | `Fetcher` interface; wall-clock stamps; T1 panic recovery. |
| `internal/gc/gc.go` | Wall-clock summary stamps. |
| `internal/simtest/simtest.go` | Create: virtual-clock helpers. |
| `internal/freshness/sim_smoke_test.go` | Create: 30-day smoke simulation. |
| `docs/log-fields.md`, `docs/configuration.md`, spec | Docs for new events, encoding note, trigger deviation. |

---

### Task 1: `internal/wallclock` and persisted-stamp wiring

**Files:**
- Create: `internal/wallclock/wallclock.go`, `internal/wallclock/wallclock_test.go`
- Modify: `internal/cache/cache.go:228-229`, `internal/freshness/freshness.go:219-222`, `internal/freshness/adoption.go:588-591`, `internal/handler/handler.go:679,867,1522,1792`, `internal/gc/gc.go:275,361`

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
	if Unix() != Now().Unix() && Unix() != Now().Unix()-1 {
		t.Fatalf("Unix() disagrees with Now()")
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

- [ ] **Step 5: Route persisted stamps through `wallclock`**

In `internal/cache/cache.go` replace lines 228-229:

```go
// nowUnix is the test seam for "what time is it". Production reads the
// shared wall clock (internal/wallclock) so simulations can step it.
var nowUnix = func() int64 { return wallclock.Unix() }
```

(add import `"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"`; drop `"time"` only if it becomes unused).

In `internal/freshness/freshness.go` (`New`) replace `now = time.Now` with `now = wallclock.Now`; same in `internal/freshness/adoption.go` (`NewAdopter`). Update the two "production uses time.Now" comments to "production uses wallclock.Now".

In `internal/handler/handler.go`: replace `time.Now().Unix()` with `wallclock.Unix()` at the `recordByHashURLPath` stamp (line ~679), the `X-Cache-Age` computation (~867), the `runFetch` stamp (~1522) and the stale-serve age (~1792). Leave `start := time.Now()` (durations) and the passthrough-log hour bucket (~2176, in-memory) unchanged.

In `internal/gc/gc.go`: replace `endUnix := time.Now().Unix()` (lines ~275 and ~361) with `endUnix := wallclock.Unix()`. Leave deadlines and `start := time.Now()` unchanged.

- [ ] **Step 6: Run the affected packages**

Run: `go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./internal/gc/ ./internal/wallclock/`
Expected: PASS (no behaviour change; offset is zero).

- [ ] **Step 7: Commit**

```bash
git add internal/wallclock internal/cache/cache.go internal/freshness/freshness.go internal/freshness/adoption.go internal/handler/handler.go internal/gc/gc.go
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
- Create: `internal/aptpath/suite.go`, `internal/aptpath/suite_test.go`
- Modify: `internal/proxy/classify.go:64-95` (remove `suiteRegex`, delegate), `internal/proxy/proxy_test.go:459-482`

**Interfaces:**
- Produces: `aptpath.SuitePath(p string) string`, `aptpath.IsCanonicalSuitePath(s string) bool`, `aptpath.ForbiddenPathBytes` (string constant `"#?%\\"`).
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
		{"/ubuntu/dists/noble/", false},
		{"/ubuntu//dists/noble", false},
		{"/ubuntu#x/dists/noble", false},
		{"/foo/dists/InRelease", false},
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
// grammar can never reach again (phase F reaps them).
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
		{"/ubuntu/dists/noble", ""},    // spec C2: a suite needs a member
		{"/dists/stable", ""},          // spec C2: a suite needs a member
```

- [ ] **Step 6: Run the proxy and handler suites**

Run: `go test -race ./internal/aptpath/ ./internal/proxy/ ./internal/handler/`
Expected: PASS. If a handler test asserted a suite for a bare `.../dists/<codename>` path, change its expectation and note it in the commit message (none are expected: apt never requests a bare suite path).

- [ ] **Step 7: Commit**

```bash
git add internal/aptpath/suite.go internal/aptpath/suite_test.go internal/proxy/classify.go internal/proxy/proxy_test.go
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
		// Unreachable for any input (proved by the all-bytes test); keep
		// a correct fallback rather than a panic.
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
```

(Add `"errors"` to the test imports if missing.)

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./internal/proxy/ -run 'ControlBytes|EncodedHash|AptEncoding'`
Expected: FAIL — control bytes accepted; `UpstreamURL` truncated at `#`; `+`/`~` unencoded.

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
- Test: `internal/freshness/adoption_url_test.go` (create), `internal/cache/adoption_anchor_test.go` (extend)

**Interfaces:**
- Consumes: `aptpath.BuildUpstreamURL`.

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

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'BuildMemberURL|URLReconstruct' && go test ./internal/cache/ -run Anchor`
Expected: FAIL on the `+`/`~` cases.

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

- [ ] **Step 4: Run the suites**

Run: `go test -race ./internal/freshness/ ./internal/cache/`
Expected: PASS. Existing fake-fetcher URLs have no `+`/`~`; if one does, update its key to the encoded form.

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/adoption.go internal/freshness/freshness.go internal/freshness/hot_set.go internal/freshness/adoption_url_test.go internal/cache/adoption.go internal/cache/adoption_anchor_test.go
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

### Task 5: v7 migration with a Go backfill step

**Files:**
- Modify: `internal/cache/schema.go` (constant, new `migrations[6]`, `migrationGoSteps`, `applyMigration`)
- Create: `internal/cache/schema_v7_test.go`, `internal/cache/sqlclock_test.go`

**Interfaces:**
- Produces: schema v7 exactly as spec §6, with the overview's trigger deviation.

- [ ] **Step 1: Write the failing tests**

`internal/cache/schema_v7_test.go`:

```go
package cache

import (
	"context"
	"database/sql"
	"log/slog"
	"io"
	"path/filepath"
	"testing"
)

const v7Now = int64(1_800_000_000)

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

const (
	hA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func TestSchemaV7_MigratesPopulatedV6(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	if err := migrate(context.Background(), db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
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

func TestSchemaV7_TriggerFillsFormAndUniqueIndexIsPartial(t *testing.T) {
	defer stubNow(t, v7Now)()
	db := buildV6(t)
	if err := migrate(context.Background(), db, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	// A legacy-shaped insert (no anchor_form) gets its form filled in.
	if _, err := db.Exec(`INSERT INTO suite_snapshot(snapshot_id,canonical_scheme,canonical_host,suite_path,release_hash,release_gpg_hash,created_at)
	                      VALUES(10,'http','h','/u/dists/s9',?,?,1)`, hB, hC); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	var f string
	if err := db.QueryRow(`SELECT anchor_form FROM suite_snapshot WHERE snapshot_id=10`).Scan(&f); err != nil || f != "detached" {
		t.Fatalf("filled form = %q (%v), want detached", f, err)
	}
	// Duplicate inline content of S1 is still rejected ...
	if _, err := db.Exec(`INSERT INTO suite_snapshot(canonical_scheme,canonical_host,suite_path,inrelease_hash,created_at)
	                      VALUES('http','h','/u/dists/s1',?,1)`, hA); err == nil {
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
// SQLite's clock, because the Go wall clock (wallclock) is the only
// clock simulations can control.
func TestNoSQLiteClockFunctionsInProductionCode(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := []string{"strftime(", "CURRENT_TIMESTAMP", "julianday(", "unixepoch("}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "e2e":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'SchemaV7|SQLiteClock'`
Expected: FAIL — version 6, no `anchor_form` column (the SQL-clock test passes already; it guards the new migration).

- [ ] **Step 3: Implement the migration**

In `internal/cache/schema.go`:

1. `const CurrentSchemaVersion = 7` (update the comment's SPEC list with "flat-repositories design §6").
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
	// The trigger FILLS an empty anchor_form from the hash columns instead
	// of rejecting it (plan deviation 1): legacy fixtures insert without
	// it, and a duplicate still fails on the unique index when the
	// trigger's UPDATE runs. Production always sets the form explicitly.
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
CREATE TRIGGER suite_snapshot_anchor_form_default
AFTER INSERT ON suite_snapshot
WHEN NEW.anchor_form = ''
BEGIN
  UPDATE suite_snapshot
     SET anchor_form = CASE WHEN NEW.inrelease_hash IS NOT NULL THEN 'inline' ELSE 'detached' END
   WHERE snapshot_id = NEW.snapshot_id;
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
// transaction, after its SQL, with now = nowUnix(). Used for backfills
// that need the current time (production SQL must not read SQLite's
// clock — see TestNoSQLiteClockFunctionsInProductionCode).
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

4. In `applyMigration`, after `tx.ExecContext(ctx, migrations[from])` succeeds, add:

```go
	if step, ok := migrationGoSteps[from]; ok {
		if err := step(ctx, tx, nowUnix()); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/cache/ -run 'SchemaV|SQLiteClock'`
Expected: PASS (including the existing `TestSchemaV6_*`).

- [ ] **Step 5: Run the whole repository**

Run: `go test -race ./...`
Expected: PASS. Raw test inserts rely on the fill trigger; if any test inserts a *duplicate* legacy row on purpose, it now fails at the trigger's UPDATE instead of the INSERT — adjust the assertion (error still expected) and note it in the commit.

- [ ] **Step 6: Commit**

```bash
git add internal/cache/schema.go internal/cache/schema_v7_test.go internal/cache/sqlclock_test.go
git commit -m "$(cat <<'EOF'
Add schema v7 for flat repositories and suite lifecycle

Adds the suite client-interest stamp and adoption failure record,
snapshot anchor form, release date and retirement, the flat filename
column with a partial index, and the signed-seen marker table. Time
dependent backfills run in a Go step inside the migration transaction so
no SQL reads SQLite's clock.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: `AnchorForm`, snapshot fields, and form-aware readers

**Files:**
- Modify: `internal/cache/types.go` (`SuiteSnapshot`, `SnapshotCandidate`, new `AnchorForm`), `internal/cache/adoption.go` (`InsertCandidateSnapshot`, `GetSuiteSnapshot`), `internal/freshness/freshness.go` (`detectForm`, inline `cachedHash`, `snapshotAnchorHash`), `internal/freshness/adoption.go` (`priorAdoptionForm`, candidate construction), all tests that read `SuiteSnapshot.InReleaseHash`
- Test: `internal/cache/anchor_form_test.go` (create)

**Interfaces:**
- Produces:

```go
type AnchorForm string

const (
	FormInline   AnchorForm = "inline"
	FormDetached AnchorForm = "detached"
	FormUnsigned AnchorForm = "unsigned"
)
```

`SuiteSnapshot` fields `SingleAnchorHash *string` (renamed from `InReleaseHash`), `Form AnchorForm`, `ReleaseDate *int64`, `RetiredAt *int64`; `SnapshotCandidate.Form AnchorForm` (empty ⇒ derived); `cache.ErrInvalidAnchorForm`.

- [ ] **Step 1: Write the failing test**

`internal/cache/anchor_form_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"testing"
)

func TestInsertCandidateSnapshot_DerivesAndStoresForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	for _, h := range []string{hA, hB, hC} {
		if err := c.PutBlob(ctx, h, 1); err != nil {
			t.Fatal(err)
		}
	}
	inl := hA
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &inl})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.GetSuiteSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Form != FormInline || snap.SingleAnchorHash == nil || *snap.SingleAnchorHash != hA {
		t.Fatalf("inline snapshot = %+v", snap)
	}

	rel, sig := hB, hC
	id2, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/y", ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	if snap2, _ := c.GetSuiteSnapshot(ctx, id2); snap2.Form != FormDetached {
		t.Fatalf("detached form = %q", snap2.Form)
	}

	// Unsigned must be explicit and uses the single-anchor slot.
	un := hA
	id3, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &un, Form: FormUnsigned})
	if err != nil {
		t.Fatalf("unsigned insert: %v", err)
	}
	if id3 == id {
		t.Fatal("unsigned candidate reused the inline candidate: form must be part of the natural key")
	}

	// Inconsistent shapes are rejected before touching SQLite.
	if _, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/z", ReleaseHash: &rel, ReleaseGPGHash: &sig, Form: FormUnsigned}); !errors.Is(err, ErrInvalidAnchorForm) {
		t.Fatalf("unsigned with detached hashes: err = %v, want ErrInvalidAnchorForm", err)
	}
}

func TestInsertCandidateSnapshot_IgnoresRetiredOnCollision(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.PutBlob(ctx, hA, 1); err != nil {
		t.Fatal(err)
	}
	h := hA
	cand := SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", InReleaseHash: &h}
	id, _, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.Exec(`UPDATE suite_snapshot SET adopted_at = 1, retired_at = 2 WHERE snapshot_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	id2, reused, err := c.InsertCandidateSnapshot(ctx, cand)
	if err != nil {
		t.Fatalf("insert after retirement: %v (want a fresh candidate, not ErrSnapshotNaturalKeyAdopted)", err)
	}
	if reused || id2 == id {
		t.Fatalf("retired snapshot reused: id=%d id2=%d reused=%v", id, id2, reused)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/cache/ -run 'InsertCandidateSnapshot_'`
Expected: FAIL — compile errors (`Form`, `SingleAnchorHash`, `FormInline`, `ErrInvalidAnchorForm` undefined).

- [ ] **Step 3: Implement the types**

In `internal/cache/types.go` add the `AnchorForm` block above `SnapshotCandidate`, add `Form AnchorForm // empty ⇒ derived from the hash columns; FormUnsigned must be explicit` to `SnapshotCandidate`, and change `SuiteSnapshot` to:

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
	ReleaseDate      *int64 // parsed Release Date (phase B); NULL before v7
	RetiredAt        *int64 // set when the suite incarnation is reaped/forgotten (phase F)

	PackageCoverageComplete bool
}
```

(keep the existing `PackageCoverageComplete` comment). Add `var ErrInvalidAnchorForm = errors.New("cache: invalid anchor form for snapshot candidate")` next to the other cache errors.

- [ ] **Step 4: Implement `InsertCandidateSnapshot` and `GetSuiteSnapshot`**

In `InsertCandidateSnapshot`, before `submitWrite`, derive and validate:

```go
	form, err := candidateForm(sc)
	if err != nil {
		return 0, false, err
	}
```

with

```go
// candidateForm derives (or validates) the anchor form of a candidate.
// Inline and unsigned use the single-anchor slot (InReleaseHash);
// detached uses Release + Release.gpg. FormUnsigned is never derived.
func candidateForm(sc SnapshotCandidate) (AnchorForm, error) {
	single := sc.InReleaseHash != nil
	detached := sc.ReleaseHash != nil && sc.ReleaseGPGHash != nil
	switch sc.Form {
	case "":
		switch {
		case single && sc.ReleaseHash == nil && sc.ReleaseGPGHash == nil:
			return FormInline, nil
		case detached && !single:
			return FormDetached, nil
		}
	case FormInline, FormUnsigned:
		if single && sc.ReleaseHash == nil && sc.ReleaseGPGHash == nil {
			return sc.Form, nil
		}
	case FormDetached:
		if detached && !single {
			return FormDetached, nil
		}
	}
	return "", fmt.Errorf("%w: form=%q", ErrInvalidAnchorForm, sc.Form)
}
```

Change the INSERT to include `anchor_form` (add the column and one `?` bound to `string(form)`), and the collision lookup to:

```go
	const lookupQ = `
SELECT snapshot_id, adopted_at FROM suite_snapshot
 WHERE canonical_scheme = ? AND canonical_host = ? AND suite_path = ?
   AND COALESCE(inrelease_hash, release_hash) = ?
   AND anchor_form = ?
   AND retired_at IS NULL`
```

passing `string(form)` as the fifth argument. Update the `AIDEV-NOTE` above the function: the natural key now includes the form and ignores retired incarnations.

In `GetSuiteSnapshot`, select `anchor_form, release_date, retired_at` and scan into `&form, &s.ReleaseDate, &s.RetiredAt`, then `s.Form = AnchorForm(form)`; scan `inrelease_hash` into `&s.SingleAnchorHash`.

- [ ] **Step 5: Update readers**

- `freshness.detectForm`: `if snap.Form == cache.FormDetached { return adoptionFormDetached }; return adoptionFormInline` (unsigned arrives in phase E; add `// AIDEV-TODO(phase E): unsigned form dispatch`).
- inline `cachedHash` in `checkLockedInline`: `case serr == nil && snap.Form == cache.FormInline && snap.SingleAnchorHash != nil: cachedHash = *snap.SingleAnchorHash`.
- `snapshotAnchorHash`: `case inReleaseFilename: if snap.Form == cache.FormInline { h = snap.SingleAnchorHash }`.
- `priorAdoptionForm`: `switch snap.Form { case cache.FormInline: return adoptionFormInline, true; case cache.FormDetached: return adoptionFormDetached, true }`.
- In `runShared` candidate construction, set `cand.Form = cache.FormInline` / `cache.FormDetached` explicitly in the two `switch p.form` arms.
- Tests: run `go vet ./...`; for every compile error `…InReleaseHash undefined (type *cache.SuiteSnapshot…)` replace `.InReleaseHash` with `.SingleAnchorHash`. Do **not** rename `SnapshotCandidate.InReleaseHash`.

- [ ] **Step 6: Run to verify it passes**

Run: `go vet ./... && go test -race ./internal/cache/ ./internal/freshness/ ./internal/handler/ ./internal/gc/ ./internal/admin/ ./internal/integrity/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A internal/
git commit -m "$(cat <<'EOF'
Make snapshot anchor form explicit

Snapshots carry an explicit anchor form (inline, detached, unsigned).
SuiteSnapshot.InReleaseHash is renamed SingleAnchorHash, since the
column also holds an unsigned snapshot's Release hash, and every reader
now dispatches on Form. Candidate inserts store the form, and the
natural-key collision lookup includes it and ignores retired snapshots.

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
- Produces: `freshness.ErrAdoptionGuardFailed` (wraps `ErrAdoptionMemberFetchFailed`, so the `member_fetch_failed` outcome label is unchanged).

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

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'NaturalKeyCollisionIsWrapped|GuardFailed'`
Expected: FAIL — `ErrSnapshotNaturalKeyAdopted` not in chain; `ErrAdoptionGuardFailed` undefined.

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
git add internal/freshness/adoption.go internal/freshness/adoption_errors_test.go
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

func TestReparseSnapshotRelease_BrokenClearsignIsAnError(t *testing.T) {
	env := newAdoptionTestEnv(t)
	broken := "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n" // no body, no signature
	id := storeInlineSnapshot(t, env, []byte(broken))
	if _, err := env.adopter.reparseSnapshotRelease(context.Background(), id); err == nil {
		t.Fatal("broken clearsign parsed without error")
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
Expected: FAIL — the armor-injection test returns 2 members; the broken-clearsign test may return a parse error or pass by accident (it must fail with a decode error after the fix).

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
// body is the verified text. The decode runs under recover(): a stored
// blob cannot trigger I-1, but reconcile must never crash the daemon.
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

### Task 9: I-1 — panic recovery in freshness, adoption and T1 goroutines

**Files:**
- Modify: `internal/freshness/freshness.go` (`Check`, the goroutines at ~339, ~356, ~1193), `internal/freshness/scheduler.go` (`tick`), `internal/freshness/metrics.go` (outcome inventory comment), `internal/handler/handler.go` (`maybeFireFreshness`)
- Test: `internal/freshness/panic_recovery_test.go`, `internal/handler/panic_recovery_test.go` (create)

**Interfaces:**
- Produces: `freshness.ErrAdoptionPanicked`; log events `adoption_panicked` and `freshness_check_panicked` (ERROR, with `stack`).

- [ ] **Step 1: Write the failing tests**

`internal/freshness/panic_recovery_test.go`:

```go
package freshness

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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

func (l *lockedBuffer) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *lockedBuffer) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// I-1: a panic inside the adoption goroutine must not kill the process,
// must be logged, and must release the suite mutex.
func TestCheck_AdoptionPanicIsRecovered(t *testing.T) {
	env := newAdoptionTestEnv(t)
	ctx := context.Background()
	stale := testHex64
	if err := env.cache.PutBlob(ctx, stale, 1); err != nil {
		t.Fatal(err)
	}
	if err := env.cache.PutURLPath(ctx, cache.URLPath{
		CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true,
	}); err != nil {
		t.Fatal(err)
	}
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
```

`internal/handler/panic_recovery_test.go`:

```go
package handler

import (
	"context"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

type panickingChecker struct{}

func (panickingChecker) Check(context.Context, string, string, string) { panic("checker bug") }

// I-1: a panicking T1 check must not kill the process and must still
// release the handler's drain WaitGroup.
func TestMaybeFireFreshness_RecoversPanic(t *testing.T) {
	h := newTestHandlerWithFreshness(t, panickingChecker{})
	h.maybeFireFreshness(&proxy.Request{CanonicalScheme: "http", CanonicalHost: "h",
		Path: "/r/dists/x/InRelease", IsMetadata: true, SuitePath: "/r/dists/x"})
	h.Close() // blocks until the goroutine finishes; returns only if Done ran
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'PanicIsRecovered|SynchronousPanic' ; go test ./internal/handler/ -run RecoversPanic`
Expected: the test binary crashes with `panic: clearsign.Decode…` / `panic: checker bug`.

- [ ] **Step 3: Implement in the freshness package**

Add to `freshness.go`:

```go
// ErrAdoptionPanicked is recorded (phase B) when an adoption, repair or
// reconcile goroutine panics (security finding I-1).
var ErrAdoptionPanicked = errors.New("adoption_panicked")

// reportPanic logs a recovered panic with its stack. where names the
// goroutine ("adoption", "repair", "reconcile", "check").
func (c *Checker) reportPanic(event, where string, suite SuiteRef, r any) {
	c.logger.Error(event,
		"where", where,
		"canonical_host", suite.CanonicalHost,
		"suite_path", suite.SuitePath,
		"panic", fmt.Sprint(r),
		"stack", string(debug.Stack()),
	)
}
```

(import `runtime/debug`). In each of the three goroutines add, as the **last** `defer` registered (so it runs first, before `adoptionWg.Done()` and `mu.Unlock()`):

```go
		defer func() {
			if r := recover(); r != nil {
				c.reportPanic("adoption_panicked", "adoption", req.suite, r)
				adoptionTotal.Inc("panicked", req.suite.CanonicalHost)
			}
		}()
```

using `"repair"`/`repair.suite` for the repair+reconcile goroutine and `"reconcile"`/`suite` for the on-demand reconcile goroutine (no `adoptionTotal` increment for those two). Add an `AIDEV-NOTE` above the adoption goroutine: "recover() must be the last defer so the mutex and WaitGroup are released after it; phase B records ErrAdoptionPanicked as a failure here."

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
// checkLockedSafe runs checkLocked and converts a panic into a failed
// check so the suite mutex is always released (I-1: T1 and the scheduler
// now handle upstream bytes on first sight).
func (c *Checker) checkLockedSafe(ctx context.Context, scheme, host, suitePath string) (req *adoptionRequest, repair *repairRequest, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			c.reportPanic("freshness_check_panicked", "check",
				SuiteRef{CanonicalScheme: scheme, CanonicalHost: host, SuitePath: suitePath}, r)
			req, repair, ok = nil, nil, false
		}
	}()
	req, repair = c.checkLocked(ctx, scheme, host, suitePath)
	return req, repair, true
}
```

In `scheduler.go` `tick`, wrap the per-suite call:

```go
		c.checkRecovered(ctx, s)
```

with

```go
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

In `metrics.go`, add `panicked` to the `acu_adoption_total` outcome inventory comment (the AIDEV-NOTE listing SPEC5 §10.4.3 outcomes).

- [ ] **Step 4: Implement in the handler**

In `maybeFireFreshness`'s goroutine, after `defer h.activeWG.Done()` add:

```go
		defer func() {
			if r := recover(); r != nil {
				h.logger.Error("freshness_check_panicked",
					"where", "t1",
					"canonical_host", req.CanonicalHost,
					"suite_path", req.SuitePath,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
			}
		}()
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test -race ./internal/freshness/ ./internal/handler/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/panic_recovery_test.go internal/handler/handler.go internal/handler/panic_recovery_test.go
git commit -m "$(cat <<'EOF'
Recover panics in freshness, adoption and T1 goroutines (I-1)

A zero-plaintext clearsigned InRelease panics inside go-crypto, and no
freshness or adoption goroutine recovered, so the daemon crash-looped.
Every such goroutine now recovers, logs the stack, and releases the
suite mutex; a panic in the synchronous part of Check counts as a failed
check.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Clock-step rule in the cooldown and scheduler gates

**Files:**
- Modify: `internal/freshness/freshness.go` (cooldown gate ~476-482, new `clockStep`), `internal/freshness/scheduler.go` (T2 threshold ~98-104)
- Test: `internal/freshness/clockstep_test.go` (create)

**Interfaces:**
- Produces: WARN event `clock_step_detected` (fields `stamp`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime`), rate-limited to one per minute per process.

- [ ] **Step 1: Write the failing test**

`internal/freshness/clockstep_test.go`:

```go
package freshness

import (
	"context"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
)

// I-7 / spec C4: a last_check_at stamp in the future (backward clock
// step) must not freeze checks.
func TestCheck_FutureLastCheckAtDoesNotFreeze(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(2 * time.Hour).Unix()
	_ = fc.PutSuiteFreshness(context.Background(), cache.SuiteFreshness{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: "/r/dists/x", LastCheckAt: &future, LastSuccessAt: &future})
	up := &changedUpstream{body: []byte("new")}
	c, err := New(Config{Cache: fc, Fetcher: up, HostLimiter: hostsem.New(8), Cooldown: time.Minute,
		Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), "http", "h", "/r/dists/x")
	if up.calls.Load() != 1 {
		t.Fatalf("check frozen by a future last_check_at")
	}
}

func TestTick_FutureLastSuccessAtIsDue(t *testing.T) {
	fc := newFakeCache()
	stale := testHex64
	fc.putURL(cache.URLPath{CanonicalScheme: "http", CanonicalHost: "h", Path: "/r/dists/x/InRelease",
		BlobHash: &stale, UpstreamURL: "http://h/r/dists/x/InRelease", IsMetadata: true})
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

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run 'FutureLast'`
Expected: FAIL — 0 upstream calls.

- [ ] **Step 3: Implement**

Cooldown gate:

```go
	// Cooldown gate. Spec C4 clock-step rule: a past-event stamp found in
	// the future (backward wall-clock step) is treated as infinitely old,
	// never as "just checked" — otherwise the suite freezes for the size
	// of the step (security finding I-7).
	if cur.LastCheckAt != nil && c.cooldown > 0 {
		last := time.Unix(*cur.LastCheckAt, 0)
		if last.After(now) {
			c.clockStep("last_check_at", scheme, host, suitePath, *cur.LastCheckAt, now)
		} else if now.Sub(last) < c.cooldown {
			return nil, nil
		}
	}
```

T2:

```go
		if s.LastSuccessAt != nil && *s.LastSuccessAt > threshold {
			if *s.LastSuccessAt <= nowUnix {
				continue
			}
			c.clockStep("last_success_at", s.CanonicalScheme, s.CanonicalHost, s.SuitePath, *s.LastSuccessAt, c.now())
		}
```

(with `nowUnix := c.now().Unix()` computed once at the top of `tick`). Add to `freshness.go`:

```go
// clockStepLogEvery rate-limits clock_step_detected to one line per
// minute per process (monotonic).
const clockStepLogEvery = time.Minute

// clockStep records a past-event stamp found in the future.
func (c *Checker) clockStep(stamp, scheme, host, suitePath string, stored int64, now time.Time) {
	clockStepsTotal.Inc(stamp)
	last := c.lastClockStepLog.Load()
	mono := time.Since(processStart)
	if last != 0 && mono-time.Duration(last) < clockStepLogEvery {
		return
	}
	if !c.lastClockStepLog.CompareAndSwap(last, int64(mono)) {
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

var processStart = time.Now()
```

Add `lastClockStepLog atomic.Int64` to `Checker` and a counter in `metrics.go`:

```go
	clockStepsTotal = metrics.NewCounterWithCap(
		"acu_clock_steps_detected_total",
		"Past-event timestamps found in the future (backward wall-clock step), by stamp.",
		metrics.DefaultMaxSeries,
		"stamp",
	)
```

(match the exact constructor used by the neighbouring counters in that file).

- [ ] **Step 4: Run to verify it passes**

Run: `go test -race ./internal/freshness/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/freshness/freshness.go internal/freshness/scheduler.go internal/freshness/metrics.go internal/freshness/clockstep_test.go
git commit -m "$(cat <<'EOF'
Stop backward clock steps from freezing freshness checks (I-7)

A last_check_at or last_success_at stamp in the future made the cooldown
and scheduler gates skip the suite until the clock caught up. Such stamps
are now treated as infinitely old, counted, and logged as
clock_step_detected (rate-limited).

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Handler fetch seam

**Files:**
- Modify: `internal/handler/handler.go` (`Config.Fetch`, `Handler.fetch`, new `Fetcher`)
- Test: `internal/handler/fetch_seam_test.go` (create)

**Interfaces:**
- Produces:

```go
type Fetcher interface {
	HostAllowed(host string) bool
	Fetch(ctx context.Context, target *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error)
}
```

- [ ] **Step 1: Write the failing test**

`internal/handler/fetch_seam_test.go`:

```go
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

var _ Fetcher = (*fetch.Client)(nil)

type memFetcher struct{ body []byte }

func (memFetcher) HostAllowed(string) bool { return true }
func (m memFetcher) Fetch(_ context.Context, _ *fetch.Target, dst fetch.FetchDst) (*fetch.FetchResult, error) {
	if _, err := dst.Write(m.body); err != nil {
		return nil, err
	}
	return &fetch.FetchResult{Status: 200, ContentLength: int64(len(m.body))}, nil
}

// Simulations drive the handler with an in-memory upstream.
func TestHandler_AcceptsInMemoryFetcher(t *testing.T) {
	parser, err := proxy.New(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(context.Background(), t.TempDir(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h, err := New(Config{Parser: parser, Cache: c, Fetch: memFetcher{body: []byte("deb")},
		Logger: silentLogger(), HostLimiter: hostsem.New(4)})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://up.example/pool/x.deb", nil))
	if rec.Code != 200 || rec.Body.String() != "deb" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}
```

(`handler.New` requires exactly `Parser`, `Cache`, `Fetch` and `HostLimiter`; `httptest.NewRequest` sets `RequestURI` to the absolute target, which is the proxy-mode form the parser expects. The default allowlist is irrelevant because `memFetcher.HostAllowed` returns true.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/handler/ -run 'AcceptsInMemoryFetcher'`
Expected: FAIL — compile error: `memFetcher` does not implement `*fetch.Client` / `Fetcher` undefined.

- [ ] **Step 3: Implement**

Add the `Fetcher` interface (with a doc comment: "the subset of *fetch.Client the handler uses; an interface so virtual-clock simulations can drive the handler with an in-memory upstream — the real client does not advance testing/synctest time") and the assertion `var _ Fetcher = (*fetch.Client)(nil)` next to the existing `fetch.FetchDst` assertion. Change `Config.Fetch` and the `Handler.fetch` field types from `*fetch.Client` to `Fetcher`. `cmd/apt-cacher-ultra/main.go` keeps passing `fetchClient` unchanged.

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./... && go test -race ./internal/handler/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/handler/handler.go internal/handler/fetch_seam_test.go
git commit -m "$(cat <<'EOF'
Add a fetcher interface seam to the handler

The handler depends on a small Fetcher interface that *fetch.Client
satisfies, so virtual-clock simulations can drive it with an in-memory
upstream.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Virtual-clock harness and 30-day smoke simulation

**Files:**
- Create: `internal/simtest/simtest.go`, `internal/freshness/sim_smoke_test.go`

**Interfaces:**
- Produces: `simtest.OpenCache(t *testing.T) *cache.Cache` (closes itself before the bubble ends), `simtest.DiscardLogger() *slog.Logger`, `simtest.StaticConditional` (a `freshness.Fetcher` returning a fixed status and counting calls).

- [ ] **Step 1: Write the failing test**

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
)

// Phase A DoD: the real cache, checker and scheduler run for 30 virtual
// days inside a synctest bubble. Asserts liveness (checks keep happening)
// and a bound (no runaway checking).
func TestSimSmoke_30DaysPeriodicRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		up := &simtest.StaticConditional{Status: 304}
		chk, err := New(Config{Cache: c, Fetcher: up, HostLimiter: hostsem.New(8),
			Cooldown: time.Minute, Refresh: 15 * time.Minute, Logger: simtest.DiscardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { chk.Run(runCtx); close(done) }()
		time.Sleep(30 * 24 * time.Hour)
		cancel()
		<-done
		chk.WaitForAdoptions()
		// A suite is due once last_success_at is older than 15m; ticks run
		// every 3m45s, so a check lands every 15m to 18m45s.
		got := up.Calls()
		if lo, hi := int32(30*24*60/19), int32(30*24*60/15)+2; got < lo || got > hi {
			t.Fatalf("checks in 30 virtual days = %d, want [%d, %d]", got, lo, hi)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/freshness/ -run SimSmoke`
Expected: FAIL — package `internal/simtest` does not exist.

- [ ] **Step 3: Implement the harness**

`internal/simtest/simtest.go`:

```go
// Package simtest holds helpers for virtual-clock simulations run inside
// testing/synctest bubbles with the real cache (spec §7). Only tests
// import it.
//
// AIDEV-NOTE: code under simulation must block only on channels and
// timers, never on a sync.Mutex held across a virtual sleep, and must use
// in-memory fakes for network I/O (real sockets do not advance bubble
// time). Backward clock steps use wallclock.SetOffsetForTest.
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

Run: `time go test -race ./internal/freshness/ -run SimSmoke -count=1`
Expected: PASS in well under 5 s of test time. If it hangs, a goroutine in the checker or cache is blocked on something other than time; find it with `GOTRACEBACK=all` and fix it before continuing (that is exactly the class of bug the harness must catch).

- [ ] **Step 5: Commit**

```bash
git add internal/simtest internal/freshness/sim_smoke_test.go
git commit -m "$(cat <<'EOF'
Add virtual-clock simulation harness

internal/simtest runs the real cache inside testing/synctest bubbles.
A 30-day smoke simulation drives the freshness scheduler and asserts
both liveness and a bound on the number of checks.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 13: Phase A documentation, anchors, and final verification

**Files:**
- Modify: `docs/log-fields.md`, `docs/configuration.md`, `docs/superpowers/specs/2026-09-27-flat-repositories-design.md` (deviation note), `docs/superpowers/plans/2026-09-28-flat-repositories-00-overview.md` (phase table)

- [ ] **Step 1: Document new events**

In `docs/log-fields.md` add an "Operational events" subsection (create it if the file only documents the request line) listing:

| Event | Level | Fields | Meaning |
|---|---|---|---|
| `adoption_panicked` | ERROR | `where`, `canonical_host`, `suite_path`, `panic`, `stack` | A panic inside an adoption, repair or reconcile goroutine was recovered; the prior snapshot keeps serving. |
| `freshness_check_panicked` | ERROR | `where` (`check`, `scheduler`, `t1`), `canonical_host`, `suite_path`, `panic`, `stack` | A panic during a freshness check was recovered; the check counts as failed. |
| `clock_step_detected` | WARN | `stamp`, `canonical_scheme`, `canonical_host`, `suite_path`, `stored_unixtime`, `now_unixtime` | A stored timestamp is in the future (the wall clock stepped backwards); the gate treated it as old. Rate-limited to one line per minute. |

- [ ] **Step 2: Document the upstream encoding change**

In `docs/configuration.md`, in the `[upstream]` section's prose, add one paragraph: "Upstream request paths are encoded exactly as apt encodes them (lowercase percent-encoding of `+`, `~`, space, `%`, control and non-ASCII bytes, plus `#` and `?`). Paths containing `+` or `~` therefore reach upstream as `%2b`/`%7e`, as they do when apt fetches directly."

- [ ] **Step 3: Record the trigger deviation in the spec**

In the spec's §6 SQL block replace the `suite_snapshot_anchor_form_required` trigger with the `suite_snapshot_anchor_form_default` trigger from Task 5, and in C5 "Anchor forms and storage" change "a trigger rejects inserts with an empty form" to "a trigger fills an empty form from the hash columns (legacy inserts); production sets it explicitly".

- [ ] **Step 4: Full verification**

Run:

```bash
gofmt -l . ; go vet ./... ; golangci-lint run ./... ; go test -race -count=1 ./...
```

Expected: `gofmt -l .` prints exactly the five files it already prints on `main` (formatter-version drift, not ours: `cmd/apt-cacher-ultra/ca_print.go`, `internal/cache/cache_test.go`, `internal/cache/gc_urlpath_test.go`, `internal/freshness/hot_prefetch_test.go`, `internal/proxy/connect_stats.go`) and no others — do not reformat those five; every file this phase creates or edits must be gofmt-clean. vet and lint clean; all packages PASS.

- [ ] **Step 5: Mark the phase in the overview and commit**

Set phase A's status in the overview table to "done (commit <sha of this commit's parent>)".

```bash
git add docs/
git commit -m "$(cat <<'EOF'
Document phase A events and encoding; record trigger deviation

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
EOF
)"
```

- [ ] **Step 6: Phase review gate**

Dispatch the max-effort review of `git diff main...HEAD` for phase A (correctness, security, robustness lenses) per the overview's execution protocol; fix findings in follow-up commits before writing the phase B plan.

## Phase A Definition of Done

- `TestSchemaV7_MigratesPopulatedV6`, `TestSchemaV7_TriggerFillsFormAndUniqueIndexIsPartial`, `TestNoSQLiteClockFunctionsInProductionCode` pass.
- `TestAptEncodePath_Table`, `TestBuildUpstreamURL_AllBytesRoundTrip`, `TestBuildUpstreamURL_PlainPathsUnchanged`, `TestParse_*` (control bytes, encoded hash, apt encoding) pass.
- `TestSuitePath_Dists`, `TestIsCanonicalSuitePath` pass; the only changed existing expectations are the two bare-suite cases.
- `TestInsertCandidateSnapshot_DerivesAndStoresForm`, `TestInsertCandidateSnapshot_IgnoresRetiredOnCollision` pass.
- `TestRun_NaturalKeyCollisionIsWrapped`, `TestErrAdoptionGuardFailed_KeepsMemberFetchOutcome` pass.
- `TestReparseSnapshotRelease_IgnoresArmorInjection` (R-1) and `TestCheck_AdoptionPanicIsRecovered` / `TestMaybeFireFreshness_RecoversPanic` (I-1) fail on `main` and pass here.
- `TestCheck_FutureLastCheckAtDoesNotFreeze`, `TestTick_FutureLastSuccessAtIsDue` pass.
- `TestSimSmoke_30DaysPeriodicRefresh` passes under `-race`.
- `go test -race ./...`, `go vet ./...`, `golangci-lint run ./...` clean; docs updated; phase review findings resolved.
