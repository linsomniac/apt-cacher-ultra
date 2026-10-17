package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const afSuite = "/r/dists/x"

func putSuite(t *testing.T, c *Cache, s SuiteFreshness) {
	t.Helper()
	if s.CanonicalScheme == "" {
		s.CanonicalScheme, s.CanonicalHost, s.SuitePath = "http", "h", afSuite
	}
	if err := c.PutSuiteFreshness(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

func getSuite(t *testing.T, c *Cache) SuiteFreshness {
	t.Helper()
	s, err := c.GetSuiteFreshness(context.Background(), "http", "h", afSuite)
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

func sampleFailure() AdoptionFailure {
	etag, lm := "e1", "Mon, 02 Jan 2006 15:04:05 GMT"
	return AdoptionFailure{
		Hash: strings.Repeat("a", 64), Reason: "gpg_failed", Count: 2,
		FirstAt: 100, LastAt: 200, NextRetryAt: 300, ETag: &etag, LastMod: &lm,
	}
}

func assertRecord(t *testing.T, where string, s SuiteFreshness, f AdoptionFailure) {
	t.Helper()
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	i64 := func(p *int64) int64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if str(s.AdoptionFailHash) != f.Hash || str(s.AdoptionFailReason) != f.Reason ||
		s.AdoptionFailCount != f.Count || i64(s.AdoptionFailFirstAt) != f.FirstAt ||
		i64(s.AdoptionFailLastAt) != f.LastAt || i64(s.AdoptionNextRetryAt) != f.NextRetryAt ||
		str(s.AdoptionFailETag) != str(f.ETag) || str(s.AdoptionFailLastMod) != str(f.LastMod) {
		t.Fatalf("%s: record = hash %q reason %q count %d first %d last %d next %d etag %q lm %q; want %+v",
			where, str(s.AdoptionFailHash), str(s.AdoptionFailReason), s.AdoptionFailCount,
			i64(s.AdoptionFailFirstAt), i64(s.AdoptionFailLastAt), i64(s.AdoptionNextRetryAt),
			str(s.AdoptionFailETag), str(s.AdoptionFailLastMod), f)
	}
}

func assertNoRecord(t *testing.T, where string, s SuiteFreshness) {
	t.Helper()
	if s.AdoptionFailHash != nil || s.AdoptionFailReason != nil || s.AdoptionFailCount != 0 ||
		s.AdoptionFailFirstAt != nil || s.AdoptionFailLastAt != nil || s.AdoptionNextRetryAt != nil ||
		s.AdoptionFailETag != nil || s.AdoptionFailLastMod != nil {
		t.Fatalf("%s: record not clear: %+v", where, s)
	}
}

// Every suite_freshness reader returns the v7 columns (spec §6, C4, C7).
func TestSuiteFreshnessReaders_ReturnV7Columns(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	restore := stubNow(t, 1_800_000_000)
	putSuite(t, c, SuiteFreshness{CheckFailCount: 3})
	restore()
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	check := func(where string, s SuiteFreshness) {
		t.Helper()
		if s.LastRequestedAt == nil || *s.LastRequestedAt != 1_800_000_000 {
			t.Fatalf("%s: last_requested_at = %v, want 1800000000", where, s.LastRequestedAt)
		}
		if s.CheckFailCount != 3 {
			t.Fatalf("%s: check_fail_count = %d, want 3", where, s.CheckFailCount)
		}
		assertRecord(t, where, s, f)
	}
	check("GetSuiteFreshness", getSuite(t, c))
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListSuites = %v, %v", all, err)
	}
	check("ListSuites", all[0])
	wa, err := c.ListSuitesWithAdoption(ctx)
	if err != nil || len(wa) != 1 {
		t.Fatalf("ListSuitesWithAdoption = %v, %v", wa, err)
	}
	check("ListSuitesWithAdoption", wa[0].SuiteFreshness)
	if wa[0].CurrentAnchorForm != "" {
		t.Fatalf("unadopted suite anchor form = %q, want empty", wa[0].CurrentAnchorForm)
	}
}

func TestListSuitesWithAdoption_ReportsCurrentAnchorForm(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	h := seedBlob(t, c, "inrelease for anchor-form listing")
	id, _, err := c.InsertCandidateSnapshot(ctx, SnapshotCandidate{
		CanonicalScheme: "http", CanonicalHost: "h", SuitePath: afSuite, InReleaseHash: &h})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CommitAdoption(ctx, id, []SnapshotMember{
		{SnapshotID: id, Path: "InRelease", BlobHash: h, DeclaredSHA256: h}}, nil, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	wa, err := c.ListSuitesWithAdoption(ctx)
	if err != nil || len(wa) != 1 {
		t.Fatalf("ListSuitesWithAdoption = %v, %v", wa, err)
	}
	if wa[0].CurrentAnchorForm != string(FormInline) {
		t.Fatalf("anchor form = %q, want inline", wa[0].CurrentAnchorForm)
	}
}

// PutSuiteFreshness owns check_fail_count but never the failure record
// (spec C7: the record has its own UPDATE-only writers).
func TestPutSuiteFreshness_WritesStreakNeverRecord(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{})
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	putSuite(t, c, SuiteFreshness{CheckFailCount: 5})
	s := getSuite(t, c)
	if s.CheckFailCount != 5 {
		t.Fatalf("check_fail_count = %d, want 5", s.CheckFailCount)
	}
	assertRecord(t, "after PutSuiteFreshness", s, f)
}

func TestRecordAdoptionFailure_IsUpdateOnly(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatalf("unknown suite: err = %v, want nil (no-op)", err)
	}
	all, err := c.ListSuites(ctx)
	if err != nil || len(all) != 0 {
		t.Fatalf("a record write created a row: %v, %v", all, err)
	}
}

func TestRecordAdoptionFailure_RejectsIncompleteRecord(t *testing.T) {
	c := openCache(t)
	putSuite(t, c, SuiteFreshness{})
	for name, mut := range map[string]func(*AdoptionFailure){
		"no hash":   func(f *AdoptionFailure) { f.Hash = "" },
		"no reason": func(f *AdoptionFailure) { f.Reason = "" },
		"count 0":   func(f *AdoptionFailure) { f.Count = 0 },
	} {
		f := sampleFailure()
		mut(&f)
		if err := c.RecordAdoptionFailure(context.Background(), "http", "h", afSuite, f); !errors.Is(err, ErrInvalidAdoptionFailure) {
			t.Errorf("%s: err = %v, want ErrInvalidAdoptionFailure", name, err)
		}
	}
	assertNoRecord(t, "after rejected writes", getSuite(t, c))
}

func TestClearAdoptionFailure_ResetsEveryColumn(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{CheckFailCount: 2})
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, sampleFailure()); err != nil {
		t.Fatal(err)
	}
	if err := c.ClearAdoptionFailure(ctx, "http", "h", afSuite); err != nil {
		t.Fatal(err)
	}
	s := getSuite(t, c)
	assertNoRecord(t, "after clear", s)
	if s.CheckFailCount != 2 {
		t.Fatalf("clear touched check_fail_count: %d", s.CheckFailCount)
	}
}

func TestSetAdoptionNextRetry_OnlyMovesAnExistingRecord(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	putSuite(t, c, SuiteFreshness{})
	if err := c.SetAdoptionNextRetry(ctx, "http", "h", afSuite, 999); err != nil {
		t.Fatal(err)
	}
	assertNoRecord(t, "no record to move", getSuite(t, c))
	f := sampleFailure()
	if err := c.RecordAdoptionFailure(ctx, "http", "h", afSuite, f); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAdoptionNextRetry(ctx, "http", "h", afSuite, 999); err != nil {
		t.Fatal(err)
	}
	f.NextRetryAt = 999
	assertRecord(t, "moved", getSuite(t, c), f)
}

// Spec C7 restart rule: failing rows get one retry each, spread over the
// refresh interval; refusal states and panic records keep their schedule.
func TestRescheduleFailingAdoptions_SkipsRefusalsAndPanics(t *testing.T) {
	c := openCache(t)
	ctx := context.Background()
	reasons := map[string]string{"/a": "gpg_failed", "/b": ReasonUpstreamOlder, "/c": ReasonPanic, "/d": ""}
	for suite, reason := range reasons {
		if err := c.PutSuiteFreshness(ctx, SuiteFreshness{CanonicalScheme: "http", CanonicalHost: "h", SuitePath: suite}); err != nil {
			t.Fatal(err)
		}
		if reason == "" {
			continue
		}
		f := sampleFailure()
		f.Reason, f.NextRetryAt = reason, 9_999_999_999
		if err := c.RecordAdoptionFailure(ctx, "http", "h", suite, f); err != nil {
			t.Fatal(err)
		}
	}
	n, err := c.RescheduleFailingAdoptions(ctx, 5_000, func() int64 { return 42 })
	if err != nil || n != 1 {
		t.Fatalf("RescheduleFailingAdoptions = %d, %v; want 1, nil", n, err)
	}
	next := func(suite string) *int64 {
		s, err := c.GetSuiteFreshness(ctx, "http", "h", suite)
		if err != nil {
			t.Fatal(err)
		}
		return s.AdoptionNextRetryAt
	}
	if v := next("/a"); v == nil || *v != 5_042 {
		t.Fatalf("/a next = %v, want 5042", v)
	}
	for _, s := range []string{"/b", "/c"} {
		if v := next(s); v == nil || *v != 9_999_999_999 {
			t.Fatalf("%s next = %v, want untouched", s, v)
		}
	}
	if v := next("/d"); v != nil {
		t.Fatalf("/d (no record) next = %v, want nil", *v)
	}
}

func TestIsRefusalReason(t *testing.T) {
	for _, r := range []string{ReasonUpstreamOlder, ReasonRollbackRefused, ReasonUnsignedDowngradeRefused,
		ReasonUnsignedUnconfirmed, ReasonUnsignedTooLarge, ReasonAnchorAbsent} {
		if !IsRefusalReason(r) {
			t.Errorf("%q: want refusal", r)
		}
	}
	for _, r := range []string{ReasonPanic, "gpg_failed", "member_fetch_failed", "timed_out", ""} {
		if IsRefusalReason(r) {
			t.Errorf("%q: want failure, not refusal", r)
		}
	}
}
