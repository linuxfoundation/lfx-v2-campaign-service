// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditKeywordTargetingWritesEnabled reports whether this deployment opted in to Reddit keyword
// targeting WRITES. Read per call, like REDDIT_METRICS_ENABLED, so it flips without a rebuild.
func redditKeywordTargetingWritesEnabled() bool {
	return os.Getenv(constants.EnvRedditKeywordTargetingWritesEnabled) == "true"
}

// redditTargetingAdGroup returns the ONE ad group the create path recorded for the campaign, or
// the reason it cannot be addressed. Shared by the read and the removal so they agree.
func redditTargetingAdGroup(op string, campaign *model.Campaign) (string, error) {
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return "", fmt.Errorf("%w: %s: reddit campaign has no platform campaign id", domain.ErrCampaignNotProvisioned, op)
	}
	adGroupID, _ := redditChildIDs(campaign)
	adGroupID = strings.TrimSpace(adGroupID)
	if adGroupID == "" {
		return "", fmt.Errorf("%s: campaign %s records no ad group created by this service: %w",
			op, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	if err := reddit.CheckAdGroupID(adGroupID); err != nil {
		return "", fmt.Errorf("%s: the campaign row's recorded ad group id cannot address a reddit request: %w: %w",
			op, err, domain.ErrKeywordTargetingUnaddressable)
	}
	return adGroupID, nil
}

// readOwnedRedditTargeting reads the ad group's targeting and PROVES it belongs to this campaign.
// A pure read: every failure is definite.
func readOwnedRedditTargeting(ctx context.Context, op string, client *reddit.Client, res *resolved, campaign *model.Campaign, adGroupID string) (*reddit.AdGroupTargeting, error) {
	t, err := client.GetAdGroupTargeting(ctx, adGroupID)
	if err != nil {
		switch {
		case errors.Is(err, reddit.ErrInvalidAccountID):
			wrapped := fmt.Errorf("%w: %w: %s: the connection's ad account id cannot address a reddit request: %w",
				domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid, op, err)
			if res != nil {
				return nil, res.systemScoped(wrapped)
			}
			return nil, wrapped
		case errors.Is(err, reddit.ErrInvalidAdGroupID), errors.Is(err, reddit.ErrTargetingUnreadable):
			return nil, fmt.Errorf("%s: %w: %w", op, err, domain.ErrKeywordTargetingUnaddressable)
		}
		return nil, fmt.Errorf("%s: read ad group targeting: %w", op, err)
	}
	if t == nil {
		return nil, fmt.Errorf("%s: Reddit holds no ad group %s for campaign %s — it may have been deleted upstream: %w",
			op, adGroupID, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	// OWNERSHIP, POSITIVELY PROVEN: an unreported campaign_id is refused like a different one.
	if t.CampaignID == "" || t.CampaignID != strings.TrimSpace(campaign.PlatformCampaignID) {
		reported := t.CampaignID
		if reported == "" {
			reported = "no campaign"
		}
		return nil, fmt.Errorf("%s: ad group %s is reported under %s, not campaign %s: %w",
			op, adGroupID, reported, campaign.PlatformCampaignID, domain.ErrKeywordTargetingUnaddressable)
	}
	return t, nil
}

// ReadKeywordTargeting implements service.KeywordTargetingReader for Reddit: the keywords in the
// targeting of the ONE ad group this service created for the campaign.
//
// The account check is the toggle's (verifyRedditAccountMatch: a row recording no creating
// account is the pre-existing-row case and proceeds) because this is a read; ownership of the ad
// group is then proven from Reddit's own answer before anything is returned.
func (d *RedditDispatcher) ReadKeywordTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign) (*model.KeywordTargeting, error) {
	const op = "read reddit keyword targeting"
	adGroupID, err := redditTargetingAdGroup(op, campaign)
	if err != nil {
		return nil, err
	}
	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.existingResolver(redditCreationAccountID(campaign)))
	if err != nil {
		return nil, err
	}
	if err := verifyRedditAccountMatch(op, campaign, client); err != nil {
		return nil, err
	}
	t, err := readOwnedRedditTargeting(ctx, op, client, res, campaign, adGroupID)
	if err != nil {
		return nil, err
	}
	out := &model.KeywordTargeting{EntityID: adGroupID, Revision: t.Revision, Keywords: make([]model.KeywordTargetingEntry, 0, len(t.Keywords))}
	for _, k := range t.Keywords {
		out.Keywords = append(out.Keywords, model.KeywordTargetingEntry{Keyword: k})
	}
	return out, nil
}

// RemoveKeywordTargeting implements service.KeywordTargetingRemover for Reddit: it takes keywords
// out of the ad group's targeting by writing the whole targeting back without them.
//
// Guard order — every refusal before the write changes nothing:
//
//  0. The deployment must have opted in (REDDIT_KEYWORD_TARGETING_WRITES_ENABLED); see the
//     constant for why the write is default-off.
//  1. The batch is validated locally: `keyword` items only, no duplicates, a revision present.
//  2. The campaign must be provisioned with a recorded ad group.
//  3. PROVENANCE FAILS CLOSED, as for every mutation: a row recording no creating account is
//     refused; then the account must match the connection.
//  4. The ad group is read and must report THIS campaign; its targeting must be legible.
//  5. COMPARE-AND-SET: the targeting's fingerprint must equal the caller's revision.
//  6. Every named keyword must be in the targeting, and at least one keyword must remain.
//
// Then ONE PATCH carries every removal, and a re-read confirms the keyword list is exactly what
// was written and no other targeting member moved. All items share the PATCH's outcome.
func (d *RedditDispatcher) RemoveKeywordTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, removals []model.KeywordTargetingRemoval, revision string) ([]model.KeywordTargetingOutcome, error) {
	const op = "remove reddit keyword targeting"
	if !redditKeywordTargetingWritesEnabled() {
		return nil, fmt.Errorf("reddit keyword targeting writes are disabled (%s is not \"true\") until the whole-targeting write is exercised against a live ad account: %w",
			constants.EnvRedditKeywordTargetingWritesEnabled, domain.ErrKeywordTargetingUnsupported)
	}
	requested, err := validateRedditKeywordRemovals(removals, revision)
	if err != nil {
		return nil, err
	}
	adGroupID, err := redditTargetingAdGroup(op, campaign)
	if err != nil {
		return nil, err
	}
	created := redditCreationAccountID(campaign)
	if created == "" {
		return nil, fmt.Errorf("%s: campaign %s does not record which ad account it was created under, so its ad group cannot be resolved against any account: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, res, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.existingResolver(created))
	if err != nil {
		return nil, err
	}
	if err := verifyRedditAccountMatch(op, campaign, client); err != nil {
		return nil, err
	}
	current, err := readOwnedRedditTargeting(ctx, op, client, res, campaign, adGroupID)
	if err != nil {
		return nil, err
	}
	if current.Revision != strings.TrimSpace(revision) {
		return nil, fmt.Errorf("%s: ad group %s targeting no longer matches the revision the caller read: %w",
			op, adGroupID, domain.ErrKeywordTargetingChanged)
	}
	present := make(map[string]bool, len(current.Keywords))
	for _, k := range current.Keywords {
		present[k] = true
	}
	for i, k := range requested {
		if !present[k] {
			return nil, fmt.Errorf("%w: removal %d names a keyword ad group %s does not target", domain.ErrKeywordTargetingInvalid, i, adGroupID)
		}
	}
	drop := make(map[string]bool, len(requested))
	for _, k := range requested {
		drop[k] = true
	}
	remaining := make([]string, 0, len(current.Keywords))
	for _, k := range current.Keywords {
		if !drop[k] {
			remaining = append(remaining, k)
		}
	}
	if len(remaining) == 0 {
		return nil, fmt.Errorf("%s: removing these keywords would leave ad group %s with none: %w", op, adGroupID, domain.ErrKeywordTargetingWouldEmpty)
	}
	before, err := current.OtherDimensionsFingerprint()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", op, reddit.ErrTargetingUnreadable, domain.ErrKeywordTargetingUnaddressable)
	}

	if err := client.ReplaceAdGroupKeywords(ctx, adGroupID, current, remaining); err != nil {
		werr := fmt.Errorf("%s for ad group %s: %w", op, adGroupID, err)
		if reddit.IsOutcomeUnconfirmed(err) {
			return nil, &unconfirmedKeywordLeverError{err: werr}
		}
		return nil, werr
	}

	// CONFIRM. The PATCH was acknowledged; the re-read is what shows it did what was meant and
	// nothing else. Any doubt is UNCONFIRMED — never APPLIED — because the write was sent.
	after, err := client.GetAdGroupTargeting(ctx, adGroupID)
	if err != nil || after == nil {
		if err == nil {
			err = errors.New("the ad group was not found on re-read")
		}
		return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s: confirm ad group %s targeting after the write: %w", op, adGroupID, err)}
	}
	if !reddit.SameKeywords(after.Keywords, remaining) {
		return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s: ad group %s reports a keyword list other than the one written", op, adGroupID)}
	}
	if afterRest, ferr := after.OtherDimensionsFingerprint(); ferr != nil || afterRest != before {
		// The keyword write was meant to touch nothing else. A changed dimension is either a
		// concurrent edit or a write that did not round-trip — an operator must look either way.
		slog.ErrorContext(ctx, "reddit ad group targeting changed outside keywords across a keyword removal; verify its targeting in Reddit Ads Manager",
			"project_id", projectID, "campaign_id", campaign.ID, "ad_group_id", adGroupID)
		return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s: ad group %s targeting changed outside keywords across the write", op, adGroupID)}
	}

	out := make([]model.KeywordTargetingOutcome, 0, len(removals))
	for _, k := range requested {
		out = append(out, model.KeywordTargetingOutcome{Keyword: k, Outcome: model.KeywordOutcomeApplied})
	}
	return out, nil
}

// validateRedditKeywordRemovals checks the batch shape Reddit needs and returns the keywords,
// trimmed, in request order.
func validateRedditKeywordRemovals(removals []model.KeywordTargetingRemoval, revision string) ([]string, error) {
	if len(removals) == 0 || len(removals) > maxKeywordTargetingRemovals {
		return nil, fmt.Errorf("%w: send 1 to %d removals, got %d", domain.ErrKeywordTargetingInvalid, maxKeywordTargetingRemovals, len(removals))
	}
	if strings.TrimSpace(revision) == "" {
		return nil, fmt.Errorf("%w: a reddit removal requires the revision get-keyword-targeting returned", domain.ErrKeywordTargetingInvalid)
	}
	seen := make(map[string]bool, len(removals))
	out := make([]string, 0, len(removals))
	for i, r := range removals {
		k := strings.TrimSpace(r.Keyword)
		if k == "" || strings.TrimSpace(r.CriterionID) != "" {
			return nil, fmt.Errorf("%w: removal %d must name a keyword and no criterion id on reddit", domain.ErrKeywordTargetingInvalid, i)
		}
		if seen[k] {
			return nil, fmt.Errorf("%w: removal %d repeats a keyword", domain.ErrKeywordTargetingInvalid, i)
		}
		seen[k] = true
		out = append(out, k)
	}
	return out, nil
}

// maxKeywordTargetingRemovals matches the design's MaxLength on remove-keyword-targeting.
const maxKeywordTargetingRemovals = 20
