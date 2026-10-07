// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// metaGraphTimeLayout is the shape Graph renders an ad set's start_time / end_time in
// ("2026-08-01T00:00:00+0000", in the ad account's timezone). RFC 3339 is accepted as well, so
// a "Z" or "+00:00" rendering parses too; anything else is an absent side, never a raw string.
const metaGraphTimeLayout = "2006-01-02T15:04:05-0700"

// ReadSettings implements service.SettingsReader for Meta: it reads the campaign node and the ad
// set the create path made under it, and compares them against what the campaign row recorded.
//
// STRICTLY READ-ONLY. The upstream calls are GETs — the campaign, the recorded ad set, and (only
// when there is an amount to render) the ad account's currency. Nothing mutates the platform and
// nothing writes to the campaign row.
//
// FIELDS. The create path puts the budget, the flight and the bid strategy ON THE AD SET, so
// those are read from the ad set the row recorded:
//
//   - compared: campaign_name (campaign), budget_amount, budget_type, start_date, end_date (ad
//     set).
//   - upstream-only: status (the campaign's configured status — a different axis from the row's
//     lifecycle status, exactly as on Google) and bidding_strategy_type (the ad set's
//     bid_strategy; the create path always sends LOWEST_COST_WITHOUT_CAP and records nothing to
//     compare it to).
//
// An ADOPTED row records no ad set, and a Campaign Budget Optimization campaign holds no ad-set
// budget; both leave the ad-set fields ABSENT and `unknown` rather than failing the read. The
// campaign-level CBO budget is deliberately not substituted: it is shared across ad sets, so it
// is not the counterpart of the ad-set budget the row recorded.
//
// UNITS. Meta reports budgets in the account currency's MINOR units, and the create path wrote
// round(budget × offset). Both sides are therefore reduced to minor units with that same offset —
// read from the account's own currency, the authority the create path used — and rendered back
// to whole units by one function, so a JPY row recorded as 1000.40 and created as 1000 compares
// as the 1000 the create path actually sent. A currency outside the supported map leaves the
// amount `unknown` rather than guessing a scale.
//
// DATES. The create path sends end_time = endDate + "T23:59:59+0000" and start_time = the start
// day at 00:00 UTC, nudged forward to dispatch time + a buffer when that day has already begun.
// Graph echoes them in the ad account's timezone, so both are compared as UTC calendar dates; the
// start goes through compareNudgedStart so a nudge across UTC midnight is not reported as a
// divergence.
//
// ABSENCE. This readback never reports the campaign absent (404). Graph code 100 / subcode 33 on
// the campaign — "does not exist, cannot be loaded due to missing permissions, or does not support
// this operation" — cannot tell a deleted campaign from one this token cannot see, so it is
// unverifiable (503) on every HTTP status, as on the adoption read. A campaign Meta has DELETED
// or ARCHIVED still answers with that status and is reported as status, not as absent.
//
// PROVENANCE. Unknown provenance fails closed before any credential is resolved (409); a recorded
// account that differs from the connection's is a mismatch (409). GET /{id} is NOT account-scoped,
// so the account Meta reports the campaign under is ALSO compared with the recorded one — the
// read's own proof that the id named this account's campaign — and an ad set the row recorded
// that belongs to another campaign upstream is refused too — both with
// ErrCampaignUpstreamIdentityMismatch (409), not the account mismatch: the connection already IS
// the recorded account, so the remedy is to re-dispatch, not to reconnect.
func (d *MetaDispatcher) ReadSettings(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.CampaignSettingsReadback, error) {
	created := metaCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("read meta campaign settings: campaign %s does not record which ad account it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return nil, err
	}
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return nil, fmt.Errorf("read meta campaign settings: %w", err)
	}
	if err := verifyMetaAccountMatch("read meta campaign settings", campaign, accountID); err != nil {
		return nil, err
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID, metaAdSetID(campaign))
	if err != nil {
		if errors.Is(err, meta.ErrAdSetNotInCampaign) {
			return nil, fmt.Errorf("read meta campaign settings: the ad set recorded for campaign %s belongs to a different campaign upstream, so its configuration is not this campaign's; re-dispatch the campaign to repair the recorded ad set: %w",
				campaign.PlatformCampaignID, domain.ErrCampaignUpstreamIdentityMismatch)
		}
		return nil, fmt.Errorf("read meta campaign settings: %w", err)
	}
	if settings == nil {
		// GetCampaignSettings never reports absence (Graph 100/33 is unverifiable, and a deleted
		// or archived campaign answers with its status), so a nil here is a contract break, not
		// a 404: it is unverifiable like every other read failure.
		return nil, fmt.Errorf("read meta campaign settings: campaign %s: the read returned no campaign and no error", campaign.PlatformCampaignID)
	}
	if settings.AccountID != created {
		return nil, fmt.Errorf("read meta campaign settings: campaign %s is reported under ad account %s, not the account %s it was created under: %w",
			campaign.PlatformCampaignID, settings.AccountID, created, domain.ErrCampaignUpstreamIdentityMismatch)
	}

	rec := recordedSettings(campaign)
	recordedBudget := rec.budget
	var upstreamBudget, upstreamBudgetType, upstreamEnd, upstreamBid *string
	var upstreamStart *time.Time
	if as := settings.AdSet; as != nil {
		upstreamBid = as.BidStrategy
		var minor *int64
		switch {
		case as.DailyMinor != nil:
			upstreamBudgetType = strPtr(string(model.BudgetDaily))
			minor = as.DailyMinor
		case as.LifetimeMinor != nil:
			upstreamBudgetType = strPtr(string(model.BudgetLifetime))
			minor = as.LifetimeMinor
		}
		if minor != nil {
			offset, known, oerr := client.AccountCurrencyOffset(ctx)
			if oerr != nil {
				return nil, fmt.Errorf("read meta campaign settings: %w", oerr)
			}
			if known {
				upstreamBudget = strPtr(formatMinorUnits(*minor, offset))
				if campaign.BudgetAmount != nil {
					recordedBudget = strPtr(formatMinorUnits(metaRecordedMinor(*campaign.BudgetAmount, offset), offset))
				}
			}
		}
		upstreamStart = parseSettingsTimestamp(as.StartTime, metaGraphTimeLayout, time.RFC3339)
		upstreamEnd = settingsUTCDate(parseSettingsTimestamp(as.EndTime, metaGraphTimeLayout, time.RFC3339))
	}

	startField := model.CompareSettingsField(settingsFieldStartDate, rec.start, settingsUTCDate(upstreamStart))
	if upstreamStart != nil && campaign.StartDate != nil {
		startField = compareNudgedStart(campaign.StartDate, upstreamStart, campaign.CreatedAt)
	}

	rb := newSettingsReadback(campaign, platform, settings.CampaignID, settingsNow(d.settingsNow))
	rb.Fields = []model.CampaignSettingsField{
		model.CompareSettingsField(settingsFieldBudgetAmount, recordedBudget, upstreamBudget),
		model.CompareSettingsField(settingsFieldBudgetType, rec.budgetType, upstreamBudgetType),
		model.CompareSettingsField(settingsFieldName, rec.name, settings.Name),
		model.CompareSettingsField(settingsFieldStatus, nil, settings.Status),
		startField,
		model.CompareSettingsField(settingsFieldEndDate, rec.end, upstreamEnd),
		model.CompareSettingsField(settingsFieldBiddingStrategy, nil, upstreamBid),
	}
	rb.SummariseSettings()
	return rb, nil
}

// metaRecordedMinor encodes a recorded whole-unit amount exactly as the create path's
// budgetToMinorUnits did — round(amount × offset) — so the recorded side is compared as the
// integer Meta was actually sent.
func metaRecordedMinor(amount float64, offset int64) int64 {
	return int64(math.Round(amount * float64(offset)))
}

// formatMinorUnits renders a minor-unit amount as whole units with exactly two decimal places,
// using integer arithmetic only. Meta's supported offsets are 1 and 100 (currencyMinorUnitOffset),
// both of which divide 100, so two places are always exact.
func formatMinorUnits(minor, offset int64) string {
	if offset <= 0 || 100%offset != 0 {
		// Unreachable with the supported map; render the raw minor units rather than a guess.
		return strconv.FormatInt(minor, 10)
	}
	cents := minor * (100 / offset)
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := strconv.FormatInt(cents/100, 10) + "." + fmt.Sprintf("%02d", cents%100)
	if neg {
		s = "-" + s
	}
	return s
}
