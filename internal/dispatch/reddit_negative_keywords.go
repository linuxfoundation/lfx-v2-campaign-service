// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// AddNegativeKeywords implements service.NegativeKeywordAdder for Reddit: it appends to the
// `excluded_keywords` of the ONE ad group this service created for the campaign, by writing the
// whole targeting back with only that member changed (LFXV2-2665).
//
// It is RemoveKeywordTargeting's write in the other direction, and keeps every one of its guards,
// in the same order — every refusal before the PATCH changes nothing:
//
//  0. REDDIT_KEYWORD_TARGETING_WRITES_ENABLED must be "true": the same whole-targeting write,
//     the same reason it is default-off. Off is ErrNegativeKeywordsUnsupported (400).
//  1. The batch: 1..60 entries, the shared text rules, match type Broad ONLY — Reddit's
//     excluded_keywords is a plain string list with no match type, so Exact or Phrase would
//     promise a matching mode Reddit does not offer — no duplicates, and a revision present.
//  2. The campaign must be provisioned with a recorded ad group.
//  3. PROVENANCE FAILS CLOSED; then the account must match the connection.
//  4. The ad group's write lock must be taken within its bounded wait, else
//     ErrKeywordTargetingWriteInProgress (409); held through the confirming re-read, the removal's.
//  5. The ad group is read and must report THIS campaign; its excluded keywords must be legible.
//  6. COMPARE-AND-SET: the targeting's fingerprint must equal the caller's revision (409).
//  7. Keywords the ad group already excludes (exact text) are ALREADY_PRESENT and not sent; the
//     rest must fit Reddit's 2,000 array elements, null elements kept from the read included
//     (ErrNegativeKeywordLimit, 409).
//
// Then ONE PATCH, sent once and never retried on a 429, carries every new keyword, and a re-read
// must show exactly the excluded list written and no other member moved; any doubt is UNCONFIRMED.
// The new keywords share the PATCH's outcome — APPLIED, FAILED (REJECTED by Reddit, or NOT_SENT)
// or UNCONFIRMED — and ALREADY_PRESENT ones keep theirs. The call errors only when no item has a
// definite outcome: every item rode on the write and its outcome is UNCONFIRMED.
func (d *RedditDispatcher) AddNegativeKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, keywords []model.NegativeKeyword, revision string) ([]model.NegativeKeywordOutcome, error) {
	const op = "add reddit negative keywords"
	if !redditKeywordTargetingWritesEnabled() {
		return nil, fmt.Errorf("reddit negative keywords are disabled (%s is not \"true\") until the whole-targeting write is exercised against a live ad account: %w",
			constants.EnvRedditKeywordTargetingWritesEnabled, domain.ErrNegativeKeywordsUnsupported)
	}
	validated, err := validateNegativeKeywordBatch("reddit", keywords, maxNegativeKeywordsPerRequest, negativeMatchBroad)
	if err != nil {
		return nil, err
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return nil, fmt.Errorf("%w: a reddit negative keyword add requires the revision get-keyword-targeting returned", domain.ErrNegativeKeywordInvalid)
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
	// SERIALIZED from here to the confirming re-read: another LFX write on this ad group — on any
	// replica — would otherwise pass the same revision check before this PATCH lands, and one of
	// the two whole-object writes would put back targeting the other had changed.
	unlock, err := d.lockRedditTargeting(ctx, op, created, adGroupID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	current, err := readOwnedRedditTargeting(ctx, op, client, res, campaign, adGroupID)
	if err != nil {
		return nil, err
	}
	if current.Revision != revision {
		return nil, fmt.Errorf("%s: ad group %s targeting no longer matches the revision the caller read: %w",
			op, adGroupID, domain.ErrKeywordTargetingChanged)
	}
	existing, err := current.ExcludedKeywords()
	if err != nil {
		return nil, fmt.Errorf("%s: ad group %s excluded keywords: %w: %w", op, adGroupID, err, domain.ErrKeywordTargetingUnaddressable)
	}
	present := make(map[string]bool, len(existing))
	for _, k := range existing {
		present[k] = true
	}
	out := make([]model.NegativeKeywordOutcome, len(validated))
	var add []string
	for i, k := range validated {
		out[i] = model.NegativeKeywordOutcome{Text: k.Text, MatchType: k.MatchType}
		if present[k.Text] {
			out[i].Outcome = model.KeywordOutcomeAlreadyPresent
			continue
		}
		add = append(add, k.Text)
	}
	if len(add) == 0 {
		// Nothing to write: every keyword is already excluded, so the requested state holds.
		return out, nil
	}
	// The cap is on the ARRAY, and the write keeps every element it read — a null one included —
	// so it is counted in elements, not keywords (PR #321 review).
	elems, err := current.ExcludedKeywordElements()
	if err != nil {
		return nil, fmt.Errorf("%s: ad group %s excluded keywords: %w: %w", op, adGroupID, err, domain.ErrKeywordTargetingUnaddressable)
	}
	if n := elems + len(add); n > reddit.MaxExcludedKeywords {
		return nil, fmt.Errorf("%s: ad group %s would hold %d excluded keywords, over Reddit's %d: %w",
			op, adGroupID, n, reddit.MaxExcludedKeywords, domain.ErrNegativeKeywordLimit)
	}
	before, err := current.FingerprintExceptExcluded()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", op, reddit.ErrTargetingUnreadable, domain.ErrKeywordTargetingUnaddressable)
	}

	// From here on the batch's ALREADY_PRESENT results are KNOWN, whatever happens to the write:
	// those keywords were excluded before it was sent. So a failed or doubtful write does not
	// discard them (PR #321 review) — settle gives every NEW keyword the write's outcome and
	// returns per-item results. The call errors only when no item has a definite outcome: every
	// item rode on the write and the write is UNCONFIRMED. A definite FAILED (REJECTED, NOT_SENT)
	// is itself a known outcome, so it is answered per item as X answers it.
	settle := func(outcome, code string, cause error) ([]model.NegativeKeywordOutcome, error) {
		if len(add) == len(out) && outcome == model.KeywordOutcomeUnconfirmed {
			return nil, &unconfirmedKeywordLeverError{err: cause}
		}
		slog.WarnContext(ctx, "reddit negative keyword write did not apply cleanly; already-excluded keywords are reported, the new ones carry the write's outcome",
			"project_id", projectID, "campaign_id", campaign.ID, "ad_group_id", adGroupID, "outcome", outcome, "error_code", code)
		for i := range out {
			if out[i].Outcome == "" {
				out[i].Outcome, out[i].ErrorCode = outcome, code
			}
		}
		return out, nil
	}

	written, err := client.AddAdGroupExcludedKeywords(ctx, adGroupID, current, add)
	if err != nil {
		werr := fmt.Errorf("%s for ad group %s: %w", op, adGroupID, err)
		switch {
		case reddit.IsOutcomeUnconfirmed(err):
			return settle(model.KeywordOutcomeUnconfirmed, "", werr)
		case reddit.IsPlatformRefusal(err):
			return settle(model.KeywordOutcomeFailed, model.KeywordTargetingErrRejected, werr)
		default:
			// Definite and not Reddit's answer: the PATCH never reached Reddit.
			return settle(model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent, werr)
		}
	}

	// CONFIRM, as the removal does: the PATCH was acknowledged, and the re-read is what shows it
	// did what was meant and nothing else. Any doubt is UNCONFIRMED, never APPLIED.
	after, err := client.GetAdGroupTargeting(ctx, adGroupID)
	if err != nil || after == nil {
		if err == nil {
			err = errors.New("the ad group was not found on re-read")
		}
		return settle(model.KeywordOutcomeUnconfirmed, "", fmt.Errorf("%s: confirm ad group %s targeting after the write: %w", op, adGroupID, err))
	}
	afterExcluded, xerr := after.ExcludedKeywords()
	if xerr != nil || !reddit.SameKeywords(afterExcluded, written) {
		return settle(model.KeywordOutcomeUnconfirmed, "", fmt.Errorf("%s: ad group %s reports an excluded keyword list other than the one written", op, adGroupID))
	}
	if afterRest, ferr := after.FingerprintExceptExcluded(); ferr != nil || afterRest != before {
		slog.ErrorContext(ctx, "reddit ad group targeting changed outside excluded keywords across a negative keyword add; verify its targeting in Reddit Ads Manager",
			"project_id", projectID, "campaign_id", campaign.ID, "ad_group_id", adGroupID)
		return settle(model.KeywordOutcomeUnconfirmed, "", fmt.Errorf("%s: ad group %s targeting changed outside excluded keywords across the write", op, adGroupID))
	}
	for i := range out {
		if out[i].Outcome == "" {
			out[i].Outcome = model.KeywordOutcomeApplied
		}
	}
	return out, nil
}
