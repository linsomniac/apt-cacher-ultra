package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// idleWatch enforces upstream.idle_read_timeout on one upstream attempt:
// it cancels the attempt's context when upstream stays silent for d while
// the attempt is waiting on it — for the response headers, or for the next
// body bytes.
//
// AIDEV-NOTE: the clock runs only while the attempt is blocked on upstream
// (armed around httpClient.Do and around each body Read), never while the
// bytes are written to the cache: a slow disk is not a silent upstream.
// Without this bound a stalled response lasted until total_timeout (5 min
// by default), which held an adoption's member pipeline, a hot prefetch or
// a repair — and with them the suite's lock — for that long (the
// 2026-11-14 publication-delay review).
type idleWatch struct {
	d      time.Duration
	cancel context.CancelFunc

	mu      sync.Mutex
	timer   *time.Timer
	armed   bool
	armedAt time.Time // monotonic: when the clock last started
	fired   bool
	done    bool
}

// watchIdle derives the attempt's context. With d <= 0 there is no bound:
// it returns ctx and a nil watch, whose methods do nothing.
func watchIdle(ctx context.Context, d time.Duration) (context.Context, *idleWatch) {
	if d <= 0 {
		return ctx, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return ctx, &idleWatch{d: d, cancel: cancel}
}

// arm starts the clock: the attempt now waits on upstream.
func (w *idleWatch) arm() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done || w.fired {
		return
	}
	// armedAt before the timer starts: a legitimate fire then always finds
	// at least d elapsed (fire's stale-callback test).
	w.armed, w.armedAt = true, time.Now()
	if w.timer == nil {
		w.timer = time.AfterFunc(w.d, w.fire)
		return
	}
	w.timer.Reset(w.d)
}

// disarm stops the clock: upstream answered, or sent bytes.
func (w *idleWatch) disarm() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	if w.timer != nil {
		w.timer.Stop()
	}
}

// fire is the timer's callback.
//
// AIDEV-NOTE: Stop and Reset do not recall a callback that has already
// started, so fire decides under the mutex whether it still stands: the
// clock must be armed and must have run for d since it was last started.
// A callback that lost a race with disarm (bytes arrived) or that belongs
// to an earlier arming (the clock was started again since) does nothing;
// Reset has scheduled the callback that will stand.
func (w *idleWatch) fire() {
	w.mu.Lock()
	if w.done || !w.armed || time.Since(w.armedAt) < w.d {
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.mu.Unlock()
	w.cancel()
}

// stop ends the watch and releases the attempt's context. Call it once the
// attempt is over, on every path.
func (w *idleWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
	w.cancel()
}

// timedOut reports whether the watch cancelled the attempt.
func (w *idleWatch) timedOut() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// attemptErr is the error an attempt returns: err itself, unless the watch
// cancelled the attempt and err is the cancellation it caused — then
// ErrIdleTimeout, with no context error in its chain, so that no caller
// reads it as a shutdown or as its own deadline, and the retry loop treats
// it as the upstream fault it is. parent is the context the watch was
// derived from: when that one ended too, its error stands.
func (w *idleWatch) attemptErr(parent context.Context, err error) error {
	if err == nil || !w.timedOut() || parent.Err() != nil || !errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w (%s)", ErrIdleTimeout, w.d)
}

// reader wraps an attempt's response body: the clock runs while a Read
// waits on upstream.
func (w *idleWatch) reader(r io.Reader) io.Reader {
	if w == nil {
		return r
	}
	return &idleReader{r: r, w: w}
}

type idleReader struct {
	r io.Reader
	w *idleWatch
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.w.arm()
	n, err := r.r.Read(p)
	r.w.disarm()
	return n, err
}
