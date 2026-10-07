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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// ReadSettings implements service.SettingsReader for X (Twitter) Ads: it reads the campaign and
// the line item the create path made under it, and compares them against what the campaign row
// recorded.
//
// STRICTLY READ-ONLY. The upstream calls are two account-scoped GETs; nothing here mutates the
// platform and nothing writes to the campaign row.
//
// FIELDS. The budget lives on the CAMPAIGN; the flight and bid strategy live on the LINE ITEM (X
// rejects start_time/end_time on a campaign create, so the create path sends them on the line
// item).
//
//   - compared: budget_amount, budget_type, campaign_name (campaign), start_date, end_date (line
//     item).
//   - upstream-only: status (entity_status — a different axis from the row's lifecycle status,
//     exactly as on Google), bidding_strategy_type (the line item's bid_strategy; the create
//     path always sends AUTO and records nothing to compare it to) and budget_optimization.
//
// An ADOPTED row records no line item, so its flight and bid strategy are ABSENT and `unknown`;
// the campaign-level fields still read normally.
//
// The budget is compared ONLY when X REPORTS budget_optimization CAMPAIGN; LINE_ITEM, an
// unreported value or anything else leaves both budget fields `unknown`. This is deliberate and
// not assumed away for campaigns this service created: the create path sends no
// budget_optimization (X's current reference lists LINE_ITEM as the only POST value, so sending
// CAMPAIGN would be undocumented), and which value X then reports is unverified — its v11
// announcement says CAMPAIGN is the default, its current reference says LINE_ITEM. A created
// campaign whose budget reads `unknown` with budget_optimization absent or LINE_ITEM is therefore
// the readback declining to guess, not a read failure.
//
// UNITS. X budgets are MICRO-units of the ad account's currency, the unit the create path converts
// the caller's daily budgetAmount into, so they are rendered exactly as Google's micros are. The
// create path sets only daily_budget_amount_local_micro, so a campaign reporting a daily amount is
// daily and that amount is compared; one reporting only a total cap is lifetime and the total is
// compared. A campaign reporting neither has both budget fields absent.
//
// DATES. The create path sends start_time/end_time as "<day>T00:00:00Z" with no nudge (a start too
// soon is refused before anything is created), so both are compared as plain UTC calendar dates.
//
// PROVENANCE. Unknown provenance fails closed before any credential is resolved (409); a recorded
// account that differs from the connection's is a mismatch (409). Both reads are account-scoped by
// their path; an account_id in the campaign answer naming a different account, and a recorded line
// item that belongs to another campaign, are refused with ErrCampaignUpstreamIdentityMismatch (409)
// rather than reported: the connection already IS the recorded account, so the remedy is to
// re-dispatch, not to reconnect.
func (d *TwitterDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	created := twitterCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read x ads campaign settings: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, res, err := d.resolveTwitterClientWithRes(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	if err := verifyTwitterAccountMatch("read x ads campaign settings", campaign, client); err != nil {
		return nil, err
	}

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID, twitterChildIDs(campaign))
	if err != nil {
		switch {
		case errors.Is(err, twitter.ErrInvalidAccountID):
			return nil, res.systemScoped(fmt.Errorf("%w: %w: read x ads campaign settings: the connection's ad account id cannot address an x ads request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, err))
		case errors.Is(err, twitter.ErrLineItemNotInCampaign):
			return nil, fmt.Errorf("read x ads campaign settings: the line item recorded for campaign %s belongs to a different campaign upstream, so its configuration is not this campaign's; re-dispatch the campaign to repair the recorded line item: %w",
				campaign.PlatformCampaignID, domain.ErrCampaignUpstreamIdentityMismatch)
		}
		return nil, fmt.Errorf("read x ads campaign settings: %w", err)
	}
	if settings == nil {
		return nil, fmt.Errorf("%w: x ads campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}
	if settings.AccountID != "" && settings.AccountID != strings.TrimSpace(client.AccountID()) {
		return nil, fmt.Errorf("read x ads campaign settings: campaign %s is reported under ad account %s, not the connection's account %s: %w",
			campaign.PlatformCampaignID, settings.AccountID, client.AccountID(), domain.ErrCampaignUpstreamIdentityMismatch)
	}

	rec := recordedSettings(campaign)
	// The campaign-level amounts are the campaign's budget only under CAMPAIGN budget
	// optimization. Under LINE_ITEM (or an unreported model) each line item governs its own
	// spend, and a campaign-level total cap compared against the recorded daily amount would be
	// two different quantities, so both budget fields stay `unknown`; budget_optimization is
	// reported upstream-only to say why.
	var upstreamBudget, upstreamBudgetType *string
	campaignBudget := settings.BudgetOptimization != nil && *settings.BudgetOptimization == twitter.BudgetOptimizationCampaign
	switch {
	case !campaignBudget:
	case settings.DailyMicros != nil:
		upstreamBudgetType = strPtr(string(model.BudgetDaily))
		upstreamBudget = settingsMicrosToUnits(settings.DailyMicros)
	case settings.TotalMicros != nil:
		upstreamBudgetType = strPtr(string(model.BudgetLifetime))
		upstreamBudget = settingsMicrosToUnits(settings.TotalMicros)
	}
	var upstreamStart, upstreamEnd, upstreamBid *string
	if li := settings.LineItem; li != nil {
		upstreamStart = settingsUTCDate(parseSettingsTimestamp(li.StartTime, time.RFC3339))
		upstreamEnd = settingsUTCDate(parseSettingsTimestamp(li.EndTime, time.RFC3339))
		upstreamBid = li.BidStrategy
	}

	rb := newSettingsReadback(campaign, platform, settings.CampaignID, settingsNow(d.settingsNow))
	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, rec.budget, upstreamBudget),
		model.CompareSettingsField(settingsFieldBudgetType, rec.budgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, rec.name, settings.Name),
		model.CompareSettingsField(settingsFieldStatus, nil, settings.EntityStatus),
		model.CompareSettingsField(settingsFieldStartDate, rec.start, upstreamStart),
		model.CompareSettingsField(settingsFieldEndDate, rec.end, upstreamEnd),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, upstreamBid),
		model.CompareSettingsField(settingsFieldBudgetOptimization, nil, settings.BudgetOptimization),
	}
	rb.SummariseSettings()
	return rb, nil
}
