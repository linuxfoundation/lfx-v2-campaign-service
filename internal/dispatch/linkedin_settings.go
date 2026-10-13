// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Upstream-only readback fields only LinkedIn reports. The bid's setup is three LinkedIn fields
// (optimizationTargetType decides manual vs automated, costType the charge unit, unitCost the
// amount), so all three are shown: bidding_strategy_type alone cannot say whether a manual bid
// is per click.
const (
	settingsFieldCostType      = "cost_type"
	settingsFieldUnitCost      = "unit_cost"
	settingsFieldObjectiveType = "objective_type"
)

// ReadSettings implements service.SettingsReader for LinkedIn: one account-scoped GET of the
// campaign (GET adAccounts/{acct}/adCampaigns/{id}), compared against what the row recorded.
//
// STRICTLY READ-ONLY: nothing is mutated upstream and nothing is written to the row.
//
// FIELDS. LinkedIn holds the budget, flight and bid ON THE CAMPAIGN, so one read answers all:
//
//   - compared: budget_amount, budget_type (dailyBudget → daily, totalBudget → lifetime),
//     campaign_name, start_date, end_date (runSchedule).
//   - upstream-only: status (a different axis from the row's lifecycle status, as on every
//     platform), bidding_strategy_type (optimizationTargetType), cost_type, unit_cost (the bid
//     amount, in whole units of its currency) and objective_type. The create path records none
//     of these, so each reads `unknown`.
//
// BUDGET. The create path sets exactly one of dailyBudget/totalBudget in USD, so a campaign
// reporting one is compared as that type. One reporting BOTH (LinkedIn allows a daily budget
// with a lifetime cap) or NEITHER (e.g. under a campaign group's DYNAMIC budget optimization,
// where the group's budget is shared) leaves both budget fields `unknown`; so does a non-USD or
// unreadable amount. Amounts are read EXACTLY as micro-units — never through a float — and
// rendered by the same function as every other reader, so a sub-cent amount cannot round into
// equality with the recorded one.
//
// DATES. The create path sends runSchedule.start as the start day at 00:00 UTC, nudged to
// "now + buffer" when that day has begun (toMs), and runSchedule.end as 23:59:59.999 UTC of the
// end day. Both are compared as UTC dates; the start goes through compareNudgedStart.
//
// PROVENANCE. Unknown provenance fails closed before any credential is resolved (409); a
// recorded account other than the connection's is a mismatch (409). The read is account-scoped
// by its path, and an `account` URN naming another account is refused with
// ErrCampaignUpstreamIdentityMismatch. A 404 is reported absent only once the ad account is
// confirmed readable (linkedin.GetCampaignSettings).
//
// An ADOPTED row records no budget or flight, so those read `unknown`; the campaign-level
// fields are read normally.
func (d *LinkedInDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	created := linkedInCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read linkedin campaign settings: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	res, creds, err := d.resolveLinkedInCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return nil, err
	}
	accountID := strings.TrimSpace(res.accountID)
	if err := verifyLinkedInAccountMatch("read linkedin campaign settings", campaign, accountID); err != nil {
		return nil, err
	}
	client := d.cachedLinkedInClient(projectID, platform, res, creds, accountID)

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID)
	if err != nil {
		if linkedinConnectionDefect(err) {
			return nil, res.systemScoped(linkedinExpiry(fmt.Errorf("read linkedin campaign settings: %w", err)))
		}
		return nil, fmt.Errorf("read linkedin campaign settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%w: linkedin campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}
	if settings.AccountID != "" && settings.AccountID != accountID {
		return nil, fmt.Errorf("read linkedin campaign settings: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaign.PlatformCampaignID, settings.AccountID, accountID, domain.ErrCampaignUpstreamIdentityMismatch)
	}

	rec := recordedSettings(campaign)
	var upstreamBudget, upstreamBudgetType *string
	if !settings.BudgetUnparseable && strings.EqualFold(settings.BudgetCurrency, "USD") {
		switch {
		case settings.DailyBudgetMicros != nil && settings.TotalBudgetMicros == nil:
			upstreamBudgetType = strPtr(string(model.BudgetDaily))
			upstreamBudget = settingsMicrosToUnits(settings.DailyBudgetMicros)
		case settings.TotalBudgetMicros != nil && settings.DailyBudgetMicros == nil:
			upstreamBudgetType = strPtr(string(model.BudgetLifetime))
			upstreamBudget = settingsMicrosToUnits(settings.TotalBudgetMicros)
		}
	}
	var unitCost *string
	if !settings.UnitCostUnparseable {
		unitCost = settingsMicrosToUnits(settings.UnitCostMicros)
	}

	startField := model.CompareSettingsField(settingsFieldStartDate, rec.start, settingsUTCDate(settings.RunStart))
	if settings.RunStart != nil && campaign.StartDate != nil {
		startField = compareNudgedStart(campaign.StartDate, settings.RunStart, campaign.CreatedAt)
	}

	rb := newSettingsReadback(campaign, platform, settings.CampaignID, settingsNow(nil))
	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, rec.budget, upstreamBudget),
		model.CompareSettingsField(settingsFieldBudgetType, rec.budgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, rec.name, settings.Name),
		model.CompareSettingsField(settingsFieldStatus, nil, settings.Status),
		startField,
		model.CompareSettingsField(settingsFieldEndDate, rec.end, settingsUTCDate(settings.RunEnd)),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, settings.OptimizationTargetType),
		model.CompareSettingsField(settingsFieldCostType, nil, settings.CostType),
		model.CompareSettingsField(settingsFieldUnitCost, nil, unitCost),
		model.CompareSettingsField(settingsFieldObjectiveType, nil, settings.ObjectiveType),
	}
	rb.SummariseSettings()
	return rb, nil
}
