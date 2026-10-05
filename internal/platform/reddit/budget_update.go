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
	"strconv"
	"strings"
)

// Reddit's campaign-level budget goals, as the create path sends them (goal_type). The create
// path only ever sends GoalTypeLifetimeSpend; GoalTypeDailySpend is the daily counterpart in
// Reddit's published campaign model and is mapped so that a campaign READ as daily is described
// truthfully. Any other value is a budget shape this service has no mapping for.
const (
	GoalTypeLifetimeSpend = "LIFETIME_SPEND"
	GoalTypeDailySpend    = "DAILY_SPEND"
)

// CampaignBudget is one campaign's live budget state, as GetCampaignBudget read it.
//
// WHERE THE BUDGET LIVES. CreateCampaign sends is_campaign_budget_optimization=true together
// with goal_type and goal_value on the CAMPAIGN, and sends no budget on the ad group — so the
// budget this service sets is the campaign's goal_value, in micro-units of the account currency.
// CampaignBudgetOptimization records whether that is still true upstream: with it off, the
// spend is governed per ad group and the campaign's goal is not the budget a caller is changing.
type CampaignBudget struct {
	CampaignID string
	// AdAccountID is the account Reddit reports the campaign under, "" when not reported.
	AdAccountID string
	// GoalType is the raw goal_type; "" when not reported.
	GoalType string
	// GoalValueMicros is goal_value; nil when not reported.
	GoalValueMicros *int64
	// GoalValueUnparseable is set when goal_value was reported but is not an integer. A caller
	// must refuse rather than read it as "no budget": the field exists, it just could not be read.
	GoalValueUnparseable bool
	// CampaignBudgetOptimization is is_campaign_budget_optimization; nil when not reported. An
	// unreported flag is NOT "on": a caller must refuse rather than assume the budget is on the
	// campaign.
	CampaignBudgetOptimization *bool
}

// ErrBudgetAmountInvalid marks every refusal BudgetMicros makes, so a caller can tell "this
// amount is permanently unacceptable" apart from "the platform call failed". The dispatcher maps
// it to domain.ErrBudgetAmountRejected (400) — the same split the Meta and LinkedIn clients make.
var ErrBudgetAmountInvalid = errors.New("reddit: budget amount is not acceptable")

// budgetAmountError carries ONLY the validator's own client-safe sentence and reaches
// ErrBudgetAmountInvalid through Unwrap, mirroring meta.budgetAmountError.
type budgetAmountError struct{ msg string }

func (e *budgetAmountError) Error() string { return e.msg }
func (e *budgetAmountError) Unwrap() error { return ErrBudgetAmountInvalid }

// BudgetAmountReason reports whether err came from BudgetMicros and, if so, returns the
// validator's own sentence — the only text from this path that may be shown to a caller.
func BudgetAmountReason(err error) (string, bool) {
	var amountErr *budgetAmountError
	if errors.As(err, &amountErr) {
		return amountErr.msg, true
	}
	return "", false
}

// BudgetMicros converts a budget in the account's currency to the integer micro-units Reddit's
// goal_value carries, with EXACTLY the rules CreateCampaign applies: finite and positive, at most
// redditMaxBudgetUSD (which keeps the ×1e6 conversion clear of int64 overflow), rounded
// half-away-from-zero to the nearest micro by toMicrodollars, and refused if that rounds to zero.
// Sharing the bound and the rounding means an amount this service would refuse to create with
// cannot be reached by editing.
func BudgetMicros(amount float64) (int64, error) {
	switch {
	case math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0:
		return 0, &budgetAmountError{msg: "the budget must be a finite number greater than zero"}
	case amount > redditMaxBudgetUSD:
		return 0, &budgetAmountError{msg: fmt.Sprintf("the budget %s exceeds the largest amount this service sets on Reddit (%s)", formatBudgetAmount(amount), formatBudgetAmount(redditMaxBudgetUSD))}
	}
	micros := toMicrodollars(amount)
	if micros <= 0 {
		return 0, &budgetAmountError{msg: fmt.Sprintf("the budget %s rounds to zero micro-units; Reddit budgets are set in micro-units, so the smallest settable amount is 0.000001", formatBudgetAmount(amount))}
	}
	return micros, nil
}

// formatBudgetAmount renders an amount for a client-facing reason in plain decimal notation —
// never %g's exponent form, which turns 2e9 into "2e+09" and 1e-7 into "1e-07" in a sentence
// a caller reads to correct their request. -1 precision is the shortest exact representation.
func formatBudgetAmount(amount float64) string {
	return strconv.FormatFloat(amount, 'f', -1, 64)
}

// campaignBudgetPath validates both interpolated ids with the letters/digits/underscores guard
// the status toggle uses and returns the campaign's resource path — the SAME path
// updateEntityStatus PATCHes, so the budget write addresses exactly the entity the toggle does.
func (c *Client) campaignBudgetPath(campaignID string) (string, string, error) {
	accountID := strings.TrimSpace(c.account.AccountID)
	campaignID = strings.TrimSpace(campaignID)
	if accountID == "" || !accountIDRe.MatchString(accountID) {
		return "", "", fmt.Errorf("reddit: campaign budget: %w", ErrInvalidAccountID)
	}
	if campaignID == "" || !accountIDRe.MatchString(campaignID) {
		return "", "", fmt.Errorf("reddit: campaign budget: %w", ErrInvalidCampaignID)
	}
	return "/ad_accounts/" + accountID + "/campaigns/" + campaignID, campaignID, nil
}

// campaignBudgetWire is the subset of a Reddit campaign this path reads. goal_value is kept raw
// so a present-but-unreadable value is distinguishable from an absent one.
type campaignBudgetWire struct {
	ID          string          `json:"id"`
	AdAccountID string          `json:"ad_account_id"`
	GoalType    string          `json:"goal_type"`
	GoalValue   json.RawMessage `json:"goal_value"`
	CBO         *bool           `json:"is_campaign_budget_optimization"`
}

// GetCampaignBudget reads one campaign's live budget state via
// GET /ad_accounts/{accountID}/campaigns/{campaignID}. It is a PURE READ: its failure is always
// definite and a caller must not classify it as an unconfirmed write.
//
// A 404 is reported as (nil, nil) — Reddit answered and holds no such campaign. A 2xx describing
// a DIFFERENT campaign id is an error: an answer about another campaign is not a budget this
// caller may act on.
func (c *Client) GetCampaignBudget(ctx context.Context, campaignID string) (*CampaignBudget, error) {
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
		return nil, fmt.Errorf("reddit: read campaign %s budget: %w", campaignID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil, fmt.Errorf("reddit: read campaign %s budget: the response carried no campaign", campaignID)
	}
	var wire campaignBudgetWire
	if err := json.Unmarshal(resp.Data, &wire); err != nil {
		return nil, fmt.Errorf("reddit: read campaign %s budget: the response is not a campaign object", campaignID)
	}
	if got := strings.TrimSpace(wire.ID); got != campaignID {
		if got == "" {
			got = "no campaign id"
		}
		return nil, fmt.Errorf("reddit: read campaign %s budget returned %s instead", campaignID, got)
	}
	out := &CampaignBudget{
		CampaignID:                 campaignID,
		AdAccountID:                strings.TrimSpace(wire.AdAccountID),
		GoalType:                   strings.TrimSpace(wire.GoalType),
		CampaignBudgetOptimization: wire.CBO,
	}
	out.GoalValueMicros = parseGoalValue(wire.GoalValue, &out.GoalValueUnparseable)
	return out, nil
}

// parseGoalValue reads goal_value. Absent or null yields nil ("not reported"). An integer — as a
// JSON number or, defensively, a decimal string — yields its value. Anything else, including a
// number with a fractional part, yields nil AND sets the unparseable flag: truncating it would be
// a silent change to money, and reading it as absent would be an actively wrong answer.
func parseGoalValue(raw json.RawMessage, unparseable *bool) *int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if unq, err := strconv.Unquote(s); err == nil {
		s = strings.TrimSpace(unq)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*unparseable = true
		return nil
	}
	return &v
}

// UpdateCampaignBudget sets an existing campaign's goal_value via
// PATCH /ad_accounts/{accountID}/campaigns/{campaignID} with {"data":{"goal_value": micros}}.
// It writes the AMOUNT only: goal_type is never sent, so the campaign's pacing is unchanged, and
// no schedule field is sent, so the flight is unchanged. The caller is responsible for having
// established that the campaign's budget lives on the campaign (CBO on) and that its goal type
// matches the requested pacing — this function writes what it is told.
//
// micros must be the value BudgetMicros produced: taking the encoded integer rather than a float
// makes it impossible to validate one value and send another.
//
// A PATCH that sets the same goal_value twice converges, so request() retries a 429 here as the
// status toggle does; an exhausted throttle is still UNCONFIRMED. Errors are otherwise returned
// for the caller to classify with IsOutcomeUnconfirmed — a transport failure, 3xx, 429 or 5xx
// may have applied the new amount.
//
// THE 2xx ECHO IS CHECKED. When Reddit's response names a campaign id or a goal_value, each must
// be the one sent; a 2xx naming a different campaign or amount means the request reached Reddit
// and something was applied that is not what was asked, so it is returned as an UNCONFIRMED
// transportError rather than as success or as a definite failure. An echo that names neither is
// accepted: the 2xx itself is the confirmation, as it is for the status toggle.
func (c *Client) UpdateCampaignBudget(ctx context.Context, campaignID string, micros int64) error {
	path, campaignID, err := c.campaignBudgetPath(campaignID)
	if err != nil {
		return err
	}
	if micros < 1 {
		return fmt.Errorf("reddit: budget %d micro-units is not a valid amount; it must come from BudgetMicros", micros)
	}
	body := map[string]any{"data": map[string]any{"goal_value": micros}}
	resp, err := c.request(ctx, http.MethodPatch, path, body)
	if err != nil {
		return fmt.Errorf("reddit: update campaign %s goal_value to %d: %w", campaignID, micros, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return nil
	}
	var echo campaignBudgetWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		// A 2xx whose data is not a campaign object says nothing about what was applied.
		return &transportError{Method: http.MethodPatch, Path: "campaign budget", Err: fmt.Errorf("decode 2xx budget update response for campaign %s: not a campaign object", campaignID)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != campaignID {
		return &transportError{Method: http.MethodPatch, Path: "campaign budget", Err: fmt.Errorf("budget update for campaign %s was acknowledged for campaign %s", campaignID, got)}
	}
	var unparseable bool
	if got := parseGoalValue(echo.GoalValue, &unparseable); unparseable || (got != nil && *got != micros) {
		return &transportError{Method: http.MethodPatch, Path: "campaign budget", Err: fmt.Errorf("budget update for campaign %s was acknowledged with a goal_value other than the %d micro-units sent", campaignID, micros)}
	}
	return nil
}
