// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// KeywordReportReader is an OPTIONAL dispatcher capability: the project-scoped keyword read for
// a platform whose keyword performance comes from an ASYNCHRONOUS report rather than a live
// query. It stands in for KeywordInsightsReader.ReadKeywordPerformance on such a platform, the
// way AccountReportReader stands in for AccountMetricsReader, and for the same reason: Microsoft
// Advertising's KeywordPerformanceReportRequest takes minutes to build and the read has a 20s
// budget. The orchestrator keeps the saved report between requests
// (ReadReportedKeywordPerformance).
//
// The trust boundary is KeywordInsightsReader's, unchanged, plus the account monitor's:
//   - the project's OWN connection only (no LF system-account fallback), and only the account
//     that connection is bound to;
//   - the scope is the project's own campaigns, read from this service's database by
//     project_id, never empty, and every entry's recorded creation account must be the bound
//     account — ANY mismatch refuses the whole read (ErrCampaignAccountMismatch), for the reason
//     given on KeywordInsightsReader;
//   - every refusal happens before any upstream call.
//
// KeywordReportAccount enforces all of that and makes NO upstream call; the orchestrator calls
// it first and keys the saved report by the account it returns. Submit and Check re-run the
// connection and account checks themselves, so the boundary does not rest on the caller.
type KeywordReportReader interface {
	// KeywordReportAccount validates window and scope against the project's own connection and
	// returns the ad account the read will be scoped to. It never contacts the platform.
	KeywordReportAccount(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (accountID string, err error)
	// SubmitKeywordReport asks the platform to build a keyword report over window for scope on
	// accountID, returning as soon as the platform has accepted it.
	SubmitKeywordReport(ctx context.Context, projectID string, platform model.Provider, accountID string, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.KeywordReportSubmission, error)
	// CheckKeywordReport checks a submitted report ONCE, without waiting, and reads its rows if
	// it has finished.
	CheckKeywordReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.KeywordReportCheck, error)
}

// Upstream operation tokens for the report-backed keyword read (see the main token block in
// orchestrator.go for the bounding rule these follow).
const (
	opSubmitKeywordReport = "submit_keyword_report"
	opCheckKeywordReport  = "check_keyword_report"
)

// keywordReportRowCap is how many keyword rows the read publishes, the same 50 the Google Ads
// keyword read caps at (googleads.maxKeywordRows), so the two tables page identically.
const keywordReportRowCap = 50

// SetKeywordReportStore injects the saved-report store the report-backed keyword read needs.
func (o *Orchestrator) SetKeywordReportStore(r domain.KeywordReportRepository) {
	o.keywordReportsMu.Lock()
	defer o.keywordReportsMu.Unlock()
	o.keywordReports = r
}

func (o *Orchestrator) keywordReportStore() domain.KeywordReportRepository {
	o.keywordReportsMu.RLock()
	defer o.keywordReportsMu.RUnlock()
	return o.keywordReports
}

// ReadReportedKeywordPerformance is the keyword read for a report-backed platform. In order:
//
//  1. resolve the project's campaign scope from the database; an EMPTY scope answers an empty
//     result with no upstream call, exactly as ReadKeywordPerformance does;
//  2. KeywordReportAccount: every trust-boundary refusal, before anything upstream;
//  3. read the saved snapshot; if a report is pending, check it once (store it, drop it if the
//     platform failed it, or abandon it past accountReportAbandonAfter);
//  4. if nothing is pending and the last finished report is missing, stale
//     (accountReportFreshFor) or does not cover every campaign the project NOW owns, submit one;
//  5. serve the last finished report — only if it covers the current scope — confined to the
//     current scope's campaigns, ranked by impressions and capped.
//
// Steps 3–4 are best-effort exactly as in ReadReportedAccountCampaigns (same budget, same
// detached mark budget, same "log and serve what is saved"), except for a permanent refusal,
// which fails the read.
//
// A finished report that does not cover a campaign the project now owns is NOT served: the
// response has no partial-coverage field, so serving it would present part of the project as all
// of it — the defect KeywordInsightsReader's contract forbids. The caller sees metrics_pending
// and no metrics_as_of until a report over the current scope finishes.
func (o *Orchestrator) ReadReportedKeywordPerformance(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow) (*model.ReportedKeywordRead, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	reader, ok := d.(KeywordReportReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	store := o.keywordReportStore()
	if store == nil {
		return nil, fmt.Errorf("%s keyword read: saved-report store is not configured", platform)
	}
	scope, err := o.projectCampaignScope(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	if len(scope) == 0 {
		// Same early return as ReadKeywordPerformance: no campaigns, nothing to show, and an
		// empty scope is exactly when an unscoped report would expose everyone else's data.
		return &model.ReportedKeywordRead{
			KeywordPerformance:  model.KeywordPerformance{Window: window, Rows: []model.KeywordRow{}},
			ConversionsComplete: true,
		}, nil
	}
	accountID, err := reader.KeywordReportAccount(ctx, projectID, platform, window, scope)
	if err != nil {
		return nil, err
	}
	key := model.KeywordReportKey{ProjectID: projectID, Platform: platform, AccountID: accountID, Window: window}
	snap, serr := store.GetKeywordReport(ctx, key)
	switch {
	case errors.Is(serr, domain.ErrNotFound):
		snap = &model.KeywordReportSnapshot{Key: key}
	case serr != nil:
		return nil, fmt.Errorf("%s keyword read: read saved report: %w", platform, serr)
	}

	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	now := time.Now()
	scopeIDs := scopeCampaignIDs(scope)
	o.collectPendingKeywordReport(callCtx, ctx, reader, store, snap, now)
	if rerr := o.refreshKeywordReport(callCtx, ctx, reader, store, snap, scope, scopeIDs, now); rerr != nil {
		return nil, rerr
	}
	return mergeKeywordReport(window, snap, scopeIDs), nil
}

// scopeCampaignIDs returns the scope's platform campaign ids as a set.
func scopeCampaignIDs(scope []model.ProjectCampaignScope) map[string]bool {
	ids := make(map[string]bool, len(scope))
	for _, s := range scope {
		ids[s.PlatformCampaignID] = true
	}
	return ids
}

// coversScope reports whether a report built for reportIDs covers every campaign in scopeIDs.
func coversScope(reportIDs []string, scopeIDs map[string]bool) bool {
	have := make(map[string]bool, len(reportIDs))
	for _, id := range reportIDs {
		have[id] = true
	}
	for id := range scopeIDs {
		if !have[id] {
			return false
		}
	}
	return true
}

// collectPendingKeywordReport is step 3 — collectPendingAccountReport's logic over the keyword
// store, updating snap in place.
func (o *Orchestrator) collectPendingKeywordReport(callCtx, ctx context.Context, reader KeywordReportReader, store domain.KeywordReportRepository, snap *model.KeywordReportSnapshot, now time.Time) {
	p := snap.Pending
	if p == nil {
		return
	}
	key := snap.Key
	overdue := now.Sub(p.SubmittedAt) > accountReportAbandonAfter
	abandon := func() {
		reason := fmt.Sprintf("report not finished %s after submission; abandoned", accountReportAbandonAfter)
		if _, ferr := store.FailKeywordReport(callCtx, key, p.ReportID, reason, now); ferr != nil {
			slog.WarnContext(ctx, "keyword read: could not record an abandoned report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
	}
	check, cerr := o.checkKeywordReport(callCtx, ctx, reader, key, p.ReportID)
	if cerr != nil || check == nil {
		slog.WarnContext(ctx, "keyword read: report check failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", cerr)
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
		ready := model.ReadyKeywordReport{
			ReportID: p.ReportID, Rows: check.Rows, Partial: check.Partial, CampaignIDs: p.CampaignIDs,
			WindowStart: p.WindowStart, WindowEnd: p.WindowEnd, AsOf: p.SubmittedAt,
		}
		applied, perr := store.CompleteKeywordReport(callCtx, key, ready)
		if perr != nil {
			slog.WarnContext(ctx, "keyword read: could not save a finished report", "platform", key.Platform, "project_id", key.ProjectID, "error", perr)
		}
		// Served either way, for collectPendingAccountReport's reason.
		snap.Ready = &ready
		if applied {
			snap.Pending = nil
		} else if perr == nil {
			if latest, gerr := store.GetKeywordReport(callCtx, key); gerr == nil {
				snap.Pending = latest.Pending
			}
		}
	case model.AccountReportFailed:
		if _, ferr := store.FailKeywordReport(callCtx, key, p.ReportID, "the platform reported the report as failed", now); ferr != nil {
			slog.WarnContext(ctx, "keyword read: could not record a failed report", "platform", key.Platform, "project_id", key.ProjectID, "error", ferr)
			return
		}
		snap.Pending = nil
	default:
		slog.WarnContext(ctx, "keyword read: report check returned an unknown status", "platform", key.Platform, "project_id", key.ProjectID, "status", string(check.Status))
	}
}

// isPermanentKeywordRefusal reports whether a submission error is one no later read can avoid.
// KeywordReportAccount refuses all of these before step 3; they are matched here too so a
// dispatcher that raises one only at submission still fails the read rather than logging forever.
func isPermanentKeywordRefusal(err error) bool {
	return errors.Is(err, domain.ErrKeywordReportScopeTooLarge) ||
		errors.Is(err, domain.ErrKeywordReportScopeInvalid) ||
		errors.Is(err, domain.ErrServiceDefect) ||
		errors.Is(err, domain.ErrMetricsWindowUnsupported) ||
		errors.Is(err, ErrCampaignAccountMismatch)
}

// refreshKeywordReport is step 4 — refreshAccountReport's logic, with scope coverage added to
// what makes a saved report stale.
func (o *Orchestrator) refreshKeywordReport(callCtx, ctx context.Context, reader KeywordReportReader, store domain.KeywordReportRepository, snap *model.KeywordReportSnapshot, scope []model.ProjectCampaignScope, scopeIDs map[string]bool, now time.Time) error {
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
	sub, serr := o.submitKeywordReport(callCtx, ctx, reader, key, scope)
	if isPermanentKeywordRefusal(serr) {
		return fmt.Errorf("%s keyword read: %w", key.Platform, serr)
	}
	if serr != nil {
		slog.WarnContext(ctx, "keyword read: report submission failed; will retry on a later read", "platform", key.Platform, "project_id", key.ProjectID, "error", serr)
		return nil
	}
	if sub == nil || sub.ReportID == "" || len(sub.CampaignIDs) == 0 {
		slog.WarnContext(ctx, "keyword read: report submission returned no report id or scope", "platform", key.Platform, "project_id", key.ProjectID)
		return nil
	}
	pending := model.PendingKeywordReport{
		ReportID: sub.ReportID, CampaignIDs: sub.CampaignIDs,
		WindowStart: sub.WindowStart, WindowEnd: sub.WindowEnd, SubmittedAt: now,
	}
	// Detached budget, for accountReportMarkTimeout's reason.
	markCtx, markCancel := context.WithTimeout(context.WithoutCancel(ctx), accountReportMarkTimeout)
	defer markCancel()
	applied, merr := store.MarkKeywordReportPending(markCtx, key, pending)
	if merr != nil {
		slog.WarnContext(ctx, "keyword read: could not save a submitted report; it will not be collected", "platform", key.Platform, "project_id", key.ProjectID, "error", merr)
		return nil
	}
	if !applied {
		if latest, gerr := store.GetKeywordReport(markCtx, key); gerr == nil && latest.Pending != nil {
			snap.Pending = latest.Pending
		}
		return nil
	}
	snap.Pending = &pending
	return nil
}

// mergeKeywordReport builds the response from the snapshot: the last finished report's rows
// confined to the CURRENT scope (a campaign the project no longer owns drops out), ranked by
// impressions descending with the ids as a stable tie-break, and capped at keywordReportRowCap.
// A report that does not cover the current scope is not served at all (see
// ReadReportedKeywordPerformance).
func mergeKeywordReport(window model.MetricsWindow, snap *model.KeywordReportSnapshot, scopeIDs map[string]bool) *model.ReportedKeywordRead {
	out := &model.ReportedKeywordRead{
		KeywordPerformance:  model.KeywordPerformance{Window: window, Rows: []model.KeywordRow{}},
		ConversionsComplete: true,
		MetricsPending:      snap.Pending != nil,
	}
	r := snap.Ready
	if r == nil || !coversScope(r.CampaignIDs, scopeIDs) {
		return out
	}
	asOf := r.AsOf
	out.MetricsAsOf = &asOf
	out.DataIncomplete = r.Partial

	rows := make([]model.KeywordReportRow, 0, len(r.Rows))
	for _, row := range r.Rows {
		if scopeIDs[row.CampaignID] {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		if a.CampaignID != b.CampaignID {
			return a.CampaignID < b.CampaignID
		}
		if a.AdGroupID != b.AdGroupID {
			return a.AdGroupID < b.AdGroupID
		}
		return a.KeywordID < b.KeywordID
	})
	if len(rows) > keywordReportRowCap {
		rows = rows[:keywordReportRowCap]
		out.Truncated = true
	}
	for _, row := range rows {
		kr := model.KeywordRow{
			CriterionID:  row.KeywordID,
			AdGroupID:    row.AdGroupID,
			CampaignID:   row.CampaignID,
			AdGroupName:  row.AdGroupName,
			CampaignName: row.CampaignName,
			Text:         row.Text,
			MatchType:    row.MatchType,
			Status:       row.Status,
			QualityScore: row.QualityScore,
			Impressions:  row.Impressions,
			Clicks:       row.Clicks,
			// The dispatcher refuses a spend whose micros would not fit an int64, so this
			// conversion cannot overflow for a stored row.
			CostMicros: int64(math.Round(row.Spend * 1e6)),
		}
		if row.Impressions > 0 {
			// A FRACTION, matching the Google keyword read's ctr (not the account monitor's
			// percent), because both feed the same keyword table.
			kr.Ctr = float64(row.Clicks) / float64(row.Impressions)
		}
		if row.Conversions != nil {
			kr.Conversions = *row.Conversions
		} else {
			out.ConversionsComplete = false
		}
		out.Rows = append(out.Rows, kr)
	}
	return out
}

// checkKeywordReport and submitKeywordReport are the instrumented upstream calls of the
// report-backed keyword read, one per method so each records exactly one upstream call.
func (o *Orchestrator) checkKeywordReport(callCtx, ctx context.Context, reader KeywordReportReader, key model.KeywordReportKey, reportID string) (*model.KeywordReportCheck, error) {
	start := time.Now()
	check, err := reader.CheckKeywordReport(callCtx, key.ProjectID, key.Platform, key.AccountID, reportID)
	o.recordUpstream(ctx, key.Platform, opCheckKeywordReport, start, err)
	return check, err
}

func (o *Orchestrator) submitKeywordReport(callCtx, ctx context.Context, reader KeywordReportReader, key model.KeywordReportKey, scope []model.ProjectCampaignScope) (*model.KeywordReportSubmission, error) {
	start := time.Now()
	sub, err := reader.SubmitKeywordReport(callCtx, key.ProjectID, key.Platform, key.AccountID, key.Window, scope)
	o.recordUpstream(ctx, key.Platform, opSubmitKeywordReport, start, err)
	return sub, err
}
