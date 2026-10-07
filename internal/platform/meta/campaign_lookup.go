// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Meta's campaign `status` values beyond the ACTIVE/PAUSED pair the toggle writes
// (https://developers.facebook.com/docs/marketing-api/reference/ad-campaign-group, "status").
const (
	StatusDeleted  = "DELETED"
	StatusArchived = "ARCHIVED"
)

// graphCodeInvalidParameter + graphSubcodeObjectMissing is the Graph API's structured "this node
// does not exist, cannot be loaded with this token, or does not support this operation" answer
// (code 100, error_subcode 33). It is the only answer this client reads as an ABSENCE; see
// GetCampaign for why the "cannot be loaded" half of it is still an absence for adoption.
const (
	graphCodeInvalidParameter = 100
	graphSubcodeObjectMissing = 33
)

// adoptCampaignFields is exactly the field set the adoption read asks for. Only id, name, status
// and account_id decide anything; the rest are read so the response is the campaign's full
// identity at the moment it was bound, and are not interpreted.
const adoptCampaignFields = "id,name,status,effective_status,account_id,objective,daily_budget,lifetime_budget,bid_strategy"

// CampaignRef is what the adoption read learns about one campaign.
type CampaignRef struct {
	// ID is the id Meta ECHOED, already checked against the requested one.
	ID   string
	Name string
	// Status is the campaign's configured `status` — ACTIVE or PAUSED here, never anything else.
	Status string
	// EffectiveStatus is reported verbatim and not interpreted: Meta grows this set (IN_PROCESS,
	// WITH_ISSUES, ...) without notice, and refusing an adoption over a value that only describes
	// delivery would be an over-refusal.
	EffectiveStatus string
	// AccountID is the ad account Meta reports the campaign under, normalised to "act_<digits>".
	AccountID string
	Objective string
}

// campaignLookupWire is the subset of the campaign node the read decodes. Every field is a
// string, so a decode can only fail with an UnmarshalTypeError naming a JSON kind and this
// struct's own field — never echoing an upstream value — and the caller drops even that.
type campaignLookupWire struct {
	ID              *string `json:"id"`
	Name            *string `json:"name"`
	Status          *string `json:"status"`
	EffectiveStatus string  `json:"effective_status"`
	AccountID       *string `json:"account_id"`
	Objective       string  `json:"objective"`
	DailyBudget     string  `json:"daily_budget"`
	LifetimeBudget  string  `json:"lifetime_budget"`
	BidStrategy     string  `json:"bid_strategy"`
}

// GetCampaign reads one campaign node — GET /{campaign_id}?fields=… — for adoption. It is a PURE
// READ, retried on throttles like every doRequest GET.
//
// The outcomes are the CampaignAdopter contract's:
//
//   - a live campaign (status ACTIVE or PAUSED)     -> (ref, nil)
//   - Graph code 100 / error_subcode 33              -> (nil, nil)
//   - a campaign whose status is DELETED or ARCHIVED -> (nil, nil)
//   - anything unverifiable                          -> (nil, error)
//
// Code 100/33 means "does not exist, cannot be loaded due to missing permissions, or does not
// support this operation". All three are an absence UNDER THIS CONNECTION'S TOKEN, which is the
// question adoption asks: a campaign this token cannot load is not one this project can bind.
// It is matched on the structured code and subcode, never on Meta's message text.
//
// DELETED and ARCHIVED are absences for the reason a REMOVED campaign is one on Google: the node
// still answers, but it is terminal (Meta documents an archived object as deletable only), it
// cannot spend, and so reporting it absent cannot license a duplicate of anything serving.
//
// "Unverifiable" is everything else: a transport failure, a 5xx, a throttle that outlasted the
// retries, any other Graph error, a body identityjson refuses or that does not decode, an id
// that is not the one asked for, a missing name, a missing or malformed account_id, and a status
// outside the four documented values.
//
// PROVENANCE is NOT decided here. GET /{id} is not account-scoped — the token may reach several
// ad accounts — so the caller compares AccountID with the connection's own account (see
// MetaDispatcher.LookupCampaign). That is why an absent or malformed account_id is an error
// rather than an empty field: without it the comparison cannot be made, and adoption must not
// proceed on an account nobody verified.
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, "/"+campaignID+"?fields="+adoptCampaignFields, nil, &raw); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 &&
			ae.Code == graphCodeInvalidParameter && ae.ErrorSubcode == graphSubcodeObjectMissing {
			return nil, nil
		}
		return nil, fmt.Errorf("meta campaign lookup for %s: %w", campaignID, err)
	}
	if err := identityjson.Check(raw); err != nil {
		return nil, fmt.Errorf("meta campaign lookup for %s: %w", campaignID, err)
	}
	var wire campaignLookupWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		// The decode error is dropped: it is derived from the response.
		return nil, fmt.Errorf("meta campaign lookup for %s: the response is not a campaign object", campaignID)
	}
	if wire.ID == nil || *wire.ID != campaignID {
		return nil, fmt.Errorf("meta campaign lookup for %s: the response does not describe the requested campaign, so nothing in it can be trusted", campaignID)
	}
	if wire.Name == nil || strings.TrimSpace(*wire.Name) == "" {
		return nil, fmt.Errorf("meta campaign lookup for %s: the campaign was returned without a usable name", campaignID)
	}
	if wire.Status == nil {
		return nil, fmt.Errorf("meta campaign lookup for %s: the campaign was returned without a status", campaignID)
	}
	switch *wire.Status {
	case StatusActive, StatusPaused:
	case StatusDeleted, StatusArchived:
		return nil, nil
	default:
		return nil, fmt.Errorf("meta campaign lookup for %s: the campaign reports a status this client does not recognise, so whether it is live cannot be established", campaignID)
	}
	account := ""
	if wire.AccountID != nil {
		account = canonicalAccountID(*wire.AccountID)
	}
	if account == "" {
		return nil, fmt.Errorf("meta campaign lookup for %s: the campaign was returned without a readable account_id, so which ad account it belongs to cannot be established", campaignID)
	}
	return &CampaignRef{
		ID:              campaignID,
		Name:            *wire.Name,
		Status:          *wire.Status,
		EffectiveStatus: strings.TrimSpace(wire.EffectiveStatus),
		AccountID:       account,
		Objective:       strings.TrimSpace(wire.Objective),
	}, nil
}

// canonicalAccountID puts a reported account_id into the "act_<digits>" form the connection
// stores. Graph reports a campaign's account_id as bare digits; an "act_"-prefixed value is
// accepted too. Anything else is "" — no account — never a token that could compare unequal to
// every real account and pass for one.
func canonicalAccountID(id string) string {
	digits := strings.TrimPrefix(id, "act_")
	if !numericIDRE.MatchString(digits) {
		return ""
	}
	return "act_" + digits
}
