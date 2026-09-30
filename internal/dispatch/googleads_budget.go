// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// WriteBudget implements service.BudgetWriter for Google Ads: it changes the AMOUNT of the
// budget the campaign is attached to, after establishing that doing so moves this campaign's
// spend and nothing else's.
//
// THE SHAPE OF THIS METHOD IS READ-THEN-WRITE, and the read is not an optimisation. Google
// models the budget as a SEPARATE RESOURCE attached to the campaign (campaign_budget), and
// three facts a write cannot proceed without live only on that resource:
//
//  1. WHICH budget resource the campaign is attached to right now. The write has to address
//     something, and the campaign id does not address it.
//  2. WHETHER that budget is shared across campaigns (campaign_budget.explicitly_shared). A
//     shared budget written through one campaign silently changes every other campaign
//     attached to it — including campaigns in this account that this service did not create,
//     cannot see, and was never asked about.
//  3. WHICH of the two mutually exclusive amount fields the budget uses, which is what
//     campaign_budget.period says. Writing the wrong one produces the self-contradictory row
//     (both amount_micros and total_amount_micros set) that GetCampaignSettings refuses to
//     read back at all — leaving a campaign whose budget this service can no longer report.
//
// GetCampaignSettings answers all three in ONE query, on the connection the write will use.
// That is why this capability was worth building on top of SettingsReader rather than beside
// it: the read that makes a budget divergence legible is the same read that makes writing
// the budget safe.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED. Each refusal below returns before
// campaignBudgets:mutate is built, so a caller seeing one of them knows the platform is
// untouched — which is what lets the service answer them 409 rather than "verify upstream".
func (d *GoogleAdsDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// Provenance, in the same TWO ARMS and the same ORDER as ReadSettings, for the reasons
	// set out in full there. They are not repeated here, but one difference is worth
	// stating: ReadSettings fails closed on absent provenance because the cost of resolving
	// the wrong campaign is a confidently wrong REPORT. Here the cost is changing the budget
	// of a campaign in another account. The guard is therefore identical and its strictness
	// is, if anything, more clearly justified on this path than on the one it was written
	// for — nothing about it may be relaxed to make a write more convenient.
	created := googleAdsCreationCustomerID(campaign)
	if created == "" {
		return fmt.Errorf("write google ads campaign budget: campaign %s does not record which customer it was created under, so its id cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return err
	}
	if created != client.CustomerID() {
		return fmt.Errorf("write google ads campaign budget: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			campaign.PlatformCampaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	settings, err := client.GetCampaignSettings(ctx, campaign.PlatformCampaignID)
	if err != nil {
		return fmt.Errorf("write google ads campaign budget: read current budget: %w", err)
	}
	if settings == nil {
		// The platform answered and holds no such campaign. Reported the same way
		// ReadSettings reports it, and for a sharper reason: there is nothing to write to.
		return fmt.Errorf("%w: google-ads campaign %s", domain.ErrPlatformCampaignAbsent, campaign.PlatformCampaignID)
	}

	// GUARD 1 — SHARED BUDGET. Checked FIRST among the budget guards, and note what the nil
	// case does: an UNREAD explicitly_shared is refused, not assumed false. "We could not
	// establish that this budget is private" and "this budget is private" are opposite
	// facts, and only one of them justifies a write that could move a stranger's spend.
	// This is the same absent-value-read-as-agreement defect the readback documents
	// throughout, in the one place where getting it wrong costs money rather than accuracy.
	if settings.BudgetExplicitlyShared == nil {
		return fmt.Errorf("write google ads campaign budget: campaign %s did not report whether its budget is shared, so whether this write would change other campaigns cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}
	if *settings.BudgetExplicitlyShared {
		return fmt.Errorf("write google ads campaign budget: campaign %s is attached to a SHARED budget, so changing its amount would change every other campaign attached to that budget — including campaigns this request did not name; give the campaign its own budget in Google Ads, or make the change there where its full effect is visible: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetShared)
	}

	// GUARD 2 — ADDRESSABILITY. The budget id comes from the platform's own answer, never
	// from the campaign row's stored CampaignBudgetID: an adopted campaign has no stored id,
	// and a campaign whose budget was swapped in Google's UI has a stale one. A write aimed
	// at a stale budget id is a write to a budget this campaign is no longer attached to,
	// which is the shared-budget failure wearing different clothes.
	if settings.BudgetID == nil || *settings.BudgetID == "" {
		return fmt.Errorf("write google ads campaign budget: campaign %s did not report the budget resource it is attached to, so there is nothing to address: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 3 — THE PACING MODEL MUST ALREADY MATCH. This capability changes HOW MUCH a
	// campaign may spend, never HOW it is paced.
	//
	// Refusing the mismatch is the deliberate choice, not a limitation waiting to be lifted.
	// Switching a live campaign between daily pacing and a whole-flight cap reinterprets
	// every figure it has already spent against, and the two vocabularies are not even
	// parallel — Google's BudgetPeriodEnum has no LIFETIME, only CUSTOM_PERIOD, which is a
	// narrower and largely legacy thing than this service's "lifetime" suggests. Translating
	// across that gap on a spending campaign, silently, as a side effect of an amount
	// change, is not a thing to do without an operator seeing it stated.
	//
	// An UNREADABLE period is refused for the same reason the unread shared flag is: it is
	// what selects between the two mutually exclusive amount fields, and a guess writes the
	// wrong one.
	if settings.BudgetPeriod == nil {
		return fmt.Errorf("write google ads campaign budget: campaign %s did not report its budget period, so which of the two mutually exclusive amount fields to write cannot be established: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}
	upstreamType := googleAdsBudgetTypeFromPeriod(*settings.BudgetPeriod)
	if upstreamType == "" {
		// Google's UNKNOWN/UNSPECIFIED, or a value added after this client's pinned version.
		return fmt.Errorf("write google ads campaign budget: campaign %s reports budget period %q, which this service has no mapping for: %w",
			campaign.PlatformCampaignID, *settings.BudgetPeriod, domain.ErrBudgetUnwritable)
	}
	if upstreamType != budget.Type {
		return fmt.Errorf("write google ads campaign budget: campaign %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in Google Ads, then set the amount here: %w",
			campaign.PlatformCampaignID, upstreamType, budget.Type, domain.ErrBudgetUnwritable)
	}

	// Converted through the SAME validation the create path uses (one extracted helper), so
	// an amount this service would refuse to create with cannot be reached by editing.
	micros, err := googleads.ValidateBudgetMicros(budget.Amount)
	if err != nil {
		return fmt.Errorf("write google ads campaign budget: %w", err)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified, unlike the settings read above: that read changes nothing,
	// so its failure is definite, but a timeout, 5xx or dropped connection on
	// campaignBudgets:mutate may leave the new amount applied upstream. Google Ads carries that
	// ambiguity in the error's SHAPE, not in an Unconfirmed() method, so an unwrapped return
	// here would reach the service as a definite failure and be answered "the campaign was not
	// modified" — an affirmative false claim about a money-moving write, with the claim lock
	// released inline instead of held through the cooldown. Same wrap every other mutating
	// Google Ads path uses (see wrapUnconfirmed in googleads.go).
	if err := client.UpdateCampaignBudget(ctx, *settings.BudgetID, micros, *settings.BudgetPeriod); err != nil {
		werr := fmt.Errorf("write google ads campaign budget for campaign %s: %w", campaign.PlatformCampaignID, err)
		if googleads.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}

// unconfirmedBudgetWriteError wraps a budget mutate whose platform outcome is unknowable (the
// new amount may have been applied). It is a sibling of unconfirmedToggleError rather than a
// reuse of it: both satisfy the same Unconfirmed() behavioral interface the service detects with
// errors.As, but that type's message says "status change", and a budget write reported in a log
// as a status change misdirects whoever reads it during exactly the incident it exists for.
type unconfirmedBudgetWriteError struct{ err error }

func (e *unconfirmedBudgetWriteError) Error() string {
	return "budget write outcome is unconfirmed (it may have been applied): " + e.err.Error()
}
func (e *unconfirmedBudgetWriteError) Unwrap() error     { return e.err }
func (e *unconfirmedBudgetWriteError) Unconfirmed() bool { return true }
