// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Line-item max bid: read + write (LFXV2-2665)
//
// WHERE AN X BID LIVES. On the LINE ITEM: bid_strategy (AUTO, MAX, TARGET) says how the line
// item bids, pay_by names the charge unit, and bid_amount_local_micro carries the amount in
// micro-units of the funding instrument's currency (USD 5.50 = 5,500,000). Source — the X Ads
// API v12 Campaign Management reference, https://docs.x.com/x-ads-api/campaign-management/reference
// (line items: POST accounts/:account_id/line_items and PUT
// accounts/:account_id/line_items/:line_item_id) and the guide at
// https://docs.x.com/x-ads-api/campaign-management, consulted 2026-10-05. The reference page is
// too long to be fetched whole from the authoring environment, so the enum spellings below were
// confirmed against the page's search index and its example line-item response
// ("bid_strategy": "MAX", "pay_by": ..., "bid_amount_local_micro": 320000) rather than quoted in
// full; the guards fail CLOSED on any value they do not recognize, which is what keeps an
// unverified detail from becoming a wrong write.
//
// WHEN THAT IS A MAX CPC: bid_strategy MAX (a manual maximum bid; AUTO is X's automatic bidding
// and TARGET a target average) AND pay_by LINK_CLICK (the line item is charged per link click —
// X's CPLC, the pricing the WEBSITE_CLICKS objective uses). pay_by IMPRESSION makes the amount a
// CPM; for the LINK_CLICKS goal X documents both LINK_CLICK and IMPRESSION, with IMPRESSION the
// default, so an unreported or impression-billed line item is refused, never assumed per click.
//
// WHAT THE CREATE PATH SENDS MAKES THIS LEG NARROW, and that is stated rather than hidden:
// CreateCampaign creates the line item with bid_strategy AUTO (objective WEBSITE_CLICKS, no
// bid_amount_local_micro, no pay_by), so EVERY X campaign this service creates is refused by the
// dispatcher (409) until an operator moves its line item to a manual max bid charged per link
// click in X Ads Manager. This path never sends bid_strategy or pay_by.
// ---------------------------------------------------------------------------

// The bid strategy and charge unit under which bid_amount_local_micro is a max CPC.
const (
	BidStrategyMax = "MAX"
	PayByLinkClick = "LINK_CLICK"
)

// twitterMaxBid bounds the bid at the contract's ceiling (1,000,000 in the account currency),
// which keeps the ×1e6 micro conversion far from int64 overflow. X's own ceiling (the bid may
// not exceed the campaign's budget) is X's to enforce.
const twitterMaxBid = 1_000_000.0

// ErrInvalidLineItemID marks a line item id the client refuses before any request: it could not
// address an X resource path.
var ErrInvalidLineItemID = errors.New("twitter: line item id must be non-empty and contain only letters and digits")

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by BidMicros, or by X's
// definite refusal naming the bid. Read the client-safe sentence with BidAmountReason.
var ErrBidAmountInvalid = errors.New("twitter: bid amount is not acceptable")

// bidAmountError carries ONLY a client-safe sentence: X's own error text is never placed in it.
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

// BidMicros converts a bid in the account's currency to the integer micro-units
// bid_amount_local_micro carries, rounding half away from zero like toMicroCurrency: finite and
// positive, at most twitterMaxBid, and refused if it rounds to zero.
func BidMicros(amount float64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return 0, &bidAmountError{msg: "the bid must be a finite number greater than zero"}
	case amount > twitterMaxBid:
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s exceeds the largest bid this service sets on X (%s)",
			strconv.FormatFloat(amount, 'f', -1, 64), strconv.FormatFloat(twitterMaxBid, 'f', -1, 64))}
	}
	micros := toMicroCurrency(amount)
	if micros <= 0 {
		return 0, &bidAmountError{msg: fmt.Sprintf("the bid %s rounds to zero micro-units; X bids are set in micro-units, so the smallest settable bid is 0.000001",
			strconv.FormatFloat(amount, 'f', -1, 64))}
	}
	return micros, nil
}

// LineItemBid is one line item's live bid state, as GetLineItemBid read it. Strings are the raw
// values X reported, "" when not reported.
type LineItemBid struct {
	LineItemID  string
	CampaignID  string
	BidStrategy string
	PayBy       string
	// Deleted is X's soft-delete flag.
	Deleted bool
	// BidAmountMicros is bid_amount_local_micro; nil when not reported or null.
	BidAmountMicros *int64
	// BidAmountUnparseable is set when bid_amount_local_micro was reported but is not an integer.
	BidAmountUnparseable bool
}

// ManualCPC reports whether bid_amount_local_micro is a maximum cost per link click on this
// line item — the one meaning this service writes.
func (b *LineItemBid) ManualCPC() bool {
	return b.BidStrategy == BidStrategyMax && b.PayBy == PayByLinkClick
}

// lineItemBidWire is the subset of an X line item this path reads. Every value is a string,
// bool or json.RawMessage so a decode error never echoes an upstream value.
type lineItemBidWire struct {
	ID          string          `json:"id"`
	CampaignID  string          `json:"campaign_id"`
	BidStrategy string          `json:"bid_strategy"`
	PayBy       string          `json:"pay_by"`
	Deleted     bool            `json:"deleted"`
	BidAmount   json.RawMessage `json:"bid_amount_local_micro"`
}

func parseMicros(raw json.RawMessage) (*int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil, true
	}
	return &v, false
}

// lineItemPath validates both interpolated ids — the same path-injection guard
// UpdateCampaignAndChildrenStatus applies — and returns the line item's account-scoped path.
func (c *Client) lineItemPath(lineItemID string) (string, string, error) {
	if !accountIDRe.MatchString(c.account.AccountID) {
		return "", "", fmt.Errorf("twitter: account id contains characters that are not allowed in a request path")
	}
	lineItemID = strings.TrimSpace(lineItemID)
	if !accountIDRe.MatchString(lineItemID) {
		return "", "", ErrInvalidLineItemID
	}
	return "line_items/" + url.PathEscape(lineItemID), lineItemID, nil
}

// GetLineItemBid reads one line item's bid state via GET accounts/:account_id/line_items/:id
// with with_deleted=true, so a soft-deleted line item is reported as deleted rather than as a
// different failure. A PURE READ, so its failure is always definite. A 404 is (nil, nil) — X
// holds no such line item. A 2xx describing a different id is an error.
func (c *Client) GetLineItemBid(ctx context.Context, lineItemID string) (*LineItemBid, error) {
	path, lineItemID, err := c.lineItemPath(lineItemID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path+"?with_deleted=true")
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("twitter: read line item %s bid: %w", lineItemID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("twitter: read line item %s bid: the response carried no line item", lineItemID)
	}
	var wire lineItemBidWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("twitter: read line item %s bid: the response is not a line item object", lineItemID)
	}
	if got := strings.TrimSpace(wire.ID); got != lineItemID {
		if got == "" {
			got = "no line item id"
		}
		return nil, fmt.Errorf("twitter: read line item %s bid returned %s instead", lineItemID, got)
	}
	out := &LineItemBid{
		LineItemID:  lineItemID,
		CampaignID:  strings.TrimSpace(wire.CampaignID),
		BidStrategy: strings.TrimSpace(wire.BidStrategy),
		PayBy:       strings.TrimSpace(wire.PayBy),
		Deleted:     wire.Deleted,
	}
	out.BidAmountMicros, out.BidAmountUnparseable = parseMicros(wire.BidAmount)
	return out, nil
}

// UpdateLineItemBid sets an existing line item's bid_amount_local_micro via
// PUT accounts/:account_id/line_items/:id, with the parameter in the QUERY STRING per the v12
// contract (folded into the OAuth 1.0a signature by doRequest). It writes the AMOUNT only:
// bid_strategy and pay_by are never sent, so the strategy cannot be switched. The caller is
// responsible for having established ManualCPC — this function writes what it is told.
//
// It takes a slot on the client's shared WRITE PACER first, like every other X mutation.
//
// THE 429 IS NOT RETRIED IN-CALL (idempotent=false), deliberately. A throttle may be reported
// AFTER X applied the write, and a definite refusal returned by a retry answers the last attempt,
// not the first; not retrying means every throttle comes back as the 429 itself, which
// createOutcomeAmbiguous classifies UNCONFIRMED for a mutating method. So a refusal after a
// retried 429 can never be reported as "nothing changed": there is no retry to produce one.
// (UpdateCampaignBudget in budget.go takes the other route — it retries the 429 and marks a
// later definite failure retriedUnconfirmedError; this path does not retry, so needs neither.)
//
// A transport failure, 3xx, 5xx or 429 is UNCONFIRMED (IsOutcomeUnconfirmed). A definite 400
// whose machine-readable error code names a bid (and not the strategy) is returned as a
// bidAmountError with this service's own sentence — X's text is never surfaced; X does not
// document its bid error codes field by field, so this is a best-effort match whose miss is
// still truthful (a definite failure: the line item unchanged). A 2xx echo naming another line
// item or another amount is UNCONFIRMED.
func (c *Client) UpdateLineItemBid(ctx context.Context, lineItemID string, micros int64) error {
	path, lineItemID, err := c.lineItemPath(lineItemID)
	if err != nil {
		return err
	}
	if micros < 1 {
		return &bidAmountError{msg: fmt.Sprintf("bid %d micro-units is not a valid amount; it must be a positive number of micro-units", micros)}
	}
	if err := c.pace(ctx); err != nil {
		return fmt.Errorf("twitter: update line item %s bid aborted before it was sent: %w", lineItemID, err)
	}
	resp, err := c.doRequest(ctx, http.MethodPut, path,
		map[string]string{"bid_amount_local_micro": strconv.FormatInt(micros, 10)},
		false /* a 429 is unconfirmed, never retried: see above */)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusBadRequest && bidAmountErrorCode(ae.ErrorCodes) {
			return &bidAmountError{msg: "X refused this max CPC bid for the line item; it is outside the range X accepts in the account's currency (a bid may not exceed the campaign's budget)"}
		}
		return fmt.Errorf("twitter: update line item %s bid_amount_local_micro to %d: %w", lineItemID, micros, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	var echo lineItemBidWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return &transportError{Method: http.MethodPut, Path: "line item bid", err: fmt.Errorf("decode 2xx bid update response for line item %s: not a line item object", lineItemID)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != lineItemID {
		return &transportError{Method: http.MethodPut, Path: "line item bid", err: fmt.Errorf("bid update for line item %s was acknowledged for line item %s", lineItemID, got)}
	}
	if got, unparseable := parseMicros(echo.BidAmount); unparseable || (got != nil && *got != micros) {
		return &transportError{Method: http.MethodPut, Path: "line item bid", err: fmt.Errorf("bid update for line item %s was acknowledged with a bid_amount_local_micro other than the %d micro-units sent", lineItemID, micros)}
	}
	return nil
}

// bidAmountErrorCode reports whether X's machine-readable error codes name the bid amount — a
// code containing BID that is not about the bid STRATEGY.
func bidAmountErrorCode(codes []string) bool {
	for _, code := range codes {
		u := strings.ToUpper(code)
		if strings.Contains(u, "BID") && !strings.Contains(u, "STRATEGY") {
			return true
		}
	}
	return false
}
