package handler

import (
	"sync"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/proxy"
)

// flatRootWarnEvery rate-limits flat_host_root_unsupported: at most one
// line per minute process-wide; suppressed counts the requests in between
// (spec C2, plan design note 2).
const flatRootWarnEvery = time.Minute

// flatRootWarn is flat_host_root_unsupported's rate limiter.
type flatRootWarn struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// noteFlatHostRoot logs, at most once a minute process-wide, that a client
// asked for a flat repository's file at the host root (spec C2:
// `deb https://h/ /` and `deb https://h ./` are unsupported — such a
// directory is no suite, so it is never refreshed, adopted or vouched).
// The request is served as before (Phase 1). Monotonic time.
//
// AIDEV-NOTE: WARN, not INFO (design note 2): the repository will never
// refresh, and this line is the only place an operator learns it. The
// limit is global, not per host, so a client population cannot turn it
// into a log flood; the suppressed count says how many requests the last
// minute hid.
func (h *Handler) noteFlatHostRoot(req *proxy.Request) {
	h.flatRoot.mu.Lock()
	now := time.Now()
	if !h.flatRoot.last.IsZero() && now.Sub(h.flatRoot.last) < flatRootWarnEvery {
		h.flatRoot.suppressed++
		h.flatRoot.mu.Unlock()
		return
	}
	suppressed := h.flatRoot.suppressed
	h.flatRoot.last, h.flatRoot.suppressed = now, 0
	h.flatRoot.mu.Unlock()
	h.logger.Warn("flat_host_root_unsupported",
		"canonical_host", req.CanonicalHost,
		"path", req.Path,
		"suppressed", suppressed,
		"detail", "a flat repository at the host root is not tracked: it is served from the ordinary cache and never refreshed",
	)
}
