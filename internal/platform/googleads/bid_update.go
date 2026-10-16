// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
)

// ErrBidAmountInvalid marks a bid refused because of the AMOUNT — by this client's own bounds.
// Read the client-safe sentence with BidAmountReason. Mirrors the Microsoft adapter's sentinel
// of the same name, because the dispatcher's classification arms are written against the same
// two questions on every platform: was it the amount, and was anything applied.
//
// Nothing is sent when this is returned: every amount check runs before adGroups:mutate is
// built.
var ErrBidAmountInvalid = errors.New("google-ads: bid amount is not accepted")

// bidAmountError carries ONLY a client-safe sentence — the amount and this client's published
// bound, never anything about the account.
type bidAmountError struct{ msg string }

func (e *bidAmountError) Error() string { return e.msg }
func (e *bidAmountError) Unwrap() error { return ErrBidAmountInvalid }

// BidAmountReason reports whether err is a bid-amount refusal from this package and, if so,
// returns its sentence. Mirrors microsoft.BidAmountReason so the dispatcher's 400 carries the
// adapter's own words rather than a rendered error chain.
func BidAmountReason(err error) (string, bool) {
	var amountErr *bidAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// ValidateMaxCPCBid applies the bounds the CREATE path applies to an ad-group CPC bid
// (minCPCBid..maxCPCBid in the ad account's currency), so a bid this service would refuse to
// create with cannot be reached by editing, and converts it to Google's micros.
//
// IT IS NOT validateCPCBid, and must not be routed through it. That function's contract is
// "0 means the caller supplied NO bid", which it answers (0, nil) so adGroupCreate's
// `cpcBidMicros,omitempty` drops the field. An EDIT always names a bid: there is no field to
// omit, 0 is a request to bid nothing, and Google would accept it as one. Reusing the create
// validator here would therefore send an updateMask naming cpc_bid_micros with a zero value and
// stop the ad group bidding — reported to the caller as a successful bid change. So zero is
// refused by the floor below, like any other out-of-range amount.
//
// NaN/Inf are rejected before any range comparison, because every comparison against NaN is
// false — a NaN bid would slip past both bounds and then round to a garbage int64.
//
// The rounding is math.Round rather than truncation, for validateCPCBid's reason: a bid of 0.07
// is 0.07000000000000001 in float64, and truncating micros would bid a cent less than asked on
// every such value.
func ValidateMaxCPCBid(bid float64) (int64, error) {
	switch {
	case math.IsNaN(bid) || math.IsInf(bid, 0):
		return 0, &bidAmountError{msg: "the Google Ads max CPC bid must be a finite number"}
	case bid < minCPCBid:
		return 0, &bidAmountError{msg: fmt.Sprintf("the Google Ads max CPC bid %.4f is below the minimum %.2f in the account currency this service sets", bid, minCPCBid)}
	case bid > maxCPCBid:
		return 0, &bidAmountError{msg: fmt.Sprintf("the Google Ads max CPC bid %.2f exceeds the maximum %.0f in the account currency this service sets", bid, maxCPCBid)}
	}
	return int64(math.Round(bid * microsPerUnit)), nil
}

// adGroupCPCBidUpdate is the update payload for adGroups:mutate. resourceName identifies the ad
// group; only the fields named in the operation's updateMask are applied.
//
// CpcBidMicros is a plain int64 with NO omitempty, unlike adGroupCreate's field, and the
// difference is the whole point of the two types being separate. On the create path the zero
// value means "the caller named no bid" and the field must disappear; here the field is named by
// the updateMask, so omitting it from the body would ask Google to set cpc_bid_micros to its
// default — a zero bid — while the caller reads back a success. UpdateAdGroupCPCBid refuses a
// non-positive amount before building this, so the value emitted is always a real bid.
type adGroupCPCBidUpdate struct {
	ResourceName string `json:"resourceName"`
	CpcBidMicros int64  `json:"cpcBidMicros"`
}

// UpdateAdGroupCPCBid sets an existing ad group's DEFAULT CPC BID via adGroups:mutate.
//
// It changes ad_group.cpc_bid_micros and NOTHING ELSE — never the ad group's status, name or
// type, and in particular never the campaign's bidding strategy. A bid is only honoured under
// MANUAL_CPC (see bidding.go: under an automated strategy Google KEEPS the ad-group bid and
// never bids it), and establishing that the campaign is on that strategy is the CALLER's
// responsibility, from a readback on this same connection. Nothing in an adGroups:mutate request
// reveals the parent campaign's strategy, so this method cannot make that check itself and must
// not be read as having made it.
//
// The mutate IS sent as idempotent (doRequest's last arg), for UpdateCampaignBudget's reason:
// that flag gates only bounded 429 retries, and re-applying the same bid converges on identical
// state. The create path declines the flag because a throttled retry there could DOUBLE-CREATE;
// setting a value twice has no such hazard.
func (c *Client) UpdateAdGroupCPCBid(ctx context.Context, adGroupID string, bidMicros int64) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	id := strings.TrimSpace(adGroupID)
	if id == "" {
		return fmt.Errorf("google-ads: cannot update bid: ad group id is empty")
	}
	// The id is interpolated into a resourceName, so keep it strictly numeric — Google ad group
	// ids are digits, and anything else could alter the resource path. Same guard, same reason,
	// as UpdateCampaignBudget applies to a budget id.
	if !customerIDRE.MatchString(id) {
		return fmt.Errorf("google-ads: ad group id %q is not numeric", adGroupID)
	}
	if bidMicros <= 0 {
		// Defensive: ValidateMaxCPCBid already refuses this, and the dispatcher converts
		// through it. Repeated here because this method is exported and Google would accept a
		// zero as a real instruction to stop the ad group bidding.
		return fmt.Errorf("google-ads: ad group cpc bid must be > 0 micros, got %d", bidMicros)
	}

	req := mutateRequest{Operations: []mutateOperation{{
		Update:     adGroupCPCBidUpdate{ResourceName: "customers/" + c.account.CustomerID + "/adGroups/" + id, CpcBidMicros: bidMicros},
		UpdateMask: "cpc_bid_micros",
	}}}
	body, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroups:mutate"), req, true)
	if err != nil {
		return fmt.Errorf("google-ads ad group %s cpc bid update failed: %w", id, err)
	}
	// A 2xx is not yet a confirmed write, and the caller's contract is that a nil return means
	// the platform APPLIED the change — it persists the new bid on that basis. Both failures
	// below are answered as AMBIGUOUS rather than definite: the request reached Google and was
	// accepted, so "nothing was modified" is precisely the claim that cannot be made.
	rn, rid, err := firstResourceName(body)
	if err != nil {
		return &unconfirmedBidMutateError{err: fmt.Errorf("google-ads ad group %s cpc bid update returned an unusable response: %w", id, err)}
	}
	// THE WHOLE RESOURCE IS VALIDATED, NOT JUST ITS TRAILING ID, and this runs BEFORE the id
	// comparison for UpdateCampaignBudget's reason: firstResourceName extracts the last path
	// segment, so an acknowledgement naming customers/OTHER/adGroups/123 or
	// customers/{ours}/campaigns/123 carries the same trailing id as the ad group that was
	// addressed and would pass an id-only comparison.
	if vErr := c.validateResourceKind("adGroups", rn, true); vErr != nil {
		return &unconfirmedBidMutateError{err: fmt.Errorf("google-ads ad group %s cpc bid update was acknowledged with an untrustworthy resource: %w", id, vErr)}
	}
	if rid != id {
		return &unconfirmedBidMutateError{err: fmt.Errorf("google-ads ad group %s cpc bid update was acknowledged for a different resource %q", id, rn)}
	}
	return nil
}

// unconfirmedBidMutateError marks a bid mutate that Google ACCEPTED but did not acknowledge
// usably — no result, or a result naming another resource. The change may have been applied, so
// it satisfies the same Unconfirmed() behavioral interface IsOutcomeUnconfirmed honors, and the
// dispatcher's classification carries it through to the service's "verify upstream before
// retrying" answer rather than "nothing was modified".
type unconfirmedBidMutateError struct{ err error }

func (e *unconfirmedBidMutateError) Error() string     { return e.err.Error() }
func (e *unconfirmedBidMutateError) Unwrap() error     { return e.err }
func (e *unconfirmedBidMutateError) Unconfirmed() bool { return true }
