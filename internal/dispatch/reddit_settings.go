// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

// ReadSettings implements service.SettingsReader for Reddit: it reads the campaign with ONE
// account-scoped GET and compares it against what the campaign row recorded.
//
// STRICTLY READ-ONLY. The only upstream call is reddit.Client.GetCampaignSettings; nothing here
// mutates the platform and nothing writes to the campaign row.
//
// FIELDS. The create path sets everything compared here ON THE CAMPAIGN — the budget as
// goal_type/goal_value with campaign budget optimization on, and the flight as
// start_time/end_time (the ad group carries the same window and no budget) — so no ad group is
// read, and an ADOPTED row, which records no ad group, reads exactly as a created one does.
//
//   - compared: budget_amount, budget_type, campaign_name, start_date, end_date.
//   - upstream-only: status (configured_status — a different axis from the row's lifecycle
//     status, exactly as on Google) and bidding_strategy_type (bid_strategy; the create path
//     always sends BIDLESS and records nothing to compare it to).
//
// UNITS. goal_value is in MICRO-units of the account currency, the unit the create path converts
// the caller's budgetUsd into, so it is rendered exactly as Google's micros are — two decimal
// places when whole cents, full precision otherwise. The budget is read only when
// is_campaign_budget_optimization is TRUE: with it off (or unreported) the spend is governed per
// ad group and goal_value is not the budget the row recorded, so both budget fields are absent
// and `unknown` rather than compared against the wrong quantity. goal_type maps LIFETIME_SPEND →
// lifetime and DAILY_SPEND → daily; anything else is `unknown`.
//
// DATES. The create path sends start_time/end_time at 00:00 UTC of the requested days, nudging a
// start that has already begun forward to dispatch time + a buffer. They are compared as UTC
// calendar dates, the start through compareNudgedStart so that nudge is not reported as a
// divergence.
//
// PROVENANCE. Unknown provenance fails closed before any credential is resolved (409); a recorded
// account that differs from the connection's is a mismatch (409). The read is account-scoped by
// its path; an ad_account_id in the answer that names a different account is the same mismatch
// rather than a configuration to report.
func (d *RedditDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	created := redditCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read reddit campaign settings: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return nil, err
	}
	if err := verifyRedditAccountMatch("read reddit campaign settings", campaign, client); err != nil {
		return nil, err
	}

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID)
	if err != nil {
		if errors.Is(err, reddit.ErrInvalidAccountID) {
			return nil, res.systemScoped(fmt.Errorf("%w: %w: read reddit campaign settings: the connection's ad account id cannot address a reddit request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, err))
		}
		return nil, fmt.Errorf("read reddit campaign settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%w: reddit campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}
	if settings.AdAccountID != "" && settings.AdAccountID != strings.TrimSpace(client.AccountID()) {
		return nil, fmt.Errorf("read reddit campaign settings: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaign.PlatformCampaignID, settings.AdAccountID, client.AccountID(), domain.ErrCampaignAccountMismatch)
	}

	rec := recordedSettings(campaign)
	var upstreamBudget, upstreamBudgetType *string
	if settings.CampaignBudgetOptimization != nil && *settings.CampaignBudgetOptimization && settings.GoalType != nil {
		var bt model.BudgetType
		switch *settings.GoalType {
		case reddit.GoalTypeLifetimeSpend:
			bt = model.BudgetLifetime
		case reddit.GoalTypeDailySpend:
			bt = model.BudgetDaily
		}
		if bt != "" {
			upstreamBudgetType = strPtr(string(bt))
			upstreamBudget = settingsMicrosToUnits(settings.GoalValueMicros)
		}
	}
	upstreamStart := parseSettingsTimestamp(settings.StartTime, time.RFC3339)
	upstreamEnd := parseSettingsTimestamp(settings.EndTime, time.RFC3339)
	startField := model.CompareSettingsField(settingsFieldStartDate, rec.start, settingsUTCDate(upstreamStart))
	if upstreamStart != nil && campaign.StartDate != nil {
		startField = compareNudgedStart(campaign.StartDate, upstreamStart, campaign.CreatedAt)
	}

	rb := newSettingsReadback(campaign, platform, settings.CampaignID, settingsNow(d.settingsNow))
	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, rec.budget, upstreamBudget),
		model.CompareSettingsField(settingsFieldBudgetType, rec.budgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, rec.name, settings.Name),
		model.CompareSettingsField(settingsFieldStatus, nil, settings.ConfiguredStatus),
		startField,
		model.CompareSettingsField(settingsFieldEndDate, rec.end, settingsUTCDate(upstreamEnd)),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, settings.BidStrategy),
	}
	rb.SummariseSettings()
	return rb, nil
}
