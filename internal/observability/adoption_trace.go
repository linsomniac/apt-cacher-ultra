package observability

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
)

// AdoptionTrace correlates one attempt, including redirects and retries. It is
// carried by context, never stored on the shared adopter or HTTP client.
type AdoptionTrace struct {
	ID      string
	mu      sync.Mutex
	traffic AdoptionTraffic
}

type AdoptionTraffic struct {
	Bytes, Responses, MemberBytes, HotPrefetchBytes int64
}

func NewAdoptionTrace() *AdoptionTrace { return &AdoptionTrace{ID: rand.Text()} }

type adoptionTraceKey struct{}

func WithAdoptionTrace(ctx context.Context, trace *AdoptionTrace) context.Context {
	return context.WithValue(ctx, adoptionTraceKey{}, trace)
}

func AdoptionTraceFromContext(ctx context.Context) *AdoptionTrace {
	trace, _ := ctx.Value(adoptionTraceKey{}).(*AdoptionTrace)
	return trace
}

func AdoptionLogger(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if trace := AdoptionTraceFromContext(ctx); trace != nil {
		return logger.With("adoption_id", trace.ID)
	}
	return logger
}

// RecordTransfer uses the same actual body bytes as upstream_response and the
// traffic counters. Response callbacks can run concurrently with body closure.
func (t *AdoptionTrace) RecordTransfer(purpose string, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.traffic.Bytes += bytes
	t.traffic.Responses++
	switch purpose {
	case "adoption_member":
		t.traffic.MemberBytes += bytes
	case "hot_prefetch":
		t.traffic.HotPrefetchBytes += bytes
	}
}

func (t *AdoptionTrace) Traffic() AdoptionTraffic {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.traffic
}
