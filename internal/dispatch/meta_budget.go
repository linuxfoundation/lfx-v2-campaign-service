// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

// WriteBudget implements service.BudgetWriter for Meta: it changes the AMOUNT of the campaign's
// budget, after establishing that doing so moves this campaign's spend and nothing else's.
//
// IT IS READ-THEN-WRITE like the Google Ads and LinkedIn implementations, but Meta's budget
// model differs from both in two ways that decide the guards below:
//
//   - THE BUDGET IS NOT ON THE CAMPAIGN. It is on the AD SET, so the object written is the ad
//     set this service created under the campaign, read from the persisted result blob. A row
//     that does not record one cannot be written at all.
//   - THE BUDGET MAY NOT BE ON THE AD SET EITHER. With Campaign Budget Optimization the amount
//     sits on the CAMPAIGN and is distributed across every ad set beneath it. That is Meta's
//     form of Google's shared budget — the guard LinkedIn has no analogue of reappears here —
//     and it is refused for the same reason: the request named one campaign, and the write
//     would move spend the caller never named and may not be able to see.
//
// Amounts are handled in the ACCOUNT CURRENCY'S MINOR UNITS throughout, resolved from the
// account's own currency by the same encoder the create path uses, so an amount this service
// would refuse to create with cannot be reached by editing.
//
// NOTHING IS MUTATED UNTIL EVERY GUARD HAS PASSED, so a caller seeing any refusal below knows
// the platform is untouched — which is what lets the service answer them 409 rather than
// "verify upstream".
func (d *MetaDispatcher) WriteBudget(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, budget model.BudgetChange) error {
	// PROVENANCE, FAILED CLOSED, BEFORE ANYTHING ELSE.
	//
	// This guard is STRICTER than verifyMetaAccountMatch, and the difference is the point.
	// That helper treats BOTH unknowns as "proceed": an absent CREATED id (the pre-existing
	// row case) and an absent CURRENT account, which Meta's toggle and metrics paths
	// deliberately tolerate because they address the campaign node by id and need no account
	// at all. A budget write can accept neither. BudgetWriter's contract requires the
	// account-identity invariant to be enforced AT LEAST AS STRICTLY as the read path
	// enforces it, and the cost of being wrong here is changing the budget of an ad set in
	// another account rather than returning a misleading report.
	//
	// The CURRENT id is additionally load-bearing for a reason no other Meta path has: the
	// account's CURRENCY decides the minor-unit scale the amount is encoded in. With no
	// account there is no currency, and no scale that could be assumed without risking a
	// budget encoded 100x wrong.
	//
	// So both absences are refused HERE, and the shared helper is still called BELOW to
	// answer the mismatch case in the one wording every adapter uses. Calling the helper
	// alone would silently inherit a contract this path cannot accept.
	created := metaCreationAccountID(campaign)
	if created == "" {
		return fmt.Errorf("write meta campaign budget: campaign %s does not record which ad account it was created under, so its ad set cannot be resolved against any account: %w",
			campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}

	res, creds, err := d.resolveMetaCredentials(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return err
	}
	// NO ACCOUNT SELECTED IS NOT UNKNOWN PROVENANCE, and the distinction is the whole
	// remedy. The campaign DID record its creating account — the guard above has already
	// refused the case where it did not — so what is missing is a selection on the
	// CONNECTION, and the fix is to save an ad account id on it, not to re-dispatch the
	// campaign. Joining ErrCampaignProvenanceUnknown here would claim the opposite and send
	// an operator to repair a row that is correct.
	//
	// requireMetaAccountID is the one place that wording and that classification live:
	// ErrConnectionNotUsable decides the status, ErrAccountNotSelected names the remedy —
	// which the service answers 409 "save an ad account id on the connection" — and
	// res.systemScoped keeps a failure on the LF SYSTEM row from being reported against the
	// caller's project. The hand-rolled refusal this replaces had all three wrong.
	//
	// The selection is load-bearing here for a reason no other Meta path has: the account's
	// CURRENCY decides the minor-unit scale the amount is encoded in, so proceeding without
	// one risks a budget written at 100x.
	accountID, err := requireMetaAccountID(res, projectID)
	if err != nil {
		return fmt.Errorf("write meta campaign budget: %w", err)
	}
	client := d.cachedMetaClient(projectID, platform, res, creds)
	if err := verifyMetaAccountMatch("write meta campaign budget", campaign, accountID); err != nil {
		return err
	}

	// The object written is the AD SET, not the campaign. A row whose persisted result does
	// not name one is unprovisioned for this purpose: there is nothing addressable to write,
	// and the refusal is deterministic, so it is a 409 rather than the default 503.
	adSetID := metaAdSetID(campaign)
	if strings.TrimSpace(adSetID) == "" {
		return fmt.Errorf("%w: meta campaign %s records no ad set, and a meta budget lives on the ad set, so there is nothing to write", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}

	current, err := client.GetAdSetBudget(ctx, adSetID)
	if err != nil {
		// A PURE READ. Its failure is DEFINITE — no mutate was built — so it is returned
		// unclassified. Marking it unconfirmed would send an operator to verify a write
		// that never existed.
		return fmt.Errorf("write meta campaign budget: read current budget: %w", err)
	}
	if current == nil {
		return fmt.Errorf("%w: meta ad set %s", domain.ErrPlatformCampaignAbsent, adSetID)
	}

	// That `current` describes the ad set that was ASKED about is the premise every guard
	// below rests on, and it is already enforced where it belongs: GetAdSetBudget refuses an
	// answer whose echoed id is not the one requested, so a mismatch never reaches this line.
	// Stated here because the guards below are only sound given it.
	//
	// GUARD 0 — THE AD SET MUST BELONG TO THE CAMPAIGN THIS REQUEST NAMED.
	//
	// Every guard above establishes that the ACCOUNT is right; none of them establishes
	// that the AD SET is. The ad set id is read from this service's own persisted row, and
	// the write is addressed to it directly, so a stale or corrupted ad_set_id sends the
	// amount to whatever ad set now holds that id — and inside one account (the shared LF
	// system account most of all) that is another campaign's ad set, with every account
	// check passing on the way there. The platform itself reports the owner, so this is a
	// fact to be checked rather than assumed, and GetAdSetBudget already carries it.
	//
	// An UNREPORTED owner is refused too. "We could not establish that this ad set belongs
	// to the named campaign" and "it does" are opposite facts, and only the second one
	// justifies a write that moves money; this is the same fail-closed reading the
	// shared-budget and pacing guards below apply to an unreported field.
	//
	// It is ErrCampaignAccountMismatch — the same 409 the account-identity guards raise,
	// because it is the same invariant one level down: the object about to be written is
	// not the object the request named. Refused before any mutate, so nothing is ambiguous.
	if current.CampaignID != campaign.PlatformCampaignID {
		reported := current.CampaignID
		if reported == "" {
			reported = "a campaign meta did not report"
		}
		return fmt.Errorf("write meta campaign budget: ad set %s recorded for campaign %s belongs to %s upstream, so writing its budget would change the spend of a campaign this request never named; re-dispatch the campaign to repair the recorded ad set: %w",
			adSetID, campaign.PlatformCampaignID, reported, domain.ErrCampaignAccountMismatch)
	}

	// GUARD 1 — THE CURRENT BUDGET MUST BE LEGIBLE. A field the platform reported but this
	// client could not parse is refused, not treated as absent. Every guard below reasons
	// about WHICH budget field is in use; a value that failed to parse reads as "this level
	// holds nothing", which is the answer that would select the wrong level to write.
	if current.AmountUnparseable {
		return fmt.Errorf("write meta campaign budget: ad set %s reported a budget amount this service could not read, so which of its budget fields is in use cannot be established: %w",
			adSetID, domain.ErrBudgetUnwritable)
	}

	// GUARD 2 — THE BUDGET MUST NOT BE HELD BY THE CAMPAIGN. This is Meta's shared budget:
	// under Campaign Budget Optimization the campaign holds one amount and distributes it
	// across every ad set under it. Writing an ad-set budget there either fails or converts
	// the campaign off CBO, and in both cases changes the spend of ad sets this request never
	// named — including ad sets this service does not own and cannot see. Refused for exactly
	// the reason the Google Ads path refuses an explicitly_shared budget.
	if current.CampaignBudgetOptimized() {
		return fmt.Errorf("write meta campaign budget: campaign %s uses a campaign-level budget (Campaign Budget Optimization), which is shared across every ad set beneath it, so changing it here would change the spend of ad sets this request never named; give the ad set its own budget in Meta Ads Manager, or make the change there where its full effect is visible: %w",
			campaign.PlatformCampaignID, domain.ErrBudgetUnwritable)
	}

	// GUARD 3 — THE PACING MODEL MUST ALREADY MATCH. This capability changes HOW MUCH a
	// campaign may spend, never HOW it is paced — the same refusal the Google Ads and
	// LinkedIn paths make, for the same reason: switching a live campaign between daily
	// pacing and a whole-flight cap reinterprets every figure it has already spent against.
	//
	// Meta reports the pacing model by WHICH FIELD IS PRESENT, and the two are mutually
	// exclusive on an ad set. NEITHER present means no budget this service can identify the
	// shape of — which, CBO already having been refused above, means the ad set has none at
	// all. BOTH present is the genuinely ambiguous row: writing one would leave the other in
	// force. Both are refused.
	hasDaily := current.DailyBudgetMinor != nil
	hasLifetime := current.LifetimeBudgetMinor != nil
	switch {
	case hasDaily && hasLifetime:
		return fmt.Errorf("write meta campaign budget: ad set %s reports BOTH a daily and a lifetime budget, so writing either would leave the other in force; resolve which one applies in Meta Ads Manager: %w",
			adSetID, domain.ErrBudgetUnwritable)
	case !hasDaily && !hasLifetime:
		return fmt.Errorf("write meta campaign budget: ad set %s reports no budget at all, so which of the two mutually exclusive amount fields to write cannot be established: %w",
			adSetID, domain.ErrBudgetUnwritable)
	}
	upstreamType := model.BudgetDaily
	if hasLifetime {
		upstreamType = model.BudgetLifetime
	}
	if upstreamType != budget.Type {
		return fmt.Errorf("write meta campaign budget: ad set %s is paced as %q upstream but the request asks for %q; this endpoint changes a budget's amount, never its pacing model — change the pacing in Meta Ads Manager, then set the amount here: %w",
			adSetID, upstreamType, budget.Type, domain.ErrBudgetUnwritable)
	}

	// Encoded through the SAME resolver the create path uses, against the account's own
	// currency. Still a read: it fails definitely, before anything is written.
	minor, _, err := client.ResolveBudgetMinorUnits(ctx, budget.Amount)
	if err != nil {
		// A refused AMOUNT is a permanent request fault, not an upstream one. Meta's floor
		// is one minor unit in the ACCOUNT's currency, a value the service layer cannot
		// know, so without this mapping the refusal falls through every errors.Is arm to the
		// default and is answered 503 "the campaign was not modified" — inviting a retry of
		// a request that can never succeed.
		//
		// This call fails three ways and they are three different faults, so they are
		// classified separately below. The remaining one — a failed account preflight —
		// establishes nothing about the currency at all and is genuinely upstream, so it
		// keeps the 503 the default arm gives it. Either way nothing has been written; all
		// three are read-side failures.
		//
		// An ad account whose currency has no known minor-unit scale is a PERMANENT property
		// of that account: no retry resolves it, and the remedy is in Meta Ads Manager or in
		// this service's currency map, never in the request. Classified as unwritable for the
		// same reason the guards above are — a settled refusal, not a retryable failure.
		if errors.Is(err, meta.ErrAccountCurrencyUnresolvable) {
			return fmt.Errorf("write meta campaign budget: %w: %w", err, domain.ErrBudgetUnwritable)
		}
		if reason, ok := meta.BudgetAmountReason(err); ok {
			return &rejectedBudgetAmountError{
				reason: reason,
				err:    fmt.Errorf("write meta campaign budget: %w: %w", err, domain.ErrBudgetAmountRejected),
			}
		}
		return fmt.Errorf("write meta campaign budget: %w", err)
	}

	// Every guard has passed; this is the first and only mutating call.
	//
	// Its error MUST be classified, unlike the reads above: a timeout, 3xx, 429 or 5xx on the
	// ad-set POST may leave the new amount applied upstream. An unwrapped return here would
	// reach the service as a definite failure and be answered "the campaign was not
	// modified" — an affirmative false claim about a money-moving write.
	lifetime := upstreamType == model.BudgetLifetime
	if err := client.UpdateAdSetBudget(ctx, adSetID, minor, lifetime); err != nil {
		werr := fmt.Errorf("write meta campaign budget for campaign %s (ad set %s): %w", campaign.PlatformCampaignID, adSetID, err)
		if meta.IsOutcomeUnconfirmed(err) {
			return &unconfirmedBudgetWriteError{err: werr}
		}
		return werr
	}
	return nil
}
