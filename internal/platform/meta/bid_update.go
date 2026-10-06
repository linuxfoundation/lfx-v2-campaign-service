// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Ad-set bid cap: read + write (LFXV2-2665)
//
// WHERE A META BID LIVES. On the AD SET, as three fields read together: bid_strategy,
// billing_event and optimization_goal decide what bid_amount MEANS, and bid_amount carries the
// value in the account currency's MINOR units. Source — the Marketing API Ad Set reference
// (Graph API v25.0), https://developers.facebook.com/docs/marketing-api/reference/ad-campaign,
// fetched 2026-10-05:
//
//   - bid_strategy LOWEST_COST_WITHOUT_CAP: "Designed to get the most results for your budget
//     based on your ad set optimization_goal without limiting your bid amount ... This
//     strategy is also known as automatic bidding."
//   - bid_strategy LOWEST_COST_WITH_BID_CAP: "... while limiting actual bid to your specified
//     amount. With a bid cap you have more control over your cost per actual optimization
//     event ... If you select this, you must provide a bid cap with the bid_amount field."
//     COST_CAP and LOWEST_COST_WITH_MIN_ROAS are the other two values.
//   - bid_amount: "The bid cap used in a lowest cost bid strategy is defined as the maximum bid
//     you want to pay for a result based on your optimization_goal ... The bid amount's unit
//     is cents for currencies like USD, EUR, and the basic unit for currencies like JPY, KRW.
//     The bid amount for ads with IMPRESSION or REACH as billing_event is per 1,000
//     occurrences ... For ads with other billing_events, the bid amount is for each
//     occurrence, and has a minimum value 1 US cents."
//
// WHEN THAT IS A MAX CPC — THE DECISION THIS FILE ENCODES. A Meta bid cap is a cap per
// OPTIMIZATION EVENT, and its unit follows the BILLING EVENT. It is a maximum cost per click
// only when both name the same click: optimization_goal LINK_CLICKS (the cap is per link click)
// AND billing_event LINK_CLICKS (the amount is per occurrence, billed per link click). Every
// other pairing is refused as not-a-CPC: billing_event IMPRESSIONS makes the amount a per-1,000
// figure — a CPM, not a CPC — and CLICKS bills any click while the cap names a link click, so
// the two units differ. COST_CAP is a target average, not a maximum; MIN_ROAS is a return
// floor; LOWEST_COST_WITHOUT_CAP is automatic bidding and has no amount to set.
//
// WHAT THE CREATE PATH SENDS MAKES THIS LEG NARROW, and that is stated rather than hidden:
// CreateCampaign creates the ad set with billing_event IMPRESSIONS and bid_strategy
// LOWEST_COST_WITHOUT_CAP, so EVERY Meta campaign this service creates is refused by the
// dispatcher (409) until an operator moves its ad set to a link-click bid cap in Ads Manager.
// Writing bid_amount under LOWEST_COST_WITHOUT_CAP would either be refused by Meta or be read
// as half of a strategy switch; this path never sends bid_strategy or billing_event.
// ---------------------------------------------------------------------------

// The bid strategy, billing event and optimization goal under which bid_amount is a max CPC.
const (
	BidStrategyBidCap      = "LOWEST_COST_WITH_BID_CAP"
	BillingEventLinkClicks = "LINK_CLICKS"
	OptimizationLinkClicks = "LINK_CLICKS"
)

// metaMaxBid bounds the bid at the contract's ceiling (1,000,000 in the account currency). The
// largest supported minor-unit scale is 100, so the encoded value stays far inside int64 and
// inside the uint32 Meta stores bid_amount in. Meta's own ceilings are Meta's to enforce.
const metaMaxBid = 1_000_000.0

// ErrInvalidAdSetID marks an ad set id the client refuses before any request: it is not the
// numeric node id Meta assigns, so it cannot address a Graph path.
var ErrInvalidAdSetID = errors.New("meta: ad set id must be a non-empty numeric id")

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by the encoder, or by Meta's
// definite refusal of bid_amount. Read the client-safe sentence with BidAmountReason.
var ErrBidAmountInvalid = errors.New("meta: bid amount is not acceptable")

// bidAmountError carries ONLY a client-safe sentence, mirroring budgetAmountError: Meta's own
// error text is never placed in it.
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

// bidToMinorUnits converts a bid in the account's currency to the minor-unit integer bid_amount
// carries, with budgetToMinorUnits's rounding (half away from zero). It is not an FX conversion.
func bidToMinorUnits(amount float64, offset int64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return 0, &bidAmountError{msg: "the bid must be a finite number greater than zero"}
	case amount > metaMaxBid:
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s exceeds the largest bid this service sets on Meta (%s)",
			strconv.FormatFloat(amount, 'f', -1, 64), strconv.FormatFloat(metaMaxBid, 'f', -1, 64))}
	}
	minor := int64(math.Round(amount * float64(offset)))
	if minor < 1 {
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s is smaller than one minor unit of the ad account's currency, the smallest bid Meta accepts",
			strconv.FormatFloat(amount, 'f', -1, 64))}
	}
	return minor, nil
}

// ResolveBidMinorUnits runs the account preflight and encodes a bid in the minor units of the
// ad account's OWN currency — the scale resolveCurrencyOffset derives for the create and the
// budget paths, so a bid can never be encoded at a guessed scale. A PURE READ plus arithmetic:
// its failure is always definite. An account currency with no known scale wraps
// ErrAccountCurrencyUnresolvable; an amount refusal is a bidAmountError.
func (c *Client) ResolveBidMinorUnits(ctx context.Context, amount float64) (int64, error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" {
		return 0, fmt.Errorf("meta: an ad account must be selected to write a bid: the account's currency determines the minor-unit scale the bid is encoded in, and it cannot be assumed")
	}
	var acct accountPreflight
	preflightErr := c.doRequest(ctx, http.MethodGet, "/"+accountID+"?fields=name,account_status,currency", nil, &acct)
	if preflightErr != nil && ctx.Err() != nil {
		return 0, fmt.Errorf("meta bid write aborted during account preflight: %w", ctx.Err())
	}
	offset, err := c.resolveCurrencyOffset(acct.Currency, preflightErr)
	if err != nil {
		if preflightErr != nil {
			return 0, fmt.Errorf("meta: the ad account's currency could not be determined because the account preflight failed, and the minor-unit scale a bid is encoded in cannot be assumed: %w", err)
		}
		return 0, fmt.Errorf("meta: the ad account's currency does not resolve to a minor-unit scale this service can encode a bid in (%w): %w", err, ErrAccountCurrencyUnresolvable)
	}
	minor, err := bidToMinorUnits(amount, offset)
	if err != nil {
		return 0, err
	}
	return minor, nil
}

// AdSetBid is one ad set's live bid state, as GetAdSetBid read it. Every string is the raw
// value Meta reported, "" when not reported.
type AdSetBid struct {
	AdSetID          string
	CampaignID       string
	BidStrategy      string
	BillingEvent     string
	OptimizationGoal string
	// BidAmountMinor is bid_amount in minor units; nil when not reported.
	BidAmountMinor *int64
	// BidAmountUnparseable is set when bid_amount was reported but is not an integer.
	BidAmountUnparseable bool
}

// CPCBidCap reports whether bid_amount on this ad set is a maximum cost per link click — the
// one meaning this service writes. See the file header for the reasoning.
func (b *AdSetBid) CPCBidCap() bool {
	return b.BidStrategy == BidStrategyBidCap && b.BillingEvent == BillingEventLinkClicks && b.OptimizationGoal == OptimizationLinkClicks
}

// parseBidAmount reads bid_amount, which Meta reports as a JSON integer (a numeric string is
// accepted too). Absent or null is nil; anything else that is not an integer is unparseable —
// never truncated, because truncation is a silent change to money.
func parseBidAmount(raw json.RawMessage) (*int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	s := string(raw)
	if raw[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil, true
		}
		s = strings.TrimSpace(str)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, true
	}
	return &v, false
}

func validAdSetID(adSetID string) (string, error) {
	adSetID = strings.TrimSpace(adSetID)
	if !numericIDRE.MatchString(adSetID) {
		return "", ErrInvalidAdSetID
	}
	return adSetID, nil
}

// GetAdSetBid reads one ad set's bid state. A PURE READ, so its failure is always definite. A
// response describing a different ad set id is an error.
func (c *Client) GetAdSetBid(ctx context.Context, adSetID string) (*AdSetBid, error) {
	adSetID, err := validAdSetID(adSetID)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ID               string          `json:"id"`
		CampaignID       string          `json:"campaign_id"`
		BidStrategy      string          `json:"bid_strategy"`
		BillingEvent     string          `json:"billing_event"`
		OptimizationGoal string          `json:"optimization_goal"`
		BidAmount        json.RawMessage `json:"bid_amount"`
	}
	const fields = "id,campaign_id,bid_strategy,billing_event,optimization_goal,bid_amount"
	if err := c.doRequest(ctx, http.MethodGet, "/"+adSetID+"?fields="+fields, nil, &resp); err != nil {
		return nil, fmt.Errorf("meta: read ad set %s bid: %w", adSetID, err)
	}
	if got := strings.TrimSpace(resp.ID); got != adSetID {
		if got == "" {
			got = "no ad set id"
		}
		return nil, fmt.Errorf("meta: read ad set %s bid returned %s instead", adSetID, got)
	}
	out := &AdSetBid{
		AdSetID:          adSetID,
		CampaignID:       strings.TrimSpace(resp.CampaignID),
		BidStrategy:      strings.TrimSpace(resp.BidStrategy),
		BillingEvent:     strings.TrimSpace(resp.BillingEvent),
		OptimizationGoal: strings.TrimSpace(resp.OptimizationGoal),
	}
	out.BidAmountMinor, out.BidAmountUnparseable = parseBidAmount(resp.BidAmount)
	return out, nil
}

// UpdateAdSetBid sets an existing ad set's bid_amount via POST /{ad_set_id} with
// {"bid_amount": minor}. It writes the AMOUNT only: bid_strategy, billing_event and
// optimization_goal are never sent, so the strategy cannot be switched by this call. The caller
// is responsible for having established CPCBidCap — this function writes what it is told.
//
// minor must be the value ResolveBidMinorUnits produced.
//
// THE THROTTLE IS NOT RETRIED IN-CALL (retryThrottle=false), deliberately and unlike
// UpdateAdSetBudget: this client's premise is that a throttle may arrive AFTER Meta applied the
// write, and a definite refusal returned by a RETRY answers the last attempt, not the first. Not
// retrying means every throttle comes back as the throttle itself, which createOutcomeAmbiguous
// classifies UNCONFIRMED — so "a refusal after a retried 429" cannot be reported as "nothing
// changed", because there is no retry to produce one.
//
// A transport failure, 3xx, 5xx or throttle is UNCONFIRMED (IsOutcomeUnconfirmed). A definite
// 4xx is an AMOUNT refusal only when it is STRUCTURED as one: the envelope's
// error_data.blame_field_specs names bid_amount as the field at fault (see blameFieldSpecs for
// the cited shape). That is returned as a bidAmountError carrying this service's own sentence —
// Meta's text is never surfaced. The message is never consulted: free text mentioning "bid"
// would also match a bid_strategy refusal, and a real floor can carry a generic message. The
// Marketing API error reference documents no bid-AMOUNT error_subcode (its one bid subcode,
// 1885204, is a strategy refusal), so no subcode is matched. Every other definite 4xx — including
// one whose blame names another field — stays a definite failure: the ad set unchanged.
func (c *Client) UpdateAdSetBid(ctx context.Context, adSetID string, minor int64) error {
	adSetID, err := validAdSetID(adSetID)
	if err != nil {
		return err
	}
	if minor < 1 {
		return fmt.Errorf("meta: bid %d minor units is not a valid amount; it must come from ResolveBidMinorUnits", minor)
	}
	body := map[string]any{"bid_amount": minor}
	if err := c.do(ctx, http.MethodPost, "/"+adSetID, body, nil, false /* a throttle is unconfirmed, never retried: see above */); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 &&
			ae.StatusCode != http.StatusTooManyRequests && !graphRateLimitCodes[ae.Code] &&
			ae.blamesField("bid_amount") {
			return &bidAmountError{msg: "Meta refused this max CPC bid for the ad set; it is outside the range Meta accepts in the ad account's currency"}
		}
		return fmt.Errorf("meta: update ad set %s bid_amount to %d: %w", adSetID, minor, err)
	}
	return nil
}
