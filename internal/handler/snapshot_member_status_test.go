package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// A snapshot member's recovery (runSnapshotMemberFetch) that fetched the
// member from upstream and then failed to store its response metadata
// (PutRepresentation) reports the upstream status it got, as its
// neighbouring failures after the fetch (the finalize and the blob row) do:
// the 502 carries X-Upstream-Status and the request line upstream_status
// (the merge of main's 0c6d2c0, the Task 8 merge report's concern 4).
func TestServeHTTP_SnapshotMemberRecoveryMetadataFailureKeepsTheUpstreamStatus(t *testing.T) {
	body := []byte("Package: x\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	h := newTestHandler(t, nil, nil)
	logs := captureRequestLog(h)
	scheme, host, port := splitURL(t, srv.URL)
	anchor := writeBlob(t, h, []byte("anchor"))
	declared := writeBlob(t, h, body)
	const member = "main/binary-amd64/Packages"
	commitInlineSnapshot(t, h, scheme, hostKey(host, port), "/dists/noble", anchor,
		[]cache.SnapshotMember{
			{Path: "InRelease", BlobHash: anchor, DeclaredSHA256: anchor},
			{Path: member, BlobHash: declared, DeclaredSHA256: declared},
		}, nil)
	// The member's pool file is lost, so the request fetches it again.
	if err := os.Remove(h.cache.BlobPath(declared)); err != nil {
		t.Fatal(err)
	}
	// The metadata write fails after the fetch and the blob row succeed.
	db := sqliteFile(t, filepath.Join(h.cache.Dir(), "cache.db"))
	if _, err := db.Exec(`CREATE TRIGGER fail_representation BEFORE INSERT ON representation_metadata
BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, proxyReq(http.MethodGet, srv.URL, "/dists/noble/"+member))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("X-Upstream-Status") != "200" {
		t.Fatalf("status=%d, X-Upstream-Status=%q, body=%q; want 502 with the upstream's 200",
			rec.Code, rec.Header().Get("X-Upstream-Status"), rec.Body.String())
	}
	var found bool
	for _, line := range logs.find("request") {
		if line["outcome"] == "snapshot_recovery_failed" {
			found = line["upstream_status"] == float64(http.StatusOK)
		}
	}
	if !found {
		t.Errorf("no snapshot_recovery_failed request line with upstream_status 200: %v", logs.find("request"))
	}
}
