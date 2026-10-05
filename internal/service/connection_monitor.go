// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
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

// googleAdsMonitorDiscovery/linkedInAdsMonitorDiscovery/metaAdsMonitorDiscovery are this
// endpoint's own descriptors, distinct from connection.go's googleAdsAccountDiscovery /
// linkedInAdsAccountDiscovery / metaAdsAccountDiscovery, which the `/…/accounts` picker uses
// with operation left empty (so accountDiscovery.label() defaults to "account discovery" —
// correct for that route). Reusing those same vars here left operation empty for this route
// too, so a monitor failure was reported as "account discovery could not be completed" instead
// of naming the operation actually attempted — the gap redditAdsAccountDiscovery above already
// closed for Reddit. remedy text is copied verbatim from each provider's *AccountDiscovery var.
var googleAdsMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderGoogleAds,
	displayName: "google ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"every field set, and that login_customer_id is digits only",
	operation: "account monitor",
}

var linkedInAdsMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
	operation: "account monitor",
}

var metaAdsMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
	operation: "account monitor",
}

// microsoftAdsMonitorDiscovery is this endpoint's own descriptor, for the same reason as the
// three above; remedy text copied verbatim from microsoftAdsAccountDiscovery.
var microsoftAdsMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderMicrosoftAds,
	displayName: "microsoft ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with client_id, client_secret, developer_token and refresh_token set",
	operation: "account monitor",
}

// twitterAdsMonitorDiscovery is this endpoint's own descriptor, for the same reason as the four
// above; remedy text copied verbatim from twitterAdsAccountDiscovery (X's OAuth 1.0a four-tuple).
var twitterAdsMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderTwitterAds,
	displayName: "x/twitter ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with consumer_key, consumer_secret, access_token and access_token_secret set",
	operation: "account monitor",
}

// validateMonitorDays enforces the 7..90 range the design layer also constrains with
// Minimum/Maximum. Enforced here too for the same reason resolveInsightsWindow re-checks its
// own enum: a runtime rejection with no matching design constraint (or the reverse) is the
// drift this repo's rules exist to prevent, and a non-HTTP caller does not go through Goa's
// generated validation at all.
func validateMonitorDays(days int) error {
	if days < domain.MonitorDaysMin || days > domain.MonitorDaysMax {
		return &conn.BadRequestError{Code: "400", Message: domain.ErrMonitorDaysInvalid.Error()}
	}
	return nil
}

// monitorTotals sums the per-campaign rows. Every platform's account totals are this sum —
// see model.AccountMonitorTotals' own doc comment — so the figures a response reports always
// describe exactly the campaigns array returned alongside them.
//
// Conversions is the one field that can come back absent: see the type's own doc comment.
//
// A row is never specially excluded from the sum: a metrics-fetch-failed row contributes its
// zero-value numeric fields, which is a no-op, since there is no correct non-zero contribution
// to substitute; a Google row whose budget alone was unparseable (round-24/25 review)
// contributes its genuinely non-zero spend/impressions/clicks, which is correct because only
// its budget field — not summed here — was untrusted.
func monitorTotals(rows []model.AccountCampaignMetrics) *model.AccountMonitorTotals {
	t := &model.AccountMonitorTotals{CampaignCount: len(rows)}
	var conversions float64
	var measured bool
	for _, r := range rows {
		t.Spend += r.Spend
		t.Impressions += r.Impressions
		t.Clicks += r.Clicks
		if r.Conversions != nil {
			conversions += *r.Conversions
			measured = true
		}
	}
	// Absent, not zero, when nothing in the sum measured conversions — a total of 0 across
	// rows that all report "unmeasured" is a claim none of them made. Reddit is the case that
	// forces it: every Reddit row carries nil since linuxfoundation/lfx-self-serve#3020.
	if measured {
		t.Conversions = &conversions
	}
	return t
}

// toConnAccountMonitorCampaign converts one rule-engine output row to the generated response
// type. campaign_url is Google Ads only and left nil for every other platform, per
// AccountMonitorCampaign's design-layer doc comment.
func toConnAccountMonitorCampaign(row model.AccountMonitorRow) *conn.AccountMonitorCampaign {
	m := row.Metrics
	c := &conn.AccountMonitorCampaign{
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
	if m.CampaignURL != "" {
		url := m.CampaignURL
		c.CampaignURL = &url
	}
	return c
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

// monitorAccount is the shared body of the four live-read monitor-*-ads-account handlers: validate,
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
	return buildAccountMonitor(accountID, days, rows, actionItems), nil
}

// buildAccountMonitor assembles the response from the rule engine's output. Shared by the live
// read (monitorAccount) and the report-backed one (monitorReportedAccount) so the totals rule —
// sum of exactly the rows returned — has one implementation whichever way the rows were read.
func buildAccountMonitor(accountID string, days int, rows []model.AccountMonitorRow, actionItems []model.AccountMonitorActionItem) *conn.AccountMonitor {
	// Sum the post-rule-engine rows (rows), not the raw dispatcher read (metricsRows):
	// EvaluateGoogleMonitor drops the operator's scratch campaigns before returning, and the
	// totals must describe the campaigns array actually returned in this same response rather
	// than a superset the caller never sees.
	filteredMetrics := make([]model.AccountCampaignMetrics, len(rows))
	for i, r := range rows {
		filteredMetrics[i] = r.Metrics
	}
	totals := monitorTotals(filteredMetrics)

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
	}
}

// monitorReportedAccount is monitorAccount for a platform whose metrics come from a saved
// asynchronous report (Orchestrator.ReadReportedAccountCampaigns) rather than a live read. The
// guards, error classification, rule evaluation and totals are the same; what differs is that
// the response also says how old the metrics are and whether newer ones are building.
func (s *ConnectionService) monitorReportedAccount(
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
	read, rerr := orch.ReadReportedAccountCampaigns(ctx, projectID, platform, accountID, days)
	if rerr != nil {
		return nil, s.classifyDiscoveryError(ctx, projectID, discovery, rerr)
	}
	rows, actionItems := evaluate(read.Rows)
	out := buildAccountMonitor(accountID, days, rows, actionItems)
	if read.MetricsAsOf != nil {
		asOf := read.MetricsAsOf.UTC().Format(time.RFC3339)
		out.MetricsAsOf = &asOf
	}
	// Always set for a report-backed platform, false included: the field's absence is how the
	// live-read platforms say "not applicable", so leaving it off here would read the same way.
	pending := read.MetricsPending
	out.MetricsPending = &pending
	return out, nil
}

// MonitorGoogleAdsAccount reads every campaign visible on a Google Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorGoogleAdsAccount(ctx context.Context, p *conn.MonitorGoogleAdsAccountPayload) (*conn.AccountMonitor, error) {
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderGoogleAds, googleAdsMonitorDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateGoogleMonitor(rows, p.Days)
		})
}

// MonitorLinkedinAdsAccount reads every campaign visible on a LinkedIn Ads account, live from
// the platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorLinkedinAdsAccount(ctx context.Context, p *conn.MonitorLinkedinAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderLinkedInAds, linkedInAdsMonitorDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateLinkedInMonitor(rows, p.Days, now)
		})
}

// MonitorMetaAdsAccount reads every campaign visible on a Meta Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine.
func (s *ConnectionService) MonitorMetaAdsAccount(ctx context.Context, p *conn.MonitorMetaAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderMetaAds, metaAdsMonitorDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateMetaMonitor(rows, p.Days, now)
		})
}

// MonitorMicrosoftAdsAccount reads every live campaign on a Microsoft Advertising account, with
// metrics from the last finished Microsoft report — see Orchestrator.ReadReportedAccountCampaigns
// for why Microsoft cannot be read live like the other four.
func (s *ConnectionService) MonitorMicrosoftAdsAccount(ctx context.Context, p *conn.MonitorMicrosoftAdsAccountPayload) (*conn.AccountMonitor, error) {
	return s.monitorReportedAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderMicrosoftAds, microsoftAdsMonitorDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateMicrosoftMonitor(rows, p.Days)
		})
}

// MonitorTwitterAdsAccount reads every live campaign on an X Ads account, with metrics from the
// last finished set of X stats jobs — report-backed for every `days` value, see
// Orchestrator.ReadReportedAccountCampaigns and internal/platform/twitter/monitor.go for why.
func (s *ConnectionService) MonitorTwitterAdsAccount(ctx context.Context, p *conn.MonitorTwitterAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorReportedAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderTwitterAds, twitterAdsMonitorDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateTwitterMonitor(rows, p.Days, now)
		})
}

// MonitorRedditAdsAccount reads every campaign visible on a Reddit Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine. Totals are the sum
// of the returned rows, the same as every other platform — see model.AccountMonitorTotals' own
// doc comment for why the BFF's separate account-level call was not carried over.
func (s *ConnectionService) MonitorRedditAdsAccount(ctx context.Context, p *conn.MonitorRedditAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderRedditAds, redditAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateRedditMonitor(rows, p.Days, now)
		})
}
