package simtest

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

// A straddle is judged net of the time the client's queue spent on held
// responses: a's index request goes out 15 s after a's anchor arrived,
// all of it behind b's held anchor, so it is still inside the window.
func TestLedger_CountsStraddlesInsideTheWindowNetOfHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gens := map[string]string{"a": "1", "b": "1"}
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor && rq.Suite == "b" {
				time.Sleep(15 * time.Second) // held while a flips
				gens["a"] = "2"
				return gens["b"], 15 * time.Second
			}
			return gens[rq.Suite], 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(7, []Request{{Suite: "a", Anchor: true}, {Suite: "b", Anchor: true}}, serve)
		s := l.Straddles()
		want := Straddle{Client: 7, Suite: "a", AnchorGen: "1", IndexGen: "2", Delay: 0}
		if len(s) != 1 || s[0] != want {
			t.Fatalf("straddles = %+v, want [%+v]", s, want)
		}
		if l.Checked() != 2 || !slices.Equal(l.AnchorGens("a"), []string{"1"}) {
			t.Fatalf("checked %d, a's anchor generations %v; want 2 and [1]", l.Checked(), l.AnchorGens("a"))
		}
	})
}

// An index request sent after the window (here behind the client's own
// 11 s download) is outside the guarantee and is not checked; so is one
// sent exactly at its end, which races a commit made at that instant.
func TestLedger_IgnoresIndexesOutsideTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serve := func(rq Request) (string, time.Duration) {
			if rq.Anchor {
				return "1", 0
			}
			return "2", 0
		}
		l := NewLedger(10 * time.Second)
		l.RunUpdate(1, []Request{{Suite: "a", Anchor: true}, {Filler: 11 * time.Second}}, serve)
		l.RunUpdate(3, []Request{{Suite: "a", Anchor: true}, {Filler: 10 * time.Second}}, serve)
		if l.Checked() != 0 || len(l.Straddles()) != 0 {
			t.Fatalf("checked %d, straddles %v; want neither", l.Checked(), l.Straddles())
		}
		l.RunUpdate(2, []Request{{Suite: "a", Anchor: true}, {Filler: 9 * time.Second}}, serve)
		if l.Checked() != 1 || len(l.Straddles()) != 1 {
			t.Fatalf("checked %d, straddles %v; want one of each", l.Checked(), l.Straddles())
		}
	})
}
