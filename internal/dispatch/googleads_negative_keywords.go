// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/textfold"
)

// googleAdsNegativeMatchType maps a platform-neutral match type onto Google's KeywordMatchType
// enum. Only the three the batch validator admits can reach it, and each has an exact Google
// counterpart — so unlike the X mapper there is no default arm to fall through to, and BROAD is
// returned explicitly rather than as a catch-all.
func googleAdsNegativeMatchType(matchType string) (string, bool) {
	switch matchType {
	case negativeMatchExact:
		return googleads.MatchTypeExact, true
	case negativeMatchPhrase:
		return googleads.MatchTypePhrase, true
	case negativeMatchBroad:
		return googleads.MatchTypeBroad, true
	}
	return "", false
}

// googleAdsNegativeKey identifies a negative keyword criterion for the ALREADY_PRESENT check.
// The text is folded with textfold.Key — the key every negative-keyword comparison in this
// package uses — so a campaign already excluding "Free Download" (or "ΟΣ") matches a repeat
// request for "free download" (or "ος"). strings.ToLower would not: it keeps σ and ς apart,
// which simple case folding does not.
//
// Whitespace is collapsed the SAME way on both sides, for the reason the fold is shared. This key
// is built from two sources that do not arrive in the same shape: the REQUEST side comes through
// validateNegativeKeywordBatch, which normalises text with NormalizeNegativeKeywordText
// (strings.Fields joined by one space — trimmed, every internal run collapsed), while the PRESENT
// side is whatever Google returned, normalised by nobody. Collapsing in only one of them made the
// two disagree on exactly the texts the caller cannot see: a criterion Google holds as
// " free download" or "free  download" — or with a non-breaking space, which strings.Fields
// splits on and Google does not have to — would not match the caller's "free download", would
// read as ABSENT, would be sent again, and a duplicate fails this ATOMIC batch, taking every
// keyword that would have been accepted down with it. Collapsing inside the key leaves both call
// sites normalised by construction, and cannot drift the way two call sites would.
//
// It only ever makes the present-set match MORE, never less, and only for texts Google itself
// treats as one keyword — which is the safe direction here: the cost of a missed match is the
// whole batch refused, the cost of a match is one keyword correctly reported ALREADY_PRESENT.
func googleAdsNegativeKey(matchType, text string) string {
	return matchType + "\x00" + textfold.Key(strings.Join(strings.Fields(text), " "))
}

// AddNegativeKeywords implements service.NegativeKeywordAdder for Google Ads: CAMPAIGN-level
// negative keyword criteria, the same level and the same resource the create path attaches its
// exclusion list through.
//
// Guard order — every refusal before the mutate changes nothing: the batch (1..60, the shared
// text rules NARROWED to Google's own 80-rune text limit, match type Broad / Phrase / Exact →
// BROAD / PHRASE / EXACT, no duplicates, no revision); provisioning; provenance FAILS CLOSED;
// the account; then the campaign's existing negatives are read.
//
// THE PRE-READ IS NOT AN OPTIMISATION, which is the one structural difference from the Microsoft
// path. campaignCriteria:mutate is ATOMIC here — partialFailure is never set, in this client or
// on this path — so a single duplicate criterion fails the WHOLE batch, taking every keyword
// that would have been accepted down with it. On Microsoft a duplicate comes back as its own
// per-item outcome and costs the batch nothing. So the already-present keywords must be
// identified and withheld BEFORE the mutate, not reconciled from its response: they are reported
// ALREADY_PRESENT (with the criterion id the campaign already holds) and never sent.
//
// EVERY KEYWORD ACTUALLY SENT SHARES ONE OUTCOME, for that same atomicity: all APPLIED, all
// FAILED, or all UNCONFIRMED. There is no partial answer to report and none is invented. The call
// errors only when NO item has a definite outcome — an already-present item is definite, so a
// batch that was entirely already-present never errors even if nothing was sent.
func (d *GoogleAdsDispatcher) AddNegativeKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, keywords []model.NegativeKeyword, revision string) ([]model.NegativeKeywordOutcome, error) {
	const op = "add google ads negative keywords"
	validated, err := validateNegativeKeywordBatch("google", keywords, maxNegativeKeywordsPerRequest, negativeMatchBroad, negativeMatchPhrase, negativeMatchExact)
	if err != nil {
		return nil, err
	}
	// GOOGLE'S TEXT LIMIT IS SHORTER THAN THE SHARED ONE, and it has to be applied HERE.
	// validateNegativeKeywordBatch admits 100 runes (Microsoft's NegativeKeyword.Text limit,
	// which is the endpoint's published one); Google Ads' KeywordInfo.text limit is 80. Without
	// this check an 81..100 rune text clears every guard above and is refused inside
	// AddCampaignNegativeKeywords, which returns a plain error — neither Unconfirmed() nor
	// NotAttempted() — so the classification arm below would report every keyword in the batch
	// as REJECTED: a claim that Google refused them, when Google was never contacted. It is a
	// permanent, caller-fixable request fault, so it is ErrNegativeKeywordInvalid's 400, and it
	// refuses the request rather than dropping the one keyword, because the client refuses the
	// whole list anyway and a silently shortened exclusion list is worse than a loud refusal.
	for i, k := range validated {
		if n := utf8.RuneCountInString(k.Text); n > googleads.MaxKeywordTextRunes {
			return nil, fmt.Errorf("%w: google: negative keyword %d is %d characters, exceeding Google Ads' %d-character keyword limit",
				domain.ErrNegativeKeywordInvalid, i, n, googleads.MaxKeywordTextRunes)
		}
	}
	if revision != "" {
		return nil, fmt.Errorf("%w: revision applies to reddit only", domain.ErrNegativeKeywordInvalid)
	}
	if campaign == nil || strings.TrimSpace(campaign.PlatformCampaignID) == "" {
		return nil, fmt.Errorf("%w: google ads campaign has no platform campaign id, so there is no campaign to add negative keywords to", domain.ErrCampaignNotProvisioned)
	}
	campaignID := strings.TrimSpace(campaign.PlatformCampaignID)
	if err := googleads.ValidateCampaignID(campaignID); err != nil {
		return nil, fmt.Errorf("%w: the recorded platform campaign id %q is not a Google Ads campaign id", domain.ErrCampaignNotProvisioned, campaign.PlatformCampaignID)
	}

	// PROVENANCE, FAILED CLOSED, then the account — ApplyKeywordActions' pair, for its reasons.
	// Google Ads is ONE customer shared across every foundation, so a row that cannot prove
	// which customer it was created under cannot prove the campaign id it carries is its own,
	// and a re-pointed connection could aim this mutate at another project's campaign. A
	// negative keyword only ever NARROWS delivery, so the cost of being wrong is lower than
	// ApplyKeywordActions' irreversible REMOVE — but it is still silently stopping a stranger's
	// ads from serving on queries they are paying to reach, and the guard costs nothing.
	created := googleAdsCreationCustomerID(campaign)
	if created == "" {
		return nil, fmt.Errorf("%s: campaign %s does not record which ad account it was created under, so it cannot be resolved against any account: %w",
			op, campaign.PlatformCampaignID, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
	client, err := d.resolveGoogleAdsClient(ctx, projectID, platform, campaign)
	if err != nil {
		return nil, err
	}
	if created != client.CustomerID() {
		return nil, fmt.Errorf("%s: campaign %s was created under customer %s but the project's current connection resolves to customer %s: %w",
			op, campaignID, created, client.CustomerID(), domain.ErrCampaignAccountMismatch)
	}

	existing, err := client.ListCampaignNegativeKeywords(ctx, campaignID)
	if err != nil {
		// A PURE READ: it changes nothing, so its failure is DEFINITE and returned unclassified.
		// It is also fatal to the request rather than something to proceed without — without it
		// a duplicate would fail the whole atomic batch, which is the outcome it exists to
		// prevent.
		return nil, fmt.Errorf("%s: read existing negative keywords: %w", op, err)
	}
	present := make(map[string]string, len(existing))
	for _, e := range existing {
		present[googleAdsNegativeKey(e.MatchType, e.Text)] = e.CriterionID
	}

	// Outcomes are POSITIONAL — index-aligned with the validated batch — so the partition keeps
	// the request order on both sides: `sent` holds the keywords to mutate, `sentAt` where each
	// belongs in `out`.
	out := make([]model.NegativeKeywordOutcome, len(validated))
	sent := make([]googleads.Keyword, 0, len(validated))
	sentAt := make([]int, 0, len(validated))
	definite := false
	for i, k := range validated {
		out[i] = model.NegativeKeywordOutcome{Text: k.Text, MatchType: k.MatchType}
		gmt, ok := googleAdsNegativeMatchType(k.MatchType)
		if !ok {
			// Unreachable: validateNegativeKeywordBatch admits only the three mapped above.
			// Refused rather than defaulted, because guessing a match type would attach an
			// exclusion with a different reach than the caller asked for.
			return nil, fmt.Errorf("%w: google: negative keyword %d has a match type Google Ads does not offer", domain.ErrNegativeKeywordInvalid, i)
		}
		if id, already := present[googleAdsNegativeKey(gmt, k.Text)]; already {
			out[i].Outcome = model.KeywordOutcomeAlreadyPresent
			out[i].NegativeKeywordID = id
			definite = true
			continue
		}
		sent = append(sent, googleads.Keyword{Text: k.Text, MatchType: gmt})
		sentAt = append(sentAt, i)
	}
	if len(sent) == 0 {
		// Everything the caller asked for is already excluded. Nothing is sent, and every
		// outcome is definite.
		return out, nil
	}

	ids, mErr := client.AddCampaignNegativeKeywords(ctx, campaignID, sent)
	if mErr != nil {
		if googleads.IsOutcomeUnconfirmed(mErr) {
			// The criteria may exist. Nothing may be reported as applied OR as failed.
			for _, i := range sentAt {
				out[i].Outcome = model.KeywordOutcomeUnconfirmed
			}
			if !definite {
				// No item has a definite outcome, so there is nothing to report per item that
				// the error does not already say — and the service must answer "verify upstream
				// before retrying" rather than hand back a list of unknowns.
				return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s for campaign %s: %w", op, campaignID, mErr)}
			}
			return out, nil
		}
		// DEFINITE: the batch is atomic, so none of the sent keywords was attached and the
		// campaign's existing exclusions are unchanged. NOT_SENT when the request never left
		// this process — the deadline was already spent, or the client proved a pre-send
		// transport failure — REJECTED when Google refused it.
		code := model.KeywordTargetingErrRejected
		var na interface{ NotAttempted() bool }
		if errors.As(mErr, &na) && na.NotAttempted() {
			code = model.KeywordTargetingErrNotSent
		}
		for _, i := range sentAt {
			out[i].Outcome, out[i].ErrorCode = model.KeywordOutcomeFailed, code
		}
		return out, nil
	}
	if len(ids) != len(sent) {
		// Unreachable: the client refuses a short mutate response as UNCONFIRMED before
		// returning ids. Guarded anyway, because pairing ids with the wrong keywords by index
		// would report a criterion id against a keyword it does not identify.
		return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s for campaign %s: google returned %d criterion ids for %d negative keywords", op, campaignID, len(ids), len(sent))}
	}
	for n, i := range sentAt {
		out[i].Outcome, out[i].NegativeKeywordID = model.KeywordOutcomeApplied, ids[n]
	}
	return out, nil
}
