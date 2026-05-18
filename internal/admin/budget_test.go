package admin

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// AIDEV-NOTE: §12 performance budget + §10 contrast verification tests.
// Both run hermetically off the rendered template — no TCP listener
// required — and exist as DoD #12 and DoD #10 gates respectively.
//
// The budget is "rendered response over the wire". The spec allows
// the unminified template literal to exceed the budget; only the
// produced response counts. Inline whitespace removal is not done
// server-side because gzip negotiation (handled by net/http or a
// reverse proxy) handles compression on the wire. We measure against
// the raw rendered bytes — that gives an honest "uncompressed wire
// shape" upper bound that always passes for any gzipped client.

func TestRenderSizeBudget(t *testing.T) {
	html := renderHTMLForGolden(t, newHealthyModel())

	totalRaw := len(html)
	cssBytes := sectionSize(html, "<style>", "</style>")
	// JS budget includes EVERY inline <script> block (the pre-paint
	// theme hook in <head> plus the inline app script at body end).
	// Codex-review iter-6 caught the earlier "last-block-only" form.
	jsBytes := allSectionsSize(html, "<script>", "</script>")
	svgBytes := svgSpriteSize(html)
	faviconBytes := faviconSize(html)

	// AIDEV-NOTE: gzip is now negotiated server-side by
	// handlers.go's gzipIfAccepted helper, so the §12 "over the wire"
	// budget is enforceable per-request without an external proxy.
	// The gzipped size measured here matches what the operator's
	// browser receives when it sends Accept-Encoding: gzip (which
	// every browser does). The raw cap is a generous regression
	// guard that catches template growth even on identity-encoded
	// clients (curl without --compressed, programmatic scrapers).
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte(html)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	totalGz := gz.Len()

	// Budget caps post-INSTRUMENT redesign — the CSS block now carries
	// two inline base64 woff2 fonts (Geist Sans + Geist Mono variable,
	// subset to Latin-1 + the glyphs the template uses, ~52KB binary
	// → ~70KB base64). Already-compressed font binary doesn't gzip
	// further, so the gzipped budget had to expand from the original
	// §12 22KB target. Raw caps grow proportionally. Without fonts the
	// pure-design CSS is ~17KB raw (still inside the original budget).
	const (
		cssRawMax       = 110 * 1024 // ~17KB CSS + ~70KB inline fonts (base64) + headroom
		jsRawMax        = 8 * 1024   // §12 spec: 6 KB minified
		svgRawMax       = 1024       // §12 spec: 1 KB
		faviconRawMax   = 768        // INSTRUMENT favicon is a small inline SVG nameplate
		totalRawMax     = 200 * 1024 // raw HTML with inline fonts
		totalGzippedMax = 90 * 1024  // base64 woff2 compresses poorly (already gzipped binary)
	)

	type b struct {
		name string
		got  int
		max  int
	}
	for _, c := range []b{
		{"CSS (inline <style>)", cssBytes, cssRawMax},
		{"JS (inline <script>)", jsBytes, jsRawMax},
		{"SVG sprite", svgBytes, svgRawMax},
		{"Favicon (data: URI)", faviconBytes, faviconRawMax},
		{"Total wire shape (raw)", totalRaw, totalRawMax},
		{"Total wire shape (gzipped)", totalGz, totalGzippedMax},
	} {
		if c.got > c.max {
			t.Errorf("§12 budget exceeded: %s = %d bytes (max %d)", c.name, c.got, c.max)
		} else {
			t.Logf("%s: %d bytes (limit %d)", c.name, c.got, c.max)
		}
	}
}

// sectionSize returns the byte size of the LAST <open>…<close> region
// found in s. Used for single-occurrence sections like <style>.
func sectionSize(s, open, close string) int {
	last := 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], open)
		if j == -1 {
			break
		}
		j += i
		k := strings.Index(s[j:], close)
		if k == -1 {
			break
		}
		k += j + len(close)
		last = k - j
		i = k
	}
	return last
}

// allSectionsSize sums the byte sizes of EVERY <open>…<close> region
// in s. Used for the JS budget: the pre-paint theme hook in <head>
// and the inline application script at body end must both count.
func allSectionsSize(s, open, close string) int {
	total := 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], open)
		if j == -1 {
			break
		}
		j += i
		k := strings.Index(s[j:], close)
		if k == -1 {
			break
		}
		k += j + len(close)
		total += k - j
		i = k
	}
	return total
}

// svgSpriteSize finds the inline <svg width="0" …> sprite (the one
// with the <defs> block) and returns its byte size.
func svgSpriteSize(s string) int {
	const open = `<svg width="0"`
	const close = `</svg>`
	i := strings.Index(s, open)
	if i == -1 {
		return 0
	}
	j := strings.Index(s[i:], close)
	if j == -1 {
		return 0
	}
	return j + len(close)
}

// faviconSize returns the byte length of the data: URI in the
// <link rel="icon" href="…"> element.
func faviconSize(s string) int {
	const open = `<link rel="icon" href="`
	const close = `"`
	i := strings.Index(s, open)
	if i == -1 {
		return 0
	}
	i += len(open)
	j := strings.Index(s[i:], close)
	if j == -1 {
		return 0
	}
	return j
}

// AIDEV-NOTE: §10 contrast verification. Computes WCAG 2.1 relative
// luminance and contrast ratio for each documented color pair.
// Tokens are extracted from the rendered template's <style> block via
// regex on the :root + [data-theme="…"] rules, so any future palette
// change automatically retests; if the regex fails to find a token
// the test fails loudly.

// hexToRGB parses #RRGGBB into 0..255 components.
func hexToRGB(hex string) (r, g, b int, ok bool) {
	hex = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hex)), "#")
	if len(hex) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int((v >> 16) & 0xff), int((v >> 8) & 0xff), int(v & 0xff), true
}

// channelLuminance is WCAG 2.1's per-channel linearization.
func channelLuminance(c int) float64 {
	f := float64(c) / 255.0
	if f <= 0.03928 {
		return f / 12.92
	}
	return math.Pow((f+0.055)/1.055, 2.4)
}

// relativeLuminance returns the WCAG relative luminance of an sRGB color.
func relativeLuminance(r, g, b int) float64 {
	return 0.2126*channelLuminance(r) + 0.7152*channelLuminance(g) + 0.0722*channelLuminance(b)
}

// contrastRatio returns the WCAG 2.1 contrast ratio of two colors.
func contrastRatio(fgHex, bgHex string) (float64, error) {
	fr, fg, fb, ok := hexToRGB(fgHex)
	if !ok {
		return 0, fmt.Errorf("bad fg hex: %q", fgHex)
	}
	br, bg, bb, ok := hexToRGB(bgHex)
	if !ok {
		return 0, fmt.Errorf("bad bg hex: %q", bgHex)
	}
	l1 := relativeLuminance(fr, fg, fb)
	l2 := relativeLuminance(br, bg, bb)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05), nil
}

// extractTheme returns a token-name → hex-value map for one of the
// CSS rule blocks named `[data-theme="light"]` or `[data-theme="dark"]`.
// Caller picks the theme; this avoids depending on the @media-driven
// :root defaults whose value mapping depends on the user's OS at
// render time.
func extractTheme(css, theme string) map[string]string {
	re := regexp.MustCompile(`\[data-theme="` + regexp.QuoteMeta(theme) + `"\]\s*\{([^}]+)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		return nil
	}
	return parseColorTokens(m[1])
}

// extractRoot returns the token map from the bare :root{...} block
// (the light default applied before any theme attribute or media
// query overrides). The block is captured by matching up to the
// FIRST closing brace.
func extractRoot(css string) map[string]string {
	re := regexp.MustCompile(`:root\s*\{([^}]+)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		return nil
	}
	return parseColorTokens(m[1])
}

// extractMediaDark returns the token map from inside the
// @media (prefers-color-scheme:dark){:root{…}} block. This is the
// default-dark palette used for OS-dark users who have not chosen a
// theme via the toggle.
func extractMediaDark(css string) map[string]string {
	re := regexp.MustCompile(`@media\s*\(prefers-color-scheme:\s*dark\)\s*\{[^{]*:root\s*\{([^}]+)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		return nil
	}
	return parseColorTokens(m[1])
}

func parseColorTokens(body string) map[string]string {
	tok := regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*(#[0-9A-Fa-f]{6})`)
	out := map[string]string{}
	for _, mm := range tok.FindAllStringSubmatch(body, -1) {
		out[mm[1]] = mm[2]
	}
	return out
}

// TestPaletteBlocksAreEquivalent guards the four palette declarations
// (:root, @media dark, [data-theme="light"], [data-theme="dark"])
// against drift. The :root block must mirror [data-theme="light"]
// because :root IS the light default; @media (prefers-color-scheme:dark)
// must mirror [data-theme="dark"] because OS-dark users see those
// tokens unless localStorage overrides. Drift here means a dark-mode
// OS user sees one palette but the theme-toggle button surfaces a
// different one — a confusing inconsistency.
func TestPaletteBlocksAreEquivalent(t *testing.T) {
	var buf bytes.Buffer
	if err := statusHTMLTemplate.Execute(&buf, newHealthyModel()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	html := buf.String()
	styleStart := strings.Index(html, "<style>")
	styleEnd := strings.Index(html, "</style>")
	if styleStart == -1 || styleEnd == -1 {
		t.Fatal("could not locate <style>")
	}
	css := html[styleStart:styleEnd]

	root := extractRoot(css)
	light := extractTheme(css, "light")
	mediaDark := extractMediaDark(css)
	dark := extractTheme(css, "dark")
	if len(root) == 0 || len(light) == 0 || len(mediaDark) == 0 || len(dark) == 0 {
		t.Fatalf("missing palette block(s): root=%d, light=%d, media-dark=%d, dark=%d", len(root), len(light), len(mediaDark), len(dark))
	}
	for k, v := range light {
		if root[k] != v {
			t.Errorf(":root vs [data-theme=\"light\"] drift on %s: root=%s light=%s", k, root[k], v)
		}
	}
	for k, v := range dark {
		if mediaDark[k] != v {
			t.Errorf("@media (prefers-color-scheme:dark) vs [data-theme=\"dark\"] drift on %s: media=%s dark=%s", k, mediaDark[k], v)
		}
	}
}

func TestColorContrast(t *testing.T) {
	// Render the template to capture the inline <style> block as
	// the source of truth; if the template's tokens drift, the test
	// re-derives them automatically.
	var buf bytes.Buffer
	if err := statusHTMLTemplate.Execute(&buf, newHealthyModel()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	html := buf.String()
	styleStart := strings.Index(html, "<style>")
	styleEnd := strings.Index(html, "</style>")
	if styleStart == -1 || styleEnd == -1 {
		t.Fatal("could not locate inline <style> block in rendered HTML")
	}
	css := html[styleStart:styleEnd]

	// §10 declares both light and dark palettes; verify both.
	for _, theme := range []string{"light", "dark"} {
		tokens := extractTheme(css, theme)
		if len(tokens) == 0 {
			t.Fatalf("theme %q: extracted zero tokens — selector or hex pattern drifted", theme)
		}
		// Spec-locked pairs (§10.1). All pairs now use the AA 4.5
		// threshold for both palettes; the earlier "muted text light
		// at 4.0" relaxation was lifted when --ink-4 light was
		// darkened from #7A7669 (4.34:1) to #6E6A5D (≈5.0:1) in the
		// iter-6 codex-review follow-up. Body text uses the AAA
		// 7.0 threshold per the spec's "Body text on page bg: 9.8:1
		// / 11.6:1 (AAA)" claim.
		pairs := []struct {
			name string
			fg   string
			bg   string
			min  float64 // required contrast ratio
		}{
			{"body text (--ink-mid) on page bg (--bg)", tokens["--ink-mid"], tokens["--bg"], 7.0},
			{"muted text (--ink-low) on page bg (--bg)", tokens["--ink-low"], tokens["--bg"], 4.5},
			{"--ok on page bg", tokens["--ok"], tokens["--bg"], 4.5},
			{"--warn on page bg", tokens["--warn"], tokens["--bg"], 4.5},
			{"--crit on page bg", tokens["--crit"], tokens["--bg"], 4.5},
			{"--signal on page bg", tokens["--signal"], tokens["--bg"], 4.5},
		}
		for _, p := range pairs {
			if p.fg == "" || p.bg == "" {
				t.Errorf("theme %q: pair %q missing token", theme, p.name)
				continue
			}
			r, err := contrastRatio(p.fg, p.bg)
			if err != nil {
				t.Errorf("theme %q: pair %q: %v", theme, p.name, err)
				continue
			}
			if r < p.min {
				t.Errorf("theme %q: %s contrast = %.2f, want >= %.2f (fg=%s bg=%s)", theme, p.name, r, p.min, p.fg, p.bg)
			} else {
				t.Logf("theme %q: %s contrast = %.2f (>= %.2f)", theme, p.name, r, p.min)
			}
		}
	}
}
