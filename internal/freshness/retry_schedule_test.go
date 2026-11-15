package freshness

// The retry schedule of a failed adoption (spec C7, as amended by the
// 2026-11-14 publication-delay review): a failure of the mirror-sync class
// — a member that did not come, or came with other bytes — is retried on a
// short schedule, and new upstream content starts that schedule again.

import (
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
// The streak's start stays, so "failing since" still tells the truth.
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
	if row.AdoptionFailCount != 1 {
		t.Errorf("count = %d for the new content's first failure, want 1", row.AdoptionFailCount)
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
