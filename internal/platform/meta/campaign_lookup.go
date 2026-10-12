// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
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

// adoptCampaignFields is exactly the field set the adoption read asks for. Only id, name, status
// and account_id are interpreted — but the CAMPAIGN-ONLY fields (objective, daily_budget,
// lifetime_budget, bid_strategy) are LOAD-BEARING all the same. GET /{id} accepts ANY node id, and
// an ad set or ad also has id, name, status and account_id; asking for a field only a campaign
// has is what makes Graph refuse a non-campaign node (code 100, "nonexisting field" — an
// unverifiable 503 here, like every Graph error). Trimming them would make an ad set or ad id
// adoptable as a campaign. TestMetaGetCampaign_RequestsCampaignOnlyFields pins that they are sent.
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
//   - a campaign whose status is DELETED or ARCHIVED -> (nil, nil)
//   - anything unverifiable                          -> (nil, error)
//
// The ONLY absence this read reports is one Meta PROVES: the node answered, and its own status
// says it is deleted or archived. Graph code 100 / error_subcode 33 is deliberately NOT an
// absence, on any HTTP status: Meta documents it as "does not exist, cannot be loaded due to
// missing permissions, or does not support this operation", so it cannot tell a missing campaign
// from one the token simply cannot see — a revoked grant, a token for the wrong business, an
// account removed from the system user. Reporting that as "no such campaign" (404) is the
// ambiguous absence an operator acts on by creating a duplicate of a campaign that may be live,
// so it is unverifiable (503) like every other error, and so is any 401/403.
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
		// Every Graph error — 100/33 included, see above — is unverifiable: none proves absence.
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
