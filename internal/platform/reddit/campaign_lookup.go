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

// Reddit's campaign configured_status values beyond the ACTIVE/PAUSED pair the toggle writes.
const (
	StatusArchived = "ARCHIVED"
	StatusDeleted  = "DELETED"
)

// CampaignRef is what the adoption read learns about one campaign.
type CampaignRef struct {
	// ID is the id Reddit ECHOED, already checked against the requested one.
	ID   string
	Name string
	// Status is configured_status — ACTIVE or PAUSED here, never anything else.
	Status string
	// AdAccountID is the account Reddit reports the campaign under, "" when not reported. The
	// read is account-scoped by its path, so the caller compares this only when it is present.
	AdAccountID string
}

// campaignLookupWire is the subset of a campaign the adoption read decodes. Pointer strings keep
// "absent" distinguishable from "empty"; with only string kinds a decode error can name a JSON
// kind and this struct's field, never an upstream value — and the caller drops even that.
type campaignLookupWire struct {
	ID               *string `json:"id"`
	Name             *string `json:"name"`
	ConfiguredStatus *string `json:"configured_status"`
	AdAccountID      string  `json:"ad_account_id"`
}

// GetCampaign reads one campaign for adoption via GET /ad_accounts/{accountID}/campaigns/{id} —
// the SAME account-scoped resource the budget read and the status toggle address. It is a PURE
// READ, retried on 429 like every request() GET.
//
// The outcomes are the CampaignAdopter contract's:
//
//   - a live campaign (configured_status ACTIVE or PAUSED) -> (ref, nil)
//   - a 404, with the ad account then confirmed readable     -> (nil, nil)
//   - configured_status DELETED or ARCHIVED                  -> (nil, nil)
//   - anything unverifiable                                  -> (nil, error)
//
// A campaign 404 alone does NOT prove absence: Reddit answers 404 the same way when the AD ACCOUNT in
// the path is inaccessible or revoked, and reporting that as "no such campaign" is the ambiguous
// absence an operator acts on by creating a duplicate. So a campaign 404 is followed by ONE
// confirming read of the account itself (GET /ad_accounts/{account_id}, confirmAccountReadable) through the
// same client and credential: only when that answers 2xx, describing this exact account, is the
// campaign definitely absent. Any other answer to the confirming read — 404, 401/403, 5xx, an
// exhausted 429, transport, a body that does not decode or names another account — leaves the
// absence unproven, and the lookup is unverifiable (503). The confirming read is made only after
// a campaign 404, never on any other outcome.
//
// The 404 is matched on the HTTP status, never on body text. DELETED and ARCHIVED are absences
// for the reason a REMOVED campaign is one on Google: the record answers, but it is terminal and
// cannot spend, so reporting it absent cannot license a duplicate of anything serving.
//
// "Unverifiable" is everything else: a transport failure, a 5xx, an exhausted 429, a 401/403
// (an auth failure says nothing about whether the campaign exists), a body identityjson refuses
// or that does not decode, an id that is not the one asked for, a missing name, and a
// configured_status outside the four documented values.
//
// PROVENANCE. The path names the connection's account, so Reddit resolves the id inside it; the
// returned ad_account_id, when present, is handed to the caller to cross-check rather than
// trusted (see RedditDispatcher.LookupCampaign).
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
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
		// A campaign 404 is proven absent only once the account itself is confirmed readable;
		// see the doc comment above.
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			if cerr := c.confirmAccountReadable(ctx); cerr != nil {
				return nil, fmt.Errorf("reddit campaign lookup for %s: the campaign read answered 404, but the ad account could not be confirmed readable, so the absence is not proven: %w", campaignID, cerr)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("reddit campaign lookup for %s: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the response carried no campaign", campaignID)
	}
	// The guard runs over the RAW response body, not resp.Data: the envelope has already been
	// decoded, and a duplicated "data" key (or "data" beside "Data") was resolved last-wins on
	// the way into Data — invisible to a check of Data alone.
	if err := identityjson.Check(resp.raw); err != nil {
		return nil, fmt.Errorf("reddit campaign lookup for %s: %w", campaignID, err)
	}
	var wire campaignLookupWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	if wire.Name == nil || strings.TrimSpace(*wire.Name) == "" {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the campaign was returned without a usable name", campaignID)
	}
	if wire.ConfiguredStatus == nil {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the campaign was returned without a configured_status", campaignID)
	}
	switch *wire.ConfiguredStatus {
	case StatusActive, StatusPaused:
	case StatusArchived, StatusDeleted:
		return nil, nil
	default:
		return nil, fmt.Errorf("reddit campaign lookup for %s: the campaign reports a configured_status this client does not recognise, so whether it is live cannot be established", campaignID)
	}
	return &CampaignRef{
		ID:          campaignID,
		Name:        *wire.Name,
		Status:      *wire.ConfiguredStatus,
		AdAccountID: strings.TrimSpace(wire.AdAccountID),
	}, nil
}

// confirmAccountReadable is the proof behind a campaign 404: ONE read of the connection's own ad
// account (GET /ad_accounts/{account_id}), which must answer 2xx with a body identityjson accepts whose data.id
// is exactly the configured account. Anything else is returned as an error, and the caller
// reports the lookup unverifiable rather than the campaign absent. The account id was already
// validated by the campaign read's path builder before this runs.
func (c *Client) confirmAccountReadable(ctx context.Context) error {
	accountID := strings.TrimSpace(c.account.AccountID)
	resp, err := c.request(ctx, http.MethodGet, "/ad_accounts/"+accountID, nil)
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
