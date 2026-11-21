package freshness

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
	"github.com/linsomniac/apt-cacher-ultra/internal/hostsem"
	"github.com/linsomniac/apt-cacher-ultra/internal/simtest"
)

// siblingUpstream is an upstream that can answer a path with any bytes, as
// Apache's content negotiation (MultiViews) answers a name that does not
// exist with a file of that name and another extension: a request for
// Contents-amd64, which the repository lists and does not publish, gets
// Contents-amd64.gz with status 200 (packages.icinga.com, 2026-11).
type siblingUpstream struct {
	suite SuiteRef

	mu       sync.Mutex
	files    map[string][]byte // suite-relative path -> body; none is a 404
	chunked  map[string]bool   // answered without a Content-Length
	requests map[string]int
}

func newSiblingUpstream(files map[string][]byte) *siblingUpstream {
	return &siblingUpstream{
		suite:    SuiteRef{CanonicalScheme: "http", CanonicalHost: "archive.example.com", SuitePath: "/ubuntu/dists/icinga-jammy"},
		files:    files,
		chunked:  map[string]bool{},
		requests: map[string]int{},
	}
}

func (u *siblingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, u.suite.SuitePath+"/")
	u.mu.Lock()
	u.requests[rel]++
	body, ok := u.files[rel]
	chunked := u.chunked[rel]
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-gzip")
	if chunked {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	} else {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	}
	_, _ = w.Write(body)
}

func (u *siblingUpstream) set(rel string, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.files[rel] = body
}

func (u *siblingUpstream) asked(rel string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[rel]
}

func (u *siblingUpstream) total() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, c := range u.requests {
		n += c
	}
	return n
}

// siblingRun is one adopter over a siblingUpstream, with production's
// member retries (two rounds) and its log.
type siblingRun struct {
	ad     *Adopter
	cache  *cache.Cache
	logs   *bytes.Buffer
	sleeps []time.Duration
}

func newSiblingRun(t *testing.T, srv *httptest.Server, u *siblingUpstream, tolerate bool) *siblingRun {
	t.Helper()
	client, err := fetch.New(fetch.Options{Proxy: srv.URL, AllowedHostRegex: []string{`^archive\.example\.com$`}})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	env := newAdoptionTestEnv(t)
	r := &siblingRun{cache: env.cache, logs: &bytes.Buffer{}}
	r.ad, err = NewAdopter(AdoptionConfig{
		Cache:                          env.cache,
		Fetcher:                        client,
		Verifier:                       passThroughVerifier{},
		HostLimiter:                    hostsem.New(8),
		TolerateOptionalMemberFailures: tolerate,
		RepairSkippedMembers:           true,
		MemberRetryCount:               2,
		MemberRetryDelay:               30 * time.Second,
		Logger:                         slog.New(slog.NewJSONHandler(r.logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}
	r.ad.retrySleep = func(_ context.Context, d time.Duration) error {
		r.sleeps = append(r.sleeps, d)
		return nil
	}
	trackSuite(t, env.cache, u.suite)
	return r
}

// events returns the log's records with that message.
func (r *siblingRun) events(t *testing.T, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(r.logs.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func (r *siblingRun) adopt(t *testing.T, u *siblingUpstream, declared map[string][]byte) error {
	t.Helper()
	release, _ := makeRelease(declared)
	return r.ad.Run(context.Background(), u.suite, simtest.ClearsignedFixture(string(release)), "", "")
}

// siblingFixture is a repository that lists Contents-amd64 and its
// compressed sibling and publishes the sibling only.
func siblingFixture(plain []byte, ext string, sibling []byte) (declared, published map[string][]byte) {
	pkgs := fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_1.deb": strings.Repeat("a", 64)})
	declared = map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64":             plain,
		"Contents-amd64" + ext:       sibling,
	}
	published = map[string][]byte{
		"main/binary-amd64/Packages": pkgs,
		"Contents-amd64" + ext:       sibling,
	}
	return declared, published
}

// An upstream that answers a plain member's URL with the bytes of the
// compressed sibling its Release declares does not publish the plain file:
// the member is left out like one answered 404, at the first attempt, with
// no retry round and nothing recorded for the repair pass. With optional
// failures not tolerated, the suite used not to adopt at all.
func TestAdopter_APlainMemberAnsweredWithItsCompressedSiblingIsNotPublished(t *testing.T) {
	contents := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	cases := []struct {
		name      string
		plain     []byte
		chunked   bool
		wantAsked int
	}{
		// The answer that failed is the evidence: no second request.
		{name: "with a Content-Length", plain: contents, wantAsked: 1},
		{name: "chunked", plain: contents, chunked: true, wantAsked: 1},
		// The sibling is longer than the plain file: the first answer ends
		// at the plain file's download limit, so the URL is asked once more
		// with the sibling's.
		{name: "a plain file smaller than its sibling", plain: []byte("x y\n"), wantAsked: 2},
		{name: "an empty plain file", plain: nil, wantAsked: 2},
	}
	for _, tc := range cases {
		for _, tolerate := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/tolerate=%t", tc.name, tolerate), func(t *testing.T) {
				gz := gzipBytes(tc.plain)
				declared, published := siblingFixture(tc.plain, ".gz", gz)
				u := newSiblingUpstream(published)
				u.set("Contents-amd64", gz) // content negotiation
				u.chunked["Contents-amd64"] = tc.chunked
				srv := httptest.NewServer(u)
				defer srv.Close()
				r := newSiblingRun(t, srv, u, tolerate)

				if err := r.adopt(t, u, declared); err != nil {
					t.Fatalf("the suite did not adopt: %v", err)
				}
				ctx := context.Background()
				id := currentID(t, r.cache, u.suite)
				if _, err := r.cache.GetSnapshotMember(ctx, id, "Contents-amd64"); !errors.Is(err, cache.ErrNotFound) {
					t.Errorf("the plain member is in the snapshot (err=%v): it holds no bytes upstream vouches for", err)
				}
				row, err := r.cache.GetSnapshotMember(ctx, id, "Contents-amd64.gz")
				if err != nil {
					t.Fatalf("the sibling is not in the snapshot: %v", err)
				}
				if got, err := os.ReadFile(r.cache.BlobPath(row.BlobHash)); err != nil || !bytes.Equal(got, gz) {
					t.Errorf("the sibling's stored bytes are not its own (err=%v)", err)
				}
				if rows, err := r.cache.ListRepairableSkippedMembers(ctx, id); err != nil || len(rows) != 0 {
					t.Errorf("%d records left for the repair pass (err=%v), want none: %+v", len(rows), err, rows)
				}
				if len(r.sleeps) != 0 {
					t.Errorf("the adoption waited %v for retry rounds, want none", r.sleeps)
				}
				skips := r.events(t, "adoption_member_skipped")
				if len(skips) != 1 {
					t.Fatalf("%d adoption_member_skipped events, want 1: %v", len(skips), skips)
				}
				if s := skips[0]; s["path"] != "Contents-amd64" || s["reason"] != "compressed_sibling_served" || s["served_as"] != "Contents-amd64.gz" {
					t.Errorf("skip event = %v, want path Contents-amd64, reason compressed_sibling_served, served_as Contents-amd64.gz", s)
				}
				if n := u.asked("Contents-amd64"); n != tc.wantAsked {
					t.Errorf("the plain URL was asked %d times, want %d", n, tc.wantAsked)
				}
				if tmp, err := os.ReadDir(filepath.Join(r.cache.Dir(), "tmp")); err != nil || len(tmp) != 0 {
					t.Errorf("tmp/ holds %d files (err %v), want none", len(tmp), err)
				}
			})
		}
	}
}

// Whichever compressed sibling the upstream answers with is recognised,
// among several the Release declares.
func TestAdopter_AnyDeclaredCompressedSiblingIsRecognised(t *testing.T) {
	plain := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	for _, ext := range []string{".gz", ".xz", ".bz2", ".lzma", ".zst", ".lz4"} {
		t.Run(ext, func(t *testing.T) {
			// The bytes need not decode: they are compared, never read.
			served := []byte("the " + ext + " sibling's bytes " + strings.Repeat("s", 300))
			declared, published := siblingFixture(plain, ext, served)
			other := ".gz"
			if ext == ".gz" {
				other = ".xz"
			}
			declared["Contents-amd64"+other] = []byte("another sibling " + strings.Repeat("o", 200))
			published["Contents-amd64"+other] = declared["Contents-amd64"+other]
			u := newSiblingUpstream(published)
			u.set("Contents-amd64", served)
			srv := httptest.NewServer(u)
			defer srv.Close()
			r := newSiblingRun(t, srv, u, true)

			if err := r.adopt(t, u, declared); err != nil {
				t.Fatalf("the suite did not adopt: %v", err)
			}
			skips := r.events(t, "adoption_member_skipped")
			if len(skips) != 1 || skips[0]["reason"] != "compressed_sibling_served" || skips[0]["served_as"] != "Contents-amd64"+ext {
				t.Errorf("skip events = %v, want one with reason compressed_sibling_served and served_as Contents-amd64%s", skips, ext)
			}
			if rows, err := r.cache.ListRepairableSkippedMembers(context.Background(), currentID(t, r.cache, u.suite)); err != nil || len(rows) != 0 {
				t.Errorf("%d records left for the repair pass (err=%v), want none", len(rows), err)
			}
		})
	}
}

// A sibling exactly as long as the plain file is recognised too: the answer
// has the declared length and fails on its hash alone.
func TestAdopter_ASiblingAsLongAsThePlainFileIsRecognised(t *testing.T) {
	plain := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	sibling := bytes.Repeat([]byte{'s'}, len(plain))
	declared, published := siblingFixture(plain, ".bz2", sibling)
	u := newSiblingUpstream(published)
	u.set("Contents-amd64", sibling)
	srv := httptest.NewServer(u)
	defer srv.Close()
	r := newSiblingRun(t, srv, u, true)

	if err := r.adopt(t, u, declared); err != nil {
		t.Fatalf("the suite did not adopt: %v", err)
	}
	skips := r.events(t, "adoption_member_skipped")
	if len(skips) != 1 || skips[0]["reason"] != "compressed_sibling_served" || skips[0]["served_as"] != "Contents-amd64.bz2" {
		t.Errorf("skip events = %v, want one with reason compressed_sibling_served and served_as Contents-amd64.bz2", skips)
	}
	if n := u.asked("Contents-amd64"); n != 1 {
		t.Errorf("the plain URL was asked %d times, want 1", n)
	}
	if len(r.sleeps) != 0 {
		t.Errorf("the adoption waited %v for retry rounds, want none", r.sleeps)
	}
}

// Bytes that are not a declared sibling's stay what they were: a failure
// that may heal, retried in the adoption's rounds and recorded for the
// repair pass. Only the sibling's exact bytes count, not its length.
// Telling costs no request beyond the three attempts' own, unless the
// sibling is longer than the plain file.
func TestAdopter_BytesThatAreNoSiblingsStayAFailureThatMayHeal(t *testing.T) {
	contents := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	tiny := []byte("x y\n")
	cases := []struct {
		name      string
		plain     []byte
		served    func(gz []byte) []byte
		wantAsked int // the plain URL's requests over the three attempts
	}{
		{name: "the sibling's length, other bytes", plain: contents, wantAsked: 3,
			served: func(gz []byte) []byte { return bytes.Repeat([]byte{'x'}, len(gz)) }},
		{name: "a shorter generation of the file", plain: contents, wantAsked: 3,
			served: func([]byte) []byte { return []byte("an older generation of the file") }},
		// The answer runs over the plain file's limit, and every sibling is
		// shorter than the plain file: none can be what was sent.
		{name: "a longer generation of the file", plain: contents, wantAsked: 3,
			served: func([]byte) []byte { return append(append([]byte(nil), contents...), "one more line\n"...) }},
		// The sibling is longer than the plain file, so the URL is asked
		// once more per attempt, with the sibling's limit; what comes is
		// not the sibling.
		{name: "a tiny plain file, bytes longer than its sibling", plain: tiny, wantAsked: 6,
			served: func(gz []byte) []byte { return bytes.Repeat([]byte{'z'}, len(gz)+11) }},
		{name: "a tiny plain file, other bytes of its sibling's length", plain: tiny, wantAsked: 6,
			served: func(gz []byte) []byte { return bytes.Repeat([]byte{'z'}, len(gz)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain, gz := tc.plain, gzipBytes(tc.plain)
			declared, published := siblingFixture(plain, ".gz", gz)
			u := newSiblingUpstream(published)
			u.set("Contents-amd64", tc.served(gz))
			srv := httptest.NewServer(u)
			defer srv.Close()
			r := newSiblingRun(t, srv, u, true)

			if err := r.adopt(t, u, declared); err != nil {
				t.Fatalf("the suite did not adopt: %v", err)
			}
			rows, err := r.cache.ListRepairableSkippedMembers(context.Background(), currentID(t, r.cache, u.suite))
			if err != nil || len(rows) != 1 || rows[0].Path != "Contents-amd64" {
				t.Fatalf("records for the repair pass = %+v (err=%v), want one for Contents-amd64", rows, err)
			}
			if len(r.sleeps) != 2 {
				t.Errorf("%d retry rounds, want 2", len(r.sleeps))
			}
			skips := r.events(t, "adoption_member_skipped")
			if len(skips) != 1 || skips[0]["reason"] != "optional_member_integrity" {
				t.Errorf("skip events = %v, want one with reason optional_member_integrity", skips)
			}
			if n := u.asked("Contents-amd64"); n != tc.wantAsked {
				t.Errorf("the plain URL was asked %d times, want %d", n, tc.wantAsked)
			}
			if tmp, err := os.ReadDir(filepath.Join(r.cache.Dir(), "tmp")); err != nil || len(tmp) != 0 {
				t.Errorf("tmp/ holds %d files (err %v), want none", len(tmp), err)
			}
		})
	}
}

// A record an earlier adoption left for the repair pass is retired once the
// upstream is seen to answer with the sibling: the record is dropped, no
// failure is logged or counted against it, and the next pass asks upstream
// for nothing. Before, the pass fetched the file again at every unchanged
// check for as long as the snapshot was current.
func TestRepairSkippedMembers_ARecordOfAMemberServedAsItsSiblingIsRetired(t *testing.T) {
	ctx := context.Background()
	plain := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	gz := gzipBytes(plain)
	declared, published := siblingFixture(plain, ".gz", gz)
	u := newSiblingUpstream(published)
	// At the adoption the plain URL answers with bytes of no declared file:
	// the record is the kind every earlier version left.
	u.set("Contents-amd64", []byte("bytes of no declared file"))
	srv := httptest.NewServer(u)
	defer srv.Close()
	r := newSiblingRun(t, srv, u, true)
	if err := r.adopt(t, u, declared); err != nil {
		t.Fatalf("the suite did not adopt: %v", err)
	}
	id := currentID(t, r.cache, u.suite)
	if rows, err := r.cache.ListRepairableSkippedMembers(ctx, id); err != nil || len(rows) != 1 {
		t.Fatalf("fixture: %d records for the repair pass (err=%v), want 1", len(rows), err)
	}
	u.set("Contents-amd64", gz) // content negotiation
	r.logs.Reset()

	r.ad.RepairSkippedMembers(ctx, u.suite, id)

	if rows, err := r.cache.ListRepairableSkippedMembers(ctx, id); err != nil || len(rows) != 0 {
		t.Errorf("%d records left for the repair pass (err=%v), want none: %+v", len(rows), err, rows)
	}
	if _, err := r.cache.GetSnapshotMember(ctx, id, "Contents-amd64"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("the plain member was promoted into the snapshot (err=%v)", err)
	}
	if failed := r.events(t, "adoption_member_repair_failed"); len(failed) != 0 {
		t.Errorf("the pass logged failures: %v", failed)
	}
	skips := r.events(t, "adoption_member_skipped")
	if len(skips) != 1 || skips[0]["reason"] != "compressed_sibling_served" || skips[0]["served_as"] != "Contents-amd64.gz" {
		t.Errorf("skip events = %v, want one with reason compressed_sibling_served and served_as Contents-amd64.gz", skips)
	}
	passes := r.events(t, "adoption_repair_pass")
	if len(passes) != 1 {
		t.Fatalf("%d adoption_repair_pass events, want 1", len(passes))
	}
	for field, want := range map[string]float64{"attempted": 1, "repaired": 0, "retired": 1, "remaining": 0} {
		if got, ok := passes[0][field].(float64); !ok || got != want {
			t.Errorf("adoption_repair_pass %s = %v, want %v", field, passes[0][field], want)
		}
	}

	before := u.total()
	r.ad.RepairSkippedMembers(ctx, u.suite, id)
	if n := u.total() - before; n != 0 {
		t.Errorf("the next pass made %d upstream requests, want none", n)
	}
}

// memberWriter.holds compares the length and every declared hash, and
// refuses a member that declares none.
func TestMemberWriter_Holds(t *testing.T) {
	body := []byte("the sibling's bytes")
	sum256 := shaOf(body)
	sum512 := fmt.Sprintf("%x", sha512.Sum512(body))
	cases := []struct {
		name string
		m    ReleaseMember
		want bool
	}{
		{name: "both hashes", m: ReleaseMember{Size: int64(len(body)), SHA256: sum256, SHA512: sum512}, want: true},
		{name: "SHA256 only", m: ReleaseMember{Size: int64(len(body)), SHA256: sum256}, want: true},
		{name: "SHA512 only", m: ReleaseMember{Size: int64(len(body)), SHA512: sum512}, want: true},
		{name: "no hash", m: ReleaseMember{Size: int64(len(body))}},
		{name: "another length", m: ReleaseMember{Size: int64(len(body)) + 1, SHA256: sum256, SHA512: sum512}},
		{name: "another SHA256", m: ReleaseMember{Size: int64(len(body)), SHA256: strings.Repeat("a", 64), SHA512: sum512}},
		{name: "another SHA512", m: ReleaseMember{Size: int64(len(body)), SHA256: sum256, SHA512: strings.Repeat("a", 128)}},
	}
	env := newAdoptionTestEnv(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := newMemberWriter(env.cache)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Abort() }()
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
			if got := w.holds(tc.m); got != tc.want {
				t.Errorf("holds = %t, want %t", got, tc.want)
			}
		})
	}
}

// A 404 at the repair pass retires nothing: while a mirror syncs, a backend
// that has not got a file yet answers 404 for it, and the pass exists to
// outlast that. The record stays, with one more failed attempt.
func TestRepairSkippedMembers_A404KeepsTheRecord(t *testing.T) {
	ctx := context.Background()
	plain := []byte(strings.Repeat("usr/bin/icinga2 admin/icinga2-bin\n", 400))
	declared, published := siblingFixture(plain, ".gz", gzipBytes(plain))
	u := newSiblingUpstream(published)
	u.set("Contents-amd64", []byte("bytes of no declared file"))
	srv := httptest.NewServer(u)
	defer srv.Close()
	r := newSiblingRun(t, srv, u, true)
	if err := r.adopt(t, u, declared); err != nil {
		t.Fatalf("the suite did not adopt: %v", err)
	}
	id := currentID(t, r.cache, u.suite)
	u.mu.Lock()
	delete(u.files, "Contents-amd64") // 404
	u.mu.Unlock()
	r.logs.Reset()

	r.ad.RepairSkippedMembers(ctx, u.suite, id)

	rows, err := r.cache.ListRepairableSkippedMembers(ctx, id)
	if err != nil || len(rows) != 1 || rows[0].RetryCount != 1 {
		t.Fatalf("records for the repair pass = %+v (err=%v), want the one, with one failed attempt", rows, err)
	}
	if failed := r.events(t, "adoption_member_repair_failed"); len(failed) != 1 {
		t.Errorf("%d adoption_member_repair_failed events, want 1", len(failed))
	}
	passes := r.events(t, "adoption_repair_pass")
	if len(passes) != 1 {
		t.Fatalf("%d adoption_repair_pass events, want 1", len(passes))
	}
	for field, want := range map[string]float64{"attempted": 1, "repaired": 0, "retired": 0, "remaining": 1} {
		if got, ok := passes[0][field].(float64); !ok || got != want {
			t.Errorf("adoption_repair_pass %s = %v, want %v", field, passes[0][field], want)
		}
	}
}
