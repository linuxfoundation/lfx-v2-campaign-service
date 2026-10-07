// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// insightReportDriver is one saved-report KIND's half of the report-backed insight reads
// (ReadReportedKeywordPerformance, ReadReportedAudience): the kind's store operations and its
// instrumented upstream calls, bound to one reader and one store. The collect/refresh steps
// below are written once over it, so the keyword and age/gender reads cannot drift apart on
// freshness, abandonment, the compare-and-set handling or what counts as permanent.
type insightReportDriver[R any] struct {
	// read names the read in log lines: "keyword read", "audience read".
	read      string
	get       func(ctx context.Context, key model.InsightReportKey) (*model.InsightReportSnapshot[R], error)
	mark      func(ctx context.Context, key model.InsightReportKey, p model.PendingInsightReport) (bool, error)
	complete  func(ctx context.Context, key model.InsightReportKey, r model.ReadyInsightReport[R]) (bool, error)
	fail      func(ctx context.Context, key model.InsightReportKey, reportID, reason string, at time.Time) (bool, error)
	check     func(callCtx, ctx context.Context, key model.InsightReportKey, reportID string) (*model.InsightReportCheck[R], error)
	submit    func(callCtx, ctx context.Context, key model.InsightReportKey, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error)
	permanent func(error) bool
}

// InsightReportPeriod is the part of every report-backed insight reader that maps a window onto
// the calendar dates a report submitted at now covers — the SAME rule (time zone, day boundary,
// month boundary) its Submit uses, so the orchestrator can tell whether a saved report describes
// the period the caller is asking about NOW. It is local: no connection, no upstream call.
type InsightReportPeriod interface {
	ReportWindowDates(window model.MetricsWindow, now time.Time) (start, end time.Time, err error)
}

// sameReportDay reports whether a and b fall on the same UTC calendar day. Saved report dates are
// DATE columns (UTC midnight) while a freshly resolved end carries the clock time, so dates are
// compared by day, never by instant.
func sameReportDay(a, b time.Time) bool {
	a, b = a.UTC(), b.UTC()
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// discardOtherPeriod drops, from the in-memory snapshot only, a finished report whose saved dates
// are not the dates the requested window resolves to NOW. Freshness by submission age alone is not
// enough: a this_month report requested at 23:50 on 31 October is minutes old at 00:10 on
// 1 November, and serving it would label October's data as November's (likewise `today` across
// midnight, or any window across a day). With the ready half dropped, the read behaves exactly as
// when no report has finished: refreshInsightReport submits a replacement (unless one is already
// pending) and the merge serves nothing, with metrics_as_of absent and metrics_pending set. The
// stored row is untouched; the replacement overwrites it when it completes.
func discardOtherPeriod[R any](snap *model.InsightReportSnapshot[R], wantStart, wantEnd time.Time) {
	if r := snap.Ready; r != nil && (!sameReportDay(r.WindowStart, wantStart) || !sameReportDay(r.WindowEnd, wantEnd)) {
		snap.Ready = nil
	}
}

// resolveInsightPeriod asks the reader for the dates window covers at now. A window the reader
// cannot date was already refused by its Enabled check, so a failure here is the same permanent
// unsupported-window refusal.
func resolveInsightPeriod(reader InsightReportPeriod, platform model.Provider, read string, window model.MetricsWindow, now time.Time) (time.Time, time.Time, error) {
	start, end, err := reader.ReportWindowDates(window, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%s %s: %w", platform, read, errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	return start, end, nil
}

// SetInsightReportClock pins the clock the report-backed insight reads judge freshness and
// abandonment by, and stamp submissions with. nil restores time.Now. For tests.
func (o *Orchestrator) SetInsightReportClock(now func() time.Time) {
	o.insightClockMu.Lock()
	defer o.insightClockMu.Unlock()
	o.insightClock = now
}

func (o *Orchestrator) insightReportNow() time.Time {
	o.insightClockMu.RLock()
	defer o.insightClockMu.RUnlock()
	if o.insightClock != nil {
		return o.insightClock()
	}
	return time.Now()
}

// collectPendingInsightReport is step 3 of a report-backed insight read —
// collectPendingAccountReport's logic over one kind's store, updating snap in place: check the
// pending report once; store it when finished, drop it when the platform failed it, abandon it
// past accountReportAbandonAfter. Best-effort: every store fault is logged, never returned.
func collectPendingInsightReport[R any](callCtx, ctx context.Context, d insightReportDriver[R], snap *model.InsightReportSnapshot[R], now time.Time) {
	p := snap.Pending
	if p == nil {
		return
	}
	key := snap.Key
	overdue := now.Sub(p.SubmittedAt) > accountReportAbandonAfter
	abandon := func() {
		reason := fmt.Sprintf("report not finished %s after submission; abandoned", accountReportAbandonAfter)
		if _, ferr := d.fail(callCtx, key, p.ReportID, reason, now); ferr != nil {
			slog.WarnContext(ctx, d.read+": could not record an abandoned report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
	}
	check, cerr := d.check(callCtx, ctx, key, p.ReportID)
	if cerr != nil || check == nil {
		slog.WarnContext(ctx, d.read+": report check failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", cerr)
		if overdue {
			abandon()
		}
		return
	}
	switch check.Status {
	case model.AccountReportPending:
		if overdue {
			abandon()
		}
	case model.AccountReportReady:
		ready := model.ReadyInsightReport[R]{
			ReportID: p.ReportID, Rows: check.Rows, Partial: check.Partial, CampaignIDs: p.CampaignIDs,
			WindowStart: p.WindowStart, WindowEnd: p.WindowEnd, AsOf: p.SubmittedAt,
		}
		applied, perr := d.complete(callCtx, key, ready)
		if perr != nil {
			slog.WarnContext(ctx, d.read+": could not save a finished report", "platform", key.Platform, "project_id", key.ProjectID, "error", perr)
		}
		// Served either way, for collectPendingAccountReport's reason.
		snap.Ready = &ready
		if applied {
			snap.Pending = nil
		} else if perr == nil {
			if latest, gerr := d.get(callCtx, key); gerr == nil {
				snap.Pending = latest.Pending
			}
		}
	case model.AccountReportFailed:
		if _, ferr := d.fail(callCtx, key, p.ReportID, "the platform reported the report as failed", now); ferr != nil {
			slog.WarnContext(ctx, d.read+": could not record a failed report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
	default:
		slog.WarnContext(ctx, d.read+": report check returned an unknown status", "platform", key.Platform, "project_id", key.ProjectID, "status", string(check.Status))
	}
}

// refreshInsightReport is step 4 — refreshAccountReport's logic, with scope coverage added to
// what makes a saved report stale. Only a permanent refusal (d.permanent) fails the read; any
// other submission failure is logged and the saved report served.
func refreshInsightReport[R any](callCtx, ctx context.Context, d insightReportDriver[R], snap *model.InsightReportSnapshot[R], scope []model.ProjectCampaignScope, scopeIDs map[string]bool, now time.Time) error {
	if snap.Pending != nil {
		return nil
	}
	if r := snap.Ready; r != nil && now.Sub(r.AsOf) < accountReportFreshFor && coversScope(r.CampaignIDs, scopeIDs) {
		return nil
	}
	if callCtx.Err() != nil {
		return nil
	}
	key := snap.Key
	sub, serr := d.submit(callCtx, ctx, key, scope)
	if serr != nil && d.permanent(serr) {
		return fmt.Errorf("%s %s: %w", key.Platform, d.read, serr)
	}
	if serr != nil {
		slog.WarnContext(ctx, d.read+": report submission failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", serr)
		return nil
	}
	if sub == nil || sub.ReportID == "" || len(sub.CampaignIDs) == 0 {
		slog.WarnContext(ctx, d.read+": report submission returned no report id or scope", "platform", key.Platform, "project_id", key.ProjectID)
		return nil
	}
	pending := model.PendingInsightReport{
		ReportID: sub.ReportID, CampaignIDs: sub.CampaignIDs,
		WindowStart: sub.WindowStart, WindowEnd: sub.WindowEnd, SubmittedAt: now,
	}
	// Detached budget, for accountReportMarkTimeout's reason.
	markCtx, markCancel := context.WithTimeout(context.WithoutCancel(ctx), accountReportMarkTimeout)
	defer markCancel()
	applied, merr := d.mark(markCtx, key, pending)
	if merr != nil {
		slog.WarnContext(ctx, d.read+": could not save a submitted report; it will not be collected", "platform", key.Platform, "project_id", key.ProjectID, "error", merr)
		return nil
	}
	if !applied {
		if latest, gerr := d.get(markCtx, key); gerr == nil && latest.Pending != nil {
			snap.Pending = latest.Pending
		}
		return nil
	}
	snap.Pending = &pending
	return nil
}
