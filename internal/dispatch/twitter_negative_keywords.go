// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// maxTwitterNegativeKeywords bounds one X add. Each keyword is its own paced POST — X allows one
// write per second per ad account — so 20 fit comfortably inside the mutation timeout (45s) with
// the reads around them; 60 would not, and the tail would be cut off as NOT_SENT every time.
const maxTwitterNegativeKeywords = 20

// twitterNegativeTargetingType maps a platform-neutral match type onto the X keyword targeting
// type it is sent as, negated with operator_type NE.
func twitterNegativeTargetingType(matchType string) string {
	switch matchType {
	case negativeMatchExact:
		return twitter.TargetingExactKeyword
	case negativeMatchPhrase:
		return twitter.TargetingPhraseKeyword
	default:
		return twitter.TargetingBroadKeyword
	}
}

// AddNegativeKeywords implements service.NegativeKeywordAdder for X: one negated keyword
// targeting criterion (operator_type NE) per keyword, on the ONE line item this service created
// for the campaign (LFXV2-2665).
//
// Guard order — every refusal before the first POST changes nothing: the batch (1..20, the shared
// text rules, match type Broad / Phrase / Exact → BROAD_ / PHRASE_ / EXACT_KEYWORD, no
// duplicates, no revision); provisioning; provenance FAILS CLOSED; the account; the line item is
// X's, live and THIS campaign's (ownedTwitterLineItem); and its targeting criteria are listed
// completely. A keyword the line item already excludes with the same type (text compared
// exactly) is ALREADY_PRESENT and not sent.
//
// Then one POST per remaining keyword, in request order, each through the account's write pacer
// and sent once — never retried on a 429. Each is APPLIED (+ the criterion id), FAILED (NOT_SENT
// when it never left — the deadline ran out or the pacer wait was cut short; REJECTED when X
// refused it) or UNCONFIRMED (a transport failure, 3xx, 5xx, 429, or a 2xx that does not report
// the negated keyword sent). Adding a negative only narrows delivery, so unlike the removal there
// is no re-read before each write. The call errors only when no item got a definite answer.
func (d *TwitterDispatcher) AddNegativeKeywords(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, keywords []model.NegativeKeyword, revision string) ([]model.NegativeKeywordOutcome, error) {
	const op = "add x negative keywords"
	validated, err := validateNegativeKeywordBatch("x", keywords, maxTwitterNegativeKeywords, negativeMatchBroad, negativeMatchPhrase, negativeMatchExact)
	if err != nil {
		return nil, err
	}
	if revision != "" {
		return nil, fmt.Errorf("%w: revision applies to reddit only", domain.ErrNegativeKeywordInvalid)
	}
	client, lineItemID, err := d.ownedTwitterLineItem(ctx, op, projectID, platform, campaign, true)
	if err != nil {
		return nil, err
	}
	all, err := client.ListLineItemTargetingCriteria(ctx, lineItemID)
	if err != nil {
		if errors.Is(err, twitter.ErrTargetingUnreadable) {
			return nil, fmt.Errorf("%s: %w: %w", op, err, domain.ErrKeywordTargetingUnaddressable)
		}
		return nil, fmt.Errorf("%s: list targeting criteria: %w", op, err)
	}
	present := make(map[string]bool, len(all))
	for _, tc := range all {
		if tc.NegativeKeyword() {
			present[tc.TargetingType+"\x00"+tc.TargetingValue] = true
		}
	}

	out := make([]model.NegativeKeywordOutcome, 0, len(validated))
	definite := false
	var lastUnconfirmed, lastDefinite error
	for _, k := range validated {
		o := model.NegativeKeywordOutcome{Text: k.Text, MatchType: k.MatchType}
		targetingType := twitterNegativeTargetingType(k.MatchType)
		switch {
		case present[targetingType+"\x00"+k.Text]:
			o.Outcome = model.KeywordOutcomeAlreadyPresent
			definite = true
			out = append(out, o)
			continue
		case ctx.Err() != nil:
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			out = append(out, o)
			continue
		}
		id, cerr := client.CreateNegativeKeywordCriterion(ctx, lineItemID, targetingType, k.Text)
		switch {
		case cerr == nil:
			o.Outcome, o.NegativeKeywordID = model.KeywordOutcomeApplied, id
			definite = true
		case errors.Is(cerr, twitter.ErrWriteNotSent):
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			lastDefinite = cerr
		case twitter.IsOutcomeUnconfirmed(cerr):
			o.Outcome = model.KeywordOutcomeUnconfirmed
			lastUnconfirmed = cerr
		default:
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrRejected
			lastDefinite = cerr
			definite = true
		}
		out = append(out, o)
	}
	if !definite {
		if lastUnconfirmed != nil {
			return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s for line item %s: %w", op, lineItemID, lastUnconfirmed)}
		}
		if lastDefinite == nil {
			lastDefinite = ctx.Err()
		}
		return nil, fmt.Errorf("%s for line item %s: no negative keyword was sent: %w", op, lineItemID, lastDefinite)
	}
	return out, nil
}
