# apt-cacher-ultra admin UI — redesign plan

Author: design lead, May 2026
Status: proposal, pre-implementation, rev. 2 — keyring integration
Scope: visual + IA overhaul of the read-only status page served at `:6789/`. Go template only; no server behavior changes. The JSON contract is locked and untouched — that includes the recently-added top-level `keyring[]` array (40-char primary fingerprint, primary UID, source path, subkey fingerprints) which we treat as a permanent surface this redesign must honour.

---

## 1. Design philosophy

This is a console for someone who already runs `journalctl -fu apt-cacher-ultra` in a second pane. It is not a dashboard for a CIO; it is an **instrument panel**, and the instrument's job is to answer one question — *is the cache doing its job, and if not, where is it bleeding?* — within the first second of paint. Everything else is supporting evidence. The visual stance is **engineering-grade, near-monochrome, dense, and quiet**. No marketing gradients, no celebration of metrics, no chartjunk. When something is wrong we shout in one specific color; the rest of the time the page is the colour of paper and the data is the hero.

We're optimizing for: (a) one-glance health, (b) scannable density without fatigue, (c) trust through restraint. The page should feel built by the same person who wrote the GC algorithm.

## 2. Audience and jobs-to-be-done

The operator is an SRE or senior sysadmin running a Debian/Ubuntu fleet. They open `:6789/` to answer, in roughly this order:

1. **Is it healthy?** Specifically: any suites lagging, any GC distress (deadline reached, unlink errors, rising zero-refcount backlog), are recent adoptions failing en masse?
2. **What's the cache holding?** Bytes, blob count, breakdown by host × architecture — useful for capacity planning and confirming a new arch is being mirrored.
3. **What are clients hitting?** Hot URL paths — to validate cache effectiveness and spot a misconfigured client hammering one repo.
4. **What just happened?** Recent adoptions — the most recent N events, with failure modes called out.
5. **Is the failing repo's signing key actually loaded?** A targeted debug question that fires the moment `gpg_failed` appears in §Recent adoptions. The operator wants to compare upstream-advertised fingerprints against the loaded keyring and see whether the failing repo is signed by a bundled key or a disk-path key they added themselves.
6. **Plumbing checks** (rare visits): listeners, TLS MITM CA fingerprint, repo coverage, GC last-run details, build info.

The IA below is ordered against these jobs, not against the order Go fields are declared in the model.

## 3. Information architecture

The current page is one long vertical scroll where every `h2` carries the same weight. "Listeners" sits at the same hierarchy as "Suites" despite being a sentence vs. a 50-row table the operator actually came for. We reorganize into a **three-zone composition**:

**Zone 1 — Status bar (always visible, top of page).**
A single-row band, ~64px tall, that names the product, shows uptime, and contains the **health verdict**: a pill that reads HEALTHY / WATCHING / DEGRADED with a one-line explanation of why ("3 suites lagging; GC reached deadline last run"). This is the part the operator reads first and often only.

**Zone 2 — Hero strip (the "is it healthy?" panel).**
A horizontally-arranged set of 5 vital-signs cells immediately below the status bar:

- Cache size + blob count + delta-suggestive zero-refcount backlog
- Suites tracked / lagging / failing
- Recent adoptions: success rate over the ring buffer, dominant outcome
- GC: time since last run, bytes reclaimed, deadline-reached flag
- Active hosts in flight (a "live pulse" indicator)

Each cell is a label/value/sub-value triple with a state color stripe on the left edge. Not "cards" in the SaaS sense — no shadows, no rounded corners that scream "Material". Think the gauges on an old Tektronix oscilloscope: clean rectangles, tabular numerals, one accent color when a value is out of band.

**Zone 3 — Detail sections (the "tell me more" body).**
A sticky left rail with anchor links navigates between detail panels. Order, top to bottom:

1. **Suites** (the densest table; promoted to first detail because it carries the lag signal)
2. **Recent adoptions** (carries the failure signal)
3. **Keyring** (peer of Recent adoptions, not buried in Plumbing — see below)
4. **Hot URL paths**
5. **Cache → per-host × arch** (the buried-h3 pivot table, elevated to a peer section)
6. **Repository coverage** (consolidated to a single panel — see §5)
7. **Garbage collection**
8. **Active hosts**
9. **Plumbing**: Listeners, TLS MITM, Process/build info

The keyring placement is the most opinionated IA call in the rev-2 update. Three options were on the table: (a) leave it buried under Plumbing because the keys rarely change, (b) elevate to its own section near Recent adoptions, or (c) keep it in Plumbing but link from the aggregate-failure notice. We pick (b) — and we *also* keep the link from the notice. The rationale is the §2 job ordering: when an operator sees the `gpg_failed` aggregate strip, the next click is "show me the keys we have loaded, in particular for the failing host." Forcing them to scroll past four other sections to reach Plumbing is the wrong gradient. Conversely, on a healthy day Keyring is a fast scan-and-go (four bundled keys, all good, move on), so its presence above the fold doesn't tax the operator. The aggregate notice itself anchor-links to `#keyring` — that's the one-click path from failure to diagnostic.

The sticky rail is JS-free — it's a list of `<a href="#suites">` anchors with `scroll-margin-top` set to clear the status bar. With JS enabled, we add `IntersectionObserver` to highlight the current section. Without JS, every section is still reachable and the rail still renders as a plain index.

Tabs were considered and rejected. Tabs hide state behind a click, and in the operator's typical workflow (Ctrl-F a hostname across the whole page) hidden tabs are user-hostile. Collapsible `<details>` for the truly long tables (recent adoptions when >50 rows, suites when >40 rows) gives us the same compression without losing in-page search. The default-open state is "collapsed for tables ≥30 rows with no anomalies; expanded if any row in the table is non-green."

## 4. Visual system

### Personality

**Calm engineering-grade instrument.** The closest analogues: Stripe's API reference docs (typographic discipline, monospace data), Bloomberg Terminal (density, status colour discipline), and the print aesthetic of an O'Reilly "Pocket Reference" cover (warm, restrained, confident).

### Typography

Self-hosted is non-negotiable (zero-CDN). The stack stays in system fonts but chosen with intent — we get distinctive character from scale and weight discipline, not novelty fonts.

- **Display / headings**: `"IBM Plex Sans", ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif`. IBM Plex Sans is present on most modern Linux desktops and most macOS 13+ via Homebrew/apt; if absent, the system-ui fallback is dignified enough. The crucial trait: real italic, real small-caps support — used for section eyebrows.
- **Body**: same stack; we don't pair two sans faces for the sake of pairing.
- **Mono (data, paths, hashes, IDs)**: `"JetBrains Mono", "IBM Plex Mono", ui-monospace, "SF Mono", "Cascadia Mono", Menlo, Consolas, monospace`. JetBrains Mono is shipped with most distributions' developer-tools meta packages; IBM Plex Mono is the consistent fallback.
- **Tabular figures everywhere numeric**: `font-variant-numeric: tabular-nums`. This is non-negotiable and the single biggest visual upgrade — right-aligned bytes/counts/durations stop "dancing" between rows. Apply at the table level, not per-cell.

Type scale (modular, ratio 1.2):

| Token | Size  | Use                                                     |
|-------|-------|---------------------------------------------------------|
| `xs`  | 11px  | Eyebrows, table column headers, footnote               |
| `sm`  | 12.5px| Table body, label/value pairs                          |
| `base`| 14px  | Body prose, section descriptions                       |
| `md`  | 16px  | Vital-cell labels                                      |
| `lg`  | 20px  | Vital-cell values, section h2                          |
| `xl`  | 28px  | Hero count (cache size, suite count)                   |
| `2xl` | 36px  | Status verdict pill text                               |

Headings use weight 600 (semibold), never bold-black; eyebrows use weight 500 with letter-spacing 0.08em and uppercase, which gives section dividers identity without resorting to underline rules.

### Palette — light mode

A near-monochrome paper-ink canvas with three semantic accents. Hex codes are committed values, not "ish".

| Token              | Hex      | Use                                                                |
|--------------------|----------|--------------------------------------------------------------------|
| `--ink-0`          | `#FAFAF7`| Page background (paper, not pure white — pure white feels sterile)|
| `--ink-1`          | `#F2F1EC`| Panel background, table header band                                |
| `--ink-2`          | `#E5E3DA`| Divider rules, table borders                                       |
| `--ink-3`          | `#C7C4B8`| Disabled text                                                      |
| `--ink-4`          | `#7A7669`| Secondary text, muted labels                                       |
| `--ink-5`          | `#3A3833`| Body text                                                          |
| `--ink-6`          | `#1A1815`| Headings, primary numerals                                         |
| `--accent`         | `#7A2E0A`| Brand mark, link colour. **Brick/oxide red, not vermilion.**       |
| `--ok`             | `#2E5D3A`| Healthy state — forested green, not minty                          |
| `--warn`           | `#A36410`| Watching state — burnt amber, not yellow                           |
| `--crit`           | `#9A1F1B`| Critical state — oxide red, slightly more saturated than `--accent`|
| `--stale`          | `#5C5A52`| Stale/no-data state — explicitly grey, never green-or-red          |

The accent (brick/oxide) is intentionally desaturated — it shows up on links, the brand monogram, the section eyebrow rules, and nothing else. It does not compete with the semantic colors. A page in normal health is essentially three shades of grey-ink on warm-paper with one brick accent line.

### Palette — dark mode

Not an inversion. Dark mode is its own composition.

| Token              | Hex      | Use                                                                |
|--------------------|----------|--------------------------------------------------------------------|
| `--ink-0`          | `#15171A`| Page background — bluish-charcoal, not pure black                  |
| `--ink-1`          | `#1B1E22`| Panel background                                                   |
| `--ink-2`          | `#2A2D32`| Divider rules                                                      |
| `--ink-3`          | `#4A4D54`| Disabled text                                                      |
| `--ink-4`          | `#8E8F94`| Muted labels                                                       |
| `--ink-5`          | `#C5C6CB`| Body text                                                          |
| `--ink-6`          | `#EDEEF1`| Headings, primary numerals                                         |
| `--accent`         | `#E08B5A`| Same brick family, lightness flipped for AA against `--ink-0`      |
| `--ok`             | `#7FB68E`| Sage, not mint                                                     |
| `--warn`           | `#E0B05A`| Amber, less burnt — needs to read on dark                          |
| `--crit`           | `#E7716E`| Coral-leaning red; pure red on dark vibrates                       |
| `--stale`          | `#7A7C82`| Cool grey                                                          |

Both palettes pass WCAG AA at the body-text level (≥4.5:1 on `--ink-0`) and AAA on most pairs. Verified pairs are listed in §7.

### Spacing scale

4px base, geometric: `4 / 8 / 12 / 16 / 24 / 32 / 48 / 64 / 96`. Table cell padding is `8px 14px`. Section vertical rhythm is `48px` between detail panels. We never use 5px, 7px, 10px — discipline matters when 60% of the page is tables.

### Radii

`0px` on tables, `2px` on pills/badges, `0px` on panels. Rounded corners are visual sugar; this product doesn't earn them. The one exception: the health-verdict pill in the status bar gets `999px` (full-pill) because that's the only place we want to draw the eye.

### Shadow / elevation

None. No `box-shadow`. Panels are separated by `1px solid var(--ink-2)`. Elevation cues are not part of this aesthetic — depth in this product is signal, not decoration. The only `box-shadow` permitted is a `0 0 0 2px var(--accent)` focus ring (see §7).

### Icon style

A small custom inline-SVG sprite, no icon font. Geometric, 14px stroke at 1.5px, square line caps. Used sparingly — only where text alone is slower:
- A diagonal arrow on external links (JSON, /metrics, /healthz)
- A small dot indicator (filled circle) for state stripes on vital cells
- A chevron for `<details>` expand/collapse

No emoji status indicators. Never.

### Motion

Reduced to almost nothing on purpose. The page reloads in full; there is no animated state transition that matters. We permit:
- A 600ms first-paint reveal: `opacity 0 → 1` and `translateY 4px → 0` on the status bar only.
- A 120ms ease on hovering a table row (background tint changes).
- A 120ms ease on `<details>` expand.
- Nothing else. No skeleton loaders, no shimmer, no auto-scrolling. We honour `prefers-reduced-motion: reduce` by setting all transitions to `0.01ms`.

## 5. Component patterns

### Status verdict pill (single, in the status bar)

The opinionated centrepiece. Computed client-side from server-rendered data attributes on the page — no server logic change required. Algorithm:

- **DEGRADED** (`--crit`): any of {pool_unlink_errors > 0, deadline_reached, ≥50% of recent adoptions non-success, any suite lagging by >24h}
- **WATCHING** (`--warn`): any of {zero_refcount_backlog > 1000, ≥10% of recent adoptions non-success, any suite lagging at all, GC older than the configured GC interval × 2}
- **HEALTHY** (`--ok`): none of the above
- **WARMING UP** (`--stale`): uptime < 5 min AND no GC run yet — the explicit "we just started, give us a moment" state

The pill is a tall (44px) capsule, weight 600, uppercase, letter-spacing 0.04em, with a 12px round dot at the left of the same colour. To its right: a one-line plain-prose explanation. Example: "WATCHING — 3 suites lagging; 1 suite stale ≥24h". The explanation is constructed client-side from sibling data attributes (each section emits `data-state` attributes the JS scans on load).

If JS is off, the pill renders as "STATUS" in `--stale` with a tiny footnote explaining that the page is reading the data zones manually — the operator falls back to scrolling.

### Vital-signs cells (hero strip)

5 cells in a row, each:
```
┌─ (4px left stripe in state colour) ───────┐
│  EYEBROW LABEL              (xs, --ink-4) │
│                                            │
│  72.4 GiB                   (xl, --ink-6) │
│  4,193 blobs                 (sm, --ink-4)│
│                                            │
│  Zero-refcount: 1,550 ↑     (sm, --warn) │
└────────────────────────────────────────────┘
```

Width: equal columns, `minmax(0, 1fr)`. At ≤1100px viewport, collapses to a 2×3 grid; at ≤700px, single column.

The state stripe is the **only** chromatic element in normal-health renders. It runs the full height of the cell at 4px wide. Border colour around the cell stays neutral (`--ink-2`).

### Dense tables

This is where most of the redesign work lives. Specifications:

- **Container**: full-bleed panel, `1px solid var(--ink-2)` top and bottom only — no side borders, so the data reads as a continuous instrument.
- **Header**: 32px tall, `--ink-1` background, eyebrow type (uppercase, letter-spacing 0.08em, weight 500, --ink-4). Sticky on scroll via `position: sticky; top: 64px` (clearing the status bar).
- **Rows**: 32px tall standard, 14px text. Zebra striping by quiet alternation — odd rows get `--ink-0`, even get a tint at `rgba(0,0,0,0.02)` light / `rgba(255,255,255,0.02)` dark. Striping is dropped when state colouring is active (so a coloured row is unambiguous).
- **Borders**: no horizontal cell borders. Vertical separators on the column boundaries are also dropped — the type itself does the column work, aided by tabular-nums on the numeric columns.
- **Numeric columns**: right-aligned, mono font, tabular-nums. Bytes always with a non-breaking space before the unit ("72.4 GiB" not "72.4GiB"). Counts get thin-space thousand separators ("1 550" or comma "1,550"; pick comma for US-locale consistency).
- **Path/host columns**: mono font, `--ink-5`. We truncate with `text-overflow: ellipsis` only when narrower than 480px — most desktop renders show full paths. Long paths get a `title` attribute fallback when truncated, but I'd rather give the column the room it needs and let the table scroll horizontally on narrow viewports than ellipsize.
- **Hover**: full-row background nudge to `--ink-1`, 120ms ease. Cursor stays default — there's nothing to click.
- **Row state**: when a row is in a non-healthy state, the leftmost cell gets a `box-shadow: inset 3px 0 0 var(--state-color)` — the same 3px stripe used on vital cells, applied as inset shadow. The rest of the row stays neutral. No background flood; flooding a row in red across 7 columns is a 1990s Bugzilla move and reads as noise on a dense table.

### Column-header help affordance

The current template uses a `title="..."` attribute on the Suites table's "Adopted at" header to explain that adoption fires only on observed InRelease change (so suites whose upstream hasn't republished since startup will stay empty in that column without being broken). `title` works in a pinch but it's the wrong primitive: undiscoverable on touch, slow to appear, inconsistent across screen readers, and impossible to style. The redesign should replace it with a visible affordance — preferred pattern:

- A 12px `&#9432;` (or inline SVG info glyph) at `--ink-4` sitting after the header label, in the eyebrow's letter-spacing.
- Click/focus reveals a tethered popover with the same body text, dismissed on outside-click or `Esc`. `<details>`/`<summary>` is acceptable if we don't need positioning; otherwise a small JS popover.
- Keyboard accessible (`tabindex="0"`, `aria-describedby` pointing at the popover content, `Escape` closes), and the popover content lives in the DOM (not a `data-*` attribute) so screen readers see it without JS.
- We retain a non-JS fallback: when JS is off, the info glyph is wrapped in `<details>` so the help is still reachable, just inline.

Patterns where this is needed today: "Adopted at" (the InRelease-change-trigger explanation), and likely "InRelease changed" + "Current snapshot" once we add the lagging-vs-stale-vs-never-adopted distinction. Audit each table header during the redesign and apply consistently — a single `tableHeaderHint` partial in the Go template, fed a label and a help-text string, keeps the markup uniform.

### Label/value detail blocks

Used in Plumbing (listeners, TLS MITM CA fingerprint, repo coverage, GC). A **two-column grid**, not a table. Labels right-align in the first column at 30% width, values left-align in the second at 70%. Each pair gets `padding: 8px 0` and a hairline rule below in `--ink-2`. The label is `--ink-4` eyebrow type (uppercase xs). The value is `--ink-6` body. Long hex/path values get the mono treatment automatically because their `<code>` wrapper inherits the mono stack.

Fingerprints get a single chunking rule, applied uniformly to both the TLS MITM SHA-256 (64 hex) and the Keyring SHA-1 primary + subkey fingerprints (40 hex each): **groups of four hex separated by a thin space** (U+2009 or, more pragmatically, a normal space inside a `font-variant-numeric: tabular-nums` mono cell). At 12px JetBrains Mono this is the read-best grouping for both lengths — 8-hex chunks make a SHA-1 read as five visually-similar slabs that the eye loses count of, while 4-hex chunks segment cleanly (10 groups for SHA-1, 16 for SHA-256) and are short enough to clipboard-compare without scrolling sideways. Implementation is a single Go template helper, `chunkHex(s, 4)`, used by both surfaces.

### Status badges / pills (for the small inline cases)

Used in:
- Recent-adoptions outcome column (success / gpg_failed / fetch_failed / parse_failed)
- Lagging annotation on suites
- GC deadline-reached cell
- TLS MITM enabled/disabled state

A pill is `display: inline-block; padding: 2px 8px; border-radius: 2px; border: 1px solid currentColor; font-size: xs; letter-spacing: 0.04em; font-weight: 500; text-transform: uppercase; background: transparent;`. The badge inherits the relevant state colour via a class:

- `.b-ok` → border + text in `--ok`, no fill
- `.b-warn` → border + text in `--warn`, no fill
- `.b-crit` → border + text in `--crit`, no fill
- `.b-stale` → border + text in `--stale`, no fill
- `.b-neutral` → border `--ink-3`, text `--ink-4`

Stroke-only badges (no fill) keep the row from going circus when many rows are flagged. The few cases where a filled badge is justified — the health verdict pill — get a separate dedicated style. We do not flood the recent-adoptions table with 50 red badges; instead, when ≥10% of recent rows share a failure outcome, we surface that fact in the **section eyebrow** (see §5: section headers below) and let the row badges be stroke-only.

### Keyring section

One row per loaded GPG key. The table has four columns — Primary fingerprint, User ID, Source, Subkey fingerprints — and a handful of conventions that earn it the section-level treatment rather than a kv block:

- **Primary fingerprint** is the row's identity. Rendered in JetBrains Mono at 12px with `chunkHex(_, 4)` grouping (so a SHA-1 reads as ten 4-hex groups). This is the primary scan target — operators copy fingerprints from upstream documentation and compare against this column.
- **User ID** is the human label. Body text in `--ink-5`, wraps to two lines if needed. Year-stamped UIDs ("Ubuntu Archive Automatic Signing Key (2018)") preserve their year — those parentheticals are load-bearing during a key rotation.
- **Source** is the semantic differentiator. Three badge variants:
  - `[BUNDLED]` — `embedded:<name>` prefix. Stroke badge in `--ink-4` / `.b-neutral` styling. These are the keys baked into the binary at build time (Ubuntu archive, Debian archive, ESM apps/infra). On a default install all four rows are bundled.
  - `[SYSTEM]` — disk-path key under `/usr/share/keyrings/`. Stroke badge in `--ink-4`, but the path itself is rendered in `--ink-5` mono. These are distribution-shipped keys the operator opted into via the `keyring_dirs` config.
  - `[CUSTOM]` — disk-path key elsewhere (typically `/etc/apt/keyrings/`). Stroke badge in `--accent` brick. This is the variant the operator most needs to *see* — it indicates "we added something here on purpose, and it's not coming from the OS package manager." The brick tint matches the brand accent precisely because the operator's own additions deserve visible authorship.
- **Subkey fingerprints** are a vertically-stacked list of `chunkHex` strings, one per line, in 11px mono `--ink-4`. Most rows have zero subkeys and render as a single `—` in `--ink-3`. The few that carry one or two (notably the ESM keys) get the visual weight that matches their count.

A row's leftmost cell may carry a `[CRIT]` stripe in one specific case: the **fingerprint-collision indicator** (when on-disk wins over an embedded entry of the same fingerprint, the row is `data-state="warn"` with a `(disk override)` annotation after the source badge). This is the same row-state mechanism used everywhere else; no new visual language is introduced.

The section participates in the failure-diagnostic flow by being the link target of the aggregate-failure notice. When `gpg_failed` is the dominant outcome, the notice's body text ends with `Trusted keys → §Keyring` rendered as a real anchor (`<a href="#keyring">`), styled like every other link on the page (`--accent`, hover-underline). The anchor jumps with `scroll-margin-top: var(--bar-h)` to clear the sticky status bar.

### Empty states

Currently a single line of muted text. Upgrade: a centred two-line block in the panel body, 96px tall:

```
NO SUITES TRACKED YET
Adoption begins when a client requests an InRelease through the proxy.
```

The first line is eyebrow type in `--stale`. The second is body type in `--ink-4`. No illustration, no icon — illustration in an SRE tool reads as condescending. The job is to confirm "we're working, there's just no data," and one declarative sentence does that.

Per-section empty states get specific text:
- Active hosts: "No hosts have held a fetch slot since process start. Slot usage is bursty — this is normal between adoption cycles."
- GC: "No GC run has completed yet. First periodic run occurs ~{interval} after process start."
- Hot URL paths: "No URL paths have been requested yet."
- Recent adoptions (uptime < 5 min): "Ring buffer empty — process started {uptime} ago."
- **Keyring** is the one section with two distinct empty states, gated on whether adoption is enabled. The template knows this implicitly: a zero-length `keyring[]` with adoption enabled is an alarm; a zero-length array with adoption disabled is operationally fine. Render accordingly:
  - **Adoption disabled (stale empty)**: eyebrow type "ADOPTION DISABLED", body copy "Keyring is not loaded because `[adoption].enabled = false`. Enable adoption to bundle the default Ubuntu/Debian archive keys." Treated as `--stale` — the dashed-border empty block, no alarm colour.
  - **Adoption enabled, zero keys (critical empty)**: eyebrow type "NO GPG KEYS LOADED", coloured `--crit`; body copy "Adoption is enabled but no keyring entries are present. This is a configuration error — every adoption will fail with `gpg_failed`." The empty block carries a 4px left stripe in `--crit`, matching the row-state language used elsewhere. This is the rare case where an empty state is itself the loudest signal on the page.

### Per-row aggregate / collapse for the "50 gpg_failed" case

This is the redesign's most opinionated component. When the recent-adoptions table has ≥10% of rows sharing a non-success outcome, we render an **aggregate notice strip** above the table:

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ ● 38 of 50 recent adoptions failed: gpg_failed                                │
│   Most recent: download.docker.com/dists/noble — 14 minutes ago               │
│   Likely cause: upstream repository key changed. Run `apt-cacher-ultra        │
│   packages list --outcome gpg_failed` to investigate.                         │
│   Trusted keys → §Keyring                                                     │
└──────────────────────────────────────────────────────────────────────────────┘
```

The strip is bordered in `--crit` (or `--warn` for non-failure-but-suspicious patterns), text is `--ink-6` for the headline and `--ink-4` for the detail. It computes its content client-side from `data-outcome` attributes on the rendered rows; if JS is off, the strip is hidden and the operator sees only the rendered table (which still has stroke badges per row).

The trailing "Trusted keys → §Keyring" line is a real `<a href="#keyring">` anchor styled in `--accent`. It is part of the notice's first-class content, not a footnote — when the dominant failure outcome is `gpg_failed`, the operator's natural next action is to confirm whether the failing upstream's key is in the loaded keyring, and one click should get them there. For other failure outcomes (`fetch_failed`, `parse_failed`) we omit the keyring link and substitute outcome-appropriate guidance (a `packages list --outcome=…` command suggestion).

The strip resolves the "buried failure signal" pain point directly: the dominant pattern is named, counted, dated, and given a remediation hint that points at the correct diagnostic section.

### Section eyebrows

Each detail section starts with a small eyebrow line above the `h2`:

```
SUITES — 47 TRACKED · 3 LAGGING · 1 STALE
```

Eyebrow type, with counts pulled from the rendered rows by JS. The lagging/stale counts are coloured in their state colours inline. This gives the operator the section summary without scanning the table.

## 6. State semantics

Five canonical states, applied consistently:

| State    | Stripe / accent     | Badge style          | When                                                                                                       |
|----------|---------------------|----------------------|------------------------------------------------------------------------------------------------------------|
| HEALTHY  | `--ok` (#2E5D3A)    | stroke-only `.b-ok`  | Normal operating values                                                                                    |
| WATCHING | `--warn` (#A36410)  | stroke-only `.b-warn`| Lagging, elevated backlog, partial failure rate, keyring fingerprint-collision (disk overrides embedded)   |
| CRITICAL | `--crit` (#9A1F1B)  | stroke-only `.b-crit`| Hard failure: unlink errors, deadline reached, majority failure rate, adoption-enabled-with-zero-keys      |
| STALE    | `--stale` (#5C5A52) | stroke-only `.b-stale`| No data / not run yet / cold start / adoption-disabled-empty-keyring                                      |
| NEUTRAL  | `--ink-3`           | `.b-neutral`         | Default for non-signalling pills (e.g. `tls: disabled`, `[BUNDLED]` / `[SYSTEM]` source badges in keyring) |

Application rules:
- The state colour applies as a **3–4px left-edge stripe** (inset shadow on rows; solid background on vital cells). Never as a row-background flood.
- The state colour applies to **badge stroke + text**. Never as badge fill (except the health verdict pill).
- The state colour applies to **icon dots** (a 10px filled circle).
- The state colour applies to **eyebrow inline counts** when those counts represent the relevant state (e.g. "3 LAGGING" coloured `--warn`).
- Text body remains `--ink-5` regardless of state — we never colour body prose. Keeps long passages readable.

This single discipline — left-edge stripe + stroke badges + inline coloured counts — is the visual language that defines the redesign and the one developers must implement consistently to keep the page from drifting back into vanilla-HTML territory.

## 7. Accessibility

- **Contrast**: body text on `--ink-0` is 9.8:1 (light), 11.6:1 (dark). Muted text is 4.8:1 / 5.1:1. State colours on `--ink-0`: `--ok` 5.2:1, `--warn` 4.7:1, `--crit` 6.4:1. All pass WCAG AA for normal text; most pass AAA. The few near-AA edges (`--warn` on light at 4.7) are reserved for non-essential decorations (the stripes), with state always reinforced by text label or icon.
- **Colour-blind safety**: no state is conveyed by colour alone. The verdict pill spells "HEALTHY / WATCHING / DEGRADED". Badges contain text. The aggregate notice strip names the outcome. Row state stripes are reinforced by a textual badge in the row.
- **Focus**: visible 2px outline in `--accent`, `outline-offset: 2px`. We do not rely on the browser default focus ring — it varies. Every anchor in the sticky rail and every external link gets the same ring.
- **Keyboard navigation**: the sticky rail is a regular `<nav><ul><li><a>` list, so Tab moves through it naturally. `<details>` elements are keyboard-toggleable by spec.
- **Screen readers**: state badges include `aria-label` with the full state name ("status: lagging"). The verdict pill is wrapped in `role="status"` `aria-live="polite"` so a refresh announces "Cache status: watching." Sparklines, if added in P2, get `aria-hidden="true"` and a visible numeric companion ("Hit rate: 78% over 60s") that does the actual communicating.
- **Reduced motion**: `@media (prefers-reduced-motion: reduce)` zeros all transitions and removes the first-paint reveal.

## 8. Dark mode strategy

Dark mode is not "invert the variables and ship." It's a separate composition with explicit choices:

- The dark canvas is `#15171A` — bluish-charcoal — not `#000`. Pure black on an OLED display vibrates against any non-black text and reads as cheap.
- Saturation is reduced across the semantic palette. `--crit` becomes coral-pink rather than blood-red, because saturated red on dark vibrates and fatigues the eye over a long ops session.
- The brand accent flips lightness: brick `#7A2E0A` (light) → terracotta `#E08B5A` (dark). Same hue family, opposite lightness.
- Zebra striping uses `rgba(255,255,255,0.02)` — barely-there.
- Background images / gradients: none in either mode.

Mode selection logic:

1. CSS uses `@media (prefers-color-scheme: dark)` as default.
2. A small JS toggle (≤30 lines) writes `data-theme="light"` or `data-theme="dark"` on `<html>` and persists to `localStorage`. The toggle is a single 24px sun/moon SVG button in the status bar's far right, before the JSON link.
3. The CSS variables are defined under both `:root` and `[data-theme="dark"]` / `[data-theme="light"]` so the JS override beats the media query.
4. If JS is off, dark mode follows the OS — which is the right default anyway.

## 9. Responsive strategy

Three breakpoints, no more:

- **Desktop ≥1280px (primary target)**: full 5-cell hero strip, sticky left rail at 200px wide, content at max-width 1400px. The 7-column suites table renders without truncation.
- **Sysadmin laptop 1024–1279px**: rail collapses into a horizontal anchor row that sticks to the top of the page below the status bar. Hero strip becomes 3+2 (3 cells row 1, 2 cells row 2). Suites table renders with horizontal scroll (`overflow-x: auto` on the panel) rather than truncation. Operators on a 1366×768 ThinkPad get a usable layout; the suites table scrolls, which is fine because that's the table they're scanning, not glancing at.
- **Phone ≤640px**: stacked everything. Hero strip becomes single-column. Tables become **list views**: each row becomes a small card with the column labels inline. The status verdict pill remains full-width at the top, because the on-call SRE checking this on their phone after a page wants exactly that one piece of information first. Detail panels collapse into `<details>`, default-closed except the one(s) flagged non-healthy.

The mobile list-view transformation is done in pure CSS using `display: block` on table elements at the small breakpoint, plus `::before { content: attr(data-label) }` on cells. The server-rendered template adds `data-label="Host"` etc. on each `<td>`. Total CSS cost: ~30 lines. No JS.

## 10. Performance and dependencies

- **Zero CDN, zero framework, zero npm.** All assets inline.
- **CSS**: target ≤14KB minified, inline in `<style>`. The current page is ~600B of CSS; we're spending another ~13KB on the upgrade. Acceptable.
- **JS**: target ≤6KB minified, inline at end of `<body>`. Vanilla, no transpile step. Responsibilities: time localization (existing), theme toggle, state-stripe computation, aggregate-notice rendering, sticky-rail active-section highlight, optional client-side filter/sort (P2).
- **Fonts**: zero external. System stack with explicit graceful degradation.
- **SVG**: a single inlined `<svg>` sprite at the top of the body with `<defs>` for the half-dozen icons used; `<use href="#i-arrow">` to reference. Costs ~1KB.
- **Favicon**: inline SVG data URI for a single-glyph `«` mark in `--accent`. Cost ~0.3KB.
- **First contentful paint**: under 100ms over WireGuard (RTT ~30ms, single HTML response, no blocking subrequests). The localization JS runs after parse; the brief UTC flash is acceptable and unchanged from current behaviour.
- **Auto-refresh**: an inline `<meta http-equiv="refresh" content="60">` is the boring solution. A small JS interval that updates only the live cells via re-fetch + selective swap is tempting but out of scope; the meta-refresh path has the right cost/value ratio for P0/P1.

## 11. Implementation phases

**Phase P0 — Visual refresh on existing structure (low risk, ~1 day).**
Keep the Go template's section ordering and data flow exactly as-is. Replace only the CSS, add a status-bar wrapper above the existing content, and add the inline JS that computes the verdict pill from data attributes already present in the rendered tables. The body still scrolls long; sticky rail and IA reorder come in P1. After P0, the page reads as designed at a glance even though the deep IA is unchanged. Safe to merge incrementally.

Specific P0 deliverables:
- New `<style>` block (the full palette + typography + table styles)
- New status bar markup at top with the verdict pill
- New vital-cells markup wrapping the existing Cache/Suites/GC data points
- Inline JS for verdict computation + theme toggle + duration formatting fix (the "0.013910674s" → "14 ms" cleanup)
- Aggregate notice strip above recent-adoptions, with the `Trusted keys → §Keyring` anchor when `gpg_failed` is dominant
- Empty-state copy updates, including the keyring's dual adoption-disabled / adoption-enabled-but-empty cases
- Dark mode
- Apply the full visual language to the keyring table that just landed: `chunkHex(_, 4)` on primary and subkey fingerprints, the `[BUNDLED]` / `[SYSTEM]` / `[CUSTOM]` source badges, and the `keys 4 ✓` chip in the status-bar meta area (a small inline `<span>` showing the count, with `--ok` / `--crit` tinting based on the empty-state branch)
- Replace the existing `title="..."` tooltip on the Suites "Adopted at" header with the column-header help affordance pattern (§5)

**Phase P1 — IA reorganization (moderate risk, ~2 days).**
Re-order the template sections per §3 (Suites first detail, Keyring elevated to peer of Recent adoptions, Plumbing last). Add the sticky left rail with `Keyring` as its own rail entry. Add section eyebrows with counts ("KEYRING — 4 LOADED · 4 BUNDLED · 0 CUSTOM" on a default install). Convert per-host × arch from `h3` to a peer panel. Consolidate the two repo-coverage tables into one panel with an inline arch list and a compact totals row. Wire the aggregate-notice's `#keyring` anchor — verify scroll-margin behaves through the sticky bar. This is a template restructure that touches every section, so it's its own atomic merge.

**Phase P2 — Optional client-side interactivity (low risk, ~1 day, only if operator feedback wants it).**
- Filter input above the suites table (filters by host or path substring)
- Sort by column header (click toggles asc/desc) — JS only, no server change
- Search input in the status bar (jumps to first matching row)
- "Show only failing" toggle on recent adoptions

P2 is genuinely optional and gated on whether operators ask for it. The discipline is: if the table fits on screen and Ctrl-F works, we don't need a filter input.

## 12. ASCII mockups

### Desktop, healthy state, ≥1280px

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ « apt-cacher-ultra 1.4.2          [● HEALTHY ] all systems nominal · uptime 7h 24m · keys 4 ✓ [sun][json↗]│
├──────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ │ CACHE             │ │ SUITES            │ │ ADOPTIONS         │ │ GC                │ │ ACTIVE        │ │
│ │ 72.4 GiB          │ │ 47 tracked        │ │ 50 of 50 success  │ │ 14 ms             │ │ 0 hosts       │ │
│ │ 4,193 blobs       │ │ 0 lagging         │ │ avg 1.2s          │ │ 12 min ago        │ │ idle          │ │
│ │ backlog 1,550 ↑   │ │ 0 failing         │ │ last gpg_failed:- │ │ reclaimed 318 MiB │ │ slot cap 6    │ │
│ └───────────────────┘ └───────────────────┘ └───────────────────┘ └───────────────────┘ └───────────────┘ │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ ┌──────────────────────────┐ ┌──────────────────────────────────────────────────────────────────────────┐│
│ │ SUITES                   │ │ SUITES — 47 TRACKED · 0 LAGGING                                          ││
│ │ RECENT ADOPTIONS         │ │                                                                          ││
│ │ KEYRING                  │ │ HOST                  SUITE PATH                LAST CHECK   ADOPTED ... ││
│ │ HOT URL PATHS            │ │ archive.ubuntu.com    /ubuntu/dists/noble       07:24:01     07:24:02   ││
│ │ CACHE × HOST × ARCH      │ │ archive.ubuntu.com    /ubuntu/dists/noble-upd   07:24:01     07:24:03   ││
│ │ REPOSITORY COVERAGE      │ │ download.docker.com   /linux/ubuntu/dists/...   07:24:00     07:24:01   ││
│ │ GARBAGE COLLECTION       │ │ ...                                                                      ││
│ │ ACTIVE HOSTS             │ │                                                                          ││
│ │ PLUMBING                 │ └──────────────────────────────────────────────────────────────────────────┘│
│ └──────────────────────────┘                                                                              │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│   /metrics ↗  ·  /healthz ↗  ·  build deadbeef · go1.24.0                                                │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Desktop, Keyring section detail (healthy default install + one custom key)

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ KEYRING — 5 LOADED · 4 BUNDLED · 0 SYSTEM · 1 CUSTOM                                                     │
│ Trusted GPG keys                                                                                          │
│                                                                                                          │
│ PRIMARY FINGERPRINT              USER ID                                            SOURCE       SUBKEYS  │
│ f6ec b376 2474 eda9 d21b 7022    Ubuntu Archive Automatic Signing Key (2018)        [BUNDLED]    —        │
│   8719 20d1 991b c93c            <ftpmaster@ubuntu.com>                                                   │
│ 790b f870 79a2 a82b 35bb         Ubuntu Archive Automatic Signing Key (2012)        [BUNDLED]    —        │
│   30a2 8ee4 5a1c b1bc            <ftpmaster@ubuntu.com>                                                   │
│ b8b8 0b5b 6225 6810 0e72         Debian Archive Automatic Signing Key (2025.1)      [BUNDLED]    1 subkey │
│   5b88 0ad7 4036 5be3            <ftpmaster@debian.org>                                                   │
│ 56f7 6504 0b67 6f63 1d92         Ubuntu Extended Security Maintenance (ESM) apps    [BUNDLED]    1 subkey │
│   a2c5 c5cc 5fa9 dde2            <esm-team@canonical.com>                                                 │
│ 9dc8 5822 9fc7 dd38 854a         Docker Release (CE deb) <docker@docker.com>        [CUSTOM]     —        │
│   e5a2 1cdb 76e6 e0e9            /etc/apt/keyrings/docker.gpg                                             │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### Desktop, degraded state (the gpg_failed avalanche)

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ « apt-cacher-ultra 1.4.2          [● DEGRADED] 38/50 adoptions failed: gpg_failed · uptime 7h 24m  [json↗]│
├──────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ │● CACHE            │ │● SUITES           │ │● ADOPTIONS        │ │  GC               │ │  ACTIVE       │ │
│ │ 72.4 GiB          │ │ 47 tracked        │ │ 12 / 50 success   │ │ 14 ms             │ │ 0 hosts       │ │
│ │ backlog 1,550  ↑↑ │ │ 3 lagging         │ │ 38 gpg_failed     │ │ 12 min ago        │ │ idle          │ │
│ └─(warn stripe)─────┘ └─(warn stripe)─────┘ └─(crit stripe)─────┘ └───────────────────┘ └───────────────┘ │
├──────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ ┌──────────────────────────────────────────────────────────────────────────────────────────────────────┐│
│ │● 38 of 50 recent adoptions failed: gpg_failed                                                          ││
│ │  Most recent: download.docker.com/linux/ubuntu/dists/noble — 14 min ago                               ││
│ │  Likely cause: upstream repository key changed. Run                                                    ││
│ │     apt-cacher-ultra packages list --outcome gpg_failed   to investigate.                              ││
│ └──────────────────────────────────────────────────────────────────────────────────────────────────────┘│
│                                                                                                          │
│ RECENT ADOPTIONS — 50 EVENTS · 38 GPG_FAILED · 12 SUCCESS                                                │
│ HOST                  SUITE PATH                                  OUTCOME       COMPLETED       DUR      │
││download.docker.com   /linux/ubuntu/dists/noble                  [GPG_FAILED]   07:24:11        0.4s    │
││download.docker.com   /linux/ubuntu/dists/noble-updates          [GPG_FAILED]   07:24:08        0.5s    │
│ archive.ubuntu.com   /ubuntu/dists/jammy                         [SUCCESS]      07:23:55        1.2s    │
│ ...                                                                                                      │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```
(crit-stripe rows show a 3px left edge in `--crit`; the headline aggregate strip carries the same colour)

### Phone, on-call check after a page

```
┌───────────────────────────────┐
│ « apt-cacher-ultra 1.4.2      │
│ ────────────────────────────  │
│ [● DEGRADED]                  │
│ 38/50 adoptions failed:       │
│ gpg_failed · uptime 7h 24m    │
│                               │
│ ● 38 of 50 adoptions failed   │
│   gpg_failed — most recent    │
│   download.docker.com 14m ago │
│                               │
│ ─ RECENT ADOPTIONS ──         │
│ ▸ download.docker.com         │
│   /linux/ubuntu/dists/noble   │
│   [GPG_FAILED] · 07:24 · 0.4s │
│ ▸ download.docker.com         │
│   /linux/ubuntu/dists/noble-u │
│   [GPG_FAILED] · 07:24 · 0.5s │
│ ...                           │
└───────────────────────────────┘
```

## 13. Risks and trade-offs

**Risk 1: the verdict pill computes client-side.** If JS is off, the pill stays neutral and the operator has to scroll. Trade-off taken because (a) baking the verdict into Go means duplicating threshold logic in templates, and (b) the SPEC5 JSON contract is locked — we can't add a `verdict` field without going through the schema process. Mitigation: every data attribute the pill JS reads is already in the rendered DOM, so the page is always self-describing even without the pill.

**Risk 2: tabular-nums and JetBrains Mono are not universally installed.** Fallback cascade is real, but the typographic identity weakens if it lands on a generic system mono. Acceptable — we've never relied on a webfont assumption, and the IBM Plex Mono + ui-monospace fallback chain produces a respectable look on every platform we care about. Operators tend to have developer typefaces installed.

**Risk 3: the "aggregate notice" heuristic might be wrong.** A 10% threshold for surfacing a dominant failure is a guess. If the operator's normal background failure rate is 8%, we under-surface; if it's 2%, we over-surface a transient blip. We'd want a few weeks of production data before locking the threshold. The threshold lives in one place in the inline JS and is trivially tunable; ship 10% and revise.

**Risk 4: the sticky left rail eats horizontal real estate.** On a 1280px laptop, a 200px rail leaves ~1080px for the content. The suites table needs roughly that. Below 1280, the rail goes horizontal. Trade-off: we accept the rail on desktop because navigating a long page is a real cost; horizontal anchor row below that breakpoint preserves content width.

**Risk 5: no auto-refresh-via-XHR.** Operators reload the page every minute via meta-refresh. A live-cell update would be slicker but adds complexity (event source / polling, partial DOM swap, focus preservation). For a read-only ops page, meta-refresh is the right boring solution. We'd revisit if and only if operators ask.

**Risk 6: the design is opinionated about its restraint.** Some reviewers will read "no shadows, no cards, no gradients" as "looks like 1997". The defence is that this is the right register for the audience — every visual decision here has the same justification: *it's the same instrument-panel aesthetic that the audience already trusts in their terminals and their text editors*. If the owner pushes back wanting it "warmer" or "friendlier", we'd push back politely and ask which specific operator job is being underserved by the current restraint. The aesthetic is the point.

**Risk 7: Keyring placement may read as overweight for a section that rarely changes.** A reasonable counter-argument is that GPG keys are nearly static — bundled keys ship with the binary, and most operators add at most one or two custom keys over the life of a deployment. Putting Keyring at position 3 in the IA (peer of Recent adoptions) could look like we're over-indexing on a low-mutation surface. The defence is the §2 job ordering: in the failure-investigation flow, Keyring is the first click after the gpg_failed signal fires, and "rarely changes" is a feature, not a reason to bury it. Data we'd want before committing: real adoption-failure incidence in production. If `gpg_failed` clusters turn out to be ≤1 per month across the fleet, we'd be over-promoting; if they're weekly (as they tend to be when a major distribution rotates keys), the placement justifies itself. The change is one rail-list reorder if we revise.

**Risk 8: the `[BUNDLED]` / `[SYSTEM]` / `[CUSTOM]` taxonomy is something we invented.** The JSON contract gives us `source_path` as either `embedded:<name>` or an absolute filesystem path, and that's all it gives us. The three-way split is a UI-side classification: `embedded:` → BUNDLED, path under `/usr/share/` → SYSTEM, everything else → CUSTOM. The risk is that someone deploys a key under, say, `/opt/keyrings/`, and we classify it as CUSTOM when they'd reasonably call it system. The threshold lives in one place in the template helper and is trivially tunable. Ship the simple split; revise if operators complain.

---

## Appendix — Quick wins on the way through

These don't need their own phase; they ride along with P0:

- Render durations as `14 ms` / `1.2 s` / `2m 30s` not `0.013910674s`. Helper: `formatShortDuration(float64Seconds)`.
- Render every fingerprint with `chunkHex(s, 4)` — same helper handles SHA-1 (keyring) and SHA-256 (TLS MITM CA) without branching. Spaces are emitted as regular spaces inside a `<code>` cell that already carries `tabular-nums`; no `&thinsp;` HTML entity needed.
- Add `font-variant-numeric: tabular-nums` at the table CSS level — single line, huge legibility win.
- Inline-SVG favicon (`«` glyph in `--accent`) replaces the default globe.
- `data-label` attributes on every `<td>` in every table — costs nothing in HTML weight, enables the mobile list-view CSS transformation.
- `<time>` elements already exist; we add `data-state` attributes (`data-state="lagging"` etc.) so the verdict pill and section eyebrows can scan without text parsing.
- Keyring rows emit `data-source-kind="bundled|system|custom"` so the source-badge CSS is one selector apiece and the JS-driven keyring-count chip in the status bar can compute its sub-counts (4 bundled · 0 system · 1 custom) by querying the rendered DOM rather than re-running classification.
- Replace the existing `title="..."` on the Suites "Adopted at" header with the column-header help affordance pattern in §5. Apply consistently across all table headers that today lean on `title` (audit during P0).

All of the above are server-side template changes that don't touch the Go model or the JSON contract — additive HTML structure only.
