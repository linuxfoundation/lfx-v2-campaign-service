// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ErrLineItemNotInCampaign reports that the line item the campaign row recorded is, upstream, a
// line item of a DIFFERENT campaign. Reading its flight as this campaign's would report another
// campaign's configuration as this one's divergence, so the dispatcher refuses it.
var ErrLineItemNotInCampaign = errors.New("twitter: the recorded line item belongs to a different campaign")

// CampaignSettings is one campaign's live configuration as the settings readback reads it, from
// the campaign and — when the row recorded one — the line item the create path made under it.
//
// Every optional field is a POINTER and nil means "X did not report it", never zero. Amounts are
// in MICRO-units of the ad account's currency.
type CampaignSettings struct {
	// CampaignID is the id X ECHOED, already checked against the requested one.
	CampaignID string
	// AccountID is the account the campaign object reports, "" when it reports none.
	AccountID    string
	Name         *string
	EntityStatus *string
	// BudgetOptimization is the raw budget_optimization (CAMPAIGN or LINE_ITEM).
	BudgetOptimization *string
	DailyMicros        *int64
	TotalMicros        *int64
	// LineItem is the recorded line item's configuration; nil when no line item id was supplied,
	// or when X reports it absent (404) or deleted.
	LineItem *LineItemSettings
}

// LineItemSettings is the configuration the create path writes onto the line item: the flight
// (X takes it on the line item, never on the campaign) and the bid strategy.
type LineItemSettings struct {
	ID string
	// StartTime / EndTime are the raw timestamps ("2026-08-01T00:00:00Z").
	StartTime   *string
	EndTime     *string
	BidStrategy *string
}

type campaignSettingsWire struct {
	ID                 *string         `json:"id"`
	AccountID          string          `json:"account_id"`
	Name               *string         `json:"name"`
	EntityStatus       *string         `json:"entity_status"`
	BudgetOptimization *string         `json:"budget_optimization"`
	Daily              json.RawMessage `json:"daily_budget_amount_local_micro"`
	Total              json.RawMessage `json:"total_budget_amount_local_micro"`
	Deleted            *bool           `json:"deleted"`
}

type lineItemSettingsWire struct {
	ID          *string `json:"id"`
	CampaignID  *string `json:"campaign_id"`
	StartTime   *string `json:"start_time"`
	EndTime     *string `json:"end_time"`
	BidStrategy *string `json:"bid_strategy"`
	Deleted     *bool   `json:"deleted"`
}

// GetCampaignSettings reads the campaign via GET accounts/:account_id/campaigns/:id — the same
// account-scoped resource the budget read, adoption read and toggle address — and, when
// lineItemID is non-empty, that line item via GET accounts/:account_id/line_items/:id. Both are
// PURE READS retried on 429, so every failure is definite.
//
//   - a 404 on the campaign, or a campaign X reports deleted → (nil, nil).
//   - a 404 on the line item, or a deleted line item → the campaign is returned with LineItem nil:
//     only the recorded child is gone, so its fields are absent rather than the read failing.
//   - a line item whose campaign_id is not this campaign → ErrLineItemNotInCampaign.
//   - anything else unusable is an error: a transport failure, a 5xx, an exhausted 429, a
//     401/403, a body identityjson refuses (checked over the RAW body, so a duplicated "data" or
//     id key cannot be resolved last-wins), a body that does not decode, an id that is not the one
//     asked for, and a budget amount that is present but not a non-negative integer.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID, lineItemID string) (*CampaignSettings, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	path, campaignID, err := c.campaignBudgetPath(campaignID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("x ads campaign settings read for %s: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the response carried no campaign", campaignID)
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, fmt.Errorf("x ads campaign settings read for %s: %w", campaignID, err)
	}
	var wire campaignSettingsWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	if wire.Deleted != nil && *wire.Deleted {
		return nil, nil
	}
	out := &CampaignSettings{
		CampaignID:         campaignID,
		AccountID:          strings.TrimSpace(wire.AccountID),
		Name:               wire.Name,
		EntityStatus:       wire.EntityStatus,
		BudgetOptimization: wire.BudgetOptimization,
	}
	var dailyBad, totalBad bool
	out.DailyMicros, dailyBad = parseBudgetMicros(wire.Daily)
	out.TotalMicros, totalBad = parseBudgetMicros(wire.Total)
	if dailyBad || totalBad {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the campaign reported a budget amount that is not a non-negative integer", campaignID)
	}

	if strings.TrimSpace(lineItemID) == "" {
		return out, nil
	}
	liPath, lineItemID, err := c.lineItemPath(lineItemID)
	if err != nil {
		return nil, fmt.Errorf("x ads campaign settings read for %s: %w", campaignID, err)
	}
	liResp, err := c.request(ctx, http.MethodGet, liPath+"?with_deleted=true")
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return out, nil
		}
		return nil, fmt.Errorf("x ads campaign settings read for %s: line item %s: %w", campaignID, lineItemID, err)
	}
	if liResp == nil || len(liResp.Data) == 0 || string(liResp.Data) == "null" {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the line item response carried no line item", campaignID)
	}
	if err := identityjson.Check(liResp.raw); err != nil {
		return nil, fmt.Errorf("x ads campaign settings read for %s: line item %s: %w", campaignID, lineItemID, err)
	}
	var li lineItemSettingsWire
	if err := json.Unmarshal(liResp.Data, &li); err != nil {
		return nil, fmt.Errorf("x ads campaign settings read for %s: line item %s: the response is not a line item object", campaignID, lineItemID)
	}
	if li.ID == nil || *li.ID != lineItemID {
		return nil, fmt.Errorf("x ads campaign settings read for %s: the line item response does not describe line item %s, so nothing in it can be trusted", campaignID, lineItemID)
	}
	if li.CampaignID == nil || *li.CampaignID != campaignID {
		return nil, fmt.Errorf("x ads campaign settings read for %s: line item %s: %w", campaignID, lineItemID, ErrLineItemNotInCampaign)
	}
	if li.Deleted != nil && *li.Deleted {
		return out, nil
	}
	out.LineItem = &LineItemSettings{ID: lineItemID, StartTime: li.StartTime, EndTime: li.EndTime, BidStrategy: li.BidStrategy}
	return out, nil
}
