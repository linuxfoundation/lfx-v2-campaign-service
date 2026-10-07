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

// maxCampaignIDLen bounds an adoption id at the adopt-campaign payload's own MaxLength, so the
// client refuses nothing longer than the API already admits and keeps an unbounded string out of
// the request path when it is called from anywhere else.
const maxCampaignIDLen = 64

// ValidateCampaignID reports whether campaignID can name a Reddit campaign: non-empty, letters,
// digits and underscores only (the path-injection guard every campaign path here applies), and
// bounded. Padding is refused, not trimmed. The error wraps ErrInvalidCampaignID — a PERMANENT
// input fault the adopt handler maps to 400.
func ValidateCampaignID(campaignID string) error {
	if campaignID == "" || len(campaignID) > maxCampaignIDLen || !accountIDRe.MatchString(campaignID) {
		return fmt.Errorf("reddit: adoption id: %w", ErrInvalidCampaignID)
	}
	return nil
}

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
//   - a 404                                                  -> (nil, nil)
//   - configured_status DELETED or ARCHIVED                  -> (nil, nil)
//   - anything unverifiable                                  -> (nil, error)
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
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("reddit campaign lookup for %s: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit campaign lookup for %s: the response carried no campaign", campaignID)
	}
	if err := identityjson.Check(resp.Data); err != nil {
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
