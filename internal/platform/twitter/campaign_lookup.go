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

// StatusDraft is X's entity_status for a campaign that exists but was never launched. It is a
// real campaign in the account — adoptable, though not serving.
const StatusDraft = "DRAFT"

// CampaignRef is what the adoption read learns about one campaign.
type CampaignRef struct {
	// ID is the id X ECHOED, already checked against the requested one.
	ID   string
	Name string
	// Status is entity_status — ACTIVE, PAUSED or DRAFT here, never anything else.
	Status string
	// AccountID is the account the campaign object reports, "" when it reports none. The read is
	// account-scoped by its path, so the caller compares this only when it is present.
	AccountID string
}

// campaignLookupWire is the subset of an X campaign the adoption read decodes. Only string,
// *string and *bool kinds, so a decode error can name a JSON kind and this struct's field, never
// an upstream value — and the caller drops even that.
type campaignLookupWire struct {
	ID           *string `json:"id"`
	Name         *string `json:"name"`
	EntityStatus *string `json:"entity_status"`
	AccountID    string  `json:"account_id"`
	Deleted      *bool   `json:"deleted"`
}

// GetCampaign reads one campaign for adoption via GET accounts/:account_id/campaigns/:campaign_id
// (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns") — the same
// account-scoped resource the budget read and the toggle address. It is a PURE READ, retried on
// 429 like every request() GET.
//
// The outcomes are the CampaignAdopter contract's:
//
//   - a live campaign (entity_status ACTIVE, PAUSED or DRAFT) -> (ref, nil)
//   - a 404, or a campaign X reports as deleted              -> (nil, nil)
//   - anything unverifiable                                  -> (nil, error)
//
// The 404 is matched on the HTTP status, never on body text. "Unverifiable" is everything else:
// a transport failure, a 5xx, an exhausted or over-long 429, a 401/403, a body identityjson
// refuses or that does not decode, an id that is not the one asked for, a missing name, and an
// entity_status outside the documented set.
//
// PROVENANCE. The path names the connection's account, so X resolves the id inside it. X's
// campaign object is not documented to carry the account id (GetCampaignBudget relies on the
// path scoping for the same reason); when a response does carry one it is handed to the caller
// to cross-check rather than ignored (see TwitterDispatcher.LookupCampaign).
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
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
		// A bare 404, judged on the status alone. That is also how X may answer a path
		// whose AD ACCOUNT is inaccessible or revoked, which would then read as "absent" too.
		// The harm is low: the connection's own account is in the path, so any dispatch to it
		// — the duplicate a false absence could invite — would fail the same way rather than
		// create a campaign. If X ever documents a structured error code that tells "no
		// such campaign" from "no such account", match on that code instead of the status.
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("x ads campaign lookup for %s: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("x ads campaign lookup for %s: the response carried no campaign", campaignID)
	}
	// The guard runs over the RAW response body, not resp.Data: the envelope has already been
	// decoded, and a duplicated "data" key (or "data" beside "Data") was resolved last-wins on
	// the way into Data — invisible to a check of Data alone.
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, fmt.Errorf("x ads campaign lookup for %s: %w", campaignID, err)
	}
	var wire campaignLookupWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("x ads campaign lookup for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("x ads campaign lookup for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	if wire.Deleted != nil && *wire.Deleted {
		return nil, nil
	}
	if wire.Name == nil || strings.TrimSpace(*wire.Name) == "" {
		return nil, fmt.Errorf("x ads campaign lookup for %s: the campaign was returned without a usable name", campaignID)
	}
	if wire.EntityStatus == nil {
		return nil, fmt.Errorf("x ads campaign lookup for %s: the campaign was returned without an entity_status", campaignID)
	}
	switch *wire.EntityStatus {
	case StatusActive, StatusPaused, StatusDraft:
	default:
		return nil, fmt.Errorf("x ads campaign lookup for %s: the campaign reports an entity_status this client does not recognise, so whether it is live cannot be established", campaignID)
	}
	return &CampaignRef{
		ID:        campaignID,
		Name:      *wire.Name,
		Status:    *wire.EntityStatus,
		AccountID: strings.TrimSpace(wire.AccountID),
	}, nil
}
