package observability

import (
	"context"
	"log/slog"
	"sync"
)

// Waits reports outstanding operations without querying the database or taking
// any of the locks those operations need. Names are a fixed vocabulary; parallel
// member downloads may contribute more than one wait of the same kind.
type Waits struct {
	// Identity is immutable once the work is published to other goroutines.
	WorkID, WorkKind string
	mu               sync.Mutex
	counts           map[string]int
}

type waitsKey struct{}

func WithWaits(ctx context.Context, w *Waits) context.Context {
	return context.WithValue(ctx, waitsKey{}, w)
}

func TrackWait(ctx context.Context, name string) func() {
	w, _ := ctx.Value(waitsKey{}).(*Waits)
	if w == nil {
		return func() {}
	}
	w.mu.Lock()
	if w.counts == nil {
		w.counts = make(map[string]int)
	}
	w.counts[name]++
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		w.counts[name]--
		if w.counts[name] == 0 {
			delete(w.counts, name)
		}
		w.mu.Unlock()
	}
}

func (w *Waits) Snapshot() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.counts))
	for k, v := range w.counts {
		out[k] = v
	}
	return out
}

// WorkLogger correlates probes and maintenance transfers as well as adoptions.
func WorkLogger(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if w, _ := ctx.Value(waitsKey{}).(*Waits); w != nil {
		return logger.With("work_id", w.WorkID, "work_kind", w.WorkKind)
	}
	return logger
}
