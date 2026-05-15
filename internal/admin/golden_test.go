package admin

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// AIDEV-NOTE: §14.2 golden tests for the redesigned status template.
// Per the spec these tests assert on the SERVER-RENDERED INITIAL
// MARKUP (data-* attributes, structural elements, empty-state
// branches) — never on JS-computed strings (verdict pill label,
// aggregate-notice headline). The Go test harness does not run JS,
// so the pill stays "STATUS" and the notice mount stays empty.
//
// Each fixture composes a representative htmlRenderModel and renders
// statusHTMLTemplate against it via the same code path renderStatus's
// HTML branch uses. Listener-bound startAdminServer is intentionally
// not used so these tests run in sandboxed CI environments without
// network access.

// renderHTMLForGolden runs statusHTMLTemplate.Execute against the
// supplied wrapper and returns the bytes. Mirrors renderStatus's HTML
// branch; if that path adds steps (a header, post-processing, etc.)
// they must be reflected here too.
func renderHTMLForGolden(t *testing.T, m htmlRenderModel) string {
	t.Helper()
	var buf bytes.Buffer
	if err := statusHTMLTemplate.Execute(&buf, m); err != nil {
		t.Fatalf("statusHTMLTemplate.Execute: %v", err)
	}
	return buf.String()
}

func TestGoldenHealthy(t *testing.T) {
	m := newHealthyModel()
	html := renderHTMLForGolden(t, m)

	mustContain(t, html,
		"<title>apt-cacher-ultra status</title>",
		`<meta http-equiv="refresh" content="60">`,
		`data-vital="cache" data-state="ok"`,
		`data-vital="suites" data-state="ok"`,
		`data-vital="adoptions" data-state="ok"`,
		`data-vital="gc" data-state="ok"`,
		`data-vital="active" data-state="ok"`,
		// Pre-paint theme hook in <head>.
		"acu-theme",
		// noscript fallback verdict.
		`id="verdict-label">STATUS<`,
		// Aggregate notice mount is present but empty.
		`id="adoptions-notice" class="notice-mount"`,
		// keys-chip carries the count attribute the JS reads.
		`data-keyring-count="`,
	)
	// No vital cell should carry crit or warn in the healthy fixture.
	// Scoping to `data-vital="…" data-state=` avoids matching CSS
	// selectors inside the inline <style> block which legitimately
	// reference `data-state="crit"` etc.
	for _, k := range []string{"cache", "suites", "adoptions", "gc", "active"} {
		for _, bad := range []string{"crit", "warn"} {
			needle := `data-vital="` + k + `" data-state="` + bad + `"`
			if strings.Contains(html, needle) {
				t.Errorf("healthy fixture: vital %q unexpectedly carries data-state=%q", k, bad)
			}
		}
	}
	// Healthy fixture seeds 5 keys (4 bundled + 1 custom). Eyebrow
	// counts come from countBundled/Custom helpers via the template.
	if !strings.Contains(html, `data-keyring-count="5"`) {
		t.Errorf("expected keys-chip count of 5 in healthy fixture; excerpt: %s", excerpt(html, "data-keyring-count=", 64))
	}
}

func TestGoldenWarmingUp(t *testing.T) {
	m := newWarmingUpModel()
	html := renderHTMLForGolden(t, m)

	// Body carries the JS-readable warm-up signals.
	mustContain(t, html,
		`data-uptime-seconds="60"`, // < 300
		`data-gc-runs="0"`,         // no GC run yet
		// Empty-state branch: "NO GC RUN YET"
		"NO GC RUN YET",
	)
	// GC vital should be stale; cache is also stale (no bytes yet).
	mustContain(t, html,
		`data-vital="gc" data-state="stale"`,
		`data-vital="cache" data-state="stale"`,
	)
	// Notice mount present, empty.
	mustContain(t, html, `id="adoptions-notice" class="notice-mount"`)
	// Adoptions panel — empty since startup branch.
	mustContain(t, html, "Empty since this process started.")
}

func TestGoldenWatchingLagging(t *testing.T) {
	m := newWatchingLaggingModel(3)
	html := renderHTMLForGolden(t, m)

	mustContain(t, html, `data-vital="suites" data-state="warn"`)
	// 3 suite rows with data-state="warn"
	if got := strings.Count(html, `<tr data-state="warn">`); got < 3 {
		t.Errorf("want at least 3 lagging-warn suite rows, got %d", got)
	}
	// The lagging annotation is rendered.
	mustContain(t, html, `class="lagging">(lagging`)
}

func TestGoldenDegradedGPG(t *testing.T) {
	m := newDegradedGPGModel()
	html := renderHTMLForGolden(t, m)

	// Adoptions vital marks crit.
	mustContain(t, html, `data-vital="adoptions" data-state="crit"`)
	// At least one adoption row with data-outcome="gpg_failed".
	if !strings.Contains(html, `data-outcome="gpg_failed"`) {
		t.Errorf("missing data-outcome=gpg_failed in adoption rows")
	}
	// Per spec: notice-mount placeholder must be present in markup
	// (JS fills it at runtime — Go test does not assert filled body).
	mustContain(t, html, `id="adoptions-notice" class="notice-mount"`)
	// Outcome badge must render with crit class for non-success.
	mustContain(t, html, `<span class="b b--crit">gpg_failed</span>`)
}

func TestGoldenKeyringEmptyDisabled(t *testing.T) {
	m := newHealthyModel()
	m.AdoptionEnabled = false
	m.Keyring = nil
	html := renderHTMLForGolden(t, m)

	mustContain(t, html,
		"ADOPTION DISABLED",
		// keys-chip carries adoption-enabled=false.
		`data-adoption-enabled="false"`,
		// Empty block (non-crit) — class="empty" without empty--crit.
		`<div class="empty"><div class="empty__head">ADOPTION DISABLED`,
	)
	// Must NOT show the crit empty branch.
	mustNotContain(t, html, "NO GPG KEYS LOADED")
}

func TestGoldenKeyringEmptyEnabled(t *testing.T) {
	m := newHealthyModel()
	m.AdoptionEnabled = true
	m.Keyring = nil
	html := renderHTMLForGolden(t, m)

	mustContain(t, html,
		"NO GPG KEYS LOADED",
		`data-adoption-enabled="true"`,
		// Empty block carries the crit modifier class.
		`empty empty--crit`,
		// keys-chip is also tinted crit because adoption is enabled
		// but no keys are loaded (§5.1.1).
		`class="keys-chip"`,
	)
	if !strings.Contains(html, `data-state="crit"`) {
		t.Errorf("keys-chip should carry data-state=crit when adoption enabled and no keys")
	}
}

func TestGoldenKeyringFull(t *testing.T) {
	m := newHealthyModel() // healthy fixture has 4 bundled + 1 custom
	html := renderHTMLForGolden(t, m)

	// data-source-kind on each row.
	if got := strings.Count(html, `data-source-kind="bundled"`); got < 4 {
		t.Errorf("want at least 4 bundled rows, got %d", got)
	}
	if got := strings.Count(html, `data-source-kind="custom"`); got != 1 {
		t.Errorf("want 1 custom row, got %d", got)
	}
	// Source badge labels.
	mustContain(t, html, `<span class="src src--bundled">BUNDLED</span>`)
	mustContain(t, html, `<span class="src src--custom">CUSTOM</span>`)
	// Fingerprint is chunked into 4-hex groups separated by single
	// space (chunkHex output rendered inside .fp span).
	mustContain(t, html, `<span class="fp">f6ec b376 2474 eda9 d21b 7022 8719 20d1 991b c93c</span>`)
	// Eyebrow counts.
	mustContain(t, html,
		`data-keyring-count="5"`,
		`data-keyring-bundled="4"`,
		`data-keyring-system="0"`,
		`data-keyring-custom="1"`,
	)
}

// --- model fixture builders --------------------------------------------------

func newHealthyModel() htmlRenderModel {
	gcLast := int64(1_700_006_800) // 200s before "now" below
	return htmlRenderModel{
		statusModel: statusModel{
			Process: processInfo{
				Version: "v0.test", VCSRevision: "deadbeef", GoVersion: "go-test",
				StartedUnixTime: 1_700_000_000, UptimeSeconds: 7000,
			},
			Cache: cacheInfo{Dir: "/var/cache/apt-cacher-ultra", BytesUsed: 1_073_741_824, BlobCount: 100, URLPathCount: 200, ZeroRefcountBacklog: 5},
			CacheSummary: cacheSummary{ByHost: map[string]cacheSummaryHost{
				"archive.ubuntu.com": {ByArchitecture: map[string]cacheSummaryArchEntry{
					"amd64": {PackageHashCount: 100, BlobCount: 80, BlobBytes: 100_000_000},
				}},
			}},
			RepoCoverage: repoCoverageInfo{
				ArchitecturesSeen:    []string{"amd64"},
				ArchitecturesFilter:  []string{},
				SnapshotsWithSources: 1,
				PackageHashRows:      packageHashRowsInfo{Binary: 100, Total: 100},
			},
			Listeners:       []listenerInfo{{Role: "proxy", Addr: "0.0.0.0:3142"}, {Role: "admin", Addr: "127.0.0.1:6789"}},
			TLSMITM:         &tlsMITMInfo{Enabled: false},
			Suites:          []suiteEntry{{Host: "archive.ubuntu.com", SuitePath: "dists/noble"}},
			GC:              &gcInfo{LastRunUnixTime: &gcLast, LastRunPhase: "periodic", LastRunDurationSeconds: 0.014},
			HotURLPaths:     []hotURLEntry{},
			RecentAdoptions: []adoptionEntry{{Host: "archive.ubuntu.com", SuitePath: "dists/noble", Outcome: "success", CompletedUnixTime: 1_700_006_500, DurationSeconds: 1.2}},
			ActiveHosts:     []activeHostInfo{},
			Keyring: []keyringEntry{
				{PrimaryFingerprint: "F6ECB3762474EDA9D21B7022871920D1991BC93C", PrimaryUID: "Ubuntu Archive Automatic Signing Key", SourcePath: "embedded:ubuntu-archive-keyring.gpg", SubkeyFingerprints: []string{}},
				{PrimaryFingerprint: "790BF87079A2A82B35BB30A28EE45A1CB1BCF1A5", PrimaryUID: "Ubuntu Archive 2012", SourcePath: "embedded:ubuntu-archive-keyring.gpg", SubkeyFingerprints: []string{}},
				{PrimaryFingerprint: "B8B80B5B62256810E725B880AD740365BE388AB1", PrimaryUID: "Debian Archive", SourcePath: "embedded:debian-archive-keyring.gpg", SubkeyFingerprints: []string{}},
				{PrimaryFingerprint: "56F765040B676F631D92A2C5C5CC5FA9DDE20107", PrimaryUID: "Ubuntu ESM apps", SourcePath: "embedded:ubuntu-pro-esm-apps.gpg", SubkeyFingerprints: []string{}},
				{PrimaryFingerprint: "9DC858229FC7DD38854AE5A21CDB76E6E0E9A91E", PrimaryUID: "Docker Release", SourcePath: "/etc/apt/keyrings/docker.gpg", SubkeyFingerprints: []string{}},
			},
		},
		AdoptionEnabled:   true,
		GCIntervalSeconds: 3600,
	}
}

func newWarmingUpModel() htmlRenderModel {
	return htmlRenderModel{
		statusModel: statusModel{
			Process:         processInfo{Version: "v0.test", VCSRevision: "deadbeef", GoVersion: "go-test", StartedUnixTime: 1_700_000_000, UptimeSeconds: 60},
			Cache:           cacheInfo{Dir: "/var/cache/apt-cacher-ultra"},
			CacheSummary:    cacheSummary{ByHost: map[string]cacheSummaryHost{}},
			RepoCoverage:    repoCoverageInfo{ArchitecturesSeen: []string{}, ArchitecturesFilter: []string{}},
			Listeners:       []listenerInfo{{Role: "admin", Addr: "127.0.0.1:6789"}},
			TLSMITM:         &tlsMITMInfo{Enabled: false},
			Suites:          []suiteEntry{},
			GC:              &gcInfo{}, // LastRunUnixTime nil → "NO GC RUN YET"
			HotURLPaths:     []hotURLEntry{},
			RecentAdoptions: []adoptionEntry{},
			ActiveHosts:     []activeHostInfo{},
			Keyring:         []keyringEntry{},
		},
		AdoptionEnabled:   true, // adoption enabled but no keys yet — will trip keyring crit
		GCIntervalSeconds: 3600,
	}
}

func newWatchingLaggingModel(nLag int) htmlRenderModel {
	m := newHealthyModel()
	m.Suites = make([]suiteEntry, 0, nLag+1)
	for i := 0; i < nLag; i++ {
		seenAt := int64(1_700_005_000)
		successAt := int64(1_700_001_000) // ~1h ago — under 24h, so warn not crit
		m.Suites = append(m.Suites, suiteEntry{
			Host:                          fmt.Sprintf("host%d.example.com", i),
			SuitePath:                     "dists/noble",
			LastSuccessUnixTime:           &successAt,
			InReleaseChangeSeenAtUnixTime: &seenAt,
			Lagging:                       "(lagging 1h 6m)",
		})
	}
	// Add one healthy suite so the panel still has a non-warn baseline.
	m.Suites = append(m.Suites, suiteEntry{Host: "archive.ubuntu.com", SuitePath: "dists/noble"})
	return m
}

func newDegradedGPGModel() htmlRenderModel {
	m := newHealthyModel()
	// 6 fail / 4 success = 60% non-success → crit
	m.RecentAdoptions = make([]adoptionEntry, 10)
	for i := 0; i < 6; i++ {
		m.RecentAdoptions[i] = adoptionEntry{Host: "download.docker.com", SuitePath: "dists/noble", Outcome: "gpg_failed", CompletedUnixTime: 1_700_006_000 + int64(i)*10, DurationSeconds: 0.4}
	}
	for i := 6; i < 10; i++ {
		m.RecentAdoptions[i] = adoptionEntry{Host: "archive.ubuntu.com", SuitePath: "dists/noble", Outcome: "success", CompletedUnixTime: 1_700_005_500 + int64(i)*10, DurationSeconds: 1.1}
	}
	return m
}

// --- helpers ----------------------------------------------------------------

func mustContain(t *testing.T, html string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(html, n) {
			t.Errorf("rendered HTML missing %q\nexcerpt around: %s", n, excerptAround(html, n, 200))
		}
	}
}

func mustNotContain(t *testing.T, html string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(html, n) {
			t.Errorf("rendered HTML unexpectedly contains %q\nexcerpt: %s", n, excerptAround(html, n, 200))
		}
	}
}

func excerpt(html, marker string, n int) string {
	i := strings.Index(html, marker)
	if i == -1 {
		return "(marker not present)"
	}
	end := i + n
	if end > len(html) {
		end = len(html)
	}
	return html[i:end]
}

func excerptAround(html, needle string, n int) string {
	if needle == "" {
		return ""
	}
	for try := len(needle); try > 1; try-- {
		i := strings.Index(html, needle[:try])
		if i == -1 {
			continue
		}
		start := max(0, i-n/2)
		end := i + try + n/2
		if end > len(html) {
			end = len(html)
		}
		return fmt.Sprintf("…%s…", html[start:end])
	}
	return "(no overlap with rendered HTML)"
}
