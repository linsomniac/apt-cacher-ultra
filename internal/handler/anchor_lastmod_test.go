package handler

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
	"github.com/linsomniac/apt-cacher-ultra/internal/wallclock"
)

const lmSuite = "/ubuntu/dists/noble"

func commitAnchor(t *testing.T, h *Handler, body, date string) string {
	t.Helper()
	hash := writeBlob(t, h, simtest.ClearsignedFixture(body))
	commitInlineSnapshot(t, h, "http", "127.0.0.1", lmSuite, hash, []cache.SnapshotMember{{Path: "InRelease", BlobHash: hash, DeclaredSHA256: hash}}, nil)
	if err := h.cache.PutRepresentation(context.Background(), "http", "127.0.0.1", lmSuite+"/InRelease", hash, date); err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestSnapshotValidators_SameAndBackwardDatesAcrossFlips(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	path := lmSuite + "/InRelease"
	var priorTag string
	for i, date := range []string{originDate, originDate, "Sun, 31 Dec 2023 00:00:00 GMT"} {
		restore := wallclock.SetOffsetForTest(time.Duration(3-i) * time.Hour)
		hash := commitAnchor(t, h, string(rune('a'+i)), date)
		restore()
		future := time.Now().Add(24 * time.Hour)
		if err := os.Chtimes(h.cache.BlobPath(hash), future, future); err != nil {
			t.Fatal(err)
		}
		w := getHeaders(h, "GET", path, map[string]string{"If-None-Match": priorTag, "If-Modified-Since": "Thu, 01 Jan 2099 00:00:00 GMT"})
		if w.Code != 200 || w.Header().Get("Last-Modified") != date {
			t.Fatal(w.Code, w.Header())
		}
		priorTag = w.Header().Get("ETag")
		if w := getHeaders(h, "GET", path, map[string]string{"If-None-Match": priorTag}); w.Code != http.StatusNotModified {
			t.Fatal(w.Code)
		}
	}
}

func TestSnapshotRecovery_PreservesFetchedDateAndETag(t *testing.T) {
	body := simtest.ClearsignedFixture("anchor")
	h := newSeamHandler(t, &datedFetcher{body: body, date: originDate})
	defer h.Close()
	hash := commitAnchor(t, h, "anchor", originDate)
	first := getHeaders(h, "GET", lmSuite+"/InRelease", nil)
	if err := os.Remove(h.cache.BlobPath(hash)); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"MISS", "HIT"} {
		w := getHeaders(h, "GET", lmSuite+"/InRelease", nil)
		if w.Code != 200 || w.Header().Get("X-Cache") != outcome || w.Header().Get("Last-Modified") != originDate || w.Header().Get("ETag") != first.Header().Get("ETag") {
			t.Fatal(w.Code, w.Header())
		}
	}
}

func TestSnapshotDates_IndependentDetachedMembers(t *testing.T) {
	h := newTestHandler(t, nil, nil)
	ctx := context.Background()
	rel := writeBlob(t, h, []byte("Release bytes"))
	sig := writeBlob(t, h, []byte("signature"))
	index := writeBlob(t, h, []byte("Package: x\n"))
	id, _, err := h.cache.InsertTrackedCandidateForTest(ctx, cache.SnapshotCandidate{CanonicalScheme: "http", CanonicalHost: "127.0.0.1", SuitePath: lmSuite, ReleaseHash: &rel, ReleaseGPGHash: &sig})
	if err != nil {
		t.Fatal(err)
	}
	members := []cache.SnapshotMember{{SnapshotID: id, Path: "Release", BlobHash: rel, DeclaredSHA256: rel}, {SnapshotID: id, Path: "Release.gpg", BlobHash: sig, DeclaredSHA256: sig}, {SnapshotID: id, Path: "main/binary-amd64/Packages", BlobHash: index, DeclaredSHA256: index}}
	if err := h.cache.CommitAdoption(ctx, id, members, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	dates := []string{originDate, "Tue, 02 Jan 2024 00:00:00 GMT", ""}
	for i, m := range members {
		if err := h.cache.PutRepresentation(ctx, "http", "127.0.0.1", lmSuite+"/"+m.Path, m.BlobHash, dates[i]); err != nil {
			t.Fatal(err)
		}
		w := getHeaders(h, "HEAD", lmSuite+"/"+m.Path, nil)
		if w.Code != 200 || w.Header().Get("Last-Modified") != dates[i] {
			t.Fatal(w.Code, w.Header())
		}
	}
}
