package freshness

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/flipgate"
	"github.com/linsomniac/apt-cacher-ultra/internal/observability"
)

// packageListReasonDisabled is the package_list_rewrite_reason of a commit
// that wrote the whole list because adoption.write_package_list_differences
// is false. The cache's own reasons (cache.PackageListWrite.Reason) say why
// a difference it was given, or could have been given, was not applied;
// this one is the adopter's, which is what decides not to ask for one.
const packageListReasonDisabled = "disabled"

type adoptionTrigger struct {
	source, reason, detail string
	retry                  bool
	previousFailures       int64
	previousFailureReason  string
	previousSnapshot       int64
	previousKnown          bool
	// changeSeenAt is when the cache first saw the upstream change this
	// attempt adopts (suite_freshness.inrelease_change_seen_at, unix
	// seconds; 0: unknown, as for a direct adopter call).
	changeSeenAt int64
}

// adoptionProgressInterval is how often an attempt that is still running
// logs adoption_in_progress: after its first minute, and every minute
// after (a constant, not a knob).
const adoptionProgressInterval = time.Minute

// phaseMark is the phase an attempt is in and since when (monotonic).
type phaseMark struct {
	name  string
	since time.Time
}

// Only the adoption goroutine updates lifecycle fields. Transfer accounting is
// separately synchronized in trace, since HTTP bodies may close concurrently.
type adoptionLog struct {
	trace                          *observability.AdoptionTrace
	work                           *backgroundWork
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
	// committed: the flip transaction committed, and packageList is how it
	// wrote the suite's package list.
	committed   bool
	packageList cache.PackageListWrite
	// releaseDate is the Release's Date field (unix seconds; 0: none, or
	// not parsed yet).
	releaseDate int64
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
	l := &adoptionLog{
		trace: observability.NewAdoptionTrace(), trigger: trigger, started: start,
		phaseStarted: start, phase: "prepare", phases: make(map[string]float64),
		previous: trigger.previousSnapshot, previousKnown: trigger.previousKnown,
	}
	return l
}

func (l *adoptionLog) nextPhase(phase string) {
	now := time.Now()
	l.phases[l.phase] += now.Sub(l.phaseStarted).Seconds()
	l.phase, l.phaseStarted = phase, now
	if l.work != nil {
		l.work.setPhase(phase)
	}
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

// finish logs adoption_finished. now is the wall clock (the adopter's),
// for the two ages measured against persisted or signed stamps.
func (l *adoptionLog) finish(logger *slog.Logger, ctx context.Context, suite SuiteRef, p *adoptionPayload, err error, panicked bool, now time.Time) {
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
	// How the commit wrote the suite's package list: the rows that changed
	// (difference), the whole list (rewrite, with why), or none (an
	// unsigned snapshot). Only an attempt that committed has one.
	if l.committed {
		attrs = append(attrs, "package_list_write", l.packageList.Mode)
		if l.packageList.Mode == cache.PackageListRewrite {
			attrs = append(attrs, "package_list_rewrite_reason", l.packageList.Reason)
		}
		attrs = append(attrs, "package_rows_added", l.packageList.Added,
			"package_rows_changed", l.packageList.Changed, "package_rows_removed", l.packageList.Removed)
	}
	// What the attempt's database writes waited behind: every write in the
	// daemon runs on one goroutine, and phase_seconds alone cannot tell a
	// slow transaction of the attempt's own from a queue.
	if l.work != nil {
		attrs = append(attrs, l.work.writerTotal()...)
	}
	// How late: how long the cache had known of this upstream change when
	// the attempt ended, and how old the release itself — by its signed
	// Date — was then. For a success the second is the publication's lag
	// behind upstream, whatever the cache's own logs cover. Each only when
	// known (a direct adopter call has no first sighting; a Release may
	// carry no Date).
	if l.trigger.changeSeenAt > 0 {
		attrs = append(attrs, "first_seen_seconds", max(now.Unix()-l.trigger.changeSeenAt, 0))
	}
	if l.releaseDate > 0 {
		attrs = append(attrs, "release_date", l.releaseDate, "release_age_seconds", now.Unix()-l.releaseDate)
	}
	if err != nil {
		path, detail := memberErrorFields(err)
		attrs = append(attrs, "err", err, "member_path", path, "detail", detail)
	}
	// Always INFO, including failures: the existing categorized warning/error
	// events retain their severity; this is one consistent audit record.
	logger.Info("adoption_finished", attrs...)
}
