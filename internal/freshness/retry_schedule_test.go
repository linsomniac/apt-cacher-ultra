package freshness

// The retry schedule of a failed adoption (spec C7, as amended by the
// 2026-11-14 publication-delay review): a failure of the mirror-sync class
// — a member that did not come, or came with other bytes — is retried on a
// short schedule, and new upstream content starts that schedule again.

import (
	"fmt"
	"testing"
	"time"
)

// staleMirror adopts the rig's base content, then makes upstream's index
// stay behind every later release: releases the rig builds from now on
// declare a new index, and the index path keeps serving other bytes, as a
// mirror that still has the previous generation does. Their adoptions fail
// member_fetch_failed (the size differs).
func (r *retryRig) staleMirror() {
	r.t.Helper()
	r.adopt()
	r.pkgs = fakePackagesStanzas(map[string]string{"pool/main/f/foo/foo_2.deb": testHex64})
	r.members.put(rigPkgsURL, []byte("the previous generation's index"))
}

func TestAdoptionFailure_MirrorSyncFailuresRetryOnAShortSchedule(t *testing.T) {
	r := newRetryRig(t, 0)
	r.staleMirror()
	r.up.set(r.release("gen1", 0))
	// 1 min doubling to 10 min (no jitter at rand 0.5).
	for i, want := range []int64{60, 120, 240, 480, 600, 600} {
		r.check()
		row := r.row()
		if row.AdoptionFailCount != int64(i+1) || strOf(row.AdoptionFailReason) != "member_fetch_failed" {
			t.Fatalf("attempt %d: record = count %d reason %q", i+1, row.AdoptionFailCount, strOf(row.AdoptionFailReason))
		}
		if got := i64Of(row.AdoptionNextRetryAt) - r.clock.now().Unix(); got != want {
			t.Fatalf("attempt %d: retry in %d s, want %d", i+1, got, want)
		}
		r.clock.advance(time.Duration(want) * time.Second) // to the retry time
	}
}

// A new publication is a new chance: its first failure waits the
// schedule's first step, not the step the previous content had reached.
// The streak itself goes on — its count and its start — so the suite still
// reads as failing since its first failure, and the persistent-failure
// warning still counts every attempt.
func TestAdoptionFailure_NewContentStartsTheShortScheduleAgain(t *testing.T) {
	r := newRetryRig(t, 0)
	r.staleMirror()
	r.up.set(r.release("gen1", 0))
	first := r.clock.now().Unix()
	for i := 0; i < 4; i++ {
		r.check()
		r.clock.advance(11 * time.Minute) // past any retry time
	}
	if row := r.row(); row.AdoptionFailCount != 4 {
		t.Fatalf("count = %d after four failures of one content, want 4", row.AdoptionFailCount)
	}
	gen2 := r.release("gen2", 0)
	r.up.set(gen2)
	r.check()
	row := r.row()
	if strOf(row.AdoptionFailHash) != hashOf(gen2) {
		t.Fatalf("record hash = %q, want the new content's", strOf(row.AdoptionFailHash))
	}
	if row.AdoptionFailCount != 5 {
		t.Errorf("count = %d, want 5: the streak goes on across the new content", row.AdoptionFailCount)
	}
	if got := i64Of(row.AdoptionNextRetryAt) - r.clock.now().Unix(); got != 60 {
		t.Errorf("retry in %d s, want 60: the schedule's first step", got)
	}
	if i64Of(row.AdoptionFailFirstAt) != first {
		t.Errorf("streak start = %d, want %d: the suite has been failing since then", i64Of(row.AdoptionFailFirstAt), first)
	}
}

// Every other failure keeps spec C7's schedule — 2 min doubling to 1 h —
// and its count across new content: a key the keyring lacks is not healed
// by the next publication.
func TestAdoptionFailure_OtherFailuresKeepTheLongSchedule(t *testing.T) {
	r := newRetryRig(t, 0)
	bad1 := r.release("bad1", 0)
	r.ver.reject(hashOf(bad1))
	r.up.set(bad1)
	for i := 0; i < 3; i++ {
		r.check()
		r.clock.advance(2 * time.Hour)
	}
	bad2 := r.release("bad2", 0)
	r.ver.reject(hashOf(bad2))
	r.up.set(bad2)
	r.check()
	row := r.row()
	if row.AdoptionFailCount != 4 || strOf(row.AdoptionFailReason) != "gpg_failed" {
		t.Fatalf("record = count %d reason %q, want 4 gpg_failed", row.AdoptionFailCount, strOf(row.AdoptionFailReason))
	}
	if got := i64Of(row.AdoptionNextRetryAt) - r.clock.now().Unix(); got != 16*60 {
		t.Errorf("retry in %d s, want 960 (2 min × 2³)", got)
	}
}

// The short schedule is for a fault that heals. Content that still fails
// after about two hours of it (fifteen attempts) is not a mirror
// mid-sync: from then on it is retried hourly, like any other failure, so
// a repository that is simply broken does not cost an attempt every ten
// minutes for ever.
func TestAdoptionFailure_MirrorSyncScheduleEndsAfterTwoHours(t *testing.T) {
	var total time.Duration
	for n := int64(1); n <= syncRetryAttempts; n++ {
		d := adoptionRetryDelay("member_mismatch", n)
		if d > syncRetryMaxDelay {
			t.Fatalf("attempt %d waits %s, more than the short schedule's %s", n, d, syncRetryMaxDelay)
		}
		total += d
	}
	if total < 115*time.Minute || total > 130*time.Minute {
		t.Errorf("the short schedule lasts %s, want about two hours", total)
	}
	for _, n := range []int64{syncRetryAttempts + 1, syncRetryAttempts + 2, 100} {
		if d := adoptionRetryDelay("member_fetch_failed", n); d != retryMaxDelay {
			t.Errorf("attempt %d waits %s, want the general schedule's %s", n, d, retryMaxDelay)
		}
	}
}

// Content that is new at every attempt — two mirrors that disagree, a
// Release generated per request — is not a series of new publications: if
// each one restarted the short schedule, the suite would be retried every
// minute for ever and never reach the persistent-failure warning. Content
// that was attempted only once before it changed does not restart the
// schedule: the suite goes on down it, to the hourly retries, and the
// warning fires at its usual counts (the review of 2026-11-15; spec C7's
// FW-3 (a) made the same rule for the confirmation records).
func TestAdoptionFailure_ContentThatChangesAtEveryAttemptGoesOnDownTheSchedule(t *testing.T) {
	r := newRetryRig(t, 0)
	r.staleMirror()
	want := []int64{60, 120, 240, 480, 600, 600}
	for i, w := range want {
		r.up.set(r.release(fmt.Sprintf("churn-%d", i), 0)) // new content every time
		r.check()
		row := r.row()
		if row.AdoptionFailCount != int64(i+1) {
			t.Fatalf("attempt %d: count = %d", i+1, row.AdoptionFailCount)
		}
		if got := i64Of(row.AdoptionNextRetryAt) - r.clock.now().Unix(); got != w {
			t.Fatalf("attempt %d: retry in %d s, want %d", i+1, got, w)
		}
		r.clock.advance(time.Duration(w) * time.Second)
	}
	var counts []float64
	for _, e := range r.events("adoption_failing_persistently") {
		counts = append(counts, e["fail_count"].(float64))
	}
	if len(counts) != 2 || counts[0] != 3 || counts[1] != 6 {
		t.Fatalf("persistent warnings at %v, want [3 6]", counts)
	}
}

// Content that was retried and then replaced is a new publication, however
// long the streak: the new content starts at the first step.
func TestAdoptionFailure_AStableContentReplacedStartsAgainEachTime(t *testing.T) {
	r := newRetryRig(t, 0)
	r.staleMirror()
	for gen := 0; gen < 3; gen++ {
		r.up.set(r.release(fmt.Sprintf("gen-%d", gen), 0))
		for i, w := range []int64{60, 120, 240} {
			r.check()
			if got := i64Of(r.row().AdoptionNextRetryAt) - r.clock.now().Unix(); got != w {
				t.Fatalf("release %d, attempt %d: retry in %d s, want %d", gen, i+1, got, w)
			}
			r.clock.advance(time.Duration(w) * time.Second)
		}
	}
	if got := r.row().AdoptionFailCount; got != 9 {
		t.Errorf("count = %d after nine failures in a row, want 9", got)
	}
}
