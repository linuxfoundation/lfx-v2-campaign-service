// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
)

// redditAdsAccountDiscovery is the account-discovery descriptor for Reddit Ads.
//
// Despite its name it names the "account monitor" operation: it was added for the monitor
// endpoint, the first Reddit surface to route through classifyDiscoveryError, before Reddit had
// account discovery. list-reddit-ads-accounts (LFXV2-2665) uses connection.go's
// redditAdsListDiscovery instead.
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
// to substitute; a row whose budget alone was unparseable — on any platform that reports a
// budget, not only Google Ads — contributes its genuinely non-zero spend/impressions/clicks,
// which is correct because only its budget field, not summed here, was untrusted. LinkedIn is
// the case that does contribute a wrong figure: a row whose costInLocalCurrency was unreadable
// carries Spend=0 with real impressions/clicks, so the spend sum understates it — which is
// exactly what UnmeasuredCampaigns exists to flag below.
//
// What the sum cannot say by itself is that some of it was not measured: UnmeasuredCampaigns
// counts the FetchFailed rows so the response does (#218). It counts every FetchFailed row,
// the budget-only ones included, which makes it an upper bound on the rows missing from the
// sum — see model.AccountMonitorTotals.UnmeasuredCampaigns. PacingUnknown alone is deliberately
// not counted: it is about pacing inputs, which nothing here sums.
func monitorTotals(rows []model.AccountCampaignMetrics) *model.AccountMonitorTotals {
	t := &model.AccountMonitorTotals{CampaignCount: len(rows)}
	var conversions float64
	var measured bool
	for _, r := range rows {
		t.Spend += r.Spend
		t.Impressions += r.Impressions
		t.Clicks += r.Clicks
		if r.FetchFailed {
			t.UnmeasuredCampaigns++
		}
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
		if it.EmailID != "" {
			id := it.EmailID
			ci.EmailID = &id
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
		Spend:               t.Spend,
		Impressions:         t.Impressions,
		Clicks:              t.Clicks,
		Conversions:         t.Conversions,
		CampaignCount:       t.CampaignCount,
		UnmeasuredCampaigns: t.UnmeasuredCampaigns,
	}
}

// monitorScopeConflictPlatforms are the monitor methods that declare a 409 for a campaign-scope
// conflict: the shared-tenancy platforms whose dispatchers implement AccountMonitorScoper. Only
// they may receive the scope arms in classifyMonitorError; a method that declares no 409 could not
// encode one.
var monitorScopeConflictPlatforms = map[model.Provider]bool{
	model.ProviderGoogleAds:  true,
	model.ProviderMetaAds:    true,
	model.ProviderRedditAds:  true,
	model.ProviderTwitterAds: true,
}

// classifyMonitorError is classifyDiscoveryError plus the campaign-scope refusals of a
// project-scoped monitor (lfx-self-serve-ops#897): the project's campaigns could not all be proven
// to belong to the monitored account, so the read fails closed rather than reporting part of them
// as the whole. Arm order and wording follow classifyInsightsErrorFor, which answers the same
// refusals on the insight reads; every other error is delegated unchanged.
//
// Every 409 here carries its reason: X's monitor declares AccountMonitorConflictError and the
// other three AccountMonitorScopeConflictError, both of which require one. Neither is the shared
// ConflictError, whose published example is a connection collision no monitor returns. The
// account ids stay in the log.
func (s *ConnectionService) classifyMonitorError(ctx context.Context, projectID string, platform model.Provider, d accountDiscovery, err error) error {
	if !monitorScopeConflictPlatforms[platform] {
		return s.classifyDiscoveryError(ctx, projectID, d, err)
	}
	var reason, message string
	switch {
	case errors.Is(err, domain.ErrCampaignProvenanceMixed):
		// Before the two arms it is always joined with, so both remedies are named.
		reason = "campaign_account_mismatch"
		message = "some of this project's campaigns were created under a different ad account than the one being monitored, and others do not record which ad account they were created under — re-dispatch the unrecorded campaigns onto this account, and re-dispatch or reconcile the others onto it (or monitor the account that owns them), to monitor this account"
	case errors.Is(err, domain.ErrCampaignProvenanceUnknown):
		// Before the mismatch arm it is joined with: only re-dispatch repairs an unrecorded row.
		reason = "campaign_account_mismatch"
		message = "some of this project's campaigns do not record which ad account they were created under, so they cannot be read safely — re-dispatch those campaigns onto the connected account to monitor this account"
	case errors.Is(err, ErrCampaignAccountMismatch):
		reason = "campaign_account_mismatch"
		message = "some of this project's campaigns were created under a different ad account than the one being monitored — monitor the account that owns them, or re-dispatch or reconcile those campaigns onto this one, to monitor this account"
	case errors.Is(err, domain.ErrAudienceScopeInvalid):
		reason = "campaign_scope_invalid"
		message = "a campaign recorded for this project has an id the platform cannot read — re-dispatch it to monitor this account"
	default:
		return s.classifyDiscoveryError(ctx, projectID, d, err)
	}
	slog.WarnContext(ctx, "account monitor blocked: the project's campaign scope does not fit the monitored account",
		"project_id", projectID, "platform", string(platform), "reason", reason, "error", safeErrSummary(err))
	if platform == model.ProviderTwitterAds {
		return &conn.AccountMonitorConflictError{Code: "409", Reason: reason, Message: message}
	}
	return &conn.AccountMonitorScopeConflictError{Code: "409", Reason: reason, Message: message}
}

// monitorAccount is the shared body of the four live-read monitor-*-ads-account handlers: validate,
// fetch, evaluate, total, and assemble the response. evaluate is the one thing that cannot be
// shared uniformly, so each caller passes its own platform's Evaluate*Monitor. Google's
// EvaluateGoogleMonitor takes `days` but no `now` (expected spend is budget_day*days, with no
// flight-date dependency). LinkedIn's and Reddit's take a `now`, which their callers close over.
// Meta's EvaluateMetaMonitor takes neither a clock nor a closure: each row carries the exact
// Insights window the read used (MetricsWindowStart/End), so its pacing never consults a clock
// of its own and the method value is passed directly.
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
	read, merr := orch.ReadAccountCampaignMetrics(ctx, projectID, platform, accountID, days)
	if merr != nil {
		return nil, s.classifyMonitorError(ctx, projectID, platform, discovery, merr)
	}

	rows, actionItems := evaluate(read.Rows)
	reportUnmeasuredRows(ctx, orch.dispatchMetrics(), projectID, platform, rows)
	return buildAccountMonitor(accountID, days, rows, actionItems, read.Currency), nil
}

// monitorFailureClassOrder is every model.MonitorFailureClass, in the order the summary log
// lists them and the metric is recorded — fixed, so the log line has the same shape every time.
var monitorFailureClassOrder = []model.MonitorFailureClass{
	model.MonitorFailureAuth,
	model.MonitorFailureThrottle,
	model.MonitorFailureUpstream5xx,
	model.MonitorFailureDecode,
	model.MonitorFailureNoReport,
	model.MonitorFailureOther,
	model.MonitorFailureUnknown,
}

// reportUnmeasuredRows is the operational signal for the FetchFailed rows of one monitor
// response (#220). The platform clients mark a failed row and drop the error — propagating it
// would fail every other row with it — so without this nothing distinguished a revoked token
// from a 429 burst from an upstream schema change when a response came back all fetch_failed.
//
// Per request, not per row: ONE Warn listing per-class counts, and one counter Add per class
// present. A per-row log would be N lines for one broken token. The rows counted are the ones
// returned (after the rule engine's filtering), the same rows totals.unmeasured_campaigns
// counts, so the log, the metric and the response agree.
//
// Only bounded values are logged: the project id (the account monitor's existing log key —
// the account id is not logged, matching the report-backed monitor's logs and the clients'
// own redaction of it from errors), the platform, and integer counts per closed-set class. No
// upstream error text, which can carry ids and response fragments.
//
// The Warn is skipped when every failed row is no_report: a report-backed account with no
// finished report yet is the expected first read, not a fault. The metric still counts them.
func reportUnmeasuredRows(ctx context.Context, m DispatchMetrics, projectID string, platform model.Provider, rows []model.AccountMonitorRow) {
	counts := make(map[model.MonitorFailureClass]int, len(monitorFailureClassOrder))
	total := 0
	for _, r := range rows {
		if !r.Metrics.FetchFailed {
			continue
		}
		counts[model.NormalizeMonitorFailureClass(r.Metrics.FailureClass)]++
		total++
	}
	if total == 0 {
		return
	}
	byClass := make([]any, 0, len(monitorFailureClassOrder))
	for _, c := range monitorFailureClassOrder {
		if counts[c] > 0 {
			m.RecordMonitorUnmeasuredRows(ctx, platform, string(c), counts[c])
		}
		byClass = append(byClass, slog.Int(string(c), counts[c]))
	}
	if counts[model.MonitorFailureNoReport] == total {
		return
	}
	slog.WarnContext(ctx, "account monitor: some campaigns could not be measured; totals may understate the account",
		"project_id", projectID,
		"platform", string(platform),
		"campaign_count", len(rows),
		"unmeasured_campaigns", total,
		slog.Group("unmeasured_by_class", byClass...),
	)
}

// buildAccountMonitor assembles the response from the rule engine's output. Shared by the live
// read (monitorAccount) and the report-backed one (monitorReportedAccount) so the totals rule —
// sum of exactly the rows returned — has one implementation whichever way the rows were read.
//
// currency is the account's ISO 4217 code as the read reported it; anything that is not one
// (including "") leaves the field absent rather than published.
func buildAccountMonitor(accountID string, days int, rows []model.AccountMonitorRow, actionItems []model.AccountMonitorActionItem, currency string) *conn.AccountMonitor {
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

	out := &conn.AccountMonitor{
		AccountID:   accountID,
		Days:        days,
		Campaigns:   connCampaigns,
		ActionItems: toConnAccountMonitorActionItems(actionItems),
		Totals:      toConnAccountMonitorTotals(totals),
	}
	if c := model.NormalizeCurrencyCode(currency); c != "" {
		out.Currency = &c
	}
	return out
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
	evaluate func(read *model.ReportedAccountRead) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem),
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
		return nil, s.classifyMonitorError(ctx, projectID, platform, discovery, rerr)
	}
	rows, actionItems := evaluate(read)
	reportUnmeasuredRows(ctx, orch.dispatchMetrics(), projectID, platform, rows)
	out := buildAccountMonitor(accountID, days, rows, actionItems, read.Currency)
	if read.MetricsAsOf != nil {
		asOf := read.MetricsAsOf.UTC().Format(time.RFC3339)
		out.MetricsAsOf = &asOf
	}
	// The days the metrics actually cover, from the saved report's own window — not re-derived
	// from `days`, which is the request and can differ (X drops a day from a 90-day window that
	// crosses a DST fall-back). Both or neither: a half-known window states nothing.
	if read.MetricsWindowStart != nil && read.MetricsWindowEnd != nil {
		ws := read.MetricsWindowStart.UTC().Format(time.DateOnly)
		we := read.MetricsWindowEnd.UTC().Format(time.DateOnly)
		out.MetricsWindowStart, out.MetricsWindowEnd = &ws, &we
	}
	// Always set for a report-backed platform, false included: the field's absence is how the
	// live-read platforms say "not applicable", so leaving it off here would read the same way.
	pending := read.MetricsPending
	out.MetricsPending = &pending
	return out, nil
}

// MonitorGoogleAdsAccount reads this project's own campaigns on a Google Ads account, live from
// the platform, with pacing and action items derived by the ported rule engine. The customer is
// shared across foundations, so the read is scoped to the campaigns this service recorded for the
// project (Orchestrator.ReadAccountCampaignMetrics).
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

// MonitorMetaAdsAccount reads this project's own campaigns on a Meta Ads account, live from the
// platform, with pacing and action items derived by the ported rule engine. The ad account is
// shared across foundations, so the read is scoped to the campaigns this service recorded for the
// project.
func (s *ConnectionService) MonitorMetaAdsAccount(ctx context.Context, p *conn.MonitorMetaAdsAccountPayload) (*conn.AccountMonitor, error) {
	// No clock here: each row carries the Insights window its spend covers, and the rules pace
	// against exactly those days (see rules.EvaluateMetaMonitor).
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderMetaAds, metaAdsMonitorDiscovery,
		rules.EvaluateMetaMonitor)
}

// MonitorMicrosoftAdsAccount reads every live campaign on a Microsoft Advertising account, with
// metrics from the last finished Microsoft report — see Orchestrator.ReadReportedAccountCampaigns
// for why Microsoft cannot be read live like the other four.
func (s *ConnectionService) MonitorMicrosoftAdsAccount(ctx context.Context, p *conn.MonitorMicrosoftAdsAccountPayload) (*conn.AccountMonitor, error) {
	return s.monitorReportedAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderMicrosoftAds, microsoftAdsMonitorDiscovery,
		func(read *model.ReportedAccountRead) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateMicrosoftMonitor(read.Rows, p.Days)
		})
}

// MonitorTwitterAdsAccount reads this project's own live campaigns on an X Ads account (the account
// is shared across foundations), with metrics from the last finished set of X stats jobs — report-backed for every `days` value, see
// Orchestrator.ReadReportedAccountCampaigns and internal/platform/twitter/monitor.go for why.
//
// The rules are evaluated on the saved report's own window — the account-local calendar days X's
// stats jobs covered — not on a "today" taken from this service's clock, which is UTC and would
// disagree with the account's days (and with the line items' account-local flight dates) for
// part of every day.
func (s *ConnectionService) MonitorTwitterAdsAccount(ctx context.Context, p *conn.MonitorTwitterAdsAccountPayload) (*conn.AccountMonitor, error) {
	return s.monitorReportedAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderTwitterAds, twitterAdsMonitorDiscovery,
		func(read *model.ReportedAccountRead) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateTwitterMonitor(read.Rows, read.MetricsWindowStart, read.MetricsWindowEnd)
		})
}

// MonitorRedditAdsAccount reads this project's own campaigns on a Reddit Ads account, live from
// the platform, with pacing and action items derived by the ported rule engine. The ad account is
// shared across foundations, so the read is scoped to the campaigns this service recorded for the
// project. Totals are the sum
// of the returned rows, the same as every other platform — see model.AccountMonitorTotals' own
// doc comment for why the BFF's separate account-level call was not carried over.
func (s *ConnectionService) MonitorRedditAdsAccount(ctx context.Context, p *conn.MonitorRedditAdsAccountPayload) (*conn.AccountMonitor, error) {
	now := time.Now()
	return s.monitorAccount(ctx, p.ProjectID, p.AccountID, p.Days, model.ProviderRedditAds, redditAdsAccountDiscovery,
		func(rows []model.AccountCampaignMetrics) ([]model.AccountMonitorRow, []model.AccountMonitorActionItem) {
			return rules.EvaluateRedditMonitor(rows, p.Days, now)
		})
}
