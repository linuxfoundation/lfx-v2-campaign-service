// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
)

// redditAdsAccountDiscovery is the account-discovery descriptor for Reddit Ads.
//
// Unlike Google/Meta/LinkedIn/Microsoft/X, connection.go declares no var for Reddit: Reddit's
// dispatcher has no ListAccounts implementation (per the migration plan's own verified note),
// so account discovery never needed one. This monitor endpoint is the first Reddit surface to
// route through classifyDiscoveryError, so the descriptor is added here rather than guessed
// into connection.go's existing block.
var redditAdsAccountDiscovery = accountDiscovery{
	provider:        model.ProviderRedditAds,
	displayName:     "reddit ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json with client_id, client_secret and refresh_token set",
	operation:       "account monitor",
}

// validateMonitorDays enforces the 7..90 range the design layer also constrains with
// Minimum/Maximum. Enforced here too for the same reason resolveInsightsWindow re-checks its
// own enum: a runtime rejection with no matching design constraint (or the reverse) is the
// drift this repo's rules exist to prevent, and a non-HTTP caller does not go through Goa's
// generated validation at all.
func validateMonitorDays(days int) error {
	if days < 7 || days > 90 {
		return &conn.BadRequestError{Code: "400", Message: "days must be between 7 and 90"}
	}
	return nil
}

// monitorTotalsFallback sums the per-campaign rows exactly the way every platform but Reddit's
// account-wide totals are derived (model.AccountMonitorTotals' own doc comment). A FetchFailed
// row contributes its zero-value numeric fields, which is a no-op on the sum — it is not
// specially excluded, because there is no correct non-zero contribution to substitute.
func monitorTotalsFallback(rows []model.AccountCampaignMetrics) *model.AccountMonitorTotals {
	t := &model.AccountMonitorTotals{CampaignCount: len(rows)}
	for _, r := range rows {
		t.Spend += r.Spend
		t.Impressions += r.Impressions
		t.Clicks += r.Clicks
		if r.Conversions != nil {
			t.Conversions += *r.Conversions
		}
	}
	return t
}

// toConnAccountMonitorCampaign converts one rule-engine output row to the generated response
// type. campaign_url and ad_groups are Google Ads only and left nil/absent for every other
// platform, per AccountMonitorCampaign's design-layer doc comment.
func toConnAccountMonitorCampaign(row model.AccountMonitorRow) *conn.AccountMonitorCampaign {
	m := row.Metrics
	return &conn.AccountMonitorCampaign{
		PlatformCampaignID: m.PlatformCampaignID,
		Name:               m.Name,
		Status:             m.Status,
		Spend:              m.Spend,
		Impressions:        m.Impressions,
		Clicks:             m.Clicks,
		Ctr:                m.Ctr,
		Conversions:        m.Conversions,
		BudgetDay:          m.BudgetDay,
		TotalBudget:        m.TotalBudget,
		StartDate:          m.StartDate,
		EndDate:            m.EndDate,
		PacingUnknown:      m.PacingUnknown,
		IsSearchChannel:    m.IsSearchChannel,
		FetchFailed:        m.FetchFailed,
		PacingPct:          row.PacingPct,
		PacingLabel:        string(row.PacingLabel),
	}
}

// toConnAccountMonitorActionItems converts the rule engine's findings to the generated
// response type. CampaignID/CampaignName are pointers on the generated type because the
// design declares them Optional (an account-wide item, per model.AccountMonitorActionItem's
// own doc comment, carries neither) — an empty domain string is converted to a nil pointer
// rather than an empty-string pointer, so a renderer's presence check behaves the same way
// for "no campaign" as it does for every other optional field on this response.
func toConnAccountMonitorActionItems(items []model.AccountMonitorActionItem) []*conn.AccountMonitorActionItem {
	out := make([]*conn.AccountMonitorActionItem, 0, len(items))
	for _, it := range items {
		ci := &conn.AccountMonitorActionItem{
			Priority: string(it.Priority),
			Issue:    it.Issue,
			Action:   it.Action,
		}
		if it.CampaignID != "" {
			id := it.CampaignID
			ci.CampaignID = &id
		}
		if it.CampaignName != "" {
			name := it.CampaignName
			ci.CampaignName = &name
		}
		out = append(out, ci)
	}
	return out
}

// toConnAccountMonitorTotals converts the domain totals to the generated response type.
func toConnAccountMonitorTotals(t *model.AccountMonitorTotals) *conn.AccountMonitorTotals {
	return &conn.AccountMonitorTotals{
		Spend:         t.Spend,
		Impressions:   t.Impressions,
		Clicks:        t.Clicks,
		Conversions:   t.Conversions,
		CampaignCount: t.CampaignCount,
	}
}

// monitorAccount is the shared body of all four monitor-*-ads-account handlers: validate,
// fetch, evaluate, total, and assemble the response. evaluate is the one thing that cannot be
// shared uniformly — Google's EvaluateGoogleMonitor takes no `now` (its pacing formula has no
// current-time dependency: expected spend is budget_day*days, not derived from flight dates),
// while LinkedIn/Meta/Reddit's take one — so each caller closes over its own platform's
// Evaluate*Monitor call.
func (s *ConnectionService) monitorAccount(
	ctx context.Context,
	projectID, accountID string,
	days int,
	platform model.Provider,
	discovery accountDiscovery,
	evaluate func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem),
) (*conn.AccountMonitor, error) {
	if err := rejectSystemScope(projectID); err != nil {
		return nil, err
	}
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(discovery.label())
	if err != nil {
		return nil, err
	}
	metricsRows, merr := orch.ReadAccountCampaignMetrics(ctx, projectID, platform, accountID, days)
	if merr != nil {
		return nil, s.classifyDiscoveryError(ctx, projectID, discovery, merr)
	}

	rows, actionItems := evaluate(metricsRows)

	// A failure here (terr != nil) falls back the same way an unsupported capability
	// (!ok) does, rather than aborting the whole endpoint: the per-campaign rows and
	// action items above already succeeded and are the response's primary content, and
	// Reddit's separate account-wide totals call (see model.AccountMonitorTotals' doc
	// comment) is a secondary, derivable figure — summing the returned rows is not the
	// platform's own number, but it is a strictly better answer than a 5xx that throws
	// away campaign data the caller already has in hand.
	totals, ok, terr := orch.ReadAccountTotals(ctx, projectID, platform, accountID, days, len(rows))
	if terr != nil {
		// terr can carry ErrConnectionNotUsable, whose detection path decodes a decrypted
		// credential blob — neither the cause nor its text may leave this function (see
		// classifyDiscoveryError's ErrConnectionNotUsable arm in connection.go). Log the
		// fixed-vocabulary reason instead, and at Warn: this is a handled/recovered condition,
		// not an error that aborts the request.
		slog.WarnContext(ctx, "account totals read failed; serving the row-summed fallback",
			"reason", unusableConnectionReason(terr), "project_id", projectID, "platform", platform)
		ok = false
	}
	if !ok {
		// Sum the post-rule-engine rows (rows), not the raw dispatcher read (metricsRows):
		// EvaluateGoogleMonitor/EvaluateMetaMonitor/etc. drop zz-prefixed campaigns before
		// returning, and the totals must agree with the campaigns array actually returned in
		// this same response rather than with a superset the caller never sees.
		filteredMetrics := make([]model.AccountCampaignMetrics, len(rows))
		for i, r := range rows {
			filteredMetrics[i] = r.Metrics
		}
		totals = monitorTotalsFallback(filteredMetrics)
	}

	connCampaigns := make([]*conn.AccountMonitorCampaign, 0, len(rows))
	for _, r := range rows {
		connCampaigns = append(connCampaigns, toConnAccountMonitorCampaign(r))
	}

	return &conn.AccountMonitor{
		AccountID:   accountID,
		Days:        days,
		Campaigns:   connCampaigns,
		ActionItems: toConnAccountMonitorActionItems(actionItems),
		Totals:      toConnAccountMonitorTotals(totals),
	}, nil
}

// MonitorGoogleAdsAccount reads every campaign visible on a Google Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorGoogleAdsAccount(ctx context.Context, p *conn.MonitorGoogleAdsAccountPayload) (*conn.AccountMonitor, error) {
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderGoogleAds, googleAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateGoogleMonitor(rows, p.Days)
		})
}

// MonitorLinkedinAdsAccount reads every campaign visible on a LinkedIn Ads account, live from
// the platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorLinkedinAdsAccount(ctx context.Context, p *conn.MonitorLinkedinAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderLinkedInAds, linkedInAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateLinkedInMonitor(rows, p.Days, now)
		})
}

// MonitorMetaAdsAccount reads every campaign visible on a Meta Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorMetaAdsAccount(ctx context.Context, p *conn.MonitorMetaAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderMetaAds, metaAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateMetaMonitor(rows, p.Days, now)
		})
}

// MonitorRedditAdsAccount reads every campaign visible on a Reddit Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine. Totals come from
// AccountTotalsReader's independent account-level call rather than the row sum every other
// platform falls back to — see monitorAccount and model.AccountMonitorTotals' own doc comment.
func (s *ConnectionService) MonitorRedditAdsAccount(ctx context.Context, p *conn.MonitorRedditAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderRedditAds, redditAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateRedditMonitor(rows, p.Days, now)
		})
}
