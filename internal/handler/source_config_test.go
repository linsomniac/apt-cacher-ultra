package handler

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

func TestServeHTTP_ExcludedSourcesDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name              string
		arches            []string
		enabled, wantHint bool
	}{
		{"binary only", []string{"amd64"}, true, true},
		{"explicit source", []string{"amd64", "source"}, true, false},
		{"default all", nil, true, false},
		{"adoption disabled", []string{"amd64"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			deps := newTestHandler(t, nil, nil)
			defer deps.Close()
			h, err := New(Config{
				Parser: deps.parser, Cache: deps.cache, Fetch: deps.fetch, HostLimiter: deps.sem,
				Logger:          slog.New(slog.NewJSONHandler(&logs, nil)),
				AdoptionEnabled: tc.enabled, AdoptionArchitectures: tc.arches,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			for _, suite := range []string{"/dists/stable", "/flat"} {
				blob := writeBlob(t, h, simtest.ClearsignedFixture("Origin: test\n"))
				commitInlineSnapshot(t, h, "http", "127.0.0.1", suite, blob, []cache.SnapshotMember{
					{Path: "InRelease", BlobHash: blob, DeclaredSHA256: blob},
				}, nil)
				if strings.Contains(logs.String(), "arch_not_in_allowlist") {
					t.Fatal("configuration warning without a source-index request")
				}
				path := suite + "/Sources.gz"
				if suite == "/dists/stable" {
					path = suite + "/main/source/Sources.gz"
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, proxyReq("GET", "http://127.0.0.1", path))
				if w.Code != 404 {
					t.Fatalf("%s: status = %d, want 404", path, w.Code)
				}
				out := logs.String()
				if !strings.Contains(out, `"msg":"snapshot_index_target_404"`) || !strings.Contains(out, `"architecture":"source"`) {
					t.Fatalf("missing source-index alert: %s", out)
				}
				if got := strings.Contains(out, `"reason":"arch_not_in_allowlist"`); got != tc.wantHint {
					t.Fatalf("configuration reason present = %v, want %v: %s", got, tc.wantHint, out)
				}
				if got := strings.Contains(out, "adoption.architectures for deb-src clients"); got != tc.wantHint {
					t.Fatalf("configuration hint present = %v, want %v: %s", got, tc.wantHint, out)
				}
				logs.Reset()
			}
		})
	}
}
