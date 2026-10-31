package simtest

import (
	"slices"
	"sync"
	"time"
)

// Request is one request in an apt client's queue: the anchor (InRelease)
// or the index of Suite, or — with Suite empty — a filler: an unrelated
// download that keeps the client's connection busy for Filler.
type Request struct {
	Suite  string
	Anchor bool
	Filler time.Duration
}

// ServeFunc answers one request once its response has fully arrived. It
// returns the response's snapshot generation (X-Cache-Snapshot) and how
// long the cache held the request before answering it (zero unless it
// waited in a flip hold).
type ServeFunc func(Request) (gen string, held time.Duration)

// Straddle is one index response whose generation differs from that of
// the anchor its client holds (spec C8).
type Straddle struct {
	Client    int
	Suite     string
	AnchorGen string
	IndexGen  string
	Delay     time.Duration // request sent this long after the anchor arrived, holds excluded
}

// Ledger runs apt client updates and counts straddles (spec C8).
//
// AIDEV-NOTE: the definitions every flip-consistency simulation relies on
// (phase C plan, "Definitions"); keep them in step with it:
//   - Client model (apt's): one connection, one request at a time (apt
//     keeps one connection and queue per host; beyond 10 hosts and in
//     mirror mode every source shares it). An update queues its anchors
//     and fillers; when an anchor response arrives, that suite's index
//     request joins the END of the queue.
//   - Straddle: an index response for suite S whose generation differs
//     from that of the anchor of S the same client received earlier in
//     the same update, when the index request was sent less than the
//     window (flip_quiet_period) after that anchor's arrival, not counting
//     time the client's queue spent on responses the cache held: a hold
//     must not push a client out of its guarantee. Later requests are not
//     checked; one sent exactly at the window's end races a commit made at
//     that instant (the gate is quiet once a full period has passed).
//   - Where the window starts: the gate measures quiet from when the cache
//     finished sending the anchor, this ledger from when the client
//     received it. In production kernel send buffering makes the first
//     earlier on a slow link (by at most the buffered bytes over the link's
//     bandwidth), so the effective window is slightly shorter than
//     flip_quiet_period; in the simulations the two instants coincide.
type Ledger struct {
	window time.Duration

	mu        sync.Mutex
	checked   int
	straddles []Straddle
	gens      map[string]map[string]bool // suite → anchor generations of checked pairs
}

// NewLedger returns a ledger whose guarantee window is window.
func NewLedger(window time.Duration) *Ledger {
	return &Ledger{window: window, gens: map[string]map[string]bool{}}
}

// RunUpdate runs one update for client, starting from queue. It blocks
// for the whole update — in real time outside a synctest bubble — and is
// safe for concurrent use: simulations run many clients' updates at once
// (deferred minor C-8).
func (l *Ledger) RunUpdate(client int, queue []Request, serve ServeFunc) {
	type arrival struct {
		gen  string
		at   time.Time
		held time.Duration // the update's held total when the anchor arrived
	}
	queue = slices.Clone(queue)
	anchors := map[string]arrival{}
	var held time.Duration
	for len(queue) > 0 {
		rq := queue[0]
		queue = queue[1:]
		if rq.Suite == "" {
			time.Sleep(rq.Filler)
			continue
		}
		sent, heldBefore := time.Now(), held
		gen, h := serve(rq)
		held += h
		if rq.Anchor {
			anchors[rq.Suite] = arrival{gen: gen, at: time.Now(), held: held}
			queue = append(queue, Request{Suite: rq.Suite})
			continue
		}
		if a, ok := anchors[rq.Suite]; ok {
			l.record(client, rq.Suite, a.gen, gen, sent.Sub(a.at)-(heldBefore-a.held))
		}
	}
}

func (l *Ledger) record(client int, suite, anchorGen, indexGen string, delay time.Duration) {
	if delay >= l.window {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checked++
	if l.gens[suite] == nil {
		l.gens[suite] = map[string]bool{}
	}
	l.gens[suite][anchorGen] = true
	if anchorGen != indexGen {
		l.straddles = append(l.straddles, Straddle{Client: client, Suite: suite, AnchorGen: anchorGen, IndexGen: indexGen, Delay: delay})
	}
}

// Straddles returns the straddles counted so far.
func (l *Ledger) Straddles() []Straddle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.straddles)
}

// Checked returns how many index responses fell inside the window.
func (l *Ledger) Checked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checked
}

// AnchorGens returns, sorted as strings ("10" before "9"), the anchor
// generations of suite's checked pairs: two or more prove its clients
// crossed a flip (deferred minor C-8).
func (l *Ledger) AnchorGens(suite string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for g := range l.gens[suite] {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}
