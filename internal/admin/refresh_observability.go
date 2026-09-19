package admin

import (
	"context"
	"time"
)

// Wait a full interval after completion so an expensive refresh never leaves
// a pending ticker event that immediately starts another round of scans.
// The initial refresh remains synchronous in Serve.
func runRefresherLoop(ctx context.Context, interval time.Duration, refresh func(context.Context)) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			refresh(ctx)
			timer.Reset(interval)
		}
	}
}

// stage is a fixed internal label, never a repository URL or a request value.
// Timing is once per query group/walk; it does not sample individual rows.
func (s *Server) observeRefresh(stage string, start time.Time, err error) {
	duration := time.Since(start)
	s.gauges.refreshDuration.Observe(duration.Seconds(), stage)
	if err != nil {
		s.gauges.refreshFailures.Inc(stage)
	} else {
		s.gauges.refreshFailures.Add(0, stage)
		s.gauges.refreshLastSuccess.Set(float64(time.Now().Unix()), stage)
	}
	s.logger.Debug("admin_refresh_complete",
		"stage", stage,
		"duration_ms", duration.Milliseconds(),
		"success", err == nil)
}
