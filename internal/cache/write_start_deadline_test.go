package cache

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

// holdWriter holds the writer goroutine until the returned release is
// called; the cleanup releases it too, so a failing test cannot leave
// Close waiting on a held writer (cleanups run last-in first-out, so this
// one runs before openCache's Close).
func holdWriter(t *testing.T, c *Cache) (release func()) {
	t.Helper()
	release, err := c.HoldWriterForTest(context.Background())
	if err != nil {
		t.Fatalf("HoldWriterForTest: %v", err)
	}
	t.Cleanup(release)
	return release
}

// flushWriter submits a no-op write and waits for it: every request
// queued before it has been run or skipped once it returns.
func flushWriter(t *testing.T, c *Cache) {
	t.Helper()
	if err := c.submitWrite(context.Background(), func(context.Context, *sql.Conn) error { return nil }); err != nil {
		t.Fatalf("flush the writer: %v", err)
	}
}

// Issue #14 Tier 1a: a write queued behind a busy writer gives up at its
// start deadline, and the writer then skips it: the op never runs.
func TestSubmitWrite_AQueuedOpAbandonedAtItsStartDeadlineNeverRuns(t *testing.T) {
	c := openCache(t)
	release := holdWriter(t, c)

	var ran atomic.Bool
	start := time.Now()
	ctx := WithWriteStartDeadline(context.Background(), start.Add(50*time.Millisecond))
	err := c.submitWrite(ctx, func(context.Context, *sql.Conn) error {
		ran.Store(true)
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrWriteNotStarted) {
		t.Fatalf("submitWrite = %v, want ErrWriteNotStarted", err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("submitWrite returned after %v, want between 50ms and 250ms (the start deadline plus slack)", elapsed)
	}

	release()
	flushWriter(t, c)
	if ran.Load() {
		t.Fatal("the abandoned op ran after the writer was released")
	}
}

// The start deadline gates only starting: an op the writer started before
// the deadline runs to completion, its ctx untouched, and the submitter
// waits for its real result. The idle writer has 500 ms to start the op
// (ample under -race on a loaded CI box), and the op works 700 ms, well
// past the deadline.
func TestSubmitWrite_AnOpStartedBeforeItsDeadlineRunsToCompletion(t *testing.T) {
	const startDeadline, work = 500 * time.Millisecond, 700 * time.Millisecond
	c := openCache(t)
	errSentinel := errors.New("op finished")

	var opCtxErr atomic.Value
	var startedAt atomic.Int64 // ns after start
	start := time.Now()
	ctx := WithWriteStartDeadline(context.Background(), start.Add(startDeadline))
	err := c.submitWrite(ctx, func(opCtx context.Context, _ *sql.Conn) error {
		startedAt.Store(int64(time.Since(start)))
		timer := time.NewTimer(work)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-opCtx.Done():
		}
		opCtxErr.Store(errBox{opCtx.Err()})
		return errSentinel
	})
	if !errors.Is(err, errSentinel) {
		t.Fatalf("submitWrite = %v, want the op's own error %v", err, errSentinel)
	}
	if began := time.Duration(startedAt.Load()); began >= startDeadline {
		t.Fatalf("the op started %v after submit, not before its %v start deadline", began, startDeadline)
	}
	if elapsed := time.Since(start); elapsed < work {
		t.Fatalf("submitWrite returned after %v, before the op's %v of work finished", elapsed, work)
	}
	if box, _ := opCtxErr.Load().(errBox); box.err != nil {
		t.Fatalf("the op's ctx was cancelled (%v); the start deadline must never cancel a running op", box.err)
	}
}

// errBox lets atomic.Value hold a nil error.
type errBox struct{ err error }

// The crux of Tier 1a: the start deadline's timer and the writer's dequeue
// race. Whichever wins, the op runs at most once, and the submitter's
// answer is the truth: nil exactly when the op ran, ErrWriteNotStarted
// exactly when it did not.
func TestSubmitWrite_StartDeadlineRaceRunsAtMostOnceAndReportsTruthfully(t *testing.T) {
	c := openCache(t)
	rng := rand.New(rand.NewPCG(14, 1))
	const iterations = 2000
	var ranCount, notStartedCount int
	for i := range iterations {
		hold := time.Duration(rng.IntN(2001)) * time.Microsecond
		holdStarted := make(chan struct{})
		holdDone := make(chan error, 1)
		go func() {
			holdDone <- c.submitWrite(context.Background(), func(context.Context, *sql.Conn) error {
				close(holdStarted)
				time.Sleep(hold)
				return nil
			})
		}()
		<-holdStarted

		var runs atomic.Int32
		deadline := time.Now().Add(time.Duration(rng.IntN(2001)) * time.Microsecond)
		err := c.submitWrite(WithWriteStartDeadline(context.Background(), deadline), func(context.Context, *sql.Conn) error {
			runs.Add(1)
			return nil
		})
		if herr := <-holdDone; herr != nil {
			t.Fatalf("iteration %d: hold op: %v", i, herr)
		}
		flushWriter(t, c) // a run the submitter did not report would land by now

		switch n := runs.Load(); {
		case n > 1:
			t.Fatalf("iteration %d: the op ran %d times", i, n)
		case n == 1:
			if err != nil {
				t.Fatalf("iteration %d: the op ran but submitWrite reported %v", i, err)
			}
			ranCount++
		default:
			if !errors.Is(err, ErrWriteNotStarted) {
				t.Fatalf("iteration %d: the op never ran but submitWrite reported %v, want ErrWriteNotStarted", i, err)
			}
			notStartedCount++
		}
	}
	t.Logf("ran %d, not started %d", ranCount, notStartedCount)
	if ranCount == 0 || notStartedCount == 0 {
		t.Fatalf("the race was not exercised: ran %d, not started %d (want both > 0)", ranCount, notStartedCount)
	}
}

// A write with no start deadline behaves exactly as before Tier 1a: a
// cancelled submitter gets ctx.Err(), and the writer still dequeues and
// calls the op (with the cancelled ctx) once it is free.
func TestSubmitWrite_NoStartDeadlineBehavesAsBefore(t *testing.T) {
	c := openCache(t)
	release := holdWriter(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	var called atomic.Bool
	err := c.submitWrite(ctx, func(context.Context, *sql.Conn) error {
		called.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submitWrite = %v, want context.Canceled", err)
	}

	release()
	flushWriter(t, c)
	if !called.Load() {
		t.Fatal("a request with no start deadline was skipped; only a start deadline may make the writer skip an op")
	}
}

// Shutdown (ctx cancelled) while a write waits for its start deadline is
// ctx.Err(), as today, not ErrWriteNotStarted: a GC pass must see a
// shutdown as a shutdown. The request is also abandoned, so the writer
// never starts the op after its submitter has gone.
func TestSubmitWrite_ShutdownWhileWaitingForTheStartDeadlineIsCtxErr(t *testing.T) {
	c := openCache(t)
	release := holdWriter(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	var ran atomic.Bool
	start := time.Now()
	err := c.submitWrite(WithWriteStartDeadline(ctx, start.Add(time.Second)), func(context.Context, *sql.Conn) error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submitWrite = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("submitWrite returned after %v; the cancel at 50ms should end the wait", elapsed)
	}

	release()
	flushWriter(t, c)
	if ran.Load() {
		t.Fatal("the op ran after its submitter was cancelled while it was still queued")
	}
}

// A start deadline already past on entry never enqueues the request.
func TestSubmitWrite_AStartDeadlineAlreadyPastIsNotStarted(t *testing.T) {
	c := openCache(t)
	var ran atomic.Bool
	err := c.submitWrite(WithWriteStartDeadline(context.Background(), time.Now().Add(-time.Second)), func(context.Context, *sql.Conn) error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, ErrWriteNotStarted) {
		t.Fatalf("submitWrite = %v, want ErrWriteNotStarted", err)
	}
	flushWriter(t, c)
	if ran.Load() {
		t.Fatal("an op whose start deadline had passed on entry ran")
	}
}

// HoldWriterForTest returns only once its op runs on the writer, and the
// writer runs nothing else until release.
func TestHoldWriterForTest_HoldsUntilRelease(t *testing.T) {
	c := openCache(t)
	release := holdWriter(t, c)

	done := make(chan error, 1)
	go func() {
		done <- c.submitWrite(context.Background(), func(context.Context, *sql.Conn) error { return nil })
	}()
	select {
	case err := <-done:
		t.Fatalf("a write ran while the writer was held (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	release() // idempotent
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not run after release")
	}
}

// A shutdown reads as a shutdown even when the start deadline is due too
// (the final review's M-1): with ctx already cancelled and the start
// deadline already past on entry, submitWrite returns ctx.Err(), never
// ErrWriteNotStarted, and the op never runs.
func TestSubmitWrite_ACancelledCtxAndAPastDeadlineOnEntryIsCtxErr(t *testing.T) {
	c := openCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ran atomic.Bool
	err := c.submitWrite(WithWriteStartDeadline(ctx, time.Now().Add(-time.Second)), func(context.Context, *sql.Conn) error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submitWrite = %v, want context.Canceled: a shutdown must read as a shutdown", err)
	}
	flushWriter(t, c)
	if ran.Load() {
		t.Fatal("the op ran though its ctx was cancelled and its start deadline had passed")
	}
}

// lateDoneCtx is a cancelled ctx whose Done waits until past deadline
// before returning: submitWrite's selects evaluate ctx.Done() before they
// choose, so its start-deadline timer has fired by then too, and a select
// sees both the cancel and the start deadline ready at once. Err and Done
// are the cancelled ctx's, so it keeps the Context contract.
type lateDoneCtx struct {
	context.Context
	deadline time.Time
}

func (c lateDoneCtx) Done() <-chan struct{} {
	time.Sleep(time.Until(c.deadline) + time.Millisecond)
	return c.Context.Done()
}

// When the cancel and the start deadline are both ready in the same select
// (behind a held writer, enqueued or not), Go picks either at random; the
// submitter must still return ctx.Err() every time, and the op, which the
// writer never started, must never run (the final review's M-1).
func TestSubmitWrite_ACancelAndAStartDeadlineReadyTogetherIsCtxErr(t *testing.T) {
	c := openCache(t)
	release := holdWriter(t, c)

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	var runs atomic.Int32
	const iterations = 64 // below writeBufferSize: an enqueued request stays queued
	for i := range iterations {
		deadline := time.Now().Add(2 * time.Millisecond)
		ctx := WithWriteStartDeadline(lateDoneCtx{Context: parent, deadline: deadline}, deadline)
		err := c.submitWrite(ctx, func(context.Context, *sql.Conn) error {
			runs.Add(1)
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: submitWrite = %v, want context.Canceled: a shutdown must read as a shutdown", i, err)
		}
	}

	release()
	flushWriter(t, c)
	if n := runs.Load(); n != 0 {
		t.Fatalf("the op ran %d times; a request whose submitter was cancelled before the writer started it must never run", n)
	}
}
