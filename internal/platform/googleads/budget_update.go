// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// campaignBudgetUpdate is the update payload for campaignBudgets:mutate. resourceName
// identifies the budget; only the fields named in the operation's updateMask are applied.
//
// Both amount fields are POINTERS and both carry omitempty, which is the whole reason this
// is a separate type from campaignBudgetCreate. The two are mutually exclusive upstream
// (campaign_settings.go treats a row carrying both as self-contradictory and refuses to read
// it), so exactly one may appear in a given request — and a non-pointer int64 would encode
// the other one as a zero, asking Google to set a second, zero-valued budget amount
// alongside the real one.
type campaignBudgetUpdate struct {
	ResourceName string `json:"resourceName"`
	// AmountMicros is campaign_budget.amount_micros, the DAILY amount.
	AmountMicros *int64 `json:"amountMicros,omitempty"`
	// TotalAmountMicros is campaign_budget.total_amount_micros, the CUSTOM_PERIOD
	// whole-flight cap.
	TotalAmountMicros *int64 `json:"totalAmountMicros,omitempty"`
}

// UpdateCampaignBudget sets an existing campaign budget's AMOUNT via campaignBudgets:mutate.
//
// It changes the amount and NOTHING ELSE. In particular it never writes
// campaign_budget.period, never writes delivery_method, and never writes explicitly_shared.
// period is a parameter here only to decide WHICH amount field carries the value — Google
// stores a DAILY budget in amount_micros and a CUSTOM_PERIOD one in total_amount_micros, and
// they are mutually exclusive — not to be applied.
//
// WHY THE PACING MODEL IS NOT WRITABLE HERE. Changing a live campaign between daily pacing
// and a whole-flight cap is a different operation from changing how much it may spend: it
// reinterprets every figure the campaign has already accumulated against, and Google's own
// vocabulary does not even name the same two ideas this service's model.BudgetType does
// (there is no LIFETIME in BudgetPeriodEnum; the counterpart is CUSTOM_PERIOD). A caller
// asking for a pacing model the budget does not already have is refused by the DISPATCHER,
// before this is reached, rather than translated into a write here. That refusal is the
// conservative half of the capability and is meant to stay conservative: adding pacing
// changes later is a deliberate decision with its own evidence, not a gap to be quietly
// closed by widening this updateMask.
//
// The caller is responsible for having established — from a readback, on this same
// connection — that the budget is NOT shared. Nothing in a campaignBudgets:mutate request
// reveals how many campaigns are attached to the budget it names, so this method cannot make
// that check itself and must not be read as having made it.
//
// The mutate IS sent as idempotent (doRequest's last arg), for exactly the reason
// UpdateCampaignStatus documents: that flag gates only bounded 429 retries, and re-applying
// the same amount converges on identical state. The create path declines the flag because a
// throttled retry there could DOUBLE-CREATE; setting a value twice has no such hazard.
func (c *Client) UpdateCampaignBudget(ctx context.Context, budgetID string, amountMicros int64, period string) error {
	if err := c.validateAccountIDs(); err != nil {
		return err
	}
	id := strings.TrimSpace(budgetID)
	if id == "" {
		return fmt.Errorf("google-ads: cannot update budget: budget id is empty")
	}
	// The id is interpolated into a resourceName, so keep it strictly numeric — Google
	// budget ids are digits, and anything else could alter the resource path. Same guard,
	// same reason, as UpdateCampaignStatus applies to a campaign id.
	if !customerIDRE.MatchString(id) {
		return fmt.Errorf("google-ads: campaign budget id %q is not numeric", budgetID)
	}
	if amountMicros <= 0 {
		// Defensive: budgetAmountMicros already refuses this, and the dispatcher converts
		// through it. Repeated here because this method is exported and a zero would be
		// accepted by Google as a real instruction to stop the campaign spending.
		return fmt.Errorf("google-ads: campaign budget amount must be > 0 micros, got %d", amountMicros)
	}

	update := campaignBudgetUpdate{
		ResourceName: "customers/" + c.account.CustomerID + "/campaignBudgets/" + id,
	}
	var mask string
	switch period {
	case budgetPeriodDaily:
		update.AmountMicros = &amountMicros
		mask = "amount_micros"
	case budgetPeriodCustom:
		update.TotalAmountMicros = &amountMicros
		mask = "total_amount_micros"
	default:
		// Includes the empty string and Google's UNKNOWN/UNSPECIFIED. An unreadable period
		// means the caller could not establish which of the two mutually exclusive amount
		// fields this budget uses, and guessing writes the wrong one — which on a
		// CUSTOM_PERIOD budget would set a daily amount alongside a whole-flight cap, the
		// self-contradictory row campaign_settings.go refuses to read back at all.
		return fmt.Errorf("google-ads: cannot update budget %s: unsupported budget period %q (want %s or %s)", id, period, budgetPeriodDaily, budgetPeriodCustom)
	}

	req := mutateRequest{Operations: []mutateOperation{{Update: update, UpdateMask: mask}}}
	body, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignBudgets:mutate"), req, true)
	if err != nil {
		return fmt.Errorf("google-ads campaign budget %s amount update failed: %w", id, err)
	}
	// A 2xx is not yet a confirmed write. The caller's contract is that a nil return means the
	// platform APPLIED the change — it persists the new amount on that basis — so a success
	// carrying no result, or a result naming a DIFFERENT budget, must not be reported as one.
	// Both are answered as ambiguous rather than definite: the request reached Google and was
	// accepted, so "nothing was modified" is precisely the claim that cannot be made.
	rn, rid, err := firstResourceName(body)
	if err != nil {
		return &unconfirmedBudgetMutateError{err: fmt.Errorf("google-ads campaign budget %s amount update returned an unusable response: %w", id, err)}
	}
	if rid != id {
		return &unconfirmedBudgetMutateError{err: fmt.Errorf("google-ads campaign budget %s amount update was acknowledged for a different resource %q", id, rn)}
	}
	return nil
}

// unconfirmedBudgetMutateError marks a budget mutate that Google ACCEPTED but did not
// acknowledge usably — no result, or a result naming another resource. The change may have
// been applied, so it satisfies the same Unconfirmed() behavioral interface
// IsOutcomeUnconfirmed honors, and the dispatcher's classification carries it through to the
// service's "verify upstream before retrying" answer rather than "nothing was modified".
type unconfirmedBudgetMutateError struct{ err error }

func (e *unconfirmedBudgetMutateError) Error() string     { return e.err.Error() }
func (e *unconfirmedBudgetMutateError) Unwrap() error     { return e.err }
func (e *unconfirmedBudgetMutateError) Unconfirmed() bool { return true }
