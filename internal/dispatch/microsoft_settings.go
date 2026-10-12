// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

// ReadSettings implements service.SettingsReader for Microsoft Advertising: it reads the
// campaign's live configuration with ONE GetCampaignsByIds call and compares it against what the
// campaign row recorded.
//
// STRICTLY READ-ONLY. The only upstream call is microsoft.Client.GetCampaignSettings; nothing
// here mutates the platform and nothing writes to the campaign row.
//
// FIELDS. Microsoft keeps the whole budget ON THE CAMPAIGN (DailyBudget + BudgetType, unless a
// shared Budget is attached), so one campaign read answers every field — no ad group is read.
//
//   - compared: budget_amount, budget_type, campaign_name.
//   - upstream-only: status (a different axis from the row's lifecycle status, exactly as on
//     Google), budget_explicitly_shared (a BudgetId attaches a shared Budget, whose amount the
//     campaign merely echoes) and bidding_strategy_type (BiddingScheme.Type; the create path
//     sends no strategy and records none, so there is nothing to compare it to).
//   - not reported: start_date / end_date. A Microsoft campaign carries no flight dates at all,
//     and the create path records none (applyCampaignConfig is passed ""), so neither side
//     exists to compare.
//
// UNITS. DailyBudget is a plain decimal in the ad account's currency — the unit the create path
// sends cfg.Budget in, unconverted — so it is rendered at the row's two decimal places when it is
// whole cents and at full precision otherwise, so a sub-cent amount can never round into a match.
// The amount is read ONLY when BudgetType names a DAILY budget: Microsoft documents a lifetime
// type (Audience campaigns only), and under it DailyBudget is not a daily rate, so comparing it
// against the row's daily amount would compare two different quantities.
//
// PROVENANCE is held exactly as strictly as Google's reader holds it: a row that records no
// creating account fails closed BEFORE any credential is resolved (unknown provenance → 409),
// and a recorded account that differs from the connection's is a mismatch (409). The read is
// account-scoped (AccountId in the body, CustomerAccountId on the request) and Microsoft answers
// CampaignServiceInvalidCampaignId for a campaign in any other account, so a successful read is
// itself proof the campaign lives in the account the row recorded.
func (d *MicrosoftDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	created := microsoftCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read microsoft campaign settings: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, err := d.resolveMicrosoftClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	if err := verifyMicrosoftAccountMatch("read microsoft campaign settings", campaign, client); err != nil {
		return nil, err
	}

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID)
	if err != nil {
		return nil, fmt.Errorf("read microsoft campaign settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%w: microsoft-ads campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	rec := recordedSettings(campaign)
	var upstreamType model.BudgetType
	if settings.BudgetType != nil {
		upstreamType = microsoftBudgetTypeFromLimit(*settings.BudgetType)
	}
	var upstreamBudget, upstreamBudgetType *string
	if upstreamType != "" {
		upstreamBudgetType = strPtr(string(upstreamType))
	}
	if upstreamType == model.BudgetDaily && settings.DailyBudget != nil {
		upstreamBudget = strPtr(formatMicrosoftBudgetAmount(*settings.DailyBudget))
	}

	rb := newSettingsReadback(campaign, platform, settings.CampaignID, settingsNow(d.settingsNow))
	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, rec.budget, upstreamBudget),
		model.CompareSettingsField(settingsFieldBudgetType, rec.budgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, rec.name, settings.Name),
		model.CompareSettingsField(settingsFieldStatus, nil, settings.Status),
		model.CompareSettingsField(settingsFieldBudgetShared, nil, boolToStrPtr(settings.Shared)),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, settings.BiddingSchemeType),
	}
	rb.SummariseSettings()
	return rb, nil
}

// microsoftBudgetTypeFromLimit maps Microsoft's BudgetLimitType onto model.BudgetType. Both daily
// types are a DAILY budget (Accelerated changes pacing within the day, not the period). Anything
// else — including a padded or unrecognised spelling — is "", an absent side and an `unknown`
// verdict, never a guessed type.
func microsoftBudgetTypeFromLimit(t string) model.BudgetType {
	switch t {
	case microsoft.BudgetTypeDailyStandard, microsoft.BudgetTypeDailyAccelerated:
		return model.BudgetDaily
	case microsoft.BudgetTypeLifetimeStandard:
		return model.BudgetLifetime
	default:
		return ""
	}
}

// formatMicrosoftBudgetAmount renders a Microsoft DailyBudget (a decimal in account-currency
// units) the way formatBudgetUnits renders the row's NUMERIC(14,2) amount when it is whole cents,
// and at full precision when it is not — so an upstream 10.004 reads "10.004" and compares
// UNEQUAL to a recorded 10.00 rather than rounding into a fabricated match.
func formatMicrosoftBudgetAmount(v float64) string {
	if math.Round(v*100)/100 == v {
		return formatBudgetUnits(v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
