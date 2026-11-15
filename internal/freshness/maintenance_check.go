package freshness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
	"github.com/linsomniac/apt-cacher-ultra/internal/fetch"
)

// checkDuringRepair observes the anchor without stopping a maintenance fetch.
// An unchanged release lets a slow repair finish across many refresh intervals.
// Anything requiring the normal state machine (change, absence, failure, form
// upgrade) returns false: the caller asks maintenance to yield, then checks
// under the suite lock. Only the guarded timestamp update below writes here;
// this path never verifies, publishes, changes validators or clears pending work.
// lateralHeld may clamp its in-memory retry memo after a backward clock step.
func (c *Checker) checkDuringRepair(ctx context.Context, ref SuiteRef, key string, opts checkOpts, run *repairRun) (report checkReport, handled bool) {
	confirmed := false
	defer func() {
		if ctx.Err() != nil {
			report, handled = checkReport{result: resultBusy}, true
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			unloggedPanic(func() { logPanic(c.logger, "freshness_check_panicked", "maintenance_probe", ref, r) })
			report, handled = checkReport{result: resultBusy}, confirmed
		}
	}()
	v, ok := c.repairs.Load(key)
	if !ok || v != run || ctx.Err() != nil {
		return checkReport{result: resultBusy}, true
	}
	if !run.probeMu.TryLock() {
		return checkReport{result: resultBusy}, true
	}
	defer run.probeMu.Unlock()
	probeCtx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()
	w := newBackgroundWork(c.logger, ref, "maintenance_probe", "", time.Minute)
	defer w.finish()
	probeCtx = w.context(probeCtx)
	cur, err := c.cache.GetSuiteFreshness(probeCtx, ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath)
	if err != nil || cur.CurrentSnapshotID == nil || *cur.CurrentSnapshotID != run.snapshotID {
		return checkReport{}, false
	}
	now := c.now()
	// Unlike run age, the persisted check stamp is correct for a manually
	// started reconcile too. A due check is never held back by its start time.
	if cur.LastCheckAt != nil {
		age := now.Sub(time.Unix(*cur.LastCheckAt, 0))
		if age < -clockStepTolerance {
			return checkReport{}, false // ordinary check reports and repairs the clock step
		}
		if !opts.bypassCooldown && age < c.cooldown {
			freshnessSkippedTotal.Inc("cooldown", ref.CanonicalHost)
			return checkReport{result: resultSkipped}, true
		}
	}
	if cur.InReleaseChangeSeenAt != nil || cur.AdoptionFailCount > 0 {
		return checkReport{}, false
	}
	snap, err := c.cache.GetSuiteSnapshot(probeCtx, run.snapshotID)
	if err != nil {
		return checkReport{}, false
	}
	name := "InRelease"
	if snap.Form != cache.FormInline {
		name = "Release"
	}
	models := map[string]string{}
	if row, err := c.cache.LookupURL(probeCtx, ref.CanonicalScheme, ref.CanonicalHost, ref.SuitePath+"/"+name); err == nil {
		models[name] = row.UpstreamURL
	}
	urls := c.anchorURLsFor(ref, models)
	w.setPhase("maintenance_probe_slot")
	slotCtx, slotCancel := context.WithTimeout(probeCtx, 2*time.Second)
	// Repair downloads may occupy every bulk slot. One additional, bounded
	// metadata probe per host prevents them from starving detection, without
	// cancelling unchanged repairs on every refresh tick. Another probe's
	// contention is not evidence that maintenance needs to yield.
	release, err := c.maintenanceProbeSem.Acquire(slotCtx, ref.CanonicalHost)
	slotCancel()
	if err != nil {
		return checkReport{result: resultBusy}, true
	}
	heldSlot := true
	defer func() {
		if heldSlot {
			release()
		}
	}()
	w.setPhase("upstream")
	if name != "InRelease" {
		// A detached/unsigned suite may have gained an inline signature.
		o := c.probe(probeCtx, ref, urls, "InRelease")
		heldInline := false
		if snap.Form == cache.FormDetached && o.class == classPresent {
			req := (probeTree{form: adoptionFormInline, found: true, inRelease: o}).request(ref)
			heldInline = c.lateralHeld(key, req.key, now.Unix())
		}
		if !heldInline && (o.class != classAbsent || o.authDenied()) {
			return checkReport{}, false
		}
	}
	if snap.Form == cache.FormUnsigned {
		// A newly available detached signature also requires the ordinary
		// upgrade/verification path, even when Release bytes are unchanged.
		if o := c.probe(probeCtx, ref, urls, "Release.gpg"); o.class != classAbsent || o.authDenied() {
			return checkReport{}, false
		}
	}
	etag, lastmod := "", ""
	if cur.InReleaseETag != nil {
		etag = *cur.InReleaseETag
	}
	if cur.InReleaseLastMod != nil {
		lastmod = *cur.InReleaseLastMod
	}
	res, err := c.fetcher.Conditional(probeCtx, &fetch.Target{CanonicalHost: ref.CanonicalHost, URL: urls.of(name)}, etag, lastmod, c.maxBody)
	if err != nil || res == nil {
		return checkReport{}, false
	}
	unchanged := res.Status == 304 && (etag != "" || lastmod != "")
	if res.Status == 200 {
		hash := sha256.Sum256(res.Body)
		anchorHash := snap.AnchorHash(name)
		unchanged = anchorHash != nil && hex.EncodeToString(hash[:]) == *anchorHash
	}
	if !unchanged {
		return checkReport{}, false
	}
	confirmed = true
	release()
	heldSlot = false
	w.setPhase("record_result")
	// Maintenance might have finished and another adoption committed during
	// the GET. The compare-and-update cannot stamp a different snapshot fresh.
	updated, err := c.cache.ConfirmSnapshotUnchanged(probeCtx, cache.SuiteKey{
		CanonicalScheme: ref.CanonicalScheme, CanonicalHost: ref.CanonicalHost, SuitePath: ref.SuitePath}, run.snapshotID, now.Unix())
	if err != nil {
		c.logger.Warn("maintenance_probe_stamp_failed", "canonical_host", ref.CanonicalHost, "suite_path", ref.SuitePath, "err", err)
	}
	if err != nil || !updated {
		return checkReport{result: resultBusy}, true
	}
	result := "unchanged"
	if res.Status == 304 {
		result = "not_modified"
	}
	c.logger.Info("freshness_check", "canonical_scheme", ref.CanonicalScheme, "canonical_host", ref.CanonicalHost,
		"suite_path", ref.SuitePath, "result", result, "upstream_status", res.Status,
		"form", snap.Form, "during_maintenance", true, "snapshot_id", run.snapshotID, "work_id", w.id)
	freshnessCheckTotal.Inc(result, ref.CanonicalHost)
	return checkReport{result: result}, true
}
