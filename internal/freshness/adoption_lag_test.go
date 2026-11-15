package freshness

// What an adoption's log says about lateness (the 2026-11-14
// publication-delay review): adoption_finished carries how long the cache
// had known of the release and how old the release itself was, and an
// attempt that runs long says where it is, once a minute.

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// adoptionLogs captures the rig adopter's log.
func adoptionLogs(r *retryRig) *lockedBuffer {
	logs := &lockedBuffer{}
	r.adopter.logger = slog.New(slog.NewJSONHandler(logs, nil))
	return logs
}

// The finish record of a release that waited two minutes for a slot, and
// was ten minutes old when the cache first saw it, says both: how long the
// cache knew of it before publishing, and how far behind upstream the
// publication was.
func TestAdoptionFinished_SaysHowLateThePublicationWas(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	date := r.clock.now().Unix() - 600
	r.up.set(r.release("v2", date))

	// First sighting: no slot, so the change is left pending.
	held := mustAcquire(t, r.checker.admission, rigHost, true)
	r.check()
	if lastResult(r) != "changed_queued" {
		t.Fatalf("setup: last result %q, want changed_queued", lastResult(r))
	}
	r.clock.advance(2 * time.Minute)
	held.release()
	r.check()

	var done map[string]any
	for _, rec := range logRecords(t, logs, "adoption_finished") {
		if rec["outcome"] == "success" {
			done = rec
		}
	}
	if done == nil {
		t.Fatalf("no successful adoption_finished record:\n%s", logs.String())
	}
	if got, _ := done["first_seen_seconds"].(float64); got != 120 {
		t.Errorf("first_seen_seconds = %v, want 120: the cache knew of the release for two minutes", done["first_seen_seconds"])
	}
	if got, _ := done["release_date"].(float64); int64(got) != date {
		t.Errorf("release_date = %v, want %d", done["release_date"], date)
	}
	if got, _ := done["release_age_seconds"].(float64); got != 720 {
		t.Errorf("release_age_seconds = %v, want 720: ten minutes old at first sight, two more waiting", done["release_age_seconds"])
	}
}

// A release without a Date has no age to report: the fields are absent,
// not zero.
func TestAdoptionFinished_UndatedReleaseHasNoAge(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	r.up.set(r.release("v2", 0))
	r.check()
	recs := logRecords(t, logs, "adoption_finished")
	if len(recs) != 1 {
		t.Fatalf("adoption_finished records = %d, want 1", len(recs))
	}
	for _, f := range []string{"release_date", "release_age_seconds"} {
		if _, ok := recs[0][f]; ok {
			t.Errorf("%s present for a release without a Date: %v", f, recs[0][f])
		}
	}
	if _, ok := recs[0]["first_seen_seconds"]; !ok {
		t.Errorf("first_seen_seconds absent: the check that started the attempt saw the change")
	}
}

// An attempt that runs long logs where it is. One that finishes within the
// interval logs nothing of the kind.
func TestAdoptionInProgress_IsLoggedWhileAnAttemptRuns(t *testing.T) {
	ctx := context.Background()
	r := newRetryRig(t, 0)
	r.adopt()
	logs := adoptionLogs(r)
	r.adopter.heartbeatInterval = 5 * time.Millisecond
	r.adopter.progressInterval = 30 * time.Millisecond
	hold := newRelease(t, r, "v2")

	r.checker.Check(ctx, "http", rigHost, rigSuite)
	deadline := time.Now().Add(10 * time.Second)
	for len(logRecords(t, logs, "adoption_in_progress")) == 0 {
		if time.Now().After(deadline) {
			close(hold.release)
			r.checker.WaitForAdoptions()
			t.Fatal("no adoption_in_progress line while the attempt waited in a member fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(hold.release)
	r.checker.WaitForAdoptions()

	recs := logRecords(t, logs, "adoption_in_progress")
	rec := recs[0]
	if rec["phase"] != "members" {
		t.Errorf("phase = %v, want members", rec["phase"])
	}
	if rec["adoption_id"] == nil || rec["adoption_id"] == "" || rec["suite_path"] != rigSuite {
		t.Errorf("record lacks the attempt's identity: %v", rec)
	}
	if e, _ := rec["elapsed_seconds"].(float64); e <= 0 {
		t.Errorf("elapsed_seconds = %v, want a positive duration", rec["elapsed_seconds"])
	}
	// The same attempt's finish record: the progress lines belong to it.
	fin := logRecords(t, logs, "adoption_finished")
	if len(fin) != 1 || fin[0]["adoption_id"] != rec["adoption_id"] {
		t.Errorf("the progress line's adoption_id %v is not the finished attempt's", rec["adoption_id"])
	}

	// A quick attempt logs none.
	before := len(logRecords(t, logs, "adoption_in_progress"))
	r.adopter.progressInterval = time.Hour
	r.pkgs = fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_v3.deb": testHex64})
	r.members.put(rigPkgsURL, r.pkgs)
	r.up.set(r.release("v3", 0))
	r.check()
	if after := len(logRecords(t, logs, "adoption_in_progress")); after != before {
		t.Errorf("a quick attempt logged %d adoption_in_progress lines, want none", after-before)
	}
}
