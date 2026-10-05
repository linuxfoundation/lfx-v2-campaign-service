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

// AccountReportReader is an OPTIONAL dispatcher capability: the account monitor for a platform
// whose delivery metrics come from an ASYNCHRONOUS report rather than a live read. Discovered by
// type assertion like AccountMetricsReader, which it stands in for on such a platform.
//
// Microsoft Advertising is why it exists. Its metrics come only from the Reporting service, which
// Microsoft documents as completing "within minutes" and to be polled at 2–15 minute intervals —
// against a monitor read that runs inside accountsCallTimeout (20s). A synchronous
// AccountMetricsReader would essentially never see a finished report. So the capability is split
// into three short calls, and the orchestrator keeps the state between requests
// (ReadReportedAccountCampaigns).
//
// Every method resolves the project's OWN connection only and refuses an account the connection
// is not bound to, exactly as AccountMetricsReader does — the trust boundary is the same, only
// the timing differs.
type AccountReportReader interface {
	// ListAccountCampaigns reads the account's live campaigns: identity, status and budget, with
	// every metric field zero. It is the synchronous half and must fit the call budget.
	ListAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AccountCampaignMetrics, error)
	// SubmitAccountReport asks the platform to build a report of every campaign on the account
	// over the trailing `days` days (today inclusive). It returns as soon as the platform has
	// accepted the request.
	SubmitAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) (*model.AccountReportSubmission, error)
	// CheckAccountReport checks a submitted report ONCE, without waiting, and reads its rows if
	// it has finished.
	CheckAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.AccountReportCheck, error)
}

// accountReportFreshFor is how long a finished report is served before the next read submits a
// newer one. Thirty minutes because the platform's own data is not fresher than that in practice
// — Microsoft's reporting lags delivery — and each submission costs a report build on Microsoft's
// side; refreshing on every page view would queue reports faster than they complete.
const accountReportFreshFor = 30 * time.Minute

// accountReportAbandonAfter is how long a submitted report may stay pending before it is given
// up on and a fresh one submitted. Microsoft's own guidance for a report still pending after an
// hour of polling is to "consider saving the report identifier, exiting the loop, and trying again
// later"; a report that has not finished in an hour is far more likely lost than slow, and
// waiting on it forever would leave the account with no newer metrics at all.
const accountReportAbandonAfter = 60 * time.Minute

// SetAccountReportStore injects the saved-report store the report-backed monitor read needs.
// Late-bound like SetIndexer so the container can wire it once the database pool exists.
func (o *Orchestrator) SetAccountReportStore(r domain.AccountReportRepository) {
	o.accountReportsMu.Lock()
	defer o.accountReportsMu.Unlock()
	o.accountReports = r
}

func (o *Orchestrator) accountReportStore() domain.AccountReportRepository {
	o.accountReportsMu.RLock()
	defer o.accountReportsMu.RUnlock()
	return o.accountReports
}

// ReadReportedAccountCampaigns is the account-monitor read for a report-backed platform. One
// call does, in order and inside ONE accountsCallTimeout budget:
//
//  1. read the live campaign list (the part that must succeed — its error is the call's error);
//  2. if a report is pending, check it once: store it if it finished, drop it if the platform
//     failed it or it has been pending past accountReportAbandonAfter;
//  3. if nothing is pending and the last finished report is missing or older than
//     accountReportFreshFor, submit a new one;
//  4. fill each live campaign's metrics from the last finished report.
//
// One budget, not one per step: three independent 20s timeouts could together outlast the 60s
// platform ingress, and the steps after the list are best-effort anyway — a check or submit that
// runs out of time leaves the saved state as it was, and the next read picks it up.
//
// Steps 2 and 3 never fail the read. A platform or store error there is logged and the response
// carries whatever metrics were already saved, with MetricsPending saying whether newer ones are
// on the way. Failing the whole monitor because a background refresh hiccuped would hide a live
// campaign list that was read successfully.
func (o *Orchestrator) ReadReportedAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) (*model.ReportedAccountRead, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", ErrAccountMetricsUnsupported, platform)
	}
	reader, ok := d.(AccountReportReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAccountMetricsUnsupported, platform)
	}
	store := o.accountReportStore()
	if store == nil {
		// Not ErrAccountMetricsUnsupported: the platform does support it, this deployment just
		// has no store wired. Unclassified, so it reaches the caller as the retryable 503.
		return nil, fmt.Errorf("%s account monitor: saved-report store is not configured", platform)
	}

	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()

	key := model.AccountReportKey{ProjectID: projectID, Platform: platform, AccountID: accountID, Days: days}
	campaigns, lerr := o.listAccountCampaigns(callCtx, ctx, reader, key)
	if lerr != nil {
		return nil, lerr
	}
	if campaigns == nil {
		// Same contract-violation guard as ReadAccountCampaignMetrics.
		return nil, fmt.Errorf("%s account campaign lister returned a nil result with no error", platform)
	}

	snap, serr := store.GetAccountReport(callCtx, key)
	switch {
	case errors.Is(serr, domain.ErrNotFound):
		snap = &model.AccountReportSnapshot{Key: key}
	case serr != nil:
		// The store is this service's own database. Unlike a platform hiccup in steps 2–3, a read
		// failure here would mean answering "no metrics yet" for an account that has them, so it
		// fails the call (503) rather than degrading silently.
		return nil, fmt.Errorf("%s account monitor: read saved report: %w", platform, serr)
	}

	now := time.Now()
	o.collectPendingAccountReport(callCtx, ctx, reader, store, snap, now)
	o.refreshAccountReport(callCtx, ctx, reader, store, snap, now)

	return mergeAccountReport(campaigns, snap), nil
}

// collectPendingAccountReport is step 2 of ReadReportedAccountCampaigns. It updates snap in
// place to reflect what it learned, so the response describes the state it left behind.
func (o *Orchestrator) collectPendingAccountReport(callCtx, ctx context.Context, reader AccountReportReader, store domain.AccountReportRepository, snap *model.AccountReportSnapshot, now time.Time) {
	p := snap.Pending
	if p == nil {
		return
	}
	key := snap.Key
	if now.Sub(p.SubmittedAt) > accountReportAbandonAfter {
		reason := fmt.Sprintf("report not finished %s after submission; abandoned", accountReportAbandonAfter)
		if _, ferr := store.FailAccountReport(callCtx, key, p.ReportID, reason, now); ferr != nil {
			slog.WarnContext(ctx, "account monitor: could not record an abandoned report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
		return
	}

	check, cerr := o.checkAccountReport(callCtx, ctx, reader, key, p.ReportID)
	if cerr != nil {
		// Transient by assumption: the report stays pending and the next read checks again. A
		// report that keeps failing to check is bounded by accountReportAbandonAfter.
		slog.WarnContext(ctx, "account monitor: report check failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", cerr)
		return
	}
	if check == nil {
		slog.WarnContext(ctx, "account monitor: report check returned no result and no error", "platform", key.Platform, "project_id", key.ProjectID)
		return
	}

	switch check.Status {
	case model.AccountReportPending:
		return
	case model.AccountReportReady:
		ready := model.ReadyAccountReport{
			ReportID: p.ReportID, Rows: check.Rows, Partial: check.Partial,
			WindowStart: p.WindowStart, WindowEnd: p.WindowEnd, CompletedAt: now,
		}
		applied, perr := store.CompleteAccountReport(callCtx, key, ready)
		if perr != nil {
			slog.WarnContext(ctx, "account monitor: could not save a finished report", "platform", key.Platform, "project_id", key.ProjectID, "error", perr)
		}
		// Served either way: the rows are a real finished report and newer than any saved one,
		// so withholding them because the save failed or lost a race would show the caller
		// older numbers than this request actually holds.
		snap.Ready = &ready
		if applied {
			snap.Pending = nil
		} else if perr == nil {
			// A concurrent read replaced the pending report while this one was being checked.
			// That newer submission is still building, so the response says so.
			if latest, gerr := store.GetAccountReport(callCtx, key); gerr == nil {
				snap.Pending = latest.Pending
			}
		}
	case model.AccountReportFailed:
		if _, ferr := store.FailAccountReport(callCtx, key, p.ReportID, "the platform reported the report as failed", now); ferr != nil {
			slog.WarnContext(ctx, "account monitor: could not record a failed report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
	default:
		slog.WarnContext(ctx, "account monitor: report check returned an unknown status", "platform", key.Platform, "project_id", key.ProjectID, "status", string(check.Status))
	}
}

// refreshAccountReport is step 3: submit a new report when none is building and the last one is
// missing or stale.
func (o *Orchestrator) refreshAccountReport(callCtx, ctx context.Context, reader AccountReportReader, store domain.AccountReportRepository, snap *model.AccountReportSnapshot, now time.Time) {
	if snap.Pending != nil {
		return
	}
	if snap.Ready != nil && now.Sub(snap.Ready.CompletedAt) < accountReportFreshFor {
		return
	}
	if callCtx.Err() != nil {
		// The budget went on the list and the check. Submitting now would start a report this
		// call cannot record, and an unrecorded report is one nobody will ever collect.
		return
	}
	key := snap.Key
	sub, serr := o.submitAccountReport(callCtx, ctx, reader, key)
	if serr != nil {
		slog.WarnContext(ctx, "account monitor: report submission failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", serr)
		return
	}
	if sub == nil || sub.ReportID == "" {
		slog.WarnContext(ctx, "account monitor: report submission returned no report id", "platform", key.Platform, "project_id", key.ProjectID)
		return
	}
	pending := model.PendingAccountReport{ReportID: sub.ReportID, WindowStart: sub.WindowStart, WindowEnd: sub.WindowEnd, SubmittedAt: now}
	if merr := store.MarkAccountReportPending(callCtx, key, pending); merr != nil {
		// The report is building on the platform but nothing here remembers it, so it will
		// never be collected; the next read submits again. Costly only in report builds.
		slog.WarnContext(ctx, "account monitor: could not save a submitted report; it will not be collected", "platform", key.Platform, "project_id", key.ProjectID, "error", merr)
		return
	}
	snap.Pending = &pending
}

// mergeAccountReport fills each live campaign's metrics from the snapshot's finished report.
//
// The live list decides WHICH campaigns appear: a campaign deleted since the report finished is
// gone, and one created since appears with zero delivery, which is what it has. With no finished
// report every row is FetchFailed, so the rule engines skip it rather than read zero metrics as
// a measurement of zero.
//
// A campaign the dispatcher already marked FetchFailed (an unparseable budget) keeps the flag:
// its metrics are filled in — they are trustworthy — but the row is still excluded from rule
// evaluation, the same way Google's budget-unparseable rows are.
func mergeAccountReport(campaigns []model.AccountCampaignMetrics, snap *model.AccountReportSnapshot) *model.ReportedAccountRead {
	out := &model.ReportedAccountRead{
		Rows:           make([]model.AccountCampaignMetrics, 0, len(campaigns)),
		MetricsPending: snap.Pending != nil,
	}
	if snap.Ready == nil {
		for _, c := range campaigns {
			c.FetchFailed = true
			out.Rows = append(out.Rows, c)
		}
		return out
	}
	asOf := snap.Ready.CompletedAt
	out.MetricsAsOf = &asOf
	byID := make(map[string]model.AccountReportRow, len(snap.Ready.Rows))
	for _, r := range snap.Ready.Rows {
		byID[r.PlatformCampaignID] = r
	}
	for _, c := range campaigns {
		// Absent from a finished ACCOUNT-scoped report means the campaign served nothing in the
		// window: the report covers every campaign on the account, so absence is a measured
		// zero for spend, impressions and clicks. Conversions stays nil there — the report says
		// nothing about whether conversion tracking covered a campaign it has no row for.
		if r, ok := byID[c.PlatformCampaignID]; ok {
			c.Spend = r.Spend
			c.Impressions = r.Impressions
			c.Clicks = r.Clicks
			c.Conversions = r.Conversions
		}
		if c.Impressions > 0 {
			// Percent, matching every other platform's Ctr (AccountCampaignMetrics.Ctr).
			c.Ctr = float64(c.Clicks) / float64(c.Impressions) * 100
		}
		out.Rows = append(out.Rows, c)
	}
	return out
}

// listAccountCampaigns, checkAccountReport and submitAccountReport are the three instrumented
// upstream calls of the report-backed read, one per method so each records exactly one
// upstream call and can be driven on its own (TestUpstreamCallsAreInstrumented). callCtx bounds
// the call; ctx carries the request's observability context for recordUpstream, matching
// ReadAccountCampaignMetrics.
func (o *Orchestrator) listAccountCampaigns(callCtx, ctx context.Context, reader AccountReportReader, key model.AccountReportKey) ([]model.AccountCampaignMetrics, error) {
	start := time.Now()
	rows, err := reader.ListAccountCampaigns(callCtx, key.ProjectID, key.Platform, key.AccountID)
	o.recordUpstream(ctx, key.Platform, opListAccountCampaigns, start, err)
	return rows, err
}

func (o *Orchestrator) checkAccountReport(callCtx, ctx context.Context, reader AccountReportReader, key model.AccountReportKey, reportID string) (*model.AccountReportCheck, error) {
	start := time.Now()
	check, err := reader.CheckAccountReport(callCtx, key.ProjectID, key.Platform, key.AccountID, reportID)
	o.recordUpstream(ctx, key.Platform, opCheckAccountReport, start, err)
	return check, err
}

func (o *Orchestrator) submitAccountReport(callCtx, ctx context.Context, reader AccountReportReader, key model.AccountReportKey) (*model.AccountReportSubmission, error) {
	start := time.Now()
	sub, err := reader.SubmitAccountReport(callCtx, key.ProjectID, key.Platform, key.AccountID, key.Days)
	o.recordUpstream(ctx, key.Platform, opSubmitAccountReport, start, err)
	return sub, err
}
