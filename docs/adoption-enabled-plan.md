# Plan: tighten `htmlRenderModel.AdoptionEnabled` to match operator intent

## Background

The admin UI redesign added a presentation-only wrapper (`htmlRenderModel`) with
an `AdoptionEnabled` boolean that drives the keyring empty-state branch (ADOPTION
DISABLED vs. NO GPG KEYS LOADED) and the keys-chip `data-state="crit"` decision
on the status bar.

`docs/admin-ui-spec.md` §0.7 specifies the field's semantics with this
implementation hint:

> `AdoptionEnabled    bool    // cfg.Keyring != nil at server build time`

The literal hint is incorrect against production wiring:
`cmd/apt-cacher-ultra/main.go` unconditionally passes
`Keyring: &keyringProvider{k: keyring}` whether `cfg.Adoption.Enabled` is true or
not — only the inner `*gpg.Keyring` pointer is `nil` when adoption is disabled.
A literal `s.cfg.Keyring != nil` check therefore always returns `true` in
production, defeating the field.

The current implementation (iter-2 of the redesign loop) works around this with a
secondary check on the snapshot:

```go
if s.cfg.Keyring != nil && s.cfg.Keyring.KeyringSnapshot() != nil {
    w.AdoptionEnabled = true
}
```

This is a convention-based detector that depends on the cmd adapter returning
`nil` (not an empty slice) when adoption is disabled. Spec §2.3 forbade editing
both `internal/admin/server.go` and `cmd/apt-cacher-ultra/*` within the UI
phase, which is why the snapshot-nil convention was used.

## Options

### Option A — cmd-side guard (recommended)

In `cmd/apt-cacher-ultra/main.go`, conditionally set `Keyring` on the admin
`Config`:

```go
adminCfg := admin.Config{
    // ...other fields...
}
if cfg.Adoption.Enabled {
    adminCfg.Keyring = &keyringProvider{k: keyring}
}
adminSrv, err = admin.New(adminCfg)
```

Then `buildHTMLRenderModel` can revert to the spec's literal hint:

```go
AdoptionEnabled: s.cfg.Keyring != nil,
```

**Pros**
- Smallest delta (≈3 lines in cmd).
- Matches the spec's §0.7 hint literally — no convention required.
- The `admin.KeyringProvider` interface remains unchanged.
- Future adapter authors don't have to know about the nil-snapshot trick.

**Cons**
- Touches `cmd/apt-cacher-ultra/main.go`, which §2.3 forbade for the redesign.
  Resolving this requires either an explicit §2.3 amendment authorizing this
  one-line wiring fix or a follow-up PR landing AFTER the redesign merges.

**Spec amendment needed:**

> §2.3 amendment: `cmd/apt-cacher-ultra/main.go` may be edited solely to wire
> the `admin.Config.Keyring` provider conditionally on `cfg.Adoption.Enabled`,
> for parity with §0.7's `AdoptionEnabled` hint. No other cmd changes.

### Option B — explicit admin.Config field

Add a new field to `admin.Config` (in `internal/admin/server.go`):

```go
type Config struct {
    // ...
    // AdoptionEnabled mirrors the operator's [adoption].enabled
    // setting onto the admin status page's keyring empty-state
    // branch. cmd passes the boolean directly; admin renders the
    // empty-state heading and keys-chip crit tinting from it.
    AdoptionEnabled bool
}
```

cmd sets `AdoptionEnabled: cfg.Adoption.Enabled` alongside `Keyring`.
`buildHTMLRenderModel` reads it directly.

**Pros**
- Most explicit; the boolean is right there on Config, no provider gymnastics.
- The Keyring provider can stay always-passed (current behavior).

**Cons**
- Touches both `internal/admin/server.go` AND `cmd/*` (§2.3 doubly forbidden).
- Slight scope creep: a Config field added solely for the UI.

**Spec amendment needed:** Same scope as Option A, plus a §0.7 wrapper update
to point at `s.cfg.AdoptionEnabled` instead of `s.cfg.Keyring != nil`.

### Option C — interface method on KeyringProvider

Add `AdoptionEnabled() bool` to the `admin.KeyringProvider` interface; the cmd
adapter implements it from `cfg.Adoption.Enabled`.

**Pros**
- The interface gains a clear contract.

**Cons**
- `AdoptionEnabled` is semantically odd on a `KeyringProvider` (one bool that
  has nothing to do with keyring inventory).
- Touches `internal/admin/server.go` AND `cmd/*`.
- Two-method interface is harder to mock; tests have to grow.

### Option D — keep current convention, document formally

Leave the snapshot-nil convention in place. Add explicit `// CONTRACT:` notes
to:

- The `KeyringProvider` interface in `server.go` documenting that
  `KeyringSnapshot()` MUST return `nil` (not an empty slice) when adoption is
  disabled.
- A test in `cmd/apt-cacher-ultra/main_test.go` (or equivalent) that exercises
  both adoption-enabled and adoption-disabled startup paths and asserts the
  resulting `admin.Server.cfg.Keyring.KeyringSnapshot()` returns nil
  vs. non-nil appropriately.

**Pros**
- Zero §2.3 violations.
- Already implemented; only documentation + a contract test needed.

**Cons**
- Contract by convention is fragile — a future adapter author could return
  `[]admin.KeyringEntrySnapshot{}` instead of `nil` and silently break the
  empty-state branching for adoption-disabled deployments.
- The §0.7 hint stays misleading without an amendment that documents the
  convention.

## Recommendation

**Option A** plus a focused §2.3 amendment.

Rationale:
- The cmd one-line change is the smallest, cleanest fix.
- It matches the spec's own §0.7 hint without modification.
- It removes the snapshot-nil convention from the implementation, which is
  the most fragile part of the current state.
- The §2.3 amendment is narrow (one file, one boolean condition) and clearly
  scoped to "UI parity", not unrelated cmd work.

## Execution checklist

1. Amend `docs/admin-ui-spec.md` §2.3 to permit the single cmd edit (one paragraph,
   already drafted above).
2. Edit `cmd/apt-cacher-ultra/main.go` lines around 679: only set
   `Keyring: &keyringProvider{k: keyring}` inside an `if cfg.Adoption.Enabled`
   block. Default the field to `nil`.
3. Revert `internal/admin/status.go`'s `buildHTMLRenderModel` to:
   ```go
   AdoptionEnabled: s.cfg.Keyring != nil,
   ```
   Drop the snapshot-nil check.
4. Update the AIDEV-NOTE above `buildHTMLRenderModel` to remove the convention
   explanation and cite the literal §0.7 hint.
5. Update `TestBuildHTMLRenderModelAdoptionDetection` to exercise the new
   semantics: `Keyring == nil` → `AdoptionEnabled == false`, `Keyring != nil`
   → `true`. The existing fake provider can stay; the test just collapses
   the four cases into two (nil provider, non-nil provider).
6. Update `cmd/apt-cacher-ultra/main_test.go` (or add a new test) that
   start-up parameters with `Adoption.Enabled = false` produce
   `adminSrv.cfg.Keyring == nil`.
7. Remove the `## Spec issues` entry for §0.7 in `.phase-loop-notes.md`.

## Estimated effort

- Spec amendment: 5 minutes
- cmd change: 5 minutes
- admin change (revert): 5 minutes
- Tests: 10 minutes
- /codex-review pass: 5 minutes

Total: ~30 minutes including review.

## Risks

- Tests that rely on the always-non-nil Keyring provider behavior would need
  to be checked, but a grep of `internal/admin/admin_test.go` shows
  `cfg.Keyring` is only set by `func(cfg *Config) { cfg.Keyring = provider }`
  callers, and `startAdminServer` defaults `Keyring: nil`. So existing tests
  already exercise the "no keyring" path; the cmd change brings production
  in line with what the tests assume.
- The `keyringProvider` adapter's defensive `if p == nil || p.k == nil` check
  in `cmd/apt-cacher-ultra/main.go:978` becomes partial dead code under the
  new convention (provider is never created with `nil` k). Keep it for
  defense-in-depth; it costs nothing.
