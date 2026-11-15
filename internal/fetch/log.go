package fetch

import (
	"context"
	"log/slog"

	"github.com/linsomniac/apt-cacher-ultra/internal/observability"

	"github.com/linsomniac/apt-cacher-ultra/internal/usage"
)

type purposeKey struct{}
type transferKey struct{}

// WithPurpose attributes upstream traffic to its caller. It survives redirects
// and retries; it never changes request headers or the upstream URL.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeKey{}, purpose)
}

type transfer struct{ host, purpose string }

func withTransfer(ctx context.Context, host, fallback string) context.Context {
	purpose, _ := ctx.Value(purposeKey{}).(string)
	if purpose == "" {
		purpose = fallback
	}
	return context.WithValue(ctx, transferKey{}, transfer{host, purpose})
}

func logResponse(logger *slog.Logger) func(usage.Response) {
	return func(r usage.Response) {
		ctx := r.Request.Context()
		t, _ := ctx.Value(transferKey{}).(transfer)
		if trace := observability.AdoptionTraceFromContext(ctx); trace != nil {
			trace.RecordTransfer(t.purpose, r.Bytes)
		}
		// Log the actual redirect destination separately from the cache host.
		// Do not expose userinfo, query tokens, headers, or raw error strings.
		observability.AdoptionLogger(ctx, logger).Info("upstream_response", "canonical_host", t.host,
			"purpose", t.purpose, "scheme", r.Request.URL.Scheme,
			"host", r.Request.URL.Host, "path", r.Request.URL.EscapedPath(),
			"method", r.Request.Method, "status", r.Status, "bytes_read", r.Bytes,
			"duration_ms", r.Duration.Milliseconds(), "complete", r.Complete,
			"read_error", r.ReadError)
	}
}
