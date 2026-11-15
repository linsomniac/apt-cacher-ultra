package freshness

// Step 5's upstream work (the 2026-11-14 publication-delay review). Until
// then runShared fetched a suite's Release members one after another, asked
// twice (by hash, then by path) for every member upstream never serves, at
// every adoption, and let each member that failed wait out its own retry
// delay. Nothing is published until the last member is in, so all of that
// was time a new release stayed invisible to clients, and time the suite
// held its host's adoption slot.
//
// Now:
//   - members are fetched MemberFetchWorkers at a time (fetchMemberRound);
//   - a member the suite's current snapshot does not serve is asked for at
//     its declared path first (memberProbeFor, adoptMember's
//     probePathFirst), so a file upstream never serves costs one request;
//   - the members whose failure may heal — a mirror mid-sync — are tried
//     again together, after one shared delay per round (fetchMembers).
//
// What is verified, and what a member's failure means for the snapshot,
// is unchanged: runShared reads each member's result in declared order.

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// MemberFetchWorkers is how many of a suite's Release members an adoption
// fetches at once (a constant, not a knob: upstream.max_concurrent_per_host
// is the operator's bound, and these fetches take their host slots from it
// like every other upstream request). Exported for the handler package's
// chaos tests, which stage as many stalled fetches as there are workers.
const MemberFetchWorkers = 4

// HotPrefetchWorkers is how many hot packages an adoption warms at once (a
// constant, like MemberFetchWorkers, and for the same reason).
const HotPrefetchWorkers = 4

// memberProbe says where adoptMember looks for a member upstream.
type memberProbe int

const (
	// probePath: the member's declared path only (a suite whose Release
	// does not advertise Acquire-By-Hash).
	probePath memberProbe = iota
	// probeByHashFirst: the by-hash URL, then the declared path (REC 2).
	probeByHashFirst
	// probePathFirst: the declared path, then — unless that path answered
	// 404 or 410 — the by-hash URL. For members the suite's current
	// snapshot does not serve (memberProbeFor).
	probePathFirst
)

// memberJob is one declared member's way through Step 5: what runShared's
// first pass decided, and what the fetch found.
type memberJob struct {
	m             ReleaseMember
	requiredGroup string // SPEC6_7 §6 group key; "" when the member's arch is not required
	probe         memberProbe

	hash     string // the verified blob's SHA-256, when err is nil
	reused   bool   // the bytes were in the pool: nothing was downloaded
	err      error  // the last attempt's error
	attempts int
}

// memberLockKey is the blobLocks key of m: its declared SHA-256, or its
// SHA-512 for a member declared by SHA-512 alone (a key apart from every
// SHA-256, which is shorter). "" for a member that declares neither.
func memberLockKey(m ReleaseMember) string {
	if m.SHA256 != "" {
		return m.SHA256
	}
	return m.SHA512
}

// priorMemberPaths returns the suite-relative paths prior — the suite's
// current snapshot — serves, or nil when there is no prior snapshot or its
// members cannot be read (logged). nil turns the path-first probe off:
// every member then gets the full sequence, as at a first adoption.
func (a *Adopter) priorMemberPaths(ctx context.Context, suite SuiteRef, prior *cache.SuiteSnapshot) map[string]struct{} {
	if prior == nil {
		return nil
	}
	rows, err := a.cache.ListSnapshotMembers(ctx, prior.SnapshotID)
	if err != nil {
		a.adoptionLogger(ctx).Warn("adoption_read_prior_members_failed",
			"canonical_host", suite.CanonicalHost,
			"suite_path", suite.SuitePath,
			"snapshot_id", prior.SnapshotID,
			"err", err,
		)
		return nil
	}
	paths := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		paths[r.Path] = struct{}{}
	}
	return paths
}

// memberProbeFor decides where m is looked for. A suite without
// Acquire-By-Hash has only the declared path. With it, by hash first
// (REC 2) — except a member the suite's current snapshot does not serve
// (servedBefore), which is asked for at its declared path first.
//
// AIDEV-NOTE: why the snapshot is evidence. A Release declares many files
// its archive never serves: every uncompressed variant on Debian (148 of
// trixie-security's 372 members, measured 2026-11-14), the foreign
// architectures and more on Ubuntu's main archive (312 of noble-security's
// 414). Each cost a by-hash 404 and a path 404 at every adoption. A member
// absent from the current snapshot was not served when that snapshot was
// adopted, so its path is the cheaper first question; if the path answers
// with anything but "not here", adoptMember still tries by hash, so a
// member that has appeared since, or one caught mid-sync last time, is
// adopted as before. What this gives up: a member that was absent
// everywhere last time and has since become available by hash only. For
// the indexes that matter that cannot happen here — the ones the
// serve-contract guard requires (requiredIndex) always keep the by-hash
// probe — and for any other requestable index the unchanged-check
// reconcile runs the full sequence again.
func (a *Adopter) memberProbeFor(suite SuiteRef, m ReleaseMember, acquireByHash bool, servedBefore map[string]struct{}) memberProbe {
	if !acquireByHash || m.Path == "" {
		return probePath
	}
	if servedBefore == nil {
		return probeByHashFirst
	}
	if _, served := servedBefore[m.Path]; served || a.requiredIndex(suite.SuitePath, m.Path) {
		return probeByHashFirst
	}
	return probePathFirst
}

// requiredIndex reports whether path is an index file of a group a
// snapshot must serve: what missingRequestableIndexGroups requires (the
// "all" pseudo-architecture always, "source" when no allowlist excludes
// it, every allowlisted architecture) and what
// adoption.required_architectures adds.
func (a *Adopter) requiredIndex(suitePath, path string) bool {
	_, arch, ok := indexTargetGroup(suitePath, path)
	if !ok {
		return false
	}
	if arch == "all" || (arch == "source" && a.architectureAllowlist == nil) {
		return true
	}
	if _, want := a.architectureAllowlist[arch]; want {
		return true
	}
	_, want := a.requiredArchitectures[arch]
	return want
}

// memberMayHeal reports a member failure of the class a later attempt may
// not see: the bytes did not come, or were not the declared ones — a
// round-robin mirror serving the previous publication generation
// mid-sync (SPEC6_7 §1). A 404 or 410 ("declared but not served") and a
// local fault are not.
func memberMayHeal(err error) bool {
	return errors.Is(err, ErrAdoptionMemberFetchFailed) || errors.Is(err, ErrAdoptionMemberMismatch)
}

// memberEndsAttempt reports an error that ends the adoption attempt
// whichever member met it: a local fault (ErrAdoptionDBFailed) or an ended
// context. Every other member error is the member's own — skipped,
// tolerated or failed on by runShared.
func memberEndsAttempt(err error) bool {
	return err != nil && !errors.Is(err, errAdoptionMemberSkipped) && !memberMayHeal(err)
}

// fetchMembers does Step 5's upstream work for jobs: every member once,
// then the SPEC6_7 §1 retries — up to memberRetryCount more rounds for the
// members whose failure may heal, each after one memberRetryDelay. Every
// attempt runs the full adoptMember sequence (pool reuse, the probes), so
// a retry's by-hash probe is the heal path, as before.
//
// AIDEV-NOTE: the delay is shared. Each failing member used to wait
// memberRetryCount × memberRetryDelay on its own, one after another: six
// files caught mid-sync kept a verified release unpublished for six
// minutes at the defaults (2 × 30 s each). They heal together — the
// mirror finishes its sync — so they wait together: at most
// memberRetryCount × memberRetryDelay for the whole adoption. No host
// slot is held during the wait (adoptMember takes one per attempt).
//
// Never retried, as before: a 404 or 410 (a permanent publication
// artifact), a local fault, and anything once ctx has ended.
//
// It returns an error only for what ends the attempt whatever the member
// (memberEndsAttempt): a member's own failure stays in its job. When ctx
// ends it returns nil with jobs unfinished; the caller checks ctx.
func (a *Adopter) fetchMembers(ctx context.Context, suite SuiteRef, snapshotID int64, jobs []*memberJob, declared map[string]ReleaseMember, tracker *blobHeartbeatTracker) error {
	pending := jobs
	for round := 0; ; round++ {
		if err := a.fetchMemberRound(ctx, suite, snapshotID, pending, declared, tracker); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil // jobs are unfinished: the caller reports the context's end
		}
		var failing []*memberJob
		for _, j := range pending {
			switch {
			case j.err == nil:
				if round > 0 {
					adoptionMemberRetriesTotal.Inc("success")
				}
			case memberMayHeal(j.err):
				failing = append(failing, j)
			}
		}
		exhausted := func() {
			if round == 0 {
				return // nothing was retried
			}
			for range failing {
				adoptionMemberRetriesTotal.Inc("exhausted")
			}
		}
		if len(failing) == 0 {
			return nil
		}
		if round >= a.memberRetryCount {
			exhausted()
			return nil
		}
		for _, j := range failing {
			a.adoptionLogger(ctx).Info("adoption_member_retry",
				"canonical_host", suite.CanonicalHost,
				"suite_path", suite.SuitePath,
				"path", j.m.Path,
				"attempt", round+1,
				"max_retries", a.memberRetryCount,
				"delay", a.memberRetryDelay.String(),
				"waiting_members", len(failing),
				"err", j.err,
			)
		}
		if serr := a.retrySleep(ctx, a.memberRetryDelay); serr != nil {
			exhausted()
			return nil
		}
		pending = failing
	}
}

// workerPanic carries a panic from a pipeline worker to the adoption
// goroutine, with the worker's stack.
type workerPanic struct {
	value any
	stack []byte
}

func (p workerPanic) String() string {
	return fmt.Sprintf("%v\n\nworker stack:\n%s", p.value, p.stack)
}

// forEachConcurrently calls fn(i) for i = 0 … n−1, at most workers calls at
// a time, handing the indexes out in order. A call that returns false ends
// the hand-out: calls in flight finish, no further index is started.
//
// AIDEV-NOTE: a panic in a worker goroutine would end the daemon: the
// adoption goroutine's recover (Checker.recoverAdoption, security finding
// I-1) cannot see another goroutine's panic. Each worker therefore
// recovers; the first panic ends the hand-out, calls abort (when non-nil:
// the caller cancels the work in flight) and is raised again on the
// calling goroutine once every worker has returned, where the existing
// boundary counts, logs and records it. The worker's stack travels in the
// panic value (workerPanic).
func forEachConcurrently(workers, n int, abort func(), fn func(i int) bool) {
	if n <= 0 {
		return
	}
	var (
		next     atomic.Int64
		ended    atomic.Bool
		wg       sync.WaitGroup
		mu       sync.Mutex
		panicked *workerPanic
	)
	work := func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				mu.Lock()
				first := panicked == nil
				if first {
					panicked = &workerPanic{value: r, stack: debug.Stack()}
				}
				mu.Unlock()
				ended.Store(true)
				if first && abort != nil {
					abort()
				}
			}
		}()
		for !ended.Load() {
			i := int(next.Add(1)) - 1
			if i >= n {
				return
			}
			if !fn(i) {
				ended.Store(true)
				return
			}
		}
	}
	workers = max(1, min(workers, n))
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go work()
	}
	wg.Wait()
	if panicked != nil {
		panic(*panicked)
	}
}

// fetchMemberRound attempts every job once, memberWorkers at a time. A
// member that verifies is added to the heartbeat tracker and followed by a
// heartbeat (SPEC4 §7.5.2 site 2), so its blob is protected from the
// moment it is in the pool. The first error that ends the attempt
// (memberEndsAttempt) stops the round — fetches in flight are cancelled —
// and is returned.
func (a *Adopter) fetchMemberRound(ctx context.Context, suite SuiteRef, snapshotID int64, jobs []*memberJob, declared map[string]ReleaseMember, tracker *blobHeartbeatTracker) error {
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		mu    sync.Mutex
		fatal error
	)
	forEachConcurrently(a.memberWorkers, len(jobs), stop, func(i int) bool {
		if rctx.Err() != nil {
			return false
		}
		j := jobs[i]
		j.hash, j.reused, j.err = a.adoptMemberFromRelease(rctx, suite, j.m, j.probe, declared)
		j.attempts++
		switch {
		case j.err == nil:
			tracker.Add(j.hash)
			a.heartbeat(ctx, suite.CanonicalHost, snapshotID, tracker)
		case memberEndsAttempt(j.err):
			mu.Lock()
			if fatal == nil {
				fatal = j.err
			}
			mu.Unlock()
			stop()
			return false
		}
		return true
	})
	return fatal
}
