package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Spec C5 safeguard 1: adoption.adopt_unsigned reaches the adopter, and
// the startup line says what it was built with.
func TestBuildAdopter_WiresAdoptUnsigned(t *testing.T) {
	for _, on := range []bool{true, false} {
		withKeyringDirs(t, []string{t.TempDir()})
		cfg := newAdoptionEnabledCfg(t, false)
		cfg.Adoption.AdoptUnsigned = on
		c, fetcher, hosts := newAdoptionWiringDeps(t)
		var logs bytes.Buffer
		a, _, err := buildAdopter(cfg, c, fetcher, hosts, slog.New(slog.NewJSONHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if a.AdoptsUnsigned() != on {
			t.Fatalf("adopt_unsigned = %v, adopter built with %v", on, a.AdoptsUnsigned())
		}
		want := `"adopt_unsigned":false`
		if on {
			want = `"adopt_unsigned":true`
		}
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("startup line lacks %s:\n%s", want, logs.String())
		}
	}
}

// Deferred minors D-45 and D-47: warnSuiteCaps says nothing for an
// unvalidated max_suites of 0 (no cap, not "exceeded"), and a failed read
// of the cap usage is a WARN.
func TestWarnSuiteCaps_ZeroCapAndReadFailure(t *testing.T) {
	c, _, _ := newAdoptionWiringDeps(t)
	if err := c.EnsureSuiteForTest(context.Background(), "http", "h", "/r/dists/x"); err != nil { // one suite: "exceeds" a cap of 0
		t.Fatal(err)
	}
	var logs bytes.Buffer
	warnSuiteCaps(context.Background(), c, 0, slog.New(slog.NewJSONHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Fatalf("max_suites 0 logged:\n%s", logs.String())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	warnSuiteCaps(context.Background(), c, 100, slog.New(slog.NewJSONHandler(&logs, nil)))
	warned := false // the decoded record, not a key-order-dependent substring (deferred minor E-73)
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == "suite_cap_usage_read_failed" && rec["level"] == "WARN" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("a failed cap read was not a WARN:\n%s", logs.String())
	}
}
