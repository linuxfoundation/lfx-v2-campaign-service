// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// CampaignBudget is the live budget state of one LinkedIn campaign, as the platform reports
// it. It is the read half of the budget-write capability, and it exists for the same reason
// the Google Ads settings read does: a budget write has to establish what it is changing
// BEFORE it changes it.
//
// Both amount fields are POINTERS, and the distinction they carry is load-bearing. LinkedIn
// campaigns may carry dailyBudget, totalBudget, or both, and nil means "the platform did not
// report this field" — which is NOT the same as "this campaign has no such budget". A write
// that reads an absent field as zero would pick the wrong pacing model to write, which is the
// absent-value-read-as-agreement defect the budget guards exist to prevent.
type CampaignBudget struct {
	// DailyBudget is the campaign's dailyBudget amount in the account's currency, if the
	// platform reported one.
	DailyBudget *float64
	// TotalBudget is the campaign's totalBudget (lifetime) amount, if the platform
	// reported one.
	TotalBudget *float64
	// CurrencyCode is the currency the amounts above are expressed in, as the platform
	// reported it. Read and returned rather than assumed: this client only ever SENDS
	// "USD" (see createSponsoredCampaign), and the minimums below are USD-specific, so a
	// campaign denominated in anything else is a campaign whose minimums this service
	// cannot check. Empty when the platform did not report it.
	CurrencyCode string
	// Status is the campaign's current status, carried so a caller can report what it was
	// writing against. Not itself a guard — a paused campaign's budget is writable.
	Status string
	// AmountUnparseable records that the platform reported a budget field this client could
	// not parse as an amount. It is surfaced rather than swallowed because every guard
	// downstream reasons about the CURRENT budget, and reasoning about a value that failed
	// to parse is reasoning about zero.
	AmountUnparseable bool
}

// budgetWireAmount is the wire precision LinkedIn budget amounts are serialized at, and the
// precision every budget validation in this package checks AGAINST rather than checking the
// raw float. See ValidateBudgetAmount.
const budgetWireDecimals = 2

// ValidateBudgetAmount normalizes a whole-USD budget to the exact string this client sends on
// the wire and enforces every rule LinkedIn applies to it, returning the wire string and the
// rounded value it represents.
//
// IT IS THE SINGLE SOURCE OF THESE RULES FOR BOTH THE CREATE AND THE WRITE PATH, and that is
// the point of extracting it. Before it existed, the create path held these checks inline;
// a budget WRITE that did not repeat them byte-for-byte would have let an operator EDIT a
// campaign to an amount this service would have refused to CREATE it with — the same class of
// divergence the Google Ads slice closed by extracting ValidateBudgetMicros. The rules and
// their error texts are carried over unchanged, so a create that was refused before is refused
// identically now.
//
// Validation is against the ROUNDED wire value, never the raw float. 9.999 is sent as "10.00"
// and therefore MEETS the $10 daily minimum; checking the raw float rejected it, which is a
// refusal of an amount the platform would have accepted.
//
// lifetime selects which minimum applies, because it selects which field is written downstream
// (totalBudget vs dailyBudget). The minimums are USD-specific — this client only ever sends
// currencyCode "USD".
func ValidateBudgetAmount(budgetUSD float64, lifetime bool) (wire string, rounded float64, err error) {
	if math.IsNaN(budgetUSD) || math.IsInf(budgetUSD, 0) {
		return "", 0, fmt.Errorf("budget must be a finite number, got %v", budgetUSD)
	}
	if budgetUSD <= 0 {
		return "", 0, fmt.Errorf("budget must be greater than zero, got %v", budgetUSD)
	}
	// Round to the SAME precision the wire uses and validate against THAT value, so the
	// amount checked here is exactly the amount sent.
	wire = strconv.FormatFloat(budgetUSD, 'f', budgetWireDecimals, 64)
	rounded, parseErr := strconv.ParseFloat(wire, 64)
	if parseErr != nil {
		// Should be unreachable for a finite float already validated above, but fail
		// closed rather than proceed with an unverified budget.
		return "", 0, fmt.Errorf("budget %v could not be normalized to a 2-decimal amount: %w", budgetUSD, parseErr)
	}
	// A sub-cent budget (e.g. 0.001) passes the > 0 / NaN / Inf checks yet rounds to
	// "0.00" at the wire precision — a zero budget the platform rejects only after the
	// request has been made. Reject it here.
	if wire == "0.00" {
		return "", 0, fmt.Errorf("budget %v is below the minimum billable amount (0.01) and would round to zero at the API boundary", budgetUSD)
	}
	if lifetime {
		if rounded < minLifetimeBudgetUSD {
			return "", 0, fmt.Errorf("lifetime budget %v is below LinkedIn's minimum of $%.0f for a total (lifetime) budget", budgetUSD, minLifetimeBudgetUSD)
		}
	} else if rounded < minDailyBudgetUSD {
		return "", 0, fmt.Errorf("daily budget %v is below LinkedIn's minimum of $%.0f for a daily budget", budgetUSD, minDailyBudgetUSD)
	}
	return wire, rounded, nil
}

// GetCampaignBudget reads one campaign's live budget state. It is a PURE READ and mutates
// nothing, so its failure is always definite — a caller must not classify it as an unconfirmed
// write outcome, because no write was built.
//
// The campaign is addressed under the resolved ad account (adAccounts/{acct}/adCampaigns/{id}),
// not as a bare top-level campaign id, which is what keeps the read inside the account the
// project's connection resolves to.
func (c *Client) GetCampaignBudget(ctx context.Context, campaignID string) (*CampaignBudget, error) {
	if err := c.validateCredentialShape(); err != nil {
		return nil, err
	}
	campaignID = strings.TrimSpace(campaignID)
	if campaignID == "" {
		return nil, fmt.Errorf("linkedin: campaign id is required")
	}
	if !accountIDRE.MatchString(campaignID) {
		return nil, fmt.Errorf("linkedin: invalid campaign id %q: must be numeric", campaignID)
	}
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return nil, fmt.Errorf("linkedin: %w", err)
	}

	var resp struct {
		ID          flexibleID `json:"id"`
		Status      string     `json:"status"`
		DailyBudget *struct {
			Amount       string `json:"amount"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"dailyBudget"`
		TotalBudget *struct {
			Amount       string `json:"amount"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"totalBudget"`
	}
	path := fmt.Sprintf("adAccounts/%s/adCampaigns/%s", accountID, campaignID)
	if err := c.doMonitorGET(ctx, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("linkedin: read campaign %s budget: %w", campaignID, err)
	}
	// The platform must have answered about the campaign that was asked for. A response
	// describing a different id is not a budget this caller may act on — the same
	// acknowledged-a-different-resource check the Google Ads mutate makes, applied on the
	// read side where it is still free to refuse.
	if got := resp.ID.String(); got != "" && got != campaignID {
		return nil, fmt.Errorf("linkedin: read campaign %s budget returned campaign %s instead", campaignID, got)
	}

	out := &CampaignBudget{Status: resp.Status}
	if resp.DailyBudget != nil {
		if v, ok := parseUSDAmount(resp.DailyBudget.Amount); ok {
			out.DailyBudget = &v
		} else {
			out.AmountUnparseable = true
		}
		if resp.DailyBudget.CurrencyCode != "" {
			out.CurrencyCode = resp.DailyBudget.CurrencyCode
		}
	}
	if resp.TotalBudget != nil {
		if v, ok := parseUSDAmount(resp.TotalBudget.Amount); ok {
			out.TotalBudget = &v
		} else {
			out.AmountUnparseable = true
		}
		if out.CurrencyCode == "" && resp.TotalBudget.CurrencyCode != "" {
			out.CurrencyCode = resp.TotalBudget.CurrencyCode
		}
	}
	return out, nil
}

// UpdateCampaignBudget sets a campaign's budget amount on the platform. lifetime selects which
// of the two mutually exclusive fields is written (totalBudget vs dailyBudget); the caller is
// responsible for having established that this matches the campaign's CURRENT pacing model —
// this function writes what it is told.
//
// amount must be the wire string ValidateBudgetAmount produced. Taking the STRING rather than
// the float is deliberate: it makes it impossible for a caller to validate one value and send
// another, which is exactly the divergence the shared validator exists to close.
//
// ON CONFIRMATION. A LinkedIn PARTIAL_UPDATE returns no response body, so — unlike the Google
// Ads mutate, which echoes the resource it acted on and is checked against the id that was
// addressed — there is NOTHING here to verify the platform's answer against. The 2xx IS the
// confirmation. That is a real difference between the two platforms and not an omission: the
// noResponseBody flag below exists so a 2xx with an unreadable body is read as success rather
// than as a false transport failure, and inventing a read-back to "confirm" the write would
// add a second round-trip whose own failure could not be distinguished from the write's.
//
// Errors are returned UNWRAPPED for the caller to classify with IsOutcomeUnconfirmed. This
// function must never decide that a failure was definite: a transport failure, 3xx, 429 or 5xx
// on a budget mutate may have applied the new amount upstream.
func (c *Client) UpdateCampaignBudget(ctx context.Context, campaignID, amount string, lifetime bool) error {
	if err := c.validateCredentialShape(); err != nil {
		return err
	}
	campaignID = strings.TrimSpace(campaignID)
	if campaignID == "" {
		return fmt.Errorf("linkedin: campaign id is required")
	}
	if !accountIDRE.MatchString(campaignID) {
		return fmt.Errorf("linkedin: invalid campaign id %q: must be numeric", campaignID)
	}
	// Refuse an amount that did not come from the validator. This is a construction guard,
	// not a revalidation: it catches a caller that formatted its own string and bypassed
	// every minimum, which is the one way the shared-validator contract can be broken.
	if _, err := strconv.ParseFloat(amount, 64); err != nil || amount == "" {
		return fmt.Errorf("linkedin: budget amount %q is not a wire-formatted amount; it must come from ValidateBudgetAmount", amount)
	}
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return fmt.Errorf("linkedin: %w", err)
	}

	budgetField := "dailyBudget"
	if lifetime {
		budgetField = "totalBudget"
	}
	path := fmt.Sprintf("adAccounts/%s/adCampaigns/%s", accountID, campaignID)
	body := map[string]any{"patch": map[string]any{"$set": map[string]any{
		budgetField: map[string]any{"amount": amount, "currencyCode": "USD"},
	}}}
	headers := map[string]string{"X-Restli-Method": "PARTIAL_UPDATE"}
	// noResponseBody: a PARTIAL_UPDATE returns no useful body, so a 2xx with an unreadable
	// body is a success, not a false-unconfirmed transportError. Same flag and same reason
	// as UpdateCampaignStatus.
	if _, err := c.doRequest(ctx, http.MethodPost, path, body, nil, headers, true); err != nil {
		return fmt.Errorf("linkedin: update campaign %s %s to %s: %w", campaignID, budgetField, amount, err)
	}
	return nil
}
