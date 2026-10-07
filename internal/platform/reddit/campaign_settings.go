// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// CampaignSettings is one campaign's live configuration as the settings readback reads it.
//
// The create path puts EVERYTHING the readback compares on the CAMPAIGN — name, the budget as
// goal_type/goal_value under is_campaign_budget_optimization=true, the flight as
// start_time/end_time, bid_strategy — so one campaign read answers every field and no ad group
// is read.
//
// Every optional field is a POINTER and nil means "Reddit did not report it", never zero.
type CampaignSettings struct {
	// CampaignID is the id Reddit ECHOED, already checked against the requested one.
	CampaignID string
	// AdAccountID is the account Reddit reports the campaign under, "" when not reported.
	AdAccountID      string
	Name             *string
	ConfiguredStatus *string
	GoalType         *string
	// GoalValueMicros is goal_value in micro-units of the account currency.
	GoalValueMicros *int64
	// CampaignBudgetOptimization is is_campaign_budget_optimization; nil when not reported. With
	// it off the spend is governed per ad group and goal_value is not the campaign's budget.
	CampaignBudgetOptimization *bool
	// StartTime / EndTime are the raw timestamps ("2026-08-01T00:00:00+00:00").
	StartTime   *string
	EndTime     *string
	BidStrategy *string
}

// campaignSettingsWire keeps every field a *string, *bool or json.RawMessage, so a decode error
// names a JSON kind and this struct's field, never an upstream value.
type campaignSettingsWire struct {
	ID               *string         `json:"id"`
	AdAccountID      string          `json:"ad_account_id"`
	Name             *string         `json:"name"`
	ConfiguredStatus *string         `json:"configured_status"`
	GoalType         *string         `json:"goal_type"`
	GoalValue        json.RawMessage `json:"goal_value"`
	CBO              *bool           `json:"is_campaign_budget_optimization"`
	StartTime        *string         `json:"start_time"`
	EndTime          *string         `json:"end_time"`
	BidStrategy      *string         `json:"bid_strategy"`
}

// GetCampaignSettings reads one campaign via GET /ad_accounts/{accountID}/campaigns/{id} — the
// SAME account-scoped resource the budget read, the adoption read and the status toggle address.
// It is a PURE READ, retried on 429 like every request() GET, so every failure is definite.
//
//   - a 404 → (nil, nil): Reddit answered and holds no such campaign in this account.
//   - anything unusable → an error: a transport failure, a 5xx, an exhausted 429, a 401/403, a
//     body identityjson refuses (checked over the RAW body, as the adoption read does, so a
//     duplicated "data" or id key cannot be resolved last-wins), a body that does not decode, an
//     id that is not the one asked for, and a goal_value that is present but not an integer.
//
// Unlike the adoption read, a DELETED or ARCHIVED campaign is NOT an absence here: the readback
// reports what the platform holds, and its configured_status is returned verbatim.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID string) (*CampaignSettings, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	path, campaignID, err := c.campaignBudgetPath(campaignID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("reddit campaign settings read for %s: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit campaign settings read for %s: the response carried no campaign", campaignID)
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, fmt.Errorf("reddit campaign settings read for %s: %w", campaignID, err)
	}
	var wire campaignSettingsWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("reddit campaign settings read for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("reddit campaign settings read for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	out := &CampaignSettings{
		CampaignID:                 campaignID,
		AdAccountID:                strings.TrimSpace(wire.AdAccountID),
		Name:                       wire.Name,
		ConfiguredStatus:           wire.ConfiguredStatus,
		GoalType:                   wire.GoalType,
		CampaignBudgetOptimization: wire.CBO,
		StartTime:                  wire.StartTime,
		EndTime:                    wire.EndTime,
		BidStrategy:                wire.BidStrategy,
	}
	var unparseable bool
	out.GoalValueMicros, unparseable = parseGoalValue(wire.GoalValue)
	if unparseable {
		return nil, fmt.Errorf("reddit campaign settings read for %s: the campaign reported a goal_value that is not an integer", campaignID)
	}
	return out, nil
}
