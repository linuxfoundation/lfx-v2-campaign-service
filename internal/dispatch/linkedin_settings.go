// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// Upstream-only readback fields only LinkedIn reports. The bid's setup is three LinkedIn fields
// (optimizationTargetType decides manual vs automated, costType the charge unit, unitCost the
// amount), so all three are shown: bidding_strategy_type alone cannot say whether a manual bid
// is per click.
const (
	settingsFieldCostType = "cost_type"
	settingsFieldUnitCost = "unit_cost"
	// settingsFieldUnitCostCurrency is unitCost.currencyCode: unit_cost is never shown without it.
	settingsFieldUnitCostCurrency = "unit_cost_currency"
	settingsFieldObjectiveType    = "objective_type"
	// settingsFieldAudienceExpansion and settingsFieldOffsiteDelivery are audienceExpansionEnabled
	// and offsiteDeliveryEnabled (#667), "true" or "false" as LinkedIn reports them.
	settingsFieldAudienceExpansion = "audience_expansion_enabled"
	settingsFieldOffsiteDelivery   = "offsite_delivery_enabled"
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
//     amount, in whole units of its currency), unit_cost_currency (that currency — unit_cost is
//     absent whenever it is not reported). The create path records none of these, so each
//     reads `unknown`. audience_expansion_enabled and offsite_delivery_enabled (#667) are
//     upstream-only too, "true" or "false" and absent when LinkedIn does not report them.
//   - objective_type is COMPARED since LFXV2-2665: the create path's objective follows the
//     bidding strategy it records (manual → WEBSITE_CONVERSION, or the WEBSITE_VISIT a manual
//     traffic create recorded in the result blob, #675; automatic → the objectiveType the
//     create recorded in the result blob, #582, and WEBSITE_VISIT for a row written before it;
//     linkedinRecordedObjective), so a row with a recorded strategy has a recorded objective. A
//     row without one (legacy, adopted) reads `unknown`, never a guessed objective.
//
// BUDGET. The create path sets dailyBudget or totalBudget in USD, or totalBudget plus a
// dailyBudget cap when linkedInConfig.dailyCapUsd is set (#584). A campaign reporting one is
// compared as that type. One reporting BOTH (a lifetime budget with a daily cap, which the
// row's single recorded amount cannot describe) or NEITHER (e.g. under a campaign group's DYNAMIC
// budget optimization, where the group's budget is shared) leaves both budget fields `unknown`;
// so does a non-USD or
// unreadable amount. Amounts are read EXACTLY as micro-units — never through a float — and
// rendered by the same function as every other reader, so a sub-cent amount cannot round into
// equality with the recorded one.
//
// DATES. The create path sends runSchedule.start as the start day at 00:00 UTC, nudged to
// "now + buffer" when that day has begun (toMs), and runSchedule.end as 23:59:59.999 UTC of the
// end day — except on a two-budget campaign (#584), sent with no end, whose end_date therefore
// reads `unknown`; its campaign group carries the end. Both are compared as UTC dates; the start
// goes through compareNudgedStart.
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
	// The stored id is validated VERBATIM before anything else: " 777" is not campaign 777, and
	// a trimmed read would describe a campaign the row does not name.
	if err := linkedin.ValidateCampaignID(campaign.PlatformCampaignID); err != nil {
		return nil, fmt.Errorf("read linkedin campaign settings: the campaign row's platform campaign id is not a canonical LinkedIn campaign id, so it names no campaign this read may describe; re-dispatch the campaign: %w: %w",
			err, domain.ErrCampaignUpstreamIdentityMismatch)
	}
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
	if !linkedin.ValidAccountID(accountID) {
		return nil, linkedinAccountIDUnusable(res, projectID, "read linkedin campaign settings")
	}
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
	// The bid amount is shown ONLY with the currency LinkedIn reported for it: unitCost is in
	// the ad account's currency (an adopted campaign may bid in EUR), so a bare number would read
	// the same for 5 USD and 5 EUR. No currency → no amount, never a unitless figure.
	var unitCost, unitCostCurrency *string
	if cur := strings.TrimSpace(settings.UnitCostCurrency); cur != "" {
		unitCostCurrency = strPtr(cur)
		if !settings.UnitCostUnparseable {
			unitCost = settingsMicrosToUnits(settings.UnitCostMicros)
		}
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
		model.CompareSettingsField(settingsFieldUnitCostCurrency, nil, unitCostCurrency),
		model.CompareSettingsField(settingsFieldObjectiveType, linkedinRecordedObjective(ctx, campaign), settings.ObjectiveType),
		model.CompareSettingsField(settingsFieldAudienceExpansion, nil, boolToStrPtr(settings.AudienceExpansionEnabled)),
		model.CompareSettingsField(settingsFieldOffsiteDelivery, nil, boolToStrPtr(settings.OffsiteDeliveryEnabled)),
	}
	rb.SummariseSettings()
	return rb, nil
}

// linkedinAccountIDUnusable is the refusal for a connection whose stored ad account id is not a
// positive int64 (linkedin.ValidAccountID): a permanent defect on the row, decided before anything
// is sent — the "repair the connection" answer, attributed to the LF system row when it came from
// there, never a 503 that promises a retry could help.
func linkedinAccountIDUnusable(res *resolved, projectID, op string) error {
	return res.systemScoped(fmt.Errorf("%w: %w: %s: the linkedin connection for project %s stores an ad account id that is not a positive 64-bit integer",
		domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, op, projectID))
}

// linkedinRecordedObjective is the objectiveType the create path sent for this row's recorded
// bidding strategy (LFXV2-2665; linkedin.campaignBidding): manual → WEBSITE_CONVERSION, unless the
// row's result blob records WEBSITE_VISIT with target NONE (a manual traffic create, #675); automatic →
// the objectiveType the create recorded in the row's result blob (#582: WEBSITE_CONVERSION for
// Maximum delivery conversions, LEAD_GENERATION for a lead gen create (#664), WEBSITE_VISIT
// otherwise). nil when no strategy is recorded — a
// legacy or adopted row — or when an automatic row's result cannot say, so the field reads
// `unknown` rather than comparing against a guess (linkedinResultObjective).
func linkedinRecordedObjective(ctx context.Context, campaign *model.Campaign) *string {
	if campaign == nil || campaign.BiddingStrategy == nil {
		return nil
	}
	switch *campaign.BiddingStrategy {
	case model.BiddingAutomatic:
		return linkedinResultObjective(ctx, campaign)
	case model.BiddingManual:
		// Every manual create before #675 was WEBSITE_CONVERSION, and so is every manual create
		// without objective "traffic" since; only the result blob (which a caller cannot write, see
		// linkedinResultObjective) recording the manual traffic create — WEBSITE_VISIT with the
		// manual target NONE — says otherwise. A blob with no objectiveType, one that does not
		// decode, or one whose target contradicts the manual strategy keeps the pre-#675 answer.
		var blob struct {
			ObjectiveType      string `json:"objectiveType"`
			OptimizationTarget string `json:"optimizationTarget"`
		}
		if len(campaign.Result) > 0 && json.Unmarshal(campaign.Result, &blob) == nil &&
			blob.ObjectiveType == linkedin.ObjectiveWebsiteVisit && blob.OptimizationTarget == linkedin.OptimizationTargetNone {
			return strPtr(linkedin.ObjectiveWebsiteVisit)
		}
		return strPtr(linkedin.ObjectiveWebsiteConversion)
	}
	return nil
}

// linkedinResultObjective reads the objectiveType an automatic row was created with from its
// Result blob (linkedin.CampaignResult.ObjectiveType, #582).
//
// The result blob, not the config snapshot, because only this service writes it: UpdateCampaign
// replaces config_snapshot with whatever JSON the caller sends, so any key in the snapshot —
// `objective` or a marker of authorship — can be forged, and a forged snapshot would fabricate a
// match or a divergence (PR #637 review). Result is never caller-writable.
//
// A blob with no objectiveType was written before #582 recorded it. Before #582 the only
// objective an automatic create sent was WEBSITE_VISIT, so that is what such a row was created
// with. An empty or undecodable blob, or an objectiveType outside the four this service creates
// (ENGAGEMENT only for a document create, #665),
// is nil: nothing trustworthy was recorded.
func linkedinResultObjective(ctx context.Context, campaign *model.Campaign) *string {
	if len(campaign.Result) == 0 {
		return nil
	}
	var blob struct {
		ObjectiveType *string `json:"objectiveType"`
	}
	if err := json.Unmarshal(campaign.Result, &blob); err != nil {
		// Not an error for the caller: the readback still reports every other field.
		slog.WarnContext(ctx, "linkedin result blob could not be decoded; objective_type has no recorded side",
			"campaign_id", campaign.ID, "error", err)
		return nil
	}
	if blob.ObjectiveType == nil {
		return strPtr(linkedin.ObjectiveWebsiteVisit)
	}
	switch *blob.ObjectiveType {
	case linkedin.ObjectiveWebsiteVisit, linkedin.ObjectiveWebsiteConversion, linkedin.ObjectiveLeadGeneration, linkedin.ObjectiveEngagement:
		return strPtr(*blob.ObjectiveType)
	}
	slog.WarnContext(ctx, "linkedin result blob records an objectiveType this service does not create; objective_type has no recorded side",
		"campaign_id", campaign.ID)
	return nil
}
