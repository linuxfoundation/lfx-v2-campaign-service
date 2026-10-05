// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Ad-group max CPC bid: read + write (LFXV2-2665)
//
// Reddit's Ads API v3 ad group carries the bid as three fields: bid_strategy (BIDLESS,
// MANUAL_BIDDING, MAXIMIZE_VOLUME, TARGET_CPX), bid_type (CPC, CPM, CPV, ...) and bid_value, in
// micro-units of the account currency like every other Reddit money field. Only MANUAL_BIDDING
// reads bid_value as the bid to pay: BIDLESS is Reddit's automatic bidding, and MAXIMIZE_VOLUME
// and TARGET_CPX are strategy-managed. The create path (CreateCampaign) sends
// bid_strategy=BIDLESS on both the campaign and its one ad group, so EVERY campaign this service
// creates is refused by the dispatcher until an operator moves the ad group to manual bidding.
//
// Source: the official OpenAPI document this package's contract follows
// (https://ads-api.reddit.com/api/v3/openapi.json, LFXV2-3282) and the ad-group reference at
// https://ads-api.reddit.com/docs/v3/. Neither could be re-fetched from the authoring
// environment on 2026-10-05 (the host refuses automated fetches), so the field names and the
// enum above were checked against the create path's own request body and secondary references
// to the same document; the guards fail CLOSED on anything they do not recognize, which is what
// keeps an unverified detail from becoming a wrong write.
//
// The read is GET /ad_accounts/{account}/ad_groups/{id}; the write is a PATCH of the same path
// naming bid_value ONLY — the path updateEntityStatus PATCHes for the status toggle.
// ---------------------------------------------------------------------------

// The bid strategies and the bid type this path recognizes.
const (
	BidStrategyManual = "MANUAL_BIDDING"
	BidTypeCPC        = "CPC"
)

// redditMaxBid bounds bid_value at the contract's ceiling (1,000,000 in the account currency),
// which keeps the ×1e6 micro conversion far from int64 overflow. Reddit's own maximum is lower
// and is Reddit's to enforce.
const redditMaxBid = 1_000_000.0

// ErrInvalidAdGroupID marks an ad group id the client refuses before any request: it could not
// address a Reddit resource path.
var ErrInvalidAdGroupID = errors.New("ad group id must be non-empty and contain only letters, digits, and underscores")

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by BidMicros, or by Reddit's
// definite 400 naming bid_value. Read the client-safe sentence with BidAmountReason.
var ErrBidAmountInvalid = errors.New("reddit: bid amount is not acceptable")

// bidAmountError carries ONLY a client-safe sentence, mirroring budgetAmountError.
type bidAmountError struct{ msg string }

func (e *bidAmountError) Error() string { return e.msg }
func (e *bidAmountError) Unwrap() error { return ErrBidAmountInvalid }

// BidAmountReason reports whether err is a bid-amount refusal from this package and, if so,
// returns its sentence — the only text from this path that may be shown to a caller.
func BidAmountReason(err error) (string, bool) {
	var amountErr *bidAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// BidMicros converts a bid in the account's currency to the integer micro-units bid_value
// carries, with BudgetMicros's rounding (half away from zero, via toMicrodollars): finite and
// positive, at most redditMaxBid, and refused if it rounds to zero.
func BidMicros(amount float64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return 0, &bidAmountError{msg: "the bid must be a finite number greater than zero"}
	case amount > redditMaxBid:
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s exceeds the largest bid this service sets on Reddit (%s)", formatBudgetAmount(amount), formatBudgetAmount(redditMaxBid))}
	}
	micros := toMicrodollars(amount)
	if micros <= 0 {
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s rounds to zero micro-units; Reddit bids are set in micro-units, so the smallest settable bid is 0.000001", formatBudgetAmount(amount))}
	}
	return micros, nil
}

// AdGroupBid is one ad group's live bid state, as GetAdGroupBid read it.
type AdGroupBid struct {
	AdGroupID string
	// CampaignID is the campaign Reddit reports the ad group under; "" when not reported.
	CampaignID string
	// BidStrategy and BidType are the raw values; "" when not reported.
	BidStrategy string
	BidType     string
	// BidValueMicros is bid_value; nil when not reported or null.
	BidValueMicros *int64
	// BidValueUnparseable is set when bid_value was reported but is not an integer.
	BidValueUnparseable bool
}

// adGroupBidWire is the subset of a Reddit ad group this path reads. As with campaignBudgetWire,
// every field is a string or json.RawMessage so a decode error can never echo an upstream value.
type adGroupBidWire struct {
	ID          string          `json:"id"`
	CampaignID  string          `json:"campaign_id"`
	BidStrategy string          `json:"bid_strategy"`
	BidType     string          `json:"bid_type"`
	BidValue    json.RawMessage `json:"bid_value"`
}

// adGroupBidPath validates both interpolated ids and returns the ad group's resource path.
func (c *Client) adGroupBidPath(adGroupID string) (string, string, error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	adGroupID = strings.TrimSpace(adGroupID)
	if accountID == "" || !accountIDRe.MatchString(accountID) {
		return "", "", fmt.Errorf("reddit: ad group bid: %w", ErrInvalidAccountID)
	}
	if adGroupID == "" || !accountIDRe.MatchString(adGroupID) {
		return "", "", fmt.Errorf("reddit: ad group bid: %w", ErrInvalidAdGroupID)
	}
	return "/ad_accounts/" + accountID + "/ad_groups/" + adGroupID, adGroupID, nil
}

// GetAdGroupBid reads one ad group's bid state via GET /ad_accounts/{account}/ad_groups/{id}. A
// PURE READ: its failure is always definite. A 404 is (nil, nil) — Reddit holds no such ad group.
// A 2xx describing a DIFFERENT ad group id is an error.
func (c *Client) GetAdGroupBid(ctx context.Context, adGroupID string) (*AdGroupBid, error) {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("reddit: read ad group %s bid: %w", adGroupID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit: read ad group %s bid: the response carried no ad group", adGroupID)
	}
	var wire adGroupBidWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("reddit: read ad group %s bid: the response is not an ad group object", adGroupID)
	}
	if got := strings.TrimSpace(wire.ID); got != adGroupID {
		if got == "" {
			got = "no ad group id"
		}
		return nil, fmt.Errorf("reddit: read ad group %s bid returned %s instead", adGroupID, got)
	}
	out := &AdGroupBid{
		AdGroupID:   adGroupID,
		CampaignID:  strings.TrimSpace(wire.CampaignID),
		BidStrategy: strings.TrimSpace(wire.BidStrategy),
		BidType:     strings.TrimSpace(wire.BidType),
	}
	out.BidValueMicros, out.BidValueUnparseable = parseGoalValue(wire.BidValue)
	return out, nil
}

// UpdateAdGroupBid sets an existing ad group's bid_value via
// PATCH /ad_accounts/{account}/ad_groups/{id} with {"data":{"bid_value": micros}}. It writes the
// AMOUNT only: bid_strategy and bid_type are never sent, so the strategy cannot be switched by
// this call. The caller is responsible for having established that the ad group bids manually in
// CPC — this function writes what it is told.
//
// micros must be the value BidMicros produced. The PATCH converges when repeated, so request()
// retries a 429, as UpdateCampaignBudget does. A transport failure, 3xx, exhausted 429 or 5xx is
// UNCONFIRMED (IsOutcomeUnconfirmed). A definite 400 whose body names bid_value is returned as a
// bidAmountError with a generic client-safe sentence — Reddit's own text is never surfaced, and
// Reddit's error shape is not documented field-by-field, so this is a best-effort match whose
// miss is still truthful (a definite failure: the platform unchanged). A 2xx echo naming another
// ad group or another bid_value is UNCONFIRMED.
func (c *Client) UpdateAdGroupBid(ctx context.Context, adGroupID string, micros int64) error {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return err
	}
	if micros < 1 {
		return &bidAmountError{msg: fmt.Sprintf("bid %d micro-units is not a valid amount; it must be a positive number of micro-units", micros)}
	}
	body := map[string]any{"data": map[string]any{"bid_value": micros}}
	resp, err := c.request(ctx, http.MethodPatch, path, body)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest && strings.Contains(ae.Body, "bid_value") {
			return &bidAmountError{msg: fmt.Sprintf("Reddit refused a max CPC bid of %s for this ad group; it is outside the range Reddit accepts in the account's currency", formatBudgetAmount(float64(micros)/1_000_000))}
		}
		return fmt.Errorf("reddit: update ad group %s bid_value to %d: %w", adGroupID, micros, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	var echo adGroupBidWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return &transportError{Method: http.MethodPatch, Path: "ad group bid", Err: fmt.Errorf("decode 2xx bid update response for ad group %s: not an ad group object: %w", adGroupID, jerr)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != adGroupID {
		return &transportError{Method: http.MethodPatch, Path: "ad group bid", Err: fmt.Errorf("bid update for ad group %s was acknowledged for ad group %s", adGroupID, got)}
	}
	if got, unparseable := parseGoalValue(echo.BidValue); unparseable || (got != nil && *got != micros) {
		return &transportError{Method: http.MethodPatch, Path: "ad group bid", Err: fmt.Errorf("bid update for ad group %s was acknowledged with a bid_value other than the %d micro-units sent", adGroupID, micros)}
	}
	return nil
}
