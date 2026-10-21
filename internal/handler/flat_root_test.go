package handler

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/config"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/metrics"
	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// metricValue returns the value of the exposition series line, the
// metric name with its labels exactly as metrics.Default renders it
// (e.g. `acu_serve_snapshot_index_target_404_total{architecture="all"}`);
// 0 when the series does not exist yet. The registry is package-global, so
// callers compare before and after.
func metricValue(t *testing.T, series string) float64 {
	t.Helper()
	var buf bytes.Buffer
	metrics.Default.Render(&buf)
	for _, line := range strings.Split(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return f
		}
	}
	return 0
}

// Spec C2 (plan design note 2): a flat repository at the host root —
// `deb https://h/ /` and `deb https://h ./` — gets no suite: its files are
// served as before (Phase 1) and the handler logs WARN
// flat_host_root_unsupported at most once a minute process-wide, each line
// counting the requests its minute suppressed. No row is created.
func TestServeHTTP_FlatHostRootIsUnsupportedAndSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parser, err := proxy.New(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		c := simtest.OpenCache(t)
		logs := &strictLogBuf{}
		h, err := New(Config{Parser: parser, Cache: c, Fetch: &recFetcher{body: rowInRelease},
			Logger: slog.New(slog.NewJSONHandler(logs, nil)), HostLimiter: hostsem.New(4), AdoptionEnabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		get := func(path string) {
			t.Helper()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://root.example"+path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200 (served as before)", path, rec.Code)
			}
		}
		get("/InRelease")
		get("/./InRelease")
		get("/Packages.gz")
		get("/repo/InRelease") // a flat suite: no line
		recs := logs.find("flat_host_root_unsupported")
		if len(recs) != 1 || recs[0]["level"] != "WARN" || recs[0]["path"] != "/InRelease" ||
			recs[0]["canonical_host"] != "root.example" || recs[0]["suppressed"] != float64(0) {
			t.Fatalf("flat_host_root_unsupported = %v, want one WARN for /InRelease, suppressed 0", recs)
		}
		time.Sleep(time.Minute)
		get("/./Packages.xz")
		recs = logs.find("flat_host_root_unsupported")
		if len(recs) != 2 || recs[1]["path"] != "/./Packages.xz" || recs[1]["suppressed"] != float64(2) {
			t.Fatalf("flat_host_root_unsupported = %v, want a second line a minute later, suppressed 2", recs)
		}
		get("/Release")
		time.Sleep(time.Minute)
		get("/InRelease")
		if recs = logs.find("flat_host_root_unsupported"); len(recs) != 3 || recs[2]["suppressed"] != float64(1) {
			t.Fatalf("flat_host_root_unsupported = %v, want a third line counting only its own minute (suppressed 1)", recs)
		}
		all, err := c.ListSuites(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].SuitePath != "/repo" {
			t.Fatalf("suite rows = %+v, want only /repo's", all)
		}
	})
}

// denyHostFetcher is a recFetcher whose allowlist refuses one host.
type denyHostFetcher struct {
	*recFetcher
	deny string
}

func (f denyHostFetcher) HostAllowed(host string) bool { return host != f.deny }

// Plan design note 2's flat_host_root_unsupported, two more properties
// (the Task 6 review's minors): a host-root request to a host the
// allowlist forbids is refused (403 on a miss) and never takes the
// minute's one line, whose canonical_host and "served from the ordinary
// cache" would then be wrong; and the limit is process-wide, not per host
// (the AIDEV-NOTE's flood argument), so a second host's host-root request
// inside the minute is folded into the next line's suppressed count.
func TestServeHTTP_FlatHostRootLineNamesAnAllowedHostOncePerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parser, err := proxy.New(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		logs := &strictLogBuf{}
		h, err := New(Config{Parser: parser, Cache: simtest.OpenCache(t),
			Fetch:  denyHostFetcher{recFetcher: &recFetcher{body: rowInRelease}, deny: "forbidden.example"},
			Logger: slog.New(slog.NewJSONHandler(logs, nil)), HostLimiter: hostsem.New(4), AdoptionEnabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		get := func(url string, want int) {
			t.Helper()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
			if rec.Code != want {
				t.Fatalf("%s = %d, want %d", url, rec.Code, want)
			}
		}
		get("http://forbidden.example/InRelease", http.StatusForbidden)
		if recs := logs.find("flat_host_root_unsupported"); len(recs) != 0 {
			t.Fatalf("flat_host_root_unsupported = %v for a forbidden host, want none", recs)
		}
		get("http://root.example/InRelease", http.StatusOK)
		get("http://other.example/InRelease", http.StatusOK) // another host, the same minute
		recs := logs.find("flat_host_root_unsupported")
		if len(recs) != 1 || recs[0]["canonical_host"] != "root.example" || recs[0]["suppressed"] != float64(0) {
			t.Fatalf("flat_host_root_unsupported = %v, want one line, for root.example, suppressed 0", recs)
		}
		time.Sleep(time.Minute)
		get("http://other.example/./InRelease", http.StatusOK)
		recs = logs.find("flat_host_root_unsupported")
		if len(recs) != 2 || recs[1]["canonical_host"] != "other.example" || recs[1]["suppressed"] != float64(1) {
			t.Fatalf("flat_host_root_unsupported = %v, want a second line a minute later counting other.example's (suppressed 1)", recs)
		}
	})
}

// Spec C5 on the handler side: the snapshot_index_target_404 alert is
// layout-aware. A flat suite's root Packages* or Sources* missing from its
// snapshot raises it, labelled "all" or "source" (the flat arch labels,
// plan design note 4), as does its `./` twin's; under a dists suite a root
// Packages.gz is no IndexTarget and raises nothing.
func TestServeHTTP_FlatSnapshotMissIndexTargetRaisesAlert(t *testing.T) {
	logs := &strictLogBuf{}
	h := newTestHandlerWithServe(t, config.ServeConfig{}, slog.New(slog.NewJSONHandler(logs, nil)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("upstream")) }))
	defer srv.Close()
	scheme, host, port := splitURL(t, srv.URL)
	for _, suite := range []string{"/flat", "/flat/.", "/dists/noble"} {
		blob := writeBlob(t, h, simtest.ClearsignedFixture("InRelease of "+suite))
		commitInlineSnapshot(t, h, scheme, host, suite, blob, []cache.SnapshotMember{
			{Path: "InRelease", BlobHash: blob, DeclaredSHA256: blob},
		}, nil)
	}
	for _, tc := range []struct {
		path, arch string
	}{
		{"/flat/Packages.xz", "all"},
		{"/flat/Sources.gz", "source"},
		{"/flat/./Packages", "all"},
		{"/dists/noble/Packages.gz", ""},
		{"/flat/Packages.diff/Index", ""},
	} {
		series := `acu_serve_snapshot_index_target_404_total{architecture="` + tc.arch + `"}`
		before := metricValue(t, series)
		logs.reset()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newProxyReqHostPort("GET", scheme, host+port, tc.path))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (not in snapshot)", tc.path, rec.Code)
		}
		recs := logs.find("snapshot_index_target_404")
		if tc.arch == "" {
			if len(recs) != 0 {
				t.Errorf("%s raised %v, want no alert", tc.path, recs)
			}
			continue
		}
		if len(recs) != 1 || recs[0]["architecture"] != tc.arch {
			t.Errorf("%s: snapshot_index_target_404 = %v, want one, architecture %q", tc.path, recs, tc.arch)
		}
		if got := metricValue(t, series) - before; got != 1 {
			t.Errorf("%s: %s grew by %v, want 1", tc.path, series, got)
		}
	}
}
