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
//   - a 404 with the account then confirmed readable, or a   -> (nil, nil)
//     campaign X reports as deleted
//   - anything unverifiable                                  -> (nil, error)
//
// A campaign 404 alone does NOT prove absence: X answers 404 the same way when the AD ACCOUNT in
// the path is inaccessible or revoked, and reporting that as "no such campaign" is the ambiguous
// absence an operator acts on by creating a duplicate. So a campaign 404 is followed by ONE
// confirming read of the account itself (GET accounts/:account_id, confirmAccountReadable) through the
// same client and credential: only when that answers 2xx, describing this exact account, is the
// campaign definitely absent. Any other answer to the confirming read — 404, 401/403, 5xx, an
// exhausted 429, transport, a body that does not decode or names another account — leaves the
// absence unproven, and the lookup is unverifiable (503). The confirming read is made only after
// a campaign 404, never on any other outcome.
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
		// A campaign 404 is proven absent only once the account itself is confirmed readable;
		// see the doc comment above.
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			if cerr := c.confirmAccountReadable(ctx); cerr != nil {
				return nil, fmt.Errorf("x ads campaign lookup for %s: the campaign read answered 404, but the ad account could not be confirmed readable, so the absence is not proven: %w", campaignID, cerr)
			}
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

// confirmAccountReadable is the proof behind a campaign 404: ONE read of the connection's own ad
// account (GET accounts/:account_id), which must answer 2xx with a body identityjson accepts whose data.id
// is exactly the configured account. Anything else is returned as an error, and the caller
// reports the lookup unverifiable rather than the campaign absent. The account id was already
// validated by the campaign read's path builder before this runs.
func (c *Client) confirmAccountReadable(ctx context.Context) error {
	accountID := strings.TrimSpace(c.account.AccountID)
	resp, err := c.request(ctx, http.MethodGet, "")
	if err != nil {
		return err
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return fmt.Errorf("the ad account read carried no account")
	}
	if err := identityjson.Check(resp.raw); err != nil {
		return err
	}
	var acct struct {
		ID *string `json:"id"`
	}
	if err := json.Unmarshal(resp.Data, &acct); err != nil {
		return fmt.Errorf("the ad account read is not an account object")
	}
	if acct.ID == nil || *acct.ID != accountID {
		return fmt.Errorf("the ad account read does not describe the connection's account")
	}
	return nil
}
