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

// AudienceReportReader is an OPTIONAL dispatcher capability: the project-scoped age/gender
// audience read for a platform whose demographics come from an ASYNCHRONOUS report (Microsoft
// Advertising's AgeGenderAudienceReportRequest). It is KeywordReportReader's contract, method for
// method, for the second saved-report kind — the same trust boundary (the project's OWN
// connection and bound account, a never-empty scope from this service's database, ANY provenance
// mismatch refusing the whole read, every refusal before any upstream call), and the same
// ordering: AudienceReportEnabled first (gate and window, no project data), then
// AudienceReportAccount (every refusal, no upstream call), then Submit/Check, which re-run the
// connection and account checks themselves.
//
// Separate from KeywordReportReader so a platform can serve one kind without the other, and
// separate from KeywordInsightsReader/MetaAudienceReader because its result is a saved report's
// rows, not a live breakdown. There is NO device dimension: Microsoft's age/gender report has
// none, and a device breakdown would need a second report (out of scope).
type AudienceReportReader interface {
	InsightReportPeriod
	AudienceReportEnabled(window model.MetricsWindow) error
	AudienceReportAccount(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (accountID string, err error)
	SubmitAudienceReport(ctx context.Context, projectID string, platform model.Provider, accountID string, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error)
	CheckAudienceReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.AudienceReportCheck, error)
}

// Upstream operation tokens for the report-backed audience read (see the main token block in
// orchestrator.go for the bounding rule these follow).
const (
	opSubmitAudienceReport = "submit_audience_report"
	opCheckAudienceReport  = "check_audience_report"
)

// SetAudienceReportStore injects the saved-report store the report-backed audience read needs.
func (o *Orchestrator) SetAudienceReportStore(r domain.AudienceReportRepository) {
	o.audienceReportsMu.Lock()
	defer o.audienceReportsMu.Unlock()
	o.audienceReports = r
}

func (o *Orchestrator) audienceReportStore() domain.AudienceReportRepository {
	o.audienceReportsMu.RLock()
	defer o.audienceReportsMu.RUnlock()
	return o.audienceReports
}

// ReadReportedAudience is the age/gender audience read for a report-backed platform, in
// ReadReportedKeywordPerformance's order and with its semantics exactly:
//
//  0. AudienceReportEnabled: rollout gate and window, refused whatever the project's campaigns;
//  1. the project's campaign scope from the database; EMPTY answers an empty result with no
//     store access and no upstream call;
//  2. AudienceReportAccount: every trust-boundary refusal, before anything upstream;
//  3. check a pending report once (store / drop / abandon);
//     then drop a finished report for another calendar period (discardOtherPeriod);
//  4. submit when nothing is pending and the last finished report is missing, stale, or does not
//     cover every campaign the project NOW owns — failing the read only on a permanent refusal;
//  5. serve the last finished report only if it covers the current scope, its rows confined to
//     the current scope's campaigns and summed into (age group, gender) buckets.
func (o *Orchestrator) ReadReportedAudience(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow) (*model.ReportedAudienceRead, error) {
	d, ok := o.dispatchers[platform]
	if !ok {
		return nil, fmt.Errorf("%w: no dispatcher registered for platform %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	reader, ok := d.(AudienceReportReader)
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrKeywordInsightsUnsupported, platform)
	}
	store := o.audienceReportStore()
	if store == nil {
		return nil, fmt.Errorf("%s audience read: saved-report store is not configured", platform)
	}
	if err := reader.AudienceReportEnabled(window); err != nil {
		return nil, err
	}
	scope, err := o.projectCampaignScope(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	if len(scope) == 0 {
		// ReadKeywordPerformance's early return, for its reason: an empty scope is exactly when
		// an unscoped report would expose every other project's audience.
		return &model.ReportedAudienceRead{Window: window, Buckets: []model.ReportedAudienceBucket{}}, nil
	}
	accountID, err := reader.AudienceReportAccount(ctx, projectID, platform, window, scope)
	if err != nil {
		return nil, err
	}
	key := model.InsightReportKey{ProjectID: projectID, Platform: platform, AccountID: accountID, Window: window}
	snap, serr := store.GetAudienceReport(ctx, key)
	switch {
	case errors.Is(serr, domain.ErrNotFound):
		snap = &model.AudienceReportSnapshot{Key: key}
	case serr != nil:
		return nil, fmt.Errorf("%s audience read: read saved report: %w", platform, serr)
	}

	callCtx, cancel := context.WithTimeout(ctx, accountsCallTimeout)
	defer cancel()
	now := o.insightReportNow()
	scopeIDs := scopeCampaignIDs(scope)
	driver := o.audienceReportDriver(reader, store)
	wantStart, wantEnd, perr := resolveInsightPeriod(reader, platform, "audience read", window, now)
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
	return mergeAudienceReport(window, snap, scopeIDs)
}

// isPermanentAudienceRefusal is isPermanentKeywordRefusal for the audience kind, whose scope
// refusals are the audience sentinels.
func isPermanentAudienceRefusal(err error) bool {
	return errors.Is(err, domain.ErrAudienceScopeTooLarge) ||
		errors.Is(err, domain.ErrAudienceScopeInvalid) ||
		errors.Is(err, domain.ErrServiceDefect) ||
		errors.Is(err, domain.ErrMetricsWindowUnsupported) ||
		errors.Is(err, ErrCampaignAccountMismatch)
}

// audienceReportDriver binds the age/gender kind's store and upstream calls for the shared
// collect/refresh steps (insight_report.go).
func (o *Orchestrator) audienceReportDriver(reader AudienceReportReader, store domain.AudienceReportRepository) insightReportDriver[model.AudienceReportRow] {
	return insightReportDriver[model.AudienceReportRow]{
		read:     "audience read",
		get:      store.GetAudienceReport,
		mark:     store.MarkAudienceReportPending,
		complete: store.CompleteAudienceReport,
		fail:     store.FailAudienceReport,
		check: func(callCtx, ctx context.Context, key model.InsightReportKey, reportID string) (*model.AudienceReportCheck, error) {
			return o.checkAudienceReport(callCtx, ctx, reader, key, reportID)
		},
		submit: func(callCtx, ctx context.Context, key model.InsightReportKey, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error) {
			return o.submitAudienceReport(callCtx, ctx, reader, key, scope)
		},
		permanent: isPermanentAudienceRefusal,
	}
}

// mergeAudienceReport builds the response from the snapshot: the last finished report's rows
// confined to the CURRENT scope, summed per (age group, gender) across campaigns, ordered by
// impressions descending with the labels as a stable tie-break. A report that does not cover the
// current scope is not served at all. No cap: the bucket count is bounded by the platform's
// demographic vocabulary (and refused past microsoft.MaxAgeGenderBuckets when folded).
//
// Cost is summed in int64 micros per row (each row's micros is exact — the dispatcher refuses a
// spend that does not fit), so an overflowing total is an error rather than a wrapped number.
func mergeAudienceReport(window model.MetricsWindow, snap *model.AudienceReportSnapshot, scopeIDs map[string]bool) (*model.ReportedAudienceRead, error) {
	out := &model.ReportedAudienceRead{Window: window, Buckets: []model.ReportedAudienceBucket{}, MetricsPending: snap.Pending != nil}
	r := snap.Ready
	if r == nil || !coversScope(r.CampaignIDs, scopeIDs) {
		return out, nil
	}
	type bucketKey struct{ age, gender string }
	byKey := map[bucketKey]*model.ReportedAudienceBucket{}
	order := []bucketKey{}
	for _, row := range r.Rows {
		if !scopeIDs[row.CampaignID] {
			continue
		}
		k := bucketKey{row.AgeGroup, row.Gender}
		b, ok := byKey[k]
		if !ok {
			b = &model.ReportedAudienceBucket{AgeGroup: row.AgeGroup, Gender: row.Gender}
			byKey[k] = b
			order = append(order, k)
		}
		micros := int64(math.Round(row.Spend * 1e6))
		if row.Impressions > math.MaxInt64-b.Impressions || row.Clicks > math.MaxInt64-b.Clicks || micros > math.MaxInt64-b.CostMicros {
			return nil, fmt.Errorf("audience read: bucket %s/%s total would overflow", row.AgeGroup, row.Gender)
		}
		b.Impressions += row.Impressions
		b.Clicks += row.Clicks
		b.CostMicros += micros
	}
	asOf := r.AsOf
	out.MetricsAsOf = &asOf
	out.DataIncomplete = r.Partial
	for _, k := range order {
		b := *byKey[k]
		if b.Impressions > 0 {
			b.Ctr = float64(b.Clicks) / float64(b.Impressions)
		}
		out.Buckets = append(out.Buckets, b)
	}
	sort.SliceStable(out.Buckets, func(i, j int) bool {
		a, b := out.Buckets[i], out.Buckets[j]
		if a.Impressions != b.Impressions {
			return a.Impressions > b.Impressions
		}
		if a.AgeGroup != b.AgeGroup {
			return a.AgeGroup < b.AgeGroup
		}
		return a.Gender < b.Gender
	})
	return out, nil
}

// checkAudienceReport and submitAudienceReport are the instrumented upstream calls of the
// report-backed audience read, one per method so each records exactly one upstream call.
func (o *Orchestrator) checkAudienceReport(callCtx, ctx context.Context, reader AudienceReportReader, key model.InsightReportKey, reportID string) (*model.AudienceReportCheck, error) {
	start := time.Now()
	check, err := reader.CheckAudienceReport(callCtx, key.ProjectID, key.Platform, key.AccountID, reportID)
	o.recordUpstream(ctx, key.Platform, opCheckAudienceReport, start, err)
	return check, err
}

func (o *Orchestrator) submitAudienceReport(callCtx, ctx context.Context, reader AudienceReportReader, key model.InsightReportKey, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error) {
	start := time.Now()
	sub, err := reader.SubmitAudienceReport(callCtx, key.ProjectID, key.Platform, key.AccountID, key.Window, scope)
	o.recordUpstream(ctx, key.Platform, opSubmitAudienceReport, start, err)
	return sub, err
}
