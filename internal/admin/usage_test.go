package admin

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

func TestUsageAdminAuthenticationHTMLAndJSON(t *testing.T) {
	path := makeBcryptHtpasswd(t, "alice", "secret")
	s, ln, c := newAdminServer(t, withHtpasswd(path))
	u, err := usage.New(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Usage = u
	u.RecordFetch(2048, time.Second)
	u.RecordServe(1024, time.Second, true)
	base := "http://" + ln.Addr().String()
	cleanup := serveAdminServer(t, s, ln, c)
	defer cleanup()
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/", "/?format=json"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 401 || bytes.Contains(body, []byte("fetched_bytes")) || bytes.Contains(body, []byte("Traffic and cache savings")) {
			t.Fatalf("unauthenticated: %d %s", resp.StatusCode, body)
		}
		req, _ := http.NewRequest("GET", base+path, nil)
		req.SetBasicAuth("alice", "secret")
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d, cache %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		if path == "/" {
			mustContain(t, string(body), `id="usage"`, "Data fetched", "Average upload rate", "−1.0 KiB")
		} else {
			var payload struct {
				Usage usage.Snapshot `json:"usage"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Usage.Lifetime.FetchedBytes != 2048 || payload.Usage.Lifetime.NetSavedBytes != -1024 || *payload.Usage.Lifetime.AverageDownloadBPS != 2048 {
				t.Fatal(payload)
			}
		}
	}
	if u.Snapshot().Lifetime.Requests != 1 {
		t.Fatal("admin reads affected usage")
	}
}

func TestUsageEmptyAndPersistenceWarningRenderWithinBudget(t *testing.T) {
	m := newHealthyModel()
	m.Usage = &usage.Snapshot{PersistenceError: true, Lifetime: usage.Window{Totals: usage.Totals{SinceUnix: 1}}}
	html := renderHTMLForGolden(t, m)
	mustContain(t, html, "Usage checkpoint failed", "n/a", "10 panels")
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(gz, html); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if compressed.Len() > 90*1024 || len(html) > 200*1024 {
		t.Fatal("render budget exceeded")
	}
}
