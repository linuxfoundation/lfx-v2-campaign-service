// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// This file holds the pieces of the settings readback (service.SettingsReader) that the
// Microsoft, Meta, Reddit and X readers share. Google Ads' reader predates them and keeps its
// own copies in googleads.go; the rules here are the same rules, stated once for the four so
// they cannot drift from one another:
//
//   - A side that was not read is ABSENT (nil), never a zero value — model.CompareSettingsField
//     then answers `unknown`, never `match`.
//   - Both sides of a comparison are rendered by the SAME function, so equal values cannot
//     compare unequal over formatting and unequal values cannot compare equal over rounding.
//   - The recorded name is carried VERBATIM (an all-blank one is unrecorded), exactly as Google's
//     reader does, so a whitespace difference is a divergence rather than a fabricated match.

// The field names (settingsFieldBudgetAmount, ...) are declared once, in googleads.go, and shared
// with the Google reader so every platform reports in the same vocabulary.

// Upstream-only fields only these readers report. Each says WHERE a platform holds the budget,
// which is what explains a budget field reading `unknown` when the campaign-level amount is not
// the one the row recorded.
const (
	// settingsFieldCampaignBudgetOptimization is Reddit's is_campaign_budget_optimization.
	settingsFieldCampaignBudgetOptimization = "is_campaign_budget_optimization"
	// settingsFieldBudgetOptimization is X's budget_optimization (CAMPAIGN or LINE_ITEM).
	settingsFieldBudgetOptimization = "budget_optimization"
)

// settingsRecorded is the campaign row's recorded side of a readback, shaped once.
type settingsRecorded struct {
	budget     *string
	budgetType *string
	name       *string
	start      *string
	end        *string
}

// recordedSettings renders what the campaign ROW holds — what the dispatch asked for — into the
// readback's string vocabulary. A NULL column stays nil, so an adopted row (which records no
// budget and no window on these platforms) reports those fields `unknown`, not `diverged`.
func recordedSettings(c *model.Campaign) settingsRecorded {
	var r settingsRecorded
	if c.BudgetAmount != nil {
		r.budget = strPtr(formatBudgetUnits(*c.BudgetAmount))
	}
	if c.BudgetType != nil {
		r.budgetType = strPtr(string(*c.BudgetType))
	}
	if strings.TrimSpace(c.CampaignName) != "" {
		r.name = strPtr(c.CampaignName)
	}
	r.start = settingsUTCDate(c.StartDate)
	r.end = settingsUTCDate(c.EndDate)
	return r
}

// settingsUTCDate renders a time as the YYYY-MM-DD the campaign row stores, in UTC. Every
// platform below receives its flight from this service as a UTC instant (midnight, or 23:59:59
// for Meta's end), so the UTC calendar date of what the platform echoes is the like-for-like
// counterpart of the recorded date — whatever timezone the platform chooses to render it in.
func settingsUTCDate(t *time.Time) *string {
	if t == nil {
		return nil
	}
	return strPtr(t.UTC().Format(campaignDateLayout))
}

// parseSettingsTimestamp parses an upstream flight timestamp STRICTLY against the layouts the
// platform documents. A value that parses against none of them is ABSENT (nil) — never passed
// through raw, because a date-only "2026-08-01" would then compare byte-equal to the recorded
// date and fabricate a match out of a value nobody validated (the defect googleAdsDateOnly
// documents).
func parseSettingsTimestamp(s *string, layouts ...string) *time.Time {
	if s == nil {
		return nil
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, *s); err == nil {
			return &t
		}
	}
	return nil
}

// settingsMicrosToUnits renders an upstream amount in MICRO-units of the account currency as the
// whole-unit string the row's budget_amount compares against, exactly as Google's reader renders
// its micros: at the row's two decimal places when the amount is whole cents, and at FULL
// precision otherwise, so a sub-cent upstream amount can never round into equality with the
// recorded one. Integer arithmetic decides which; formatBudgetUnits and formatSubCentBudgetUnits
// are the same renderers the Google reader uses.
func settingsMicrosToUnits(micros *int64) *string {
	if micros == nil {
		return nil
	}
	if *micros%microsPerCent != 0 {
		return strPtr(formatSubCentBudgetUnits(*micros))
	}
	return strPtr(formatBudgetUnits(float64(*micros) / microsPerBudgetUnit))
}

// compareNudgedStart compares a flight START on a platform whose create path nudges a start
// that has already begun forward to "dispatch time + a small buffer" (Meta's adSetStartTime,
// Reddit's effectiveStart).
//
// A plain date comparison is right almost always: the nudged instant usually falls on the same
// UTC day as the requested one. But when it does not — a dispatch just before UTC midnight, or a
// start date already past at dispatch — the platform holds a LATER day than the row records
// because this service's own create path put it there. Reporting that as `diverged` would send
// an operator after a campaign that is set exactly as this service made it.
//
// So a later upstream day that is the row's creation day or the day after — the only window the
// nudge (dispatch time + a buffer of minutes) can produce — is reported with BOTH values and an `unknown` verdict: the readback
// cannot tell a nudge from a later edit, and says so. Everything else keeps the ordinary verdict:
// an EARLIER upstream day, or a later one far from the dispatch, is a real divergence the nudge
// cannot explain. A row with no CreatedAt cannot bound the window, so any later day is `unknown`.
func compareNudgedStart(recorded, upstream *time.Time, createdAt time.Time) model.CampaignSettingsField {
	rec, up := settingsUTCDate(recorded), settingsUTCDate(upstream)
	f := model.CompareSettingsField(settingsFieldStartDate, rec, up)
	if f.Comparison != model.SettingsDiverged {
		return f
	}
	recDay := utcDay(*recorded)
	upDay := utcDay(*upstream)
	if !upDay.After(recDay) {
		return f
	}
	if createdAt.IsZero() {
		return model.UncomparableSettingsField(settingsFieldStartDate, rec, up)
	}
	created := utcDay(createdAt)
	if !upDay.Before(created) && !upDay.After(created.AddDate(0, 0, 1)) {
		return model.UncomparableSettingsField(settingsFieldStartDate, rec, up)
	}
	return f
}

// utcDay truncates t to the start of its UTC calendar day.
func utcDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// settingsNow returns the readback's ReadAt clock: the injected one when set, else the wall
// clock, always in UTC.
func settingsNow(now func() time.Time) time.Time {
	if now != nil {
		return now().UTC()
	}
	return time.Now().UTC()
}

// newSettingsReadback starts a readback for campaign, with the id the PLATFORM echoed.
func newSettingsReadback(campaign *model.Campaign, platform model.Provider, echoedID string, readAt time.Time) *model.CampaignSettingsReadback {
	return &model.CampaignSettingsReadback{
		CampaignID:         campaign.ID,
		PlatformCampaignID: echoedID,
		Platform:           platform,
		ReadAt:             readAt,
	}
}
