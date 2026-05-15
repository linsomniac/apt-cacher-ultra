# apt-cacher-ultra — Admin UI Redesign Specification

This document specifies the implementation contract for the read-only admin
status page redesign served at `:6789/`. It is a UI/UX delta over
[SPEC5.md](../SPEC5.md) §10.5. The companion design plan at
[admin-ui-redesign-plan.md](admin-ui-redesign-plan.md) records the rationale
and the visual case; this spec records the *what to build* and *how to verify
it landed correctly*. The static HTML mockup at
[admin-ui-mockup.html](admin-ui-mockup.html) is the visual ground truth — when
this spec and the mockup disagree, the mockup wins.

The redesign is **read-only**, **server-rendered HTML with progressive
JavaScript enhancement**, and has **zero external dependencies** (no CDN, no
npm, no webfonts, no framework). Operators run this in air-gapped
environments; any asset that is not inline does not exist for them.

---

## 0. Locked contracts (must not regress)

The redesign is a presentation change. The following surfaces are unchanged
and any deviation is a regression:

1. **JSON contract.** `GET /?format=json` and `GET /` with
   `Accept: application/json` return the identical payload they return today.
   No new top-level keys, no renamed keys, no changed types. The schema is
   defined in SPEC5 §10.4 plus the additively-locked `keyring[]` from commit
   e5b6024 (`primary_fingerprint`, `primary_uid`, `source_path`,
   `subkey_fingerprints`).
2. **Endpoint set.** `/`, `/healthz`, `/metrics` continue to exist with their
   current method/status-code behavior. No new endpoints land in this phase.
3. **Content negotiation.** `?format=json` and `Accept: application/json` HTML
   negotiation in `internal/admin/handlers.go` is untouched.
4. **Go data model (JSON path).** The `statusModel` struct in
   `internal/admin/status.go` is unchanged in shape. JSON rendering
   marshals `statusModel` directly via `json.Encode`. No new top-level
   fields, no renamed fields, no changed types.
5. **Refresh cadence.** A `<meta http-equiv="refresh" content="60">` is
   present at top of `<head>`. The current template lacks one — P0
   adds it. No XHR/SSE/WebSocket polling is introduced.
6. **Goroutine surface.** No new goroutines, no new background workers, no
   new caches. The admin server's hot path is unchanged.
7. **HTML render wrapper (additive, HTML-only).** The HTML template
   path composes a small `htmlRenderModel` struct that embeds
   `statusModel` (so every existing field is reachable verbatim) and
   adds presentation-only fields the template needs but the JSON
   contract does not expose:

   ```go
   type htmlRenderModel struct {
       statusModel       // embedded; every JSON field reachable as before
       AdoptionEnabled    bool    // cfg.Keyring != nil at server build time
       GCIntervalSeconds  float64 // cfg.GC's run interval (for "stale" threshold)
   }
   ```

   The wrapper is constructed inside `renderStatus()` immediately before
   `statusHTMLTemplate.Execute` and is never touched by the JSON path.
   `template.Execute` receives the wrapper; the template references
   `statusModel`'s fields through promotion (`.Cache.BytesUsed`, etc.)
   and the new fields directly (`.AdoptionEnabled`, `.GCIntervalSeconds`).

   This is *not* a relaxation of item 4 — the JSON path is unchanged.
   The wrapper exists strictly so the template has the operator-facing
   context it needs without polluting the wire contract.

If any item above must change, this spec is amended before code lands.

---

## 1. Goals & non-goals

### 1.1 Goals

1. **One-glance health.** The operator's first question — *is the cache
   doing its job?* — is answered above the fold by a status verdict pill
   (HEALTHY / WATCHING / DEGRADED / WARMING UP) plus five vital-signs
   cells. The verdict is reinforced by row-level state stripes throughout
   the page.
2. **Buried failures surfaced.** When ≥10% of the recent-adoptions ring
   buffer shares a non-success outcome, an aggregate-notice strip renders
   above the table naming the outcome, count, most-recent example, and a
   remediation hint. For the `gpg_failed` case the hint deep-links to the
   Keyring section as the natural next diagnostic.
3. **Keyring is a first-class section.** Promoted from a buried table
   between Suites and GC to an IA peer of Recent Adoptions. Source
   attribution (bundled / system / custom) and fingerprint chunking are
   uniform with the rest of the page.
4. **Density without fatigue.** Tabular numerals, monospace for paths
   and hashes, near-monochrome paper-ink canvas, single accent color.
   Tables are the hero; chartjunk is forbidden.
5. **JS-optional.** Without JS, every section is reachable, every datum
   visible, every state legible from the textual badges. JS layers in
   the verdict pill, the aggregate notice, the theme toggle, the sticky
   rail highlight, and the help popovers — additive enhancements only.
6. **Dark mode that is not an inversion.** A separate composition with
   reduced-saturation semantic colors and a lightness-flipped accent.
   Follows OS preference by default; user toggle persists to
   `localStorage`.
7. **Mobile-usable for on-call.** A phone visitor (SRE after a page)
   sees the verdict and aggregate notice first, with tables reflowing
   into card-style list views via CSS only.
8. **Accessibility AA.** WCAG AA on all body text; state is never
   conveyed by color alone; visible 2px focus rings; `prefers-reduced-motion`
   honoured.

### 1.2 Non-goals (deferred)

1. **Mutating actions.** No buttons, no forms, no `POST`/`DELETE`. The
   page stays read-only in this phase. (Phase 7 control-plane work,
   per SPEC.)
2. **Live partial updates.** No XHR cell refresh, no SSE, no
   WebSocket. Meta-refresh remains the refresh mechanism.
3. **Charts and sparklines.** No time-series rendering. If P2 adds
   any visual data summary, it is a numeric companion only; sparklines
   are decoration without operator value at this density.
4. **Per-suite per-host search APIs.** Client-side filter/sort over the
   already-rendered DOM is in P2 scope; server-side filtering is out
   of scope.
5. **Internationalization.** The page is English; timestamps localize
   to the browser via the existing JS path. No new locale infrastructure.
6. **Authentication.** The admin port stays unauthenticated and bound
   to the operator-chosen interface. Operators control access by
   network position.

### 1.3 Resolved during scoping

- **Keyring placement.** Resolved: IA position 3, peer of Recent
  Adoptions. Rationale: the `gpg_failed` → "what keys do I have?"
  gradient. See plan §3.
- **Keyring vital cell?** Resolved: no. Instead, a small `keys N ✓`
  meta chip in the status bar (see §5.1.1), anchor-linked to
  `#keyring`. Tints `--crit` when adoption is enabled and the
  array is empty.
- **Fingerprint chunking.** Resolved: groups of 4 hex digits, unified
  rule for SHA-1 (40 hex → 10 groups) and SHA-256 (64 hex → 16
  groups). Single Go template helper, `chunkHex(s, 4)`. Applies to
  both the TLS MITM CA fingerprint and the keyring primary/subkey
  fingerprints.
- **Aggregate-notice threshold.** Resolved: 10% of the recent-adoptions
  ring buffer with the dominant non-success outcome. Threshold lives
  in one place in the inline JS and is trivially tunable (see §8.2).
- **Source taxonomy.** Resolved: three-way split based on `source_path`
  prefix —
  - `embedded:` → `[BUNDLED]`
  - prefix `/usr/share/` → `[SYSTEM]`
  - everything else (typically `/etc/apt/keyrings/`) → `[CUSTOM]`
  Done in a Go template helper, `sourceKind(path)`. Returns a string
  the template emits as a class and as `data-source-kind` attribute.

---

## 2. File and surface inventory

### 2.1 Files modified

| Path                                  | Change                                                  |
|---------------------------------------|---------------------------------------------------------|
| `internal/admin/status.go`            | HTML template rewrite; new helper funcs in `funcMap`; `htmlRenderModel` wrapper struct; `renderStatus` composes the wrapper for the HTML branch (JSON branch unchanged) |
| `internal/admin/status_test.go`       | Golden-output tests for new template + helper unit tests |
| `internal/admin/handlers.go`          | No changes (verify in §14)                              |
| `internal/admin/server.go`            | No changes (verify in §14)                              |

### 2.2 Files added

None. The CSS, JS, and SVG sprite live inline in the existing template
constant in `internal/admin/status.go`. The mockup
(`docs/admin-ui-mockup.html`) is documentation only; it does not ship.

### 2.3 Files explicitly NOT touched

- `internal/admin/server.go` (provider plumbing, snapshot wiring)
- `internal/freshness/*` (adoption pipeline)
- `internal/cache/*`, `internal/storage/*` (storage)
- `internal/handler/*` (proxy hot path)
- `cmd/apt-cacher-ultra/*` (entry point)
- `packaging/config/config.toml.default` (no new config keys)

Any PR touching these in the course of the admin-UI work is a scope
violation and is rejected.

---

## 3. Visual system tokens

All tokens live in the inline `<style>` block at the top of the rendered
HTML, declared as CSS custom properties on `:root` (light) and on
`[data-theme="dark"]` (dark explicit override). The
`@media (prefers-color-scheme: dark)` media query applies the dark tokens
as the default for OS-dark users; the `[data-theme="..."]` attribute
written by the toggle JS overrides the media query.

### 3.1 Light palette

| Token       | Hex      | Use                                              |
|-------------|----------|--------------------------------------------------|
| `--ink-0`   | `#FAFAF7`| Page background                                  |
| `--ink-1`   | `#F2F1EC`| Panel background, table header band              |
| `--ink-2`   | `#E5E3DA`| Divider rules, table borders                     |
| `--ink-3`   | `#C7C4B8`| Disabled text                                    |
| `--ink-4`   | `#7A7669`| Secondary text, muted labels                     |
| `--ink-5`   | `#3A3833`| Body text                                        |
| `--ink-6`   | `#1A1815`| Headings, primary numerals                       |
| `--accent`  | `#7A2E0A`| Brand mark, link color, `[CUSTOM]` source badge  |
| `--ok`      | `#2E5D3A`| Healthy state                                    |
| `--warn`    | `#A36410`| Watching state                                   |
| `--crit`    | `#9A1F1B`| Critical state                                   |
| `--stale`   | `#5C5A52`| Stale / no-data state                            |

### 3.2 Dark palette

| Token       | Hex      | Use                                              |
|-------------|----------|--------------------------------------------------|
| `--ink-0`   | `#15171A`| Page background                                  |
| `--ink-1`   | `#1B1E22`| Panel background                                 |
| `--ink-2`   | `#2A2D32`| Divider rules                                    |
| `--ink-3`   | `#4A4D54`| Disabled text                                    |
| `--ink-4`   | `#8E8F94`| Muted labels                                     |
| `--ink-5`   | `#C5C6CB`| Body text                                        |
| `--ink-6`   | `#EDEEF1`| Headings, primary numerals                       |
| `--accent`  | `#E08B5A`| Lightness-flipped brick (same hue family)        |
| `--ok`      | `#7FB68E`| Sage                                             |
| `--warn`    | `#E0B05A`| Amber                                            |
| `--crit`    | `#E7716E`| Coral                                            |
| `--stale`   | `#7A7C82`| Cool grey                                        |

Both palettes are verified to pass WCAG AA at the body-text level
(≥4.5:1 against `--ink-0`). The verification matrix is recorded in §10.

### 3.3 Type scale

| Token  | Size  | Use                                                |
|--------|-------|----------------------------------------------------|
| `xs`   | 11px  | Eyebrows, table column headers, footnote           |
| `sm`   | 12.5px| Table body, label/value pairs                      |
| `base` | 14px  | Body prose, section descriptions                   |
| `md`   | 16px  | Vital-cell labels                                  |
| `lg`   | 20px  | Vital-cell values, section h2                      |
| `xl`   | 28px  | Hero count (cache size)                            |
| `2xl`  | 36px  | Status verdict pill                                |

Font stacks:

- **Sans (headings, body):** `"IBM Plex Sans", ui-sans-serif, system-ui,
  -apple-system, "Segoe UI", sans-serif`
- **Mono (data, paths, hashes, IDs):** `"JetBrains Mono", "IBM Plex Mono",
  ui-monospace, "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace`

`font-variant-numeric: tabular-nums` is set on the table-root selector
(not per cell) and on the `.fp` class (fingerprint chunking).

### 3.4 Spacing scale

4px base, geometric: `4 / 8 / 12 / 16 / 24 / 32 / 48 / 64 / 96`. Table
cell padding is `8px 14px`. Vertical rhythm between detail panels is
`48px`. Never `5px / 7px / 10px`.

### 3.5 Radii and shadow

- `border-radius: 0` on tables and panels
- `border-radius: 2px` on inline pills/badges
- `border-radius: 999px` (full-pill) on the verdict pill only
- `box-shadow` is forbidden except for:
  - `box-shadow: 0 0 0 2px var(--accent)` as the focus ring
  - `box-shadow: inset 3px 0 0 var(--state-color)` for row state stripes
  - `box-shadow: inset 4px 0 0 var(--state-color)` for vital-cell state
    stripes

### 3.6 Motion

- 600ms first-paint reveal (opacity + translateY 4px) on the status bar only
- 120ms ease on `<tr>` hover background tint
- 120ms ease on `<details>` expand/collapse
- All other transitions disabled
- `@media (prefers-reduced-motion: reduce)` zeros every transition

---

## 4. Template structure

### 4.1 Document outline

The body is structured as:

```
<body>
  <svg sprite>                   <!-- inline icon defs -->
  <header class="status-bar">    <!-- Zone 1: always visible -->
  <section class="vitals">       <!-- Zone 2: hero strip, 5 cells -->
  <main class="content">
    <nav class="rail">           <!-- sticky left rail, anchor links -->
    <div class="detail-panels">  <!-- Zone 3: ordered detail sections -->
      <section id="suites">
      <section id="adoptions">    <!-- Recent adoptions -->
      <section id="keyring">      <!-- promoted from Plumbing -->
      <section id="hot-paths">
      <section id="cache-by-host-arch">
      <section id="repo-coverage">
      <section id="gc">
      <section id="active-hosts">
      <section id="plumbing">     <!-- listeners, TLS MITM, build info -->
    </div>
  </main>
  <footer class="meta-foot">     <!-- /metrics, /healthz, build, go version -->
  <script>                       <!-- inline vanilla JS -->
</body>
```

### 4.2 Section order rationale (per plan §3)

| # | Section ID              | Why this position                             |
|---|-------------------------|-----------------------------------------------|
| 1 | `#suites`               | Densest table, carries the lag signal         |
| 2 | `#adoptions`            | Carries the failure signal                    |
| 3 | `#keyring`              | First diagnostic from `gpg_failed`            |
| 4 | `#hot-paths`            | Cache-effectiveness check                     |
| 5 | `#cache-by-host-arch`   | Capacity / arch-population check              |
| 6 | `#repo-coverage`        | Plumbing-adjacent but still data-rich         |
| 7 | `#gc`                   | Reclamation health                            |
| 8 | `#active-hosts`         | Slot-pressure debug                           |
| 9 | `#plumbing`             | Listeners, TLS MITM, build info               |

### 4.3 Sticky bar / sticky header offsets

- Status bar: `position: sticky; top: 0; height: 64px;`
- Table headers: `position: sticky; top: 64px;`
- Anchor targets: `scroll-margin-top: 72px` so anchor jumps clear
  the status bar

### 4.4 Sticky rail

Renders as `<nav class="rail"><ul><li><a href="#suites">Suites</a>…`. At
viewport ≥1280px it sits left-side, 200px wide, sticky from the top of
the content area. Between 1024–1279px it collapses into a horizontal
anchor row that sticks below the status bar. Below 1024px it disappears
and detail panels become `<details>` (default-closed except non-healthy).
With JS enabled, an `IntersectionObserver` highlights the rail entry
corresponding to the section currently in view.

---

## 5. Component contracts

### 5.1 Status bar (Zone 1)

A single-row band, height 64px, full-width, contains in order:

```
[ « ] apt-cacher-ultra <version>   [● VERDICT] <one-line explanation>   keys <N> ✓  [☀/☾]  [json↗]
```

- The `«` glyph is the brand monogram, color `--accent`, font-weight 600.
- `<version>` is `BuildInfo.Version` or `"dev"` if empty.
- The verdict pill is described in §5.1.2.
- The one-line explanation is generated client-side (§8.1) and is empty
  with JS disabled.
- The `keys N ✓` chip is described in §5.1.1.
- The theme toggle is a 24px sun/moon SVG button (§8.3).
- `json↗` is a real anchor to `/?format=json` with the
  diagonal-arrow inline-SVG icon.

The status bar renders WITHOUT JavaScript with the pill in a neutral
`STATUS` state and the keys chip and explanation absent.

#### 5.1.1 Keys chip

Inline `<span class="keys-chip">` in the status-bar meta area, anchored
to `#keyring`:

```html
<a href="#keyring" class="keys-chip"
   data-keyring-count="{{ len .Keyring }}"
   data-adoption-enabled="{{ .AdoptionEnabled }}"
   aria-label="{{ len .Keyring }} GPG keys loaded; jump to Keyring section">
  keys {{ len .Keyring }}
  <span class="keys-chip__mark">{{ if gt (len .Keyring) 0 }}✓{{ else }}!{{ end }}</span>
</a>
```

Tinting rules:

- `len(Keyring) > 0` → `.keys-chip--ok` (text `--ok`, check mark)
- `len(Keyring) == 0 && AdoptionEnabled` → `.keys-chip--crit` (text
  `--crit`, exclamation)
- `len(Keyring) == 0 && !AdoptionEnabled` → `.keys-chip--stale` (text
  `--stale`, dash mark)

`.AdoptionEnabled` is read from the `htmlRenderModel` wrapper (§0.7).
The wrapper sets it from `cfg.Keyring != nil` at server construction —
the same signal cmd already uses to decide whether to install a
`KeyringProvider`. The JSON path is unaffected; this field exists only
on the HTML render context.

#### 5.1.2 Verdict pill

A 44px-tall capsule, `font-weight: 600`, uppercase,
`letter-spacing: 0.04em`, with a 12px filled-circle leading dot in the
state color. Computed client-side from `data-state` attributes scattered
through the rendered DOM (§7.1). Algorithm in §9.

Without JS, the pill renders as:

```html
<span class="verdict verdict--init">● STATUS</span>
```

…with `color: var(--stale)` and a tiny `<small>` footnote: "live verdict
requires JS; scroll for details."

### 5.2 Vital-signs cells (Zone 2)

Five cells in a CSS grid: `grid-template-columns: repeat(5, minmax(0, 1fr))`.
Each cell:

```html
<div class="vital" data-state="{ok|warn|crit|stale}">
  <span class="vital__eyebrow">CACHE</span>
  <span class="vital__value">72.4 GiB</span>
  <span class="vital__sub">4,193 blobs</span>
  <span class="vital__hint">Zero-refcount 1,550 ↑</span>
</div>
```

The state stripe is `box-shadow: inset 4px 0 0 var(--state-color)` on
`.vital`, driven by the `data-state` attribute via CSS attribute selector:

```css
.vital[data-state="warn"]  { box-shadow: inset 4px 0 0 var(--warn); }
.vital[data-state="crit"]  { box-shadow: inset 4px 0 0 var(--crit); }
.vital[data-state="stale"] { box-shadow: inset 4px 0 0 var(--stale); }
.vital[data-state="ok"]    { box-shadow: inset 4px 0 0 transparent; }
```

The five cells (left → right):

| Cell | Eyebrow         | Value source                                | Sub source                          | Hint source                                  |
|------|-----------------|---------------------------------------------|-------------------------------------|----------------------------------------------|
| 1    | CACHE           | `formatBytes .Cache.BytesUsed`              | `.Cache.BlobCount` + " blobs"       | `.Cache.ZeroRefcountBacklog` (when > 0)      |
| 2    | SUITES          | `len .Suites`                               | lagging count (suites with `.Lagging != ""`) | stale count (lagging > 24h)         |
| 3    | ADOPTIONS       | success-count / `len .RecentAdoptions`      | avg `.DurationSeconds`              | most-recent non-success `.Outcome`           |
| 4    | GC              | `formatShortDuration .GC.LastRunDurationSeconds` | `durationOf` since `.GC.LastRunUnixTime` | `formatBytes .GC.LastRunBytesReclaimed` |
| 5    | ACTIVE          | `len .ActiveHosts`                          | "idle" / "busy"                     | max `.SlotCapacity`                          |

Each cell's `data-state` is computed server-side at template render time
using helper `vitalState(...)` (§6.2) — the per-cell threshold logic
lives in Go, not in the template literal.

Responsive collapse:

- ≤1100px viewport: `repeat(3, 1fr)` row 1, `repeat(2, 1fr)` row 2
- ≤700px: single column

### 5.3 Dense tables

Container is a `<section class="panel">`. Each table has:

```html
<table class="data data--{kind}">
  <thead>
    <tr><th>HOST</th><th>SUITE PATH</th>…</tr>
  </thead>
  <tbody>
    <tr data-state="..." [data-outcome="..."]>
      <td data-label="HOST">archive.ubuntu.com</td>
      <td data-label="SUITE PATH">/ubuntu/dists/noble</td>
      …
    </tr>
  </tbody>
</table>
```

Mandatory styling:

- `font-variant-numeric: tabular-nums` on `.data`
- Header row 32px tall, `--ink-1` background, eyebrow type
- Body rows 32px tall, 14px text
- Zebra: odd `--ink-0`, even `rgba(0,0,0,0.02)` / dark
  `rgba(255,255,255,0.02)`; suppressed on `tr[data-state]:not([data-state="ok"])`
- No horizontal cell borders, no vertical separators
- Row state stripe: `tr[data-state="warn"] > td:first-child {
  box-shadow: inset 3px 0 0 var(--warn); }` (and `crit`, `stale`)
- Numeric columns right-aligned; mono; bytes formatted as
  "72.4 GiB" with a non-breaking space; counts with comma separators
- `<td data-label="...">` on every cell so the mobile list-view CSS
  works without JS

### 5.4 Column-header help affordance

Replaces the current `title="..."` tooltip on the Suites "Adopted at"
header. Audit every column header during P0 implementation; apply this
pattern wherever a `title=` is present today.

Markup (a single Go template partial, `tableHeaderHint`, used everywhere):

```html
<th>
  <span class="th-label">ADOPTED AT</span>
  <details class="th-hint">
    <summary aria-label="What this column means" tabindex="0">
      <svg class="i-info" aria-hidden="true"><use href="#i-info"/></svg>
    </summary>
    <div class="th-hint__popover" role="note">
      Adoption fires only on observed InRelease change.
      A suite whose upstream has not republished since startup
      stays empty here without being broken.
    </div>
  </details>
</th>
```

`<details>` is keyboard-accessible by spec. Without JS the popover still
opens inline. With JS, a small handler closes any open `<details>` on
outside-click and on `Escape` (§8.4).

### 5.5 Status badges

`.b` is the base. Variants are stroke-only (no fill) except where noted:

```css
.b {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 2px;
  border: 1px solid currentColor;
  font-size: var(--xs);
  letter-spacing: 0.04em;
  font-weight: 500;
  text-transform: uppercase;
  background: transparent;
}
.b--ok      { color: var(--ok); }
.b--warn    { color: var(--warn); }
.b--crit    { color: var(--crit); }
.b--stale   { color: var(--stale); }
.b--neutral { color: var(--ink-4); border-color: var(--ink-3); }
```

Used in:
- Recent-adoptions outcome column (one of: `success`, `gpg_failed`,
  `fetch_failed`, `parse_failed`, etc.)
- Lagging annotation on suites
- GC deadline-reached cell
- TLS MITM enabled/disabled state
- Keyring source attribution (§5.6)

The badge for an outcome maps via helper `outcomeBadgeClass(s)` (§6.2):
`success` → `.b--ok`, others → `.b--crit` (or `.b--warn` for the few
soft-failure modes the operator has classified that way; implementer
chooses based on the existing outcome enum and notes the mapping inline).

### 5.6 Keyring section

Markup contract for `#keyring`:

```html
<section class="panel" id="keyring">
  <header class="panel__header">
    <h2 class="eyebrow">
      Keyring <span class="sep">—</span>
      <span data-keyring-total>{{ len .Keyring }} loaded</span> ·
      <span data-keyring-bundled>{{ countBundled .Keyring }} bundled</span> ·
      <span data-keyring-system>{{ countSystem .Keyring }} system</span> ·
      <span data-keyring-custom>{{ countCustom .Keyring }} custom</span>
    </h2>
    <p class="panel__desc">
      Bundled keys ship with the binary; system keys come from
      <code>/usr/share/keyrings</code>; custom keys come from operator
      <code>keyring_dirs</code> paths.
    </p>
  </header>
  {{ if .Keyring }}
  <table class="data data--keyring">
    <thead><tr>
      <th>PRIMARY FINGERPRINT</th>
      <th>USER ID</th>
      <th>SOURCE</th>
      <th>SUBKEY FINGERPRINTS</th>
    </tr></thead>
    <tbody>
      {{ range .Keyring }}
      <tr data-source-kind="{{ sourceKind .SourcePath }}">
        <td data-label="PRIMARY FINGERPRINT">
          <code class="fp">{{ chunkHex .PrimaryFingerprint 4 }}</code>
        </td>
        <td data-label="USER ID">{{ .PrimaryUID }}</td>
        <td data-label="SOURCE">
          <span class="b b--neutral src src--{{ sourceKind .SourcePath }}">
            {{ sourceKindLabel .SourcePath }}
          </span>
          <span class="src-path">{{ .SourcePath }}</span>
        </td>
        <td data-label="SUBKEYS">
          {{ if .SubkeyFingerprints }}
            {{ range .SubkeyFingerprints }}
              <code class="fp fp--sub">{{ chunkHex . 4 }}</code><br>
            {{ end }}
          {{ else }}
            <span class="muted">—</span>
          {{ end }}
        </td>
      </tr>
      {{ end }}
    </tbody>
  </table>
  {{ else }}
    {{ template "keyringEmpty" . }}
  {{ end }}
</section>
```

Source-badge classes:

```css
.src--bundled,
.src--system  { color: var(--ink-4); border-color: var(--ink-3); }
.src--custom  { color: var(--accent); border-color: var(--accent); }
```

The `[CUSTOM]` badge gets the brick accent specifically — the operator's
own keys deserve visible authorship.

Fingerprint chunking is per §5.4-of-the-plan: 4-hex groups, normal
space separator inside a `.fp` cell that carries `font-variant-numeric:
tabular-nums` and mono font. The `.fp--sub` variant is 11px instead
of 12px so subkey lists read as a hierarchy below the primary.

`{{ template "keyringEmpty" . }}` is defined as:

```html
{{ define "keyringEmpty" }}
{{ if .AdoptionEnabled }}
  <div class="empty empty--crit" role="status" aria-live="polite">
    <span class="empty__eyebrow">NO GPG KEYS LOADED</span>
    <p class="empty__body">
      Adoption is enabled but no keyring entries are present.
      This is a configuration error — every adoption will fail
      with <code>gpg_failed</code>.
    </p>
  </div>
{{ else }}
  <div class="empty empty--stale">
    <span class="empty__eyebrow">ADOPTION DISABLED</span>
    <p class="empty__body">
      Keyring is not loaded because <code>[adoption].enabled = false</code>.
      Enable adoption to bundle the default Ubuntu/Debian archive keys.
    </p>
  </div>
{{ end }}
{{ end }}
```

### 5.7 Aggregate-failure notice

Rendered above the Recent Adoptions table when JS detects that ≥10% of
the table's `<tr data-outcome="...">` rows share a non-success outcome.
The notice is created by JS into a placeholder `<div id="adoptions-notice"
class="notice-mount"></div>` server-rendered between the section
eyebrow and the table:

```html
<div class="notice notice--crit" role="status">
  <p class="notice__headline">
    <span class="notice__dot">●</span>
    <span data-notice-count>38</span> of
    <span data-notice-total>50</span> recent adoptions failed:
    <span data-notice-outcome>gpg_failed</span>
  </p>
  <p class="notice__detail">
    Most recent: <code data-notice-recent>download.docker.com/dists/noble</code>
    — <span data-notice-recent-when>14 minutes ago</span>
  </p>
  <p class="notice__hint">
    Likely cause: upstream repository key changed or the matching
    archive key isn't loaded. Cross-check the Keyring section below.
  </p>
  <p class="notice__link-row">
    Trusted keys <a href="#keyring">→ Keyring</a>
  </p>
</div>
```

The `notice__link-row` is conditional: it renders only when
`outcome === "gpg_failed"`. For other outcomes the JS substitutes an
outcome-appropriate hint and omits the link row.

Without JS, the mount stays empty. The operator sees the table with
its per-row stroke badges and resolves the pattern by eye — the
notice is enhancement.

### 5.8 Section eyebrows

Every detail section's `h2` is preceded by an eyebrow line:

```
SUITES — 47 TRACKED · 3 LAGGING · 1 STALE
```

Eyebrow type (`xs`, `--ink-4`, uppercase, letter-spacing 0.08em). The
counts are coloured inline in their state color when non-healthy. The
count generation is server-side via template helpers (the data is in the
model); JS does not need to touch eyebrows.

### 5.9 Empty states

A two-line block in the panel body, height 96px, centered:

```html
<div class="empty empty--{stale|crit}" role="status">
  <span class="empty__eyebrow">NO SUITES TRACKED YET</span>
  <p class="empty__body">Adoption begins when a client requests an
  InRelease through the proxy.</p>
</div>
```

`.empty` carries the 4px left stripe when state is non-stale (the
keyring critical-empty case is the principal example).

Per-section copy:

| Section          | Eyebrow                  | Body                                                                                                  |
|------------------|--------------------------|-------------------------------------------------------------------------------------------------------|
| Suites           | NO SUITES TRACKED YET    | Adoption begins when a client requests an InRelease through the proxy.                                |
| Adoptions        | RING BUFFER EMPTY        | Process started {uptime} ago.                                                                         |
| Keyring (disabled) | ADOPTION DISABLED      | Keyring is not loaded because `[adoption].enabled = false`. Enable adoption to bundle default keys.   |
| Keyring (crit)   | NO GPG KEYS LOADED       | Adoption is enabled but no keyring entries are present. Every adoption will fail with `gpg_failed`.   |
| Hot paths        | NO URL PATHS REQUESTED   | No URL paths have been requested yet.                                                                 |
| Active hosts     | NO ACTIVE SLOTS          | No hosts have held a fetch slot since process start. Slot usage is bursty — this is normal.           |
| GC               | NO GC RUN YET            | First periodic run occurs ~{interval} after process start.                                            |

---

## 6. Template helpers (Go)

All helpers live in the existing `template.FuncMap` constructed in
`internal/admin/status.go`. Each is a pure function over its inputs;
none touch package-level state or perform I/O.

### 6.1 New helpers

| Function                | Signature                                          | Behavior                                                                                       |
|-------------------------|----------------------------------------------------|------------------------------------------------------------------------------------------------|
| `chunkHex`              | `func(s string, n int) string`                     | Lowercases input; groups into `n`-hex chunks separated by ASCII space. Non-hex input returned verbatim. |
| `sourceKind`            | `func(path string) string`                         | Returns `"bundled"`, `"system"`, or `"custom"` from prefix rules below.                       |
| `sourceKindLabel`       | `func(path string) string`                         | Returns uppercase label `"BUNDLED"`, `"SYSTEM"`, `"CUSTOM"` for the badge.                    |
| `countBundled`          | `func(ks []keyringEntry) int`                      | Counts rows where `sourceKind(.SourcePath) == "bundled"`.                                     |
| `countSystem`           | `func(ks []keyringEntry) int`                      | Counts rows where `sourceKind(.SourcePath) == "system"`.                                      |
| `countCustom`           | `func(ks []keyringEntry) int`                      | Counts rows where `sourceKind(.SourcePath) == "custom"`.                                      |
| `formatShortDuration`   | `func(seconds float64) string`                     | `< 1` → `"%d ms"`; `< 60` → `"%.1f s"`; `< 3600` → `"%dm %ds"`; else `"%dh %dm"`.            |
| `vitalState`            | `func(kind string, m htmlRenderModel) string`      | Returns `"ok"`, `"warn"`, `"crit"`, `"stale"` per the §9 thresholds for the given cell kind. Takes the wrapper so it can read both `statusModel` fields and `GCIntervalSeconds`. |
| `outcomeBadgeClass`     | `func(outcome string) string`                      | Maps outcome enum to one of the `.b--*` classes.                                              |
| `verdictExplanation`    | `func(m htmlRenderModel) string`                   | Server-side fallback used only in `<noscript>`. JS computes the live version (§8.1).         |

`sourceKind` prefix rules:

```go
func sourceKind(p string) string {
    switch {
    case strings.HasPrefix(p, "embedded:"):
        return "bundled"
    case strings.HasPrefix(p, "/usr/share/"):
        return "system"
    default:
        return "custom"
    }
}
```

`chunkHex` is whitespace-conservative: it produces single-space separators,
not `&nbsp;` or `&thinsp;`. The mono cell's `tabular-nums` carries the
visual rhythm.

`formatShortDuration` examples (used in the GC vital cell and adoption
duration column):

- `0.0139` → `"14 ms"`
- `0.5`    → `"500 ms"`
- `1.2`    → `"1.2 s"`
- `90`     → `"1m 30s"`
- `5400`   → `"1h 30m"`

### 6.2 Existing helpers preserved

`formatBytes`, `durationOf`, `hitRatePct`, and the existing time
helpers in `internal/admin/status.go` are unchanged. The redesign relies
on them for cache-size formatting, hit-rate counts, and pretty
timestamps.

---

## 7. Data-attribute contract

The HTML serves a dual role: human-readable, and machine-readable by the
inline JS. The attributes below are **the contract between server-rendered
HTML and the inline JS modules**. Any change to attribute names, allowed
values, or placement is a contract change and requires updating both ends
in the same PR.

### 7.1 `data-state` (universal)

Applied to `.vital`, `tr` inside `.data`, and the document `<body>`
(rolled-up overall verdict server-side as a fallback). Allowed values:

| Value    | Semantic                                            |
|----------|-----------------------------------------------------|
| `ok`     | Normal operating value                              |
| `warn`   | Lag / elevated backlog / partial failure            |
| `crit`   | Hard failure                                        |
| `stale`  | No data / not run yet / cold start                  |

Absence of the attribute is treated as `ok`.

The attribute value vocabulary (`ok` / `warn` / `crit` / `stale`) is
deliberately shorter than the operator-facing verdict labels (`HEALTHY`
/ `WATCHING` / `DEGRADED` / `WARMING UP`). The former lives in markup
and CSS selectors where short tokens compress well and keep selector
strings readable; the latter is what the operator reads on the verdict
pill. The two vocabularies map: `ok→HEALTHY`, `warn→WATCHING`,
`crit→DEGRADED`. `stale` is the cell-only fourth state with no direct
verdict-label peer (warming-up is computed from body uptime + GC
attributes, not from a stale roll-up).

### 7.2 `data-outcome` (Recent Adoptions only)

Applied to `<tr>` rows in the Recent Adoptions table. Value is the
outcome enum string (`success`, `gpg_failed`, `fetch_failed`,
`parse_failed`, etc.) — the JS uses this to compute the dominant
non-success outcome for the aggregate notice (§8.2). The value is the
canonical enum string from the JSON payload, not a localized label.

### 7.3 `data-label` (all `<td>` in `.data` tables)

Mirrors the column header text. Used by the mobile list-view CSS via
`::before { content: attr(data-label); }`. Mandatory — without it, the
mobile transformation reads as anonymous data.

### 7.4 `data-source-kind` (Keyring `<tr>` only)

Allowed values: `bundled`, `system`, `custom`. Drives the source-badge
CSS class selector and the keyring rail-chip count aggregation.

### 7.5 `data-keyring-{count,bundled,system,custom}` (section eyebrow spans)

Provide the per-source-kind counts so the keyring section eyebrow stays
in sync with the JS-computed status-bar chip without requiring the JS to
re-classify entries.

### 7.6 `data-adoption-enabled` (`.keys-chip`)

`"true"` or `"false"`. Drives the empty-state tinting branch of the
keys chip.

### 7.7 `data-notice-{count,total,outcome,recent,recent-when}` (notice placeholder)

Optional pre-rendered values to support a no-JS fallback at some future
point. In this phase, the placeholder div is empty and the JS fills it
in. Reserved for future use.

---

## 8. JavaScript modules (inline)

All JS is inline, vanilla, no transpile step. Total budget: ≤6KB minified
(see §12). Each module is an IIFE attached to a single `init` function
called from a `DOMContentLoaded` listener at the bottom of the page.

### 8.1 Verdict pill computation

Reads:
- `<body>[data-state-rollup]` (server-side fallback)
- All `[data-state]` attributes on `.vital` and `tr.data`
- The keys chip's `data-keyring-count` and `data-adoption-enabled`

Algorithm (matches §9 thresholds):

```js
function computeVerdict(doc) {
  const states = [...doc.querySelectorAll('[data-state]')]
    .map(el => el.dataset.state);
  if (states.includes('crit')) return 'degraded';
  if (states.includes('warn')) return 'watching';
  if (isWarmingUp(doc)) return 'warming-up';
  return 'healthy';
}
```

The string the function returns (`degraded` / `watching` / `warming-up`
/ `healthy`) is the verdict-pill class suffix and the operator-facing
label source — it is intentionally distinct from the `data-state`
attribute vocabulary the function READS (`crit` / `warn` / `stale` /
`ok`). See §7.1 for the vocabulary mapping rationale.

`isWarmingUp` reads `body[data-uptime-seconds]` and `body[data-gc-runs]`
— uptime <300s AND gc-runs=0 → warming up.

The function returns one of `healthy | watching | degraded | warming-up`.
The pill's class is set to `verdict--{kind}` and the leading-dot color
follows. The trailing explanation is a single sentence summarising the
worst state(s):

- `degraded`: e.g., "38/50 adoptions failed: gpg_failed · 3 suites lagging"
- `watching`: e.g., "3 suites lagging · zero-refcount backlog rising"
- `warming-up`: e.g., "uptime 2m 14s · waiting for first GC run"
- `healthy`: "all systems nominal · uptime 7h 24m"

### 8.2 Aggregate-failure notice

Reads `tr[data-outcome]` rows from the Recent Adoptions table. Computes
the top non-success outcome and its count. If `count / total >= 0.10`,
renders the notice into `#adoptions-notice`. Otherwise leaves the mount
empty.

Tunable constant at top of the module:

```js
const NOTICE_THRESHOLD = 0.10;
```

Hint text dispatch:

```js
const HINTS = {
  gpg_failed:    {text: "Likely cause: upstream repository key changed or the matching archive key isn't loaded. Cross-check the Keyring section.",
                  link: {label: "Trusted keys", href: "#keyring", arrow: "→ Keyring"}},
  fetch_failed:  {text: "Likely cause: upstream unreachable, TLS failure, or rate-limiting. Check the proxy logs for the upstream host.",
                  link: null},
  parse_failed:  {text: "Likely cause: malformed Release / Sources / Packages payload from upstream. Capture a failing fetch and inspect.",
                  link: null},
  // …extend as outcomes are added
};
```

No HINT may reference a CLI invocation that does not exist in
`cmd/apt-cacher-ultra/`. If the operator-facing remediation requires a
new subcommand or flag, that is out of scope for the read-only UI
phase — write a runbook instead.

Unknown outcomes fall through to a generic hint.

### 8.3 Theme toggle

A single 24px sun/moon button in the status bar's far-right area, before
the JSON link. On click: toggles `document.documentElement.dataset.theme`
between `"light"` and `"dark"`, writes to `localStorage["acu-theme"]`.
On page load, reads `localStorage` and applies before first paint (the
inline script for this is positioned in `<head>` so it runs before the
CSS-variable-driven first paint). If `localStorage` has no entry, the
media query controls.

```js
(function preTheme() {
  try {
    const saved = localStorage.getItem('acu-theme');
    if (saved === 'light' || saved === 'dark') {
      document.documentElement.dataset.theme = saved;
    }
  } catch (_) {}
})();
```

### 8.4 Sticky rail active-section highlight + help-popover dismiss

`IntersectionObserver` on every `<section class="panel">` with a 25%
threshold; on intersection, mark the corresponding `.rail a` as
`aria-current="location"`. CSS handles the visual treatment.

Help popover dismiss: on `click` outside any open `<details class="th-hint">`
or on `Escape`, close it via `el.open = false`.

### 8.5 Duration localization (existing, preserved)

The existing `<time datetime="...">` localization code is preserved. New
code uses the existing helper rather than re-implementing.

---

## 9. State semantics algorithm

The single source of truth for verdict computation. Server-side
helpers `vitalState()` and client-side JS in §8.1 implement the same
rules; both reference this section.

### 9.1 Per-cell vital state

| Cell      | `crit`                                                       | `warn`                                                           | `stale`                                |
|-----------|--------------------------------------------------------------|------------------------------------------------------------------|----------------------------------------|
| CACHE     | `.GC.PoolUnlinkErrors > 0`                                   | `.Cache.ZeroRefcountBacklog > 1000`                              | `.Cache.BytesUsed == 0`                |
| SUITES    | any suite lagging > 24h                                      | any suite lagging at all                                         | `len .Suites == 0`                     |
| ADOPTIONS | non-success rate ≥ 50%                                       | non-success rate ≥ 10%                                           | ring buffer empty (uptime <5m)         |
| GC        | `.GC.LastRunDeadlineReached` true on the last run            | last run older than `2 × .GCIntervalSeconds` (wrapper)           | `.GC.LastRunUnixTime == nil`           |
| ACTIVE    | (no crit threshold defined; reserved)                        | (no warn threshold; reserved)                                    | no active hosts and uptime <5m         |

The CACHE `crit` threshold reads pool-unlink errors from the GC block
(that's where `PoolUnlinkErrors` lives) rather than from `cacheInfo`.
The GC `warn` threshold needs the configured run interval, which is
not exposed in `statusModel` — `htmlRenderModel.GCIntervalSeconds`
(populated from `cfg.GC`'s interval at render time) is the single
source. If the wrapper field is zero (interval unknown), the GC
warn branch is suppressed and the cell goes straight from `ok` to
`stale`.

The reserved Active cells are deliberate — current data doesn't justify
elevated states. If the operator later wants slot-pressure signal, the
threshold lands here.

### 9.2 Overall verdict

Computed as the max severity across all `[data-state]` carriers:

1. If any element has `data-state="crit"` → **DEGRADED**
2. Else if any has `data-state="warn"` → **WATCHING**
3. Else if uptime < 300s AND no GC run yet → **WARMING UP**
4. Else → **HEALTHY**

### 9.3 Keyring fingerprint-collision state (deferred)

The current `internal/gpg` keyring loader silently deduplicates by
primary fingerprint (first-seen wins, so an on-disk copy supersedes
the embedded copy of the same key) and does not surface the
displaced-entry information. The `KeyringEntrySnapshot` shape in
`internal/admin/server.go` carries no annotation field. Rendering a
`(disk override)` row state would require:

1. Extending `internal/gpg` to retain and expose the displaced sources,
2. Adding an annotation field to `KeyringEntrySnapshot` and `keyringEntry`,
3. Widening the locked JSON contract (which §0.1 forbids in this phase).

The disk-override annotation is therefore **out of scope** for this
phase. The default-resolution behavior of the loader is correct
operationally; surfacing the override in the UI is a follow-up that
travels with a new SPEC bump if/when operators ask for it.

---

## 10. Accessibility requirements

1. **Contrast.** Verified pairs (Light / Dark), measured against the
   palette in `internal/admin/status.go`:
   - Body text on page bg: 11.2:1 / 10.5:1 (AAA)
   - Muted text on page bg: 5.2:1 / 5.6:1 (AA)
   - `--ok` on page bg: 7.3:1 / 7.7:1 (AA)
   - `--warn` on page bg: 4.6:1 / 9.0:1 (AA)
   - `--crit` on page bg: 7.8:1 / 6.0:1 (AA)
   - `--accent` on page bg: 9.0:1 / 6.9:1 (AA)
   All pairs are programmatically verified by `TestColorContrast`
   (§14.1) against WCAG 2.1 thresholds (≥4.5 for AA normal text,
   ≥7.0 for AAA). The contrast helper extracts tokens from the
   rendered `<style>` block so a palette change automatically
   re-runs the verification.
2. **State never by color alone.**
   - Verdict pill spells the state name.
   - Badges contain text.
   - Aggregate notice names the outcome.
   - Row stripes are paired with textual badges.
3. **Focus.** All anchors and `<details>` summaries: 2px outline in
   `--accent`, `outline-offset: 2px`. Browser default focus ring is
   suppressed in favor of this rule.
4. **Keyboard navigation.** Sticky rail is `<nav><ul><li><a>`. The
   help-popover `<details>` is keyboard-toggleable by spec.
   `Escape` closes any open `.th-hint`.
5. **Screen readers.**
   - Verdict pill: `role="status"` `aria-live="polite"`.
   - State badges: `aria-label` includes the full state name.
   - Aggregate notice: `role="status"`.
   - Decorative SVGs: `aria-hidden="true"`.
6. **Reduced motion.** `@media (prefers-reduced-motion: reduce)` zeros
   every transition and removes the first-paint reveal.
7. **No JS, no problem.** The page is usable end-to-end without JS.
   The verdict pill stays neutral, the aggregate notice doesn't render,
   the rail's active-highlight is absent, the theme tracks OS only —
   but every datum is visible and every section reachable via anchors.

---

## 11. Responsive strategy

Three breakpoints, no more:

| Range          | Layout                                                                                                  |
|----------------|---------------------------------------------------------------------------------------------------------|
| ≥1280px        | 5-cell hero strip; 200px sticky left rail; content max-width 1400px                                     |
| 1024–1279px    | Rail collapses to horizontal anchor row sticky below status bar; hero strip 3+2; tables `overflow-x: auto` |
| ≤640px         | Single-column hero; tables become card-style list views via CSS (`display: block` + `::before { content: attr(data-label) }`); detail panels collapse into `<details>`, default-closed except non-healthy |

The mobile list-view CSS is ~30 lines and adds no JS cost.

---

## 12. Performance budget

| Asset    | Budget  | Means                                                       |
|----------|---------|-------------------------------------------------------------|
| CSS      | ≤14KB minified, inline    | Hand-written; one inline `<style>` block      |
| JS       | ≤6KB minified, inline     | Vanilla; one inline `<script>` block at body end (plus a tiny pre-paint theme hook in `<head>`) |
| SVG sprite | ≤1KB                    | Single inline `<svg>` with `<defs>` block      |
| Favicon  | ≤0.3KB                    | Inline data-URI SVG                            |
| Fonts    | 0 bytes                   | System font stack only                         |
| Total    | ≤22KB over the wire (gzipped) | Verified in `TestRenderSizeBudget` (§14.1) |

The unminified template literal in `internal/admin/status.go` is allowed
to be larger; the budget is measured against the rendered response.

**Compression.** The admin handler at `internal/admin/handlers.go`
gzips text/html responses when the client sends
`Accept-Encoding: gzip` (which all browsers do). The 22KB total budget
is enforced against the gzipped wire shape, since that is what the
operator's browser actually downloads. A representative healthy
render is ~10KB gzipped (~41KB raw). Operators bypassing the gzip
middleware (curl without `--compressed`, programmatic JSON scrapers
hitting `/?format=json` — which doesn't gzip — etc.) see the
uncompressed bytes; that's an expected trade-off.

**First contentful paint target:** under 100ms over WireGuard
(RTT ~30ms, single HTML response, no blocking subrequests).

---

## 13. Implementation phases

The work splits into three atomic merges. P0 is the visual refresh; P1
is the IA reorganization; P2 is optional interactivity. P1 must not
land until P0 has been on a test deployment for at least one full
adoption cycle.

### 13.1 Phase P0 — Visual refresh on existing structure (~1 day)

Keeps the existing template's section ordering and data flow. Replaces
only the CSS, adds a status-bar wrapper above the existing content, and
adds the inline JS that computes the verdict pill from data attributes
already emitted into the rendered DOM.

P0 deliverables:

1. New inline `<style>` block with the full palette (light + dark),
   typography, spacing, table styles, badge/pill primitives, and
   responsive media queries
2. New status-bar markup at the top of `<body>` containing the brand
   monogram, verdict pill, explanation slot, keys chip, theme toggle,
   and JSON link
3. New vital-signs strip wrapping the existing Cache / Suites /
   Adoptions / GC / Active data points; each cell carries the
   server-computed `data-state` attribute
4. Inline JS for verdict computation (§8.1), theme toggle (§8.3),
   duration localization preservation (§8.5)
5. Aggregate-notice rendering on Recent Adoptions (§8.2 + §5.7), with
   the `Trusted keys → Keyring` link active when outcome is `gpg_failed`
6. Empty-state copy updates, including the keyring dual cases (§5.9)
7. Dark mode
8. Full keyring section styling: `chunkHex(_, 4)` on primary and subkey
   fingerprints, `[BUNDLED]` / `[SYSTEM]` / `[CUSTOM]` source badges,
   `keys N ✓` status-bar chip with `data-source-kind`-driven counts
9. Replace existing `title="..."` on Suites "Adopted at" header with
   the column-header help affordance pattern (§5.4); audit all other
   `title=` and apply consistently
10. New Go helpers in `funcMap` (§6.1)
11. Section eyebrows with server-side counts on every panel
12. `data-label` attributes on every `<td>` in every table; verify the
    mobile list-view CSS triggers correctly at ≤640px
13. Inline-SVG favicon
14. Acceptance: §15.1–§15.7 pass; manual test in `/?format=json` returns
    the byte-identical payload it returned before P0

### 13.2 Phase P1 — IA reorganization (~2 days)

Re-orders template sections per §4.2; promotes Keyring to position 3;
adds sticky left rail with `Keyring` rail entry; converts per-host-by-arch
from `h3` to peer panel; consolidates the two repo-coverage tables
into one. Wires `IntersectionObserver` for rail active-highlight.

P1 deliverables:

1. Template section reorder per §4.2
2. Sticky left rail with `Keyring` rail entry; `scroll-margin-top`
   verified through the sticky bar
3. Per-host × arch as peer panel
4. Consolidated repo-coverage panel with inline arch list and compact
   totals
5. Section eyebrow counts updated to reflect new section positions
6. `IntersectionObserver` rail active-section highlight (§8.4)
7. Help-popover dismiss handler (§8.4)
8. Acceptance: §15.8–§15.10 pass

### 13.3 Phase P2 — Optional client-side interactivity (~1 day, gated)

Only landed if operator feedback after P1 asks for it. Deliverables:

1. Filter input above Suites table (substring on host or path)
2. Sortable column headers (click toggles asc/desc) — JS only
3. Search input in status bar (jumps to first matching row across the page)
4. "Show only failing" toggle on Recent Adoptions

P2 is gated on demand. If the operator never asks, P2 does not land.

---

## 14. Test strategy

### 14.1 Unit tests (Go)

In `internal/admin/status_test.go`:

| Test                                  | Asserts                                                                                          |
|---------------------------------------|--------------------------------------------------------------------------------------------------|
| `TestChunkHex`                        | Table-driven: empty string, 40-hex SHA-1, 64-hex SHA-256, non-hex input passthrough              |
| `TestSourceKind`                      | `embedded:` → bundled; `/usr/share/keyrings/foo.gpg` → system; `/etc/apt/keyrings/x.gpg` → custom |
| `TestFormatShortDuration`             | 0.014 → "14 ms"; 1.2 → "1.2 s"; 90 → "1m 30s"; 5400 → "1h 30m"                                    |
| `TestVitalState`                      | Each cell's threshold thresholds: ok/warn/crit/stale combinations                                |
| `TestOutcomeBadgeClass`               | success → b--ok; gpg_failed → b--crit; unknown outcome → b--stale (or chosen default)            |
| `TestKeyringCounts`                   | Mixed-source slice returns correct bundled/system/custom counts                                  |
| `TestColorContrast`                   | Programmatic AA contrast check on every documented pair in §3 + §10                              |
| `TestRenderSizeBudget`                | A full render with a representative non-empty model fits within the §12 budget                   |
| `TestJSONContractPreserved`           | Render with `?format=json` produces byte-identical output before/after the redesign              |

### 14.2 Template golden tests

Golden tests assert on the **server-rendered initial markup** —
specifically the structural elements and `data-*` attributes that the
JS later reads. Tests must not assert on JS-computed strings (the
verdict pill's `HEALTHY` / `DEGRADED` text, the aggregate notice's
populated headline), because the Go test harness does not execute JS;
without JS the pill is `STATUS` and the notice mount is empty.

In `internal/admin/status_test.go`, golden-output testing for selected
fixtures:

| Fixture                          | Asserts on initial server-rendered HTML                                       |
|----------------------------------|-------------------------------------------------------------------------------|
| `golden_healthy.html`            | All `.vital[data-state]` values are `ok`; `tr[data-state]` rows have no crit/warn; `.notice-mount` is empty; `.keys-chip[data-keyring-count]` is `>0`; verdict pill renders as `STATUS` (the no-JS fallback) |
| `golden_warming_up.html`         | `body[data-uptime-seconds] < 300`; `.GC.LastRunUnixTime == nil` branch produces "NO GC RUN YET" empty state |
| `golden_watching_lagging.html`   | 3 suite rows carry `data-state="warn"`; suites vital cell carries `data-state="warn"` |
| `golden_degraded_gpg.html`       | Recent-adoptions rows carry `data-outcome="gpg_failed"` at the expected ratio; the `notice-mount` placeholder is present (JS fills at runtime — markup only checked) |
| `golden_keyring_empty_disabled.html` | Wrapper renders `.AdoptionEnabled=false`; `.empty--stale` block visible; "ADOPTION DISABLED" eyebrow present |
| `golden_keyring_empty_enabled.html`  | Wrapper renders `.AdoptionEnabled=true`; `.empty--crit` block visible; "NO GPG KEYS LOADED" eyebrow present |
| `golden_keyring_full.html`       | 5 bundled + 1 custom rows; each `tr` carries the expected `data-source-kind` value; `.fp` cells contain the chunked fingerprint with single-space separators |

Golden files live in `internal/admin/testdata/`. Updates require a code
review note explaining why the rendered HTML changed.

JS-computed behavior (verdict pill transition, aggregate-notice render)
is verified in the manual exercise step (§14.5) and the visual
regression sweep (§14.4) — not in Go-side goldens.

### 14.3 JS unit tests

There is no headless JS harness in this project's Go toolchain, and
adding jsdom / a Node runtime for a 6KB inline script is an
infrastructure-cost tail well over the value. JS behavior is verified
in two complementary places instead:

- **Markup-level** in §14.2: the Go golden tests assert that every
  `data-*` attribute the JS reads is present with the correct value
  in the server-rendered HTML. If the contract in §7 is honoured, the
  JS has the inputs it needs.
- **End-to-end** in §14.4 + §14.5: the operator opens the mockup or the
  live deployment in a browser and verifies that the verdict pill, the
  aggregate notice, the theme toggle, and the rail highlight behave per
  spec across the four state scenarios.

Adding a JS harness is a follow-up only if the JS module count or
complexity grows past the §12 budget.

### 14.4 Visual regression

Manual exercise via the mockup (`docs/admin-ui-mockup.html`) — operator
opens it in Chrome/Firefox/Safari and verifies each state (healthy /
watching / degraded / warming-up) and theme (light / dark) renders as
the mockup intends. Mockup is the visual reference; any deviation in the
rendered Go template is a P0 bug.

### 14.5 Manual exercise

Mandatory at end of P0 and end of P1:

1. **Healthy deployment.** Bring up a fresh cache; visit `/`; verify
   verdict is `WARMING UP` initially, transitions to `HEALTHY` after
   the first GC, and all 5 vital cells render.
2. **Failure injection.** Misconfigure adoption to point at an upstream
   with rotated keys; verify the aggregate notice surfaces with the
   keyring link, click the link, verify the page scrolls to `#keyring`
   with the sticky bar cleared.
3. **JSON contract regression.** `curl -H 'Accept: application/json' /`
   before and after the redesign; diff the bytes; must match.
4. **No-JS path.** Disable JS in DevTools; verify every section is
   reachable, every datum visible, every state legible via textual
   badges. The verdict pill is neutral; that is expected.
5. **Mobile reflow.** Resize viewport to 360px; verify hero strip
   collapses, tables become card-style lists, panels collapse into
   `<details>`.
6. **Dark mode.** Toggle theme; verify both palettes render correctly;
   verify `localStorage` persists across reload.
7. **Reduced motion.** Set `prefers-reduced-motion: reduce`; verify the
   first-paint reveal does not play.

---

## 15. Definition of done

The redesign is complete when all of the following hold:

1. **`go test -race ./internal/admin/...` passes**, including all new
   tests under §14.1, §14.2.
2. **JSON contract unchanged.** `TestJSONContractPreserved` is green;
   manual diff of `/?format=json` bytes before and after the change
   is empty.
3. **Verdict pill correct under all four states.** Manual exercise
   §14.5.1 produces `WARMING UP` → `HEALTHY`; failure injection
   §14.5.2 produces `DEGRADED`.
4. **Aggregate notice renders for `gpg_failed` ≥10%.** The Keyring
   link is present and scroll-jumps to `#keyring` with the sticky bar
   cleared.
5. **Keyring section live.** With a default install (4 bundled keys),
   the section eyebrow reads "KEYRING — 4 LOADED · 4 BUNDLED · 0 SYSTEM
   · 0 CUSTOM"; fingerprints render in 4-hex chunks; source badges are
   `[BUNDLED]`.
6. **Keyring custom-key visible authorship.** Adding a key under
   `/etc/apt/keyrings/` produces a row with the `[CUSTOM]` badge tinted
   `--accent`.
7. **Empty-state branches correct.** With `[adoption].enabled = false`,
   the keyring panel shows the STALE empty state. With adoption
   enabled and no keys loaded, the panel shows the CRIT empty state
   with a `--crit` left stripe.
8. **No-JS path graceful.** §14.5.4 passes: every section reachable,
   every datum visible.
9. **Mobile reflow correct.** §14.5.5 passes at 360px viewport.
10. **Dark mode correct.** §14.5.6 passes; both palettes pass AA;
    `localStorage` persists.
11. **`prefers-reduced-motion`** honoured. §14.5.7 passes.
12. **Performance budget met.** `TestRenderSizeBudget` green; CSS ≤14KB,
    JS ≤6KB, total ≤22KB on a representative render.
13. **Help-popover replaces `title=`.** No `title=` remains in the
    rendered HTML for column headers. Tab + Enter on the info glyph
    opens the popover; `Escape` closes it.
14. **Sticky rail.** At ≥1280px the rail is present and the active
    section's rail entry is marked `aria-current="location"` as the
    user scrolls.
15. **No regression in `/healthz`, `/metrics`.** Both endpoints
    return their previous payloads with their previous content-types.
16. **No goroutine leak.** A 60-second soak of repeated `/` requests
    with `-race` enabled and `goleak.VerifyNone()` at test teardown
    shows no leaked goroutines.
17. **Documentation.** This spec is locked. The plan
    (`docs/admin-ui-redesign-plan.md`) and mockup
    (`docs/admin-ui-mockup.html`) are referenced from the spec but
    not modified by the implementation PR.

P2 work, when undertaken, has its own definition-of-done as a delta to
this section, scoped to its four deliverables.

---

## 16. Risks and mitigations

Carried forward from plan §13 with brief notes on each. The plan holds
the rationale; this section names the risk and the mitigation in
implementation terms.

1. **Verdict pill is client-side computed.** Without JS, the pill is
   neutral. *Mitigation:* every data attribute the pill reads is
   already in the DOM, so the page is self-describing without the
   pill. The `<noscript>` block names the limitation.
2. **Tabular-nums and JetBrains Mono are not universally installed.**
   *Mitigation:* font stack falls back to ui-monospace; `tabular-nums`
   is a font-variant that all modern OS-default mono fonts support.
3. **Aggregate-notice 10% threshold is a guess.** *Mitigation:* threshold
   lives in a single `const NOTICE_THRESHOLD = 0.10` at the top of the
   inline JS; trivially tunable after operator feedback.
4. **Sticky rail eats ~200px on 1280px laptops.** *Mitigation:* horizontal
   anchor row below 1280px preserves table width.
5. **No XHR live-update.** *Mitigation:* meta-refresh remains; revisit
   only on operator demand.
6. **Aesthetic may read as "1997" to some reviewers.** *Mitigation:* the
   plan §13 Risk 6 holds the rationale; defer to it during review.
7. **Keyring placement may read as overweight.** *Mitigation:* the
   `gpg_failed` failure-investigation flow is the dominant case where
   Keyring is hot; on a healthy day the section is a fast scan-and-go.
   If real failure incidence is ≤1/month across the fleet, revisit IA
   in a follow-up — the change is a single rail-list reorder.
8. **`[BUNDLED]` / `[SYSTEM]` / `[CUSTOM]` taxonomy is invented.**
   *Mitigation:* classification lives in one Go helper (`sourceKind`);
   misclassifications are a one-line fix.

---

## Appendix A — Helper signatures (cheat sheet)

```go
// chunkHex groups a hex string into n-character chunks separated by spaces.
// Non-hex input is returned verbatim.
func chunkHex(s string, n int) string

// sourceKind classifies a keyring source_path into "bundled" / "system" / "custom".
func sourceKind(path string) string

// sourceKindLabel returns the uppercase badge label for a source path.
func sourceKindLabel(path string) string

// countBundled / countSystem / countCustom count keyring entries by source kind.
func countBundled(ks []keyringEntry) int
func countSystem(ks []keyringEntry) int
func countCustom(ks []keyringEntry) int

// formatShortDuration formats a duration in seconds as the most human-friendly form.
//   0.014  -> "14 ms"
//   0.5    -> "500 ms"
//   1.2    -> "1.2 s"
//   90     -> "1m 30s"
//   5400   -> "1h 30m"
func formatShortDuration(seconds float64) string

// vitalState returns the data-state value for a named vital cell.
//   kind ∈ {"cache", "suites", "adoptions", "gc", "active"}
//   returns ∈ {"healthy", "watching", "crit", "stale"}
// Takes the htmlRenderModel wrapper because the GC stale threshold
// needs GCIntervalSeconds (not present on statusModel).
func vitalState(kind string, m htmlRenderModel) string

// outcomeBadgeClass maps an adoption outcome enum to a badge class.
func outcomeBadgeClass(outcome string) string

// verdictExplanation produces the noscript fallback verdict explanation.
// Takes the wrapper for consistency with vitalState.
func verdictExplanation(m htmlRenderModel) string
```

## Appendix B — Quick wins folded into P0

These don't merit their own phase; they ride along with P0:

- Render durations as `14 ms` / `1.2 s` / `2m 30s` (helper `formatShortDuration`)
- Render every fingerprint via `chunkHex(s, 4)` — unified for SHA-1 and SHA-256
- `font-variant-numeric: tabular-nums` at the table CSS level
- Inline-SVG favicon (`«` glyph in `--accent`)
- `data-label` attributes on every `<td>` (enables mobile list-view CSS)
- `data-state` / `data-outcome` / `data-source-kind` data attributes per §7
- Replace `title="..."` column-header tooltips with the help-popover pattern
- Keyring section eyebrow counts via `countBundled` / `countSystem` /
  `countCustom` helpers

All are server-side template changes that don't touch the Go data model
or the JSON contract — additive HTML structure only.

---

## Appendix C — Anchor comments to add

Per the project's `CLAUDE.md` anchor-comment convention, add these AIDEV
notes during implementation:

- `AIDEV-NOTE`: at the top of the template literal in
  `internal/admin/status.go`, naming the design plan and mockup
  documents as the visual ground truth, and naming this spec as the
  contract.
- `AIDEV-NOTE`: above the `funcMap` declaration, listing the helpers
  added in this phase and pointing at §6.1 of this spec.
- `AIDEV-NOTE`: above the inline `<script>` block, listing the JS
  modules in load order and pointing at §8.
- `AIDEV-NOTE`: above the `data-*` attribute emissions in the template,
  pointing at §7 for the attribute contract.
- `AIDEV-NOTE`: at the keyring-section template block, pointing at §5.6
  and naming the empty-state dual-branch logic.
- `AIDEV-TODO`: at the aggregate-notice JS module, naming
  `NOTICE_THRESHOLD = 0.10` as tunable based on operator feedback.

Do not remove `AIDEV-*` anchors during implementation without an explicit
note in the PR description.
