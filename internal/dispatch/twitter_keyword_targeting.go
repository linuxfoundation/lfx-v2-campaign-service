// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// twitterEntityIDRE is the shape of an X entity id (base-36), the same as the client's own
// path guard; checked here so a malformed removal is a 400 before any request.
var twitterEntityIDRE = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

// ownedTwitterLineItem resolves the client and PROVES the recorded line item is this campaign's:
// the row must record one, X must hold it live, and X must report it under the row's campaign.
// requireProvenance makes a row recording no creating account a refusal (mutations) rather than
// the pre-existing-row pass-through (reads).
func (d *TwitterDispatcher) ownedTwitterLineItem(ctx context.Context, op, projectID string, platform model.Provider, campaign *model.Campaign, requireProvenance bool) (*twitter.Client, string, error) {
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, "", fmt.Errorf("%w: %s: x campaign has no platform campaign id", domain.ErrCampaignNotProvisioned, op)
	}
	if requireProvenance && twitterCreationAccountID(campaign) == "" {
		return nil, "", fmt.Errorf("%s: campaign %s does not record which ad account it was created under, so its line item cannot be resolved against any account: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	lineItemID := strings.TrimSpace(twitterChildIDs(campaign))
	if lineItemID == "" {
		return nil, "", fmt.Errorf("%s: campaign %s records no line item created by this service: %w",
			op, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	client, res, err := d.resolveTwitterClientWithRes(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, "", err
	}
	if err := verifyTwitterAccountMatch(op, campaign, client); err != nil {
		return nil, "", err
	}
	li, err := client.GetLineItemBid(ctx, lineItemID)
	if err != nil {
		if errors.Is(err, twitter.ErrInvalidAccountID) {
			return nil, "", res.systemScoped(fmt.Errorf("%w: %w: %s: the connection's ad account id cannot address an x ads request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, op, err))
		}
		if errors.Is(err, twitter.ErrInvalidLineItemID) {
			return nil, "", fmt.Errorf("%s: the campaign row's recorded line item id cannot address an x request: %w: %w", op, err, domain.ErrKeywordTargetingUnaddressable)
		}
		return nil, "", fmt.Errorf("%s: read line item: %w", op, err)
	}
	if li == nil || li.Deleted {
		return nil, "", fmt.Errorf("%s: X holds no live line item %s for campaign %s: %w",
			op, lineItemID, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	if li.CampaignID == "" || li.CampaignID != strings.TrimSpace(campaign.PlatformCampaignID) {
		reported := li.CampaignID
		if reported == "" {
			reported = "no campaign"
		}
		return nil, "", fmt.Errorf("%s: line item %s is reported under %s, not campaign %s: %w",
			op, lineItemID, reported, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	return client, lineItemID, nil
}

// liveTwitterKeywords lists the line item's criteria and keeps the live, positive keyword ones.
func liveTwitterKeywords(ctx context.Context, op string, client *twitter.Client, lineItemID string) ([]twitter.TargetingCriterion, error) {
	all, err := client.ListLineItemTargetingCriteria(ctx, lineItemID)
	if err != nil {
		if errors.Is(err, twitter.ErrTargetingUnreadable) {
			return nil, fmt.Errorf("%s: %w: %w", op, err, domain.ErrKeywordTargetingUnaddressable)
		}
		return nil, fmt.Errorf("%s: list targeting criteria: %w", op, err)
	}
	out := make([]twitter.TargetingCriterion, 0, len(all))
	for _, tc := range all {
		if tc.PositiveKeyword() {
			out = append(out, tc)
		}
	}
	return out, nil
}

// ReadKeywordTargeting implements service.KeywordTargetingReader for X: the live, positive
// keyword targeting criteria of the ONE line item this service created. The create path sets
// none, so this is empty unless an operator added keywords in X Ads Manager.
func (d *TwitterDispatcher) ReadKeywordTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.KeywordTargeting, error) {
	const op = "read x keyword targeting"
	client, lineItemID, err := d.ownedTwitterLineItem(ctx, op, projectID, platform, campaign, false)
	if err != nil {
		return nil, err
	}
	live, err := liveTwitterKeywords(ctx, op, client, lineItemID)
	if err != nil {
		return nil, err
	}
	out := &model.KeywordTargeting{EntityID: lineItemID, Keywords: make([]model.KeywordTargetingEntry, 0, len(live))}
	for _, tc := range live {
		out.Keywords = append(out.Keywords, model.KeywordTargetingEntry{Keyword: tc.TargetingValue, CriterionID: tc.ID, MatchType: tc.TargetingType})
	}
	return out, nil
}

// RemoveKeywordTargeting implements service.KeywordTargetingRemover for X: one DELETE per named
// keyword targeting criterion, in request order, each with its own outcome.
//
// Guard order — every refusal before the first DELETE changes nothing: the batch (criterion ids
// only, well-formed, no duplicates, no revision); provisioning; provenance FAILS CLOSED; the
// account; the line item is X's, live and THIS campaign's; every named criterion is a live
// positive keyword criterion OF THAT LINE ITEM (read, so an id from any other line item in the
// account is refused rather than deleted); and at least one keyword criterion must remain.
//
// The DELETEs are not atomic. Each is APPLIED, FAILED (NOT_FOUND, REJECTED, or NOT_SENT when the
// deadline ran out before it was sent) or UNCONFIRMED (a transport failure, 3xx, 5xx, 429, or a
// 2xx that does not report this criterion deleted). The call errors only when no item got a
// definite answer from X.
func (d *TwitterDispatcher) RemoveKeywordTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, removals []model.KeywordTargetingRemoval, revision string) ([]model.KeywordTargetingOutcome, error) {
	const op = "remove x keyword targeting"
	requested, err := validateTwitterKeywordRemovals(removals, revision)
	if err != nil {
		return nil, err
	}
	client, lineItemID, err := d.ownedTwitterLineItem(ctx, op, projectID, platform, campaign, true)
	if err != nil {
		return nil, err
	}
	live, err := liveTwitterKeywords(ctx, op, client, lineItemID)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(live))
	for _, tc := range live {
		present[tc.ID] = true
	}
	for i, id := range requested {
		if !present[id] {
			return nil, fmt.Errorf("%w: removal %d names a criterion that is not a live keyword of line item %s", domain.ErrKeywordTargetingInvalid, i, lineItemID)
		}
	}
	if len(live)-len(requested) <= 0 {
		return nil, fmt.Errorf("%s: removing these criteria would leave line item %s with no keyword: %w", op, lineItemID, domain.ErrKeywordTargetingWouldEmpty)
	}

	out := make([]model.KeywordTargetingOutcome, 0, len(requested))
	definite := false
	var lastUnconfirmed, lastDefinite error
	for _, id := range requested {
		o := model.KeywordTargetingOutcome{CriterionID: id}
		if ctx.Err() != nil {
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			out = append(out, o)
			continue
		}
		derr := client.DeleteTargetingCriterion(ctx, id)
		switch {
		case derr == nil:
			o.Outcome = model.KeywordOutcomeApplied
			definite = true
		case errors.Is(derr, twitter.ErrWriteNotSent):
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			lastDefinite = derr
		case twitter.IsOutcomeUnconfirmed(derr):
			o.Outcome = model.KeywordOutcomeUnconfirmed
			lastUnconfirmed = derr
		case errors.Is(derr, twitter.ErrTargetingCriterionNotFound):
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotFound
			definite = true
		default:
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrRejected
			definite = true
		}
		out = append(out, o)
	}
	if !definite {
		// No item got a definite answer from X. Any UNCONFIRMED makes the whole call so.
		if lastUnconfirmed != nil {
			return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s for line item %s: %w", op, lineItemID, lastUnconfirmed)}
		}
		if lastDefinite == nil {
			lastDefinite = ctx.Err()
		}
		return nil, fmt.Errorf("%s for line item %s: no removal was sent: %w", op, lineItemID, lastDefinite)
	}
	return out, nil
}

// validateTwitterKeywordRemovals checks the batch shape X needs and returns the criterion ids,
// trimmed, in request order.
func validateTwitterKeywordRemovals(removals []model.KeywordTargetingRemoval, revision string) ([]string, error) {
	if len(removals) == 0 || len(removals) > maxKeywordTargetingRemovals {
		return nil, fmt.Errorf("%w: send 1 to %d removals, got %d", domain.ErrKeywordTargetingInvalid, maxKeywordTargetingRemovals, len(removals))
	}
	if strings.TrimSpace(revision) != "" {
		return nil, fmt.Errorf("%w: revision applies to reddit only", domain.ErrKeywordTargetingInvalid)
	}
	seen := make(map[string]bool, len(removals))
	out := make([]string, 0, len(removals))
	for i, r := range removals {
		id := strings.TrimSpace(r.CriterionID)
		if !twitterEntityIDRE.MatchString(id) || strings.TrimSpace(r.Keyword) != "" {
			return nil, fmt.Errorf("%w: removal %d must name a well-formed criterion id and no keyword on x", domain.ErrKeywordTargetingInvalid, i)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: removal %d repeats a criterion", domain.ErrKeywordTargetingInvalid, i)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
