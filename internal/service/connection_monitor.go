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

// monitorTotalsFallback sums the per-campaign rows exactly the way every platform but Reddit's
// account-wide totals are derived (model.AccountMonitorTotals' own doc comment). A FetchFailed
// row contributes its zero-value numeric fields, which is a no-op on the sum — it is not
// specially excluded, because there is no correct non-zero contribution to substitute.
//
// derived controls DerivedFromRows on the result and must be true only when this sum stands in
// for a platform-native figure that was expected but unavailable — i.e. only ever for Reddit
// (the one platform with its own AccountTotalsReader) when that call failed. For every other
// platform this sum is not a stand-in, it IS the documented contract (see
// model.AccountMonitorTotals), so the caller must pass false there even though the arithmetic
// is identical.
func monitorTotalsFallback(rows []model.AccountCampaignMetrics, derived bool) *model.AccountMonitorTotals {
	t := &model.AccountMonitorTotals{CampaignCount: len(rows), DerivedFromRows: derived}
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
		Spend:           t.Spend,
		Impressions:     t.Impressions,
		Clicks:          t.Clicks,
		Conversions:     t.Conversions,
		CampaignCount:   t.CampaignCount,
		DerivedFromRows: t.DerivedFromRows,
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
	//
	// len(rows) is safe to pass as the row count only because no AccountTotalsReader
	// implementation today filters rows the way the rule engines above do (see
	// EvaluateGoogleMonitor's zz-prefix drop) — if one ever did, this count would need to
	// come from whatever that implementation actually returned, not from rows.
	totals, ok, terr := orch.ReadAccountTotals(ctx, projectID, platform, accountID, days, len(rows))
	totalsReadFailed := terr != nil
	if terr != nil {
		if errors.Is(terr, errAccountTotalsContractViolation) {
			// A broken AccountTotalsReader adapter, not an ordinary upstream failure — see
			// errAccountTotalsContractViolation's doc comment. Logged at ERROR so it doesn't
			// blend into the routine WARN-level fallback traffic below; the row-summed
			// fallback is still served, since the campaign rows above already succeeded.
			slog.ErrorContext(ctx, "account totals reader violated its contract; serving the row-summed fallback",
				"error", terr, "project_id", projectID, "platform", platform)
		} else if errors.Is(terr, domain.ErrConnectionNotUsable) ||
			errors.Is(terr, domain.ErrCredentialDecryptionFailed) ||
			errors.Is(terr, domain.ErrServiceDefect) {
			// terr can carry ErrConnectionNotUsable, ErrCredentialDecryptionFailed, or
			// ErrServiceDefect — all three have detection/classification paths that touch a
			// decrypted credential blob, so neither the cause nor its text may leave this
			// function (see classifyDiscoveryError's arms in connection.go). Log the
			// fixed-vocabulary reason instead; unusableConnectionReason has no case for the
			// latter two and safely falls through to "unclassified" for them.
			slog.WarnContext(ctx, "account totals read failed; serving the row-summed fallback",
				"reason", unusableConnectionReason(terr), "project_id", projectID, "platform", platform)
		} else {
			// Any other failure carries no credential-derived material, so the error itself is
			// safe to log directly — a fixed-vocabulary reason would otherwise hide the actual
			// upstream cause for an ordinary API/network failure.
			slog.WarnContext(ctx, "account totals read failed; serving the row-summed fallback",
				"error", terr, "project_id", projectID, "platform", platform)
		}
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
		// derived must be true only when a platform-native figure was expected and its read
		// actually failed (totalsReadFailed, captured before the !ok-with-nil-err path below
		// resets ok), not whenever this branch is reached at all: a platform with no
		// AccountTotalsReader implementation reaches this branch via !ok-with-nil-err on
		// EVERY request, and for it this sum IS the contractual figure, not a stand-in.
		// Keying off the provider instead of the actual failure signal would silently
		// mislabel a future second AccountTotalsReader implementation's failures as
		// non-derived.
		totals = monitorTotalsFallback(filteredMetrics, totalsReadFailed)
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
