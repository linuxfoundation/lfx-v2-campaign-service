// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Campaign existence check: the retry that would otherwise create an orphan budget (#540).
//
// Every channel creates its budget first and its campaign second. The composed budget name
// is the create path's idempotency key, and the DUPLICATE_NAME arm (budget_lookup.go) adopts
// the orphan an earlier attempt left. That key only holds UNTIL A CAMPAIGN ATTACHES: Google
// then renames a non-shared budget to its campaign's name (the campaignNamePartial contract
// in campaign.go). So after a create that SUCCEEDED, nothing holds the composed budget name
// any more. A retry's campaignBudgets:mutate is then accepted, a second budget is created,
// and the duplicate surfaces only one step later as DUPLICATE_CAMPAIGN_NAME. The probe on
// 2026-11-06 left budget 16011237348 exactly this way: 5.00/day, reference_count 0, and
// reported as campaign_name_taken carrying the NEW budget's id.
//
// No budget-name lookup can close this. The budget of interest no longer carries the name,
// and the create of the new one is never refused. The campaign name is what still holds, so
// it is what is checked, before the first write of the create.
// ---------------------------------------------------------------------------

// campaignExistsBeforeCreateError reports that a live campaign of the composed name was
// already in the account when a create began, so this attempt sent no mutate at all.
//
// CreateOutcome classifies it as campaign_name_taken, the same token Google's own
// DUPLICATE_CAMPAIGN_NAME refusal earns at the campaign step. The two report the same
// account state: a campaign of this name exists, and the partial carries a budget id for it.
// Here that id is the budget the existing campaign is attached to, read from the account,
// and no longer one this attempt created for nothing.
type campaignExistsBeforeCreateError struct {
	channel      string
	campaignName string
	campaignID   string
	budgetID     string
}

func (e *campaignExistsBeforeCreateError) Error() string {
	return fmt.Sprintf("google-ads %s campaign %q already exists (campaign %s on budget %s) — nothing was created by this attempt; a prior attempt likely created it, verify in Google Ads before retrying", e.channel, e.campaignName, e.campaignID, e.budgetID)
}

// isCampaignExistsBeforeCreateErr reports whether err's chain carries
// campaignExistsBeforeCreateError.
func isCampaignExistsBeforeCreateErr(err error) bool {
	var e *campaignExistsBeforeCreateError
	return errors.As(err, &e)
}

// refuseIfCampaignExists is the shared pre-write check every channel's create runs. It runs
// immediately before the channel's first mutate: after the preflight and the image fetch,
// which cost nothing upstream, and before Demand Gen's Audiences, the budget, or anything
// else is written.
//
// It returns (nil, nil) when no live campaign holds campaignName, and the create proceeds
// exactly as before. Otherwise it returns an error, and the caller returns both values
// unchanged:
//
//   - a live campaign of the name, with its attached budget read back: a partial carrying
//     that budget's id and a campaignExistsBeforeCreateError, which classifies as
//     campaign_name_taken. The claim is retained, as it is for the DUPLICATE_CAMPAIGN_NAME
//     refusal this replaces.
//   - anything this check could not verify: the lookup failed, or a campaign exists but its
//     budget could not be read. (nil, err), because nothing was sent, and the claim is
//     released, so a later retry asks again. Proceeding instead would rebuild the orphan
//     this check exists to prevent.
//
// Both reads are read-only GAQL searches. FindCampaignByName already fail-closes on every
// response it cannot verify, and it is the same lookup the dispatcher's adopt-on-create uses.
// The dispatcher acts on a found campaign only when the caller opted into adoption. This
// check never adopts: binding a brief to a campaign it did not ask for is the dispatcher's
// decision, not the client's.
//
// partial is the channel's own name-only partial. The result is built from it so the account,
// names and steps match every other partial that channel returns.
func (c *Client) refuseIfCampaignExists(ctx context.Context, channel, campaignName string, partial func() *CampaignResult) (*CampaignResult, error) {
	campaignID, err := c.FindCampaignByName(ctx, campaignName)
	if err != nil {
		return nil, fmt.Errorf("google-ads %s campaign creation not attempted: could not establish whether a campaign named %q already exists, and creating a budget without knowing would orphan it on a retry: %w", channel, campaignName, err)
	}
	if campaignID == "" {
		return nil, nil
	}

	settings, err := c.GetCampaignSettings(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("google-ads %s campaign %q already exists (campaign %s) and its budget could not be read — nothing was created by this attempt: %w", channel, campaignName, campaignID, err)
	}
	if settings == nil || settings.Status == nil || *settings.Status == StatusRemoved {
		// FindCampaignByName saw it live a moment ago. A campaign gone or removed in between
		// is not one to report a budget for. Nothing was sent, so a retry will see the
		// account as it now is.
		return nil, fmt.Errorf("google-ads %s campaign %q (campaign %s) was live a moment ago and is now absent or removed — nothing was created by this attempt; retry to re-check", channel, campaignName, campaignID)
	}
	var budgetID string
	if settings.BudgetID != nil {
		budgetID = canonicalBudgetID(*settings.BudgetID)
	}
	if budgetID == "" {
		return nil, fmt.Errorf("google-ads %s campaign %q already exists (campaign %s) but did not report a usable budget id — nothing was created by this attempt", channel, campaignName, campaignID)
	}

	r := partial()
	r.CampaignBudgetID = budgetID
	r.Steps = append(r.Steps, fmt.Sprintf("Campaign already exists: %s on budget %s; no budget or campaign was created", campaignID, budgetID))
	return r, &campaignExistsBeforeCreateError{channel: channel, campaignName: campaignName, campaignID: campaignID, budgetID: budgetID}
}
