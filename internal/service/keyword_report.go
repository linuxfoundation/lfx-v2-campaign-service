// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
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
	// InsightReportPeriod: the dates a report over a window covers, so a saved report is served
	// only for its own period.
	InsightReportPeriod
	// KeywordReportEnabled refuses a read this platform will not serve AT ALL — its rollout gate
	// is off, or window is not one it can report on — whatever the project's campaigns are. It
	// needs neither a connection nor a scope and never contacts the platform, so the orchestrator
	// calls it FIRST: a gated-off read must answer the same refusal for a project with no
	// campaigns as for one with many, rather than an empty success that depends on project data.
	KeywordReportEnabled(window model.MetricsWindow) error
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
//  0. KeywordReportEnabled: the platform's rollout gate and window, refused whatever the
//     project's campaigns are;
//  1. resolve the project's campaign scope from the database; an EMPTY scope answers an empty
//     result with no upstream call, exactly as ReadKeywordPerformance does;
//  2. KeywordReportAccount: every trust-boundary refusal, before anything upstream;
//  3. read the saved snapshot; if a report is pending, check it once (store it, drop it if the
//     platform failed it, or abandon it past accountReportAbandonAfter);
//     then drop (in memory) a finished report whose saved dates are not the dates window
//     resolves to NOW (reader.ReportWindowDates) — a this_month or today report does not
//     describe the new period after a calendar rollover, however young it is
//     (discardOtherPeriod);
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
	// Before the scope: the rollout gate and the window are properties of the platform, not of
	// the project, so the empty-scope success below must not bypass them.
	if err := reader.KeywordReportEnabled(window); err != nil {
		return nil, err
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
	now := o.insightReportNow()
	scopeIDs := scopeCampaignIDs(scope)
	driver := o.keywordReportDriver(reader, store)
	wantStart, wantEnd, perr := resolveInsightPeriod(reader, platform, "keyword read", window, now)
	if perr != nil {
		return nil, perr
	}
	// A report still BUILDING for another calendar period must not block one for this period.
	supersedeOtherPeriodPending(callCtx, ctx, driver, snap, wantStart, wantEnd, now)
	collectPendingInsightReport(callCtx, ctx, driver, snap, now)
	// A finished report for another calendar period (the window rolled over since it was
	// requested) is neither fresh nor servable: see discardOtherPeriod.
	discardOtherPeriod(snap, wantStart, wantEnd)
	if rerr := refreshInsightReport(callCtx, ctx, driver, snap, scope, scopeIDs, now); rerr != nil {
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

// keywordReportDriver binds the keyword kind's store and upstream calls for the shared
// collect/refresh steps (insight_report.go).
func (o *Orchestrator) keywordReportDriver(reader KeywordReportReader, store domain.KeywordReportRepository) insightReportDriver[model.KeywordReportRow] {
	return insightReportDriver[model.KeywordReportRow]{
		read:     "keyword read",
		get:      store.GetKeywordReport,
		mark:     store.MarkKeywordReportPending,
		complete: store.CompleteKeywordReport,
		fail:     store.FailKeywordReport,
		check: func(callCtx, ctx context.Context, key model.InsightReportKey, reportID string) (*model.KeywordReportCheck, error) {
			return o.checkKeywordReport(callCtx, ctx, reader, key, reportID)
		},
		submit: func(callCtx, ctx context.Context, key model.InsightReportKey, scope []model.ProjectCampaignScope) (*model.KeywordReportSubmission, error) {
			return o.submitKeywordReport(callCtx, ctx, reader, key, scope)
		},
		permanent: isPermanentKeywordRefusal,
	}
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
