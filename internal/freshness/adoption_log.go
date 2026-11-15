package freshness

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

type adoptionTrigger struct {
	source, reason, detail string
	retry                  bool
	previousFailures       int64
	previousFailureReason  string
	previousSnapshot       int64
	previousKnown          bool
}

// Only the adoption goroutine updates lifecycle fields. Transfer accounting is
// separately synchronized in trace, since HTTP bodies may close concurrently.
type adoptionLog struct {
	trace                          *observability.AdoptionTrace
	trigger                        adoptionTrigger
	started, phaseStarted          time.Time
	phase                          string
	phases                         map[string]float64
	previous, candidate, resulting int64
	previousKnown                  bool
	memberCount                    int
	membersReady                   int
	membersReused                  int
	membersDownloaded              int
	membersSkipped                 int
	hot                            hotPrefetchStats
	pipelineDone                   bool
}

type adoptionLogKey struct{}

func (a *Adopter) adoptionLogger(ctx context.Context) *slog.Logger {
	return observability.AdoptionLogger(ctx, a.logger)
}

func (c *Checker) adoptionLogger(req *adoptionRequest) *slog.Logger {
	if req.log != nil {
		return c.logger.With("adoption_id", req.log.trace.ID)
	}
	return c.logger
}

func adoptionLogFromContext(ctx context.Context) *adoptionLog {
	l, _ := ctx.Value(adoptionLogKey{}).(*adoptionLog)
	return l
}

func newAdoptionLog(trigger adoptionTrigger, start time.Time) *adoptionLog {
	if trigger.source == "" {
		trigger.source = "direct"
	}
	if trigger.reason == "" {
		trigger.reason = "explicit_run"
	}
	return &adoptionLog{
		trace: observability.NewAdoptionTrace(), trigger: trigger, started: start,
		phaseStarted: start, phase: "prepare", phases: make(map[string]float64),
		previous: trigger.previousSnapshot, previousKnown: trigger.previousKnown,
	}
}

func (l *adoptionLog) nextPhase(phase string) {
	now := time.Now()
	l.phases[l.phase] += now.Sub(l.phaseStarted).Seconds()
	l.phase, l.phaseStarted = phase, now
}

func (l *adoptionLog) attrs(suite SuiteRef, form adoptionForm) []any {
	return []any{
		"adoption_id", l.trace.ID, "canonical_scheme", suite.CanonicalScheme,
		"canonical_host", suite.CanonicalHost, "suite_path", suite.SuitePath,
		"form", formName(form), "trigger_source", l.trigger.source,
		"trigger_reason", l.trigger.reason, "trigger_detail", l.trigger.detail,
		"retry", l.trigger.retry, "previous_failures", l.trigger.previousFailures,
		"previous_failure_reason", l.trigger.previousFailureReason,
		"previous_snapshot_id", l.previous, "previous_snapshot_known", l.previousKnown,
	}
}

func (l *adoptionLog) finish(logger *slog.Logger, ctx context.Context, suite SuiteRef, p *adoptionPayload, err error, panicked bool) {
	lastPhase := l.phase
	l.nextPhase("finished")
	outcome := classifyAdoptionOutcome(err)
	if err != nil && !l.pipelineDone && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		outcome = "timed_out"
	}
	if errors.Is(err, flipgate.ErrAborted) || (err != nil && p.stop != nil && p.stop.Err() != nil) {
		outcome = "abandoned_at_shutdown"
	} else if err != nil && p.stop == nil && errors.Is(ctx.Err(), context.Canceled) {
		outcome = "canceled"
	}
	if panicked {
		outcome = "panicked"
	}
	reason := classifyAdoptionReason(err, outcome, p.gpgReasonClassifier)
	if panicked {
		reason = "panic"
	}
	traffic := l.trace.Traffic()
	attrs := append(l.attrs(suite, p.form),
		"outcome", outcome, "reason", reason, "last_phase", lastPhase,
		"duration_seconds", time.Since(l.started).Seconds(), "phase_seconds", l.phases,
		"candidate_snapshot_id", l.candidate, "resulting_snapshot_id", l.resulting,
		"upstream_bytes", traffic.Bytes, "upstream_responses", traffic.Responses,
		"member_upstream_bytes", traffic.MemberBytes, "hot_prefetch_upstream_bytes", traffic.HotPrefetchBytes,
		"member_count", l.memberCount, "members_ready", l.membersReady,
		"members_reused", l.membersReused, "members_downloaded", l.membersDownloaded,
		"members_skipped", l.membersSkipped,
		"hot_count", l.hot.hotCount, "hot_fetched", l.hot.fetched, "hot_reused", l.hot.reused,
		"hot_failed", l.hot.failed, "hot_mismatched", l.hot.mismatched, "hot_unattempted", l.hot.unattempted,
	)
	if err != nil {
		path, detail := memberErrorFields(err)
		attrs = append(attrs, "err", err, "member_path", path, "detail", detail)
	}
	// Always INFO, including failures: the existing categorized warning/error
	// events retain their severity; this is one consistent audit record.
	logger.Info("adoption_finished", attrs...)
}
