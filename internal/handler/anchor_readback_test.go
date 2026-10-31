package handler

import (
	"log/slog"
	"net/http"
	"testing"
)

// Deferred minor D-38: a fetched signed anchor whose pool file cannot be
// read back for its sniff is a local disk fault, labelled like the others
// (outcome cache_write_failed, ERROR "cache write failed"), not an
// unclassified fetch error.
func TestServeHTTP_UnreadableFetchedAnchorIsACacheFault(t *testing.T) {
	r := newSuiteRig(t, true, 0)
	r.serve("/r/dists/s/InRelease", upFile{body: rowInRelease})
	r.unreadableBlob(rowInRelease)
	if rec := r.get("/r/dists/s/InRelease"); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	lines := logRecords(r.logs, "request", slog.LevelInfo)
	if len(lines) != 1 || lines[0]["outcome"] != "cache_write_failed" {
		t.Fatalf("request lines %v, want one with outcome cache_write_failed", lines)
	}
	if n := len(logRecords(r.logs, "unclassified fetch error", slog.LevelError)); n != 0 {
		t.Fatalf("%d unclassified fetch error lines, want 0", n)
	}
	if n := len(logRecords(r.logs, "cache write failed", slog.LevelError)); n != 1 {
		t.Fatalf("%d cache write failed lines, want 1", n)
	}
}
