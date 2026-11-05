package acmedns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/testutil/dnsfixture"
)

// Exercise a stateful API-backed provider with its real SDK. No Cloudflare
// account or public API is used; the token and record ID are test fixtures.
func TestCloudflareRetainsRecordIDForCleanup(t *testing.T) {
	if !Supported("cloudflare") {
		t.Skip("provider not compiled")
	}
	dns := dnsfixture.New(t)
	name := "_acme-challenge." + domain
	dns.Add(name, "unrelated-record")
	var deleted atomic.Bool
	var created atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer fixture-only-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /zones":
			if r.URL.Query().Get("name") != "example.net" {
				t.Errorf("unexpected zone %s", r.URL.Query().Get("name"))
			}
			_, _ = fmt.Fprint(w, `{"success":true,"result":[{"id":"zone-id","name":"example.net"}]}`)
		case "POST /zones/zone-id/dns_records":
			var record struct{ Name, Type, Content string }
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil || record.Name != name || record.Type != "TXT" {
				t.Error("unexpected record request")
				http.Error(w, "invalid record", http.StatusBadRequest)
				return
			}
			dns.Add(record.Name, strings.Trim(record.Content, `"`))
			created.Store(true)
			_, _ = fmt.Fprint(w, `{"success":true,"result":{"id":"created-record-id"}}`)
		case "DELETE /zones/zone-id/dns_records/created-record-id":
			if !created.Load() {
				t.Error("cleanup before creation")
			}
			records := dns.Records(name)
			for _, v := range records {
				if v != "unrelated-record" {
					dns.Remove(name, v)
				}
			}
			deleted.Store(true)
			_, _ = fmt.Fprint(w, `{"success":true}`)
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	secret := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secret, []byte("fixture-only-token"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDFLARE_BASE_URL", api.URL)
	t.Setenv("CLOUDFLARE_DNS_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_DNS_API_TOKEN_FILE", secret)
	t.Setenv("CLOUDFLARE_ZONE_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_ZONE_API_TOKEN_FILE", secret)
	ctx := context.Background()
	provider, err := New(ctx, "cloudflare", []string{dns.Address})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if err := provider.Present(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if err := provider.Wait(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	if err := provider.CleanUp(ctx, domain, token, keyAuth); err != nil {
		t.Fatal(err)
	}
	records := dns.Records(name)
	if !deleted.Load() || len(records) != 1 || records[0] != "unrelated-record" {
		t.Fatalf("record cleanup: %v", records)
	}
}
