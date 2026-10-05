// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
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

// X's campaign budget_optimization values, as GET accounts/:account_id/campaigns/:campaign_id
// reports them (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns").
//
// BudgetOptimizationCampaign is the shape CreateCampaign produces. It sends no
// budget_optimization (so X applies its default) and puts daily_budget_amount_local_micro on the
// CAMPAIGN, with no budget on the line item. X's v11 announcement made budget_optimization an
// optional campaign parameter defaulting to CAMPAIGN, and states the two shapes are exclusive:
// under CAMPAIGN the daily budget is required on the campaign and must be absent from the line
// item; under LINE_ITEM it is required on the line item and must be absent from the campaign
// (https://devcommunity.x.com/t/ads-api-version-11/168814). So a campaign this service created
// reads back CAMPAIGN, and its spend is governed by the campaign's own budget fields.
//
// BudgetOptimizationLineItem is the other documented value. Its budget lives on each line item,
// so writing the campaign's fields would not change what the caller thinks it changes; it is
// mapped only so the dispatcher can name it in a refusal. The current reference page lists
// LINE_ITEM as the only POST/PUT value and a LINE_ITEM default, which contradicts the v11
// announcement; that disagreement is exactly why an unreported or unrecognised value is refused
// rather than assumed (see CampaignBudget.BudgetOptimization).
const (
	BudgetOptimizationCampaign = "CAMPAIGN"
	BudgetOptimizationLineItem = "LINE_ITEM"
)

// BudgetField names which of the campaign's two budget amounts a write sets. The two are X's
// own field names, so the value goes on the wire verbatim.
type BudgetField string

const (
	// BudgetFieldDaily is the campaign's daily cap. CreateCampaign sets only this one.
	BudgetFieldDaily BudgetField = "daily_budget_amount_local_micro"
	// BudgetFieldTotal is the campaign's whole-flight (lifetime) cap.
	BudgetFieldTotal BudgetField = "total_budget_amount_local_micro"
)

// CampaignBudget is one campaign's live budget state, as GetCampaignBudget read it. Amounts are
// in micro-units of the ad account's own currency (X: "The currency associated with the
// specified funding instrument will be used"); this client does no FX conversion.
type CampaignBudget struct {
	CampaignID string
	// BudgetOptimization is the raw budget_optimization; "" when not reported. An unreported
	// value is NOT "CAMPAIGN": a caller must refuse rather than assume the budget is on the
	// campaign.
	BudgetOptimization string
	// DailyMicros is daily_budget_amount_local_micro; nil when absent or null.
	DailyMicros *int64
	// DailyUnparseable is set when the daily amount was reported but is not an integer. A caller
	// must refuse rather than read it as "no daily budget".
	DailyUnparseable bool
	// TotalMicros is total_budget_amount_local_micro; nil when absent or null.
	TotalMicros *int64
	// TotalUnparseable mirrors DailyUnparseable for the total amount.
	TotalUnparseable bool
}

// ErrBudgetAmountInvalid marks every refusal BudgetMicros makes, so a caller can tell "this
// amount is permanently unacceptable" apart from "the platform call failed". The dispatcher maps
// it to domain.ErrBudgetAmountRejected (400) — the same split the reddit and meta clients make.
var ErrBudgetAmountInvalid = errors.New("twitter: budget amount is not acceptable")

// budgetAmountError carries ONLY the validator's own client-safe sentence and reaches
// ErrBudgetAmountInvalid through Unwrap, mirroring reddit.budgetAmountError.
type budgetAmountError struct{ msg string }

func (e *budgetAmountError) Error() string { return e.msg }
func (e *budgetAmountError) Unwrap() error { return ErrBudgetAmountInvalid }

// BudgetAmountReason reports whether err came from BudgetMicros (or UpdateCampaignBudget's own
// amount guard) and, if so, returns the validator's own sentence — the only text from this path
// that may be shown to a caller.
func BudgetAmountReason(err error) (string, bool) {
	var amountErr *budgetAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// BudgetMicros converts a budget in the account's currency to the integer micro-units X's
// *_local_micro fields carry, with EXACTLY the rules CreateCampaign applies: finite and positive,
// at most maxBudgetUsd (which keeps the ×1e6 conversion clear of int64 overflow), rounded
// half-away-from-zero by toMicroCurrency, and refused if that rounds to zero. Sharing the bound
// and the rounding means an amount this service would refuse to create with cannot be reached by
// editing.
//
// X's reference documents no per-currency minimum or maximum for either field — only that the
// daily amount "should be less than or equal to the total_budget_amount_local_micro" — so no
// platform floor is invented here. An amount X refuses on rules it does not publish comes back
// as X's own definite 4xx on the PUT.
func BudgetMicros(amount float64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return 0, &budgetAmountError{msg: "the budget must be a finite number greater than zero"}
	case amount > maxBudgetUsd:
		return 0, &budgetAmountError{msg: fmt.Sprintf("the budget %s exceeds the largest amount this service sets on X (%s)", formatBudgetAmount(amount), formatBudgetAmount(maxBudgetUsd))}
	}
	micros := toMicroCurrency(amount)
	if micros <= 0 {
		return 0, &budgetAmountError{msg: fmt.Sprintf("the budget %s rounds to zero micro-units; X budgets are set in micro-units, so the smallest settable amount is 0.000001", formatBudgetAmount(amount))}
	}
	return micros, nil
}

// formatBudgetAmount renders an amount in plain decimal notation, never %g's exponent form,
// because it lands in a sentence a caller reads to correct their request.
func formatBudgetAmount(amount float64) string {
	return strconv.FormatFloat(amount, 'f', -1, 64)
}

// campaignBudgetPath validates both interpolated ids with the same alphanumeric guard the status
// toggle uses and returns the account-relative path of the campaign — the SAME entity
// UpdateCampaignAndChildrenStatus PUTs, so the budget write addresses exactly what the toggle
// does. The account id is the CONNECTION's, so it is refused with ErrInvalidAccountID; the
// campaign id is the persisted row's, so it is refused with ErrInvalidCampaignID. The two have
// different owners and the dispatcher sends their callers to different remedies.
func (c *Client) campaignBudgetPath(campaignID string) (string, string, error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	campaignID = strings.TrimSpace(campaignID)
	if accountID == "" || !accountIDRe.MatchString(accountID) || len(accountID) > maxAccountIDLen {
		return "", "", fmt.Errorf("twitter: campaign budget: %w", ErrInvalidAccountID)
	}
	if campaignID == "" || !campaignIDRe.MatchString(campaignID) {
		return "", "", fmt.Errorf("twitter: campaign budget: %w", ErrInvalidCampaignID)
	}
	return "campaigns/" + url.PathEscape(campaignID), campaignID, nil
}

// campaignBudgetWire is the subset of an X campaign this path reads. The amounts are kept raw so
// a present-but-unreadable value is distinguishable from an absent or null one.
//
// Keep every field a string, json.RawMessage or *bool. With only those kinds json.Unmarshal can
// fail only with an UnmarshalTypeError naming a JSON kind and this struct's own field path,
// never an upstream value, which is what lets UpdateCampaignBudget keep the decode error in its
// chain. A numeric field would echo an out-of-range literal into the error text.
type campaignBudgetWire struct {
	ID                 string          `json:"id"`
	BudgetOptimization string          `json:"budget_optimization"`
	Daily              json.RawMessage `json:"daily_budget_amount_local_micro"`
	Total              json.RawMessage `json:"total_budget_amount_local_micro"`
	Deleted            *bool           `json:"deleted"`
}

// GetCampaignBudget reads one campaign's live budget state via
// GET accounts/:account_id/campaigns/:campaign_id
// (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns"). It is a PURE READ:
// its failure is always definite and a caller must not classify it as an unconfirmed write.
//
// The path is account-scoped, and X's campaign object carries no account_id field to cross-check
// it against, so the path scoping (plus the dispatcher's provenance guard) is the account check.
//
// A 404, or a campaign X reports as deleted, is (nil, nil) — X answered and holds no live
// campaign by that id. A 2xx describing a DIFFERENT campaign id is an error: an answer about
// another campaign is not a budget this caller may act on.
func (c *Client) GetCampaignBudget(ctx context.Context, campaignID string) (*CampaignBudget, error) {
	path, campaignID, err := c.campaignBudgetPath(campaignID)
	if err != nil {
		return nil, err
	}
	resp, err := c.request(ctx, http.MethodGet, path)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("twitter: read campaign %s budget: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("twitter: read campaign %s budget: the response carried no campaign", campaignID)
	}
	var wire campaignBudgetWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("twitter: read campaign %s budget: the response is not a campaign object", campaignID)
	}
	if got := strings.TrimSpace(wire.ID); got != campaignID {
		if got == "" {
			got = "no campaign id"
		}
		return nil, fmt.Errorf("twitter: read campaign %s budget returned %s instead", campaignID, got)
	}
	if wire.Deleted != nil && *wire.Deleted {
		return nil, nil
	}
	out := &CampaignBudget{
		CampaignID:         campaignID,
		BudgetOptimization: strings.TrimSpace(wire.BudgetOptimization),
	}
	out.DailyMicros, out.DailyUnparseable = parseBudgetMicros(wire.Daily)
	out.TotalMicros, out.TotalUnparseable = parseBudgetMicros(wire.Total)
	return out, nil
}

// parseBudgetMicros reads a *_local_micro amount as integer micro-units, for the budget WRITE
// path. Absent or null yields nil ("not set" — X's own sample response carries
// "total_budget_amount_local_micro": null). A non-negative JSON integer yields its value.
// Anything else — a string, a fraction, a negative, an overflow — yields nil AND reports
// unparseable: truncating it would be a silent change to money, and reading it as absent would
// be an actively wrong answer. It accepts exactly what the monitor's parseLocalMicro accepts;
// it differs only in returning micro-units rather than currency units, because a write compares
// integers and must not round-trip through float64.
func parseBudgetMicros(raw json.RawMessage) (*int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return nil, true
	}
	return &v, false
}

// retriedUnconfirmedError marks a budget PUT whose FINAL attempt failed definitely after at
// least one earlier attempt was answered with a 429 and retried. Mirrors the microsoft client's
// type of the same name (LFXV2-2665, PR #255): idempotence makes a retry converge when it
// eventually succeeds, but it does not make a later refusal speak for the earlier attempt, which
// X may have applied before throttling. The final refusal stays reachable through Unwrap for
// logging; Unconfirmed() makes IsOutcomeUnconfirmed report true, which the dispatcher checks
// before anything else.
type retriedUnconfirmedError struct {
	retries int
	err     error
}

func (e *retriedUnconfirmedError) Error() string {
	return fmt.Sprintf("twitter: campaign budget update unconfirmed: %d earlier attempt(s) were rate-limited with an unknown outcome before this failure: %s", e.retries, e.err.Error())
}
func (e *retriedUnconfirmedError) Unwrap() error { return e.err }

// Unconfirmed marks the outcome as ambiguous-applied for IsOutcomeUnconfirmed.
func (e *retriedUnconfirmedError) Unconfirmed() bool { return true }

// UpdateCampaignBudget sets ONE of an existing campaign's budget amounts via
// PUT accounts/:account_id/campaigns/:campaign_id with field=micros
// (https://docs.x.com/x-ads-api/campaign-management/reference, "Campaigns": the PUT accepts
// daily_budget_amount_local_micro and total_budget_amount_local_micro). As on every v12 write
// this client makes, the parameter rides in the QUERY STRING and is folded into the OAuth 1.0a
// signature (see createRequest).
//
// It writes the AMOUNT only: budget_optimization, entity_status and the other amount are never
// sent, so the campaign's budget model, delivery and other cap are unchanged. The caller is
// responsible for having established that the budget lives on the campaign and that field is the
// one that carries the requested pacing — this function writes what it is told.
//
// micros must be the value BudgetMicros produced: taking the encoded integer rather than a float
// makes it impossible to validate one value and send another.
//
// PACED like every write: it reserves a slot on the client's shared write pacer (X allows one
// write per second per account), so it queues behind a concurrent create or toggle on the same
// account rather than bursting past it.
//
// OUTCOME. A PUT that sets the same amount twice converges, so a 429 is retried as the status
// toggle's is. Errors are otherwise returned for the caller to classify with
// IsOutcomeUnconfirmed: a transport failure, mutating 3xx, exhausted 429 or 5xx may have applied
// the new amount; a definite 4xx did not — UNLESS an earlier attempt was throttled and retried,
// in which case the 4xx answers only the last attempt and the outcome is reported UNCONFIRMED
// (retriedUnconfirmedError).
//
// THE 2xx ECHO IS CHECKED. When X's response names a campaign id or the written amount, each
// must be the one sent; a 2xx naming a different campaign or amount means the request reached X
// and something other than what was asked was applied, so it is returned as an UNCONFIRMED
// transportError rather than as success or as a definite failure. An echo that names neither is
// accepted: the 2xx itself is the confirmation, as it is for the status toggle.
func (c *Client) UpdateCampaignBudget(ctx context.Context, campaignID string, field BudgetField, micros int64) error {
	if field != BudgetFieldDaily && field != BudgetFieldTotal {
		return fmt.Errorf("twitter: campaign budget: unsupported budget field %q", field)
	}
	path, campaignID, err := c.campaignBudgetPath(campaignID)
	if err != nil {
		return err
	}
	if micros < 1 {
		// Client-safe: this text can reach the caller through BudgetAmountReason, so it states
		// the constraint, not which helper should have produced the value.
		return &budgetAmountError{msg: fmt.Sprintf("budget %d micro-units is not a valid amount; it must be a positive number of micro-units", micros)}
	}
	if err := c.pace(ctx); err != nil {
		// Nothing was sent: the caller's context ended while queued for a write slot.
		return fmt.Errorf("twitter: update campaign %s %s: %w", campaignID, field, err)
	}
	reqURL := c.accountURL() + "/" + path
	var retries int
	resp, err := c.doRequestAbsCounted(ctx, http.MethodPut, reqURL, path,
		map[string]string{string(field): strconv.FormatInt(micros, 10)},
		true /* idempotent: setting the same amount converges */, &retries)
	if err != nil {
		werr := fmt.Errorf("twitter: update campaign %s %s to %d: %w", campaignID, field, micros, err)
		if retries > 0 && !IsOutcomeUnconfirmed(err) {
			return &retriedUnconfirmedError{retries: retries, err: werr}
		}
		return werr
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	var echo campaignBudgetWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		// A 2xx whose data is not a campaign object says nothing about what was applied. The
		// decode error carries no upstream value only because of campaignBudgetWire's field
		// kinds; see that type before adding a field.
		return &transportError{Method: http.MethodPut, Path: "campaign budget", err: fmt.Errorf("decode 2xx budget update response for campaign %s: not a campaign object: %w", campaignID, jerr)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != campaignID {
		return &transportError{Method: http.MethodPut, Path: "campaign budget", err: fmt.Errorf("budget update for campaign %s was acknowledged for campaign %s", campaignID, got)}
	}
	raw := echo.Daily
	if field == BudgetFieldTotal {
		raw = echo.Total
	}
	if got, unparseable := parseBudgetMicros(raw); unparseable || (got != nil && *got != micros) {
		return &transportError{Method: http.MethodPut, Path: "campaign budget", err: fmt.Errorf("budget update for campaign %s was acknowledged with a %s other than the %d micro-units sent", campaignID, field, micros)}
	}
	return nil
}
