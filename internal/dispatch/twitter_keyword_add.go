// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X positive keyword add (#564 gap 12): the counterpart of RemoveKeywordTargeting. It adds
// BROAD_KEYWORD criteria (operator_type EQ) to the ONE line item this service created, the
// criterion the create path sends for twitterConfig.keywords (#574), with the create path's text
// rules and caps.

// twitterKeywordAddEnabled reports whether this deployment opted in. Read per call, like
// twitterCreateTargetingEnabled, so it flips without a rebuild.
func twitterKeywordAddEnabled() bool {
	return os.Getenv(constants.EnvTwitterKeywordAddEnabled) == "true"
}

// twitterTargetingLockScope is the lock scope of one line item's targeting. The ad account is part
// of it because every criteria request is addressed under it (accounts/:account_id/...).
func twitterTargetingLockScope(accountID, lineItemID string) string {
	return "twitter/" + accountID + "/line_items/" + lineItemID
}

// SetKeywordTargetingWriteLock binds the cross-replica lock that serializes the positive keyword
// add per line item (see domain.KeywordTargetingWriteLock). The container binds the Postgres
// advisory lock on both wiring paths; until then — and in direct-construction tests — the
// dispatcher uses an in-process lock, which serializes adds within this process only.
func (d *TwitterDispatcher) SetKeywordTargetingWriteLock(l domain.KeywordTargetingWriteLock) {
	if l != nil {
		d.targetingLock = l
	}
}

// lockTwitterTargeting takes the line item's write lock, as lockRedditTargeting takes an ad
// group's: the wait is reported to the caller's lock-wait recorder, and any failure other than a
// timeout with the lock held elsewhere is tagged domain.ErrKeywordTargetingLockUnavailable. A
// refusal here is definite: nothing was read for the write and nothing was sent.
func (d *TwitterDispatcher) lockTwitterTargeting(ctx context.Context, op, accountID, lineItemID string) (domain.KeywordTargetingLockHold, error) {
	start := time.Now()
	hold, err := d.targetingLock.Lock(ctx, twitterTargetingLockScope(accountID, lineItemID))
	domain.RecordLockWait(ctx, time.Since(start))
	if err != nil {
		if !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
			err = fmt.Errorf("%w: %w", domain.ErrKeywordTargetingLockUnavailable, err)
		}
		return nil, fmt.Errorf("%s: line item %s: %w", op, lineItemID, err)
	}
	return hold, nil
}

// AddKeywordTargeting implements service.KeywordTargetingAdder for X: one BROAD_KEYWORD targeting
// criterion (operator_type EQ) per keyword on the ONE line item this service created for the
// campaign (#564 gap 12).
//
// Guard order — every refusal before the first POST changes nothing: the deployment gate
// (TWITTER_KEYWORD_ADD_ENABLED, exact "true"); the batch (twitter.NormalizeKeywordAdditions);
// provisioning; provenance FAILS CLOSED; the account; the line item is X's, live and THIS
// campaign's (ownedTwitterLineItem); the line item's write lock; its criteria listed completely;
// and the line item must not end up with more than twitter.MaxCreateTargetingCriteria live
// criteria of any type, counting those it holds now plus the keywords this request would send.
//
// A keyword the line item already targets as a live BROAD_KEYWORD criterion with operator EQ (text
// compared under twitterNegativeKey's fold and whitespace collapse) is ALREADY_PRESENT, reports
// that criterion's id, and is not sent — so a repeated request adds nothing. A positive PHRASE or
// EXACT criterion with the same text is a different criterion and does not count.
//
// Then one POST per remaining keyword, in order, each through the account's write pacer and sent
// once — never retried on a 429. Each is APPLIED (+ the criterion id), FAILED (NOT_SENT when it
// never left; REJECTED when X refused it) or UNCONFIRMED. The call errors (UNCONFIRMED) only when
// no item has a definite outcome.
//
// THE LOCK. Held from the criteria read through the last POST, so two LFX adds to one line item
// cannot both read the same criteria, both pass the cap and both send — together exceeding it, or
// each sending the same keyword as a duplicate criterion. It is released at once when the add
// settled, and kept for targetingCooldown after any UNCONFIRMED POST (X may still be applying it;
// released at once, the next add would not see it and send the same keyword again). A change made
// outside LFX (X Ads Manager) is not serialized.
func (d *TwitterDispatcher) AddKeywordTargeting(ctx context.Context, projectID string, platform model.Provider, campaign *model.Campaign, keywords []string) (retOut []model.KeywordTargetingOutcome, retErr error) {
	const op = "add x keyword targeting"
	if !twitterKeywordAddEnabled() {
		return nil, fmt.Errorf("%w: %s: %s is not \"true\"", domain.ErrKeywordTargetingUnsupported, op, constants.EnvTwitterKeywordAddEnabled)
	}
	kws, err := twitter.NormalizeKeywordAdditions(keywords)
	if err != nil {
		return nil, err
	}
	client, lineItemID, err := d.ownedTwitterLineItem(ctx, op, projectID, platform, campaign, true)
	if err != nil {
		return nil, err
	}
	hold, err := d.lockTwitterTargeting(ctx, op, client.AccountID(), lineItemID)
	if err != nil {
		return nil, err
	}
	defer func() {
		outcomes := make([]string, 0, len(retOut))
		for _, o := range retOut {
			outcomes = append(outcomes, o.Outcome)
		}
		// The same rule as Reddit's whole-targeting writes: an UNCONFIRMED error, or any
		// UNCONFIRMED item in a per-item answer, keeps the lock for the cooldown.
		if redditWriteUnsettled(retErr, outcomes...) {
			hold.ReleaseAfterCooldown(d.targetingCooldown)
			return
		}
		hold.Release()
	}()

	all, err := client.ListLineItemTargetingCriteria(ctx, lineItemID)
	if err != nil {
		if errors.Is(err, twitter.ErrTargetingUnreadable) {
			return nil, fmt.Errorf("%s: %w: %w", op, err, domain.ErrKeywordTargetingUnaddressable)
		}
		return nil, fmt.Errorf("%s: list targeting criteria: %w", op, err)
	}
	liveCriteria := 0
	present := make(map[string]string, len(all))
	for _, tc := range all {
		if tc.Deleted {
			continue
		}
		liveCriteria++
		if tc.PositiveKeyword() && tc.TargetingType == twitter.TargetingBroadKeyword {
			if _, seen := present[twitterNegativeKey(tc.TargetingType, tc.TargetingValue)]; !seen {
				present[twitterNegativeKey(tc.TargetingType, tc.TargetingValue)] = tc.ID
			}
		}
	}
	toSend := 0
	pending := make(map[string]bool, len(kws))
	for _, k := range kws {
		key := twitterNegativeKey(twitter.TargetingBroadKeyword, k)
		if _, held := present[key]; held || pending[key] {
			continue
		}
		pending[key] = true
		toSend++
	}
	if liveCriteria+toSend > twitter.MaxCreateTargetingCriteria {
		return nil, fmt.Errorf("%s: line item %s holds %d targeting criteria and this request would add %d; at most %d are accepted on one line item: %w",
			op, lineItemID, liveCriteria, toSend, twitter.MaxCreateTargetingCriteria, domain.ErrKeywordTargetingLimit)
	}

	out := make([]model.KeywordTargetingOutcome, 0, len(kws))
	definite := false
	var lastUnconfirmed error
	for _, k := range kws {
		o := model.KeywordTargetingOutcome{Keyword: k}
		key := twitterNegativeKey(twitter.TargetingBroadKeyword, k)
		if id, held := present[key]; held {
			o.Outcome, o.CriterionID = model.KeywordOutcomeAlreadyPresent, id
			definite = true
			out = append(out, o)
			continue
		}
		if ctx.Err() != nil {
			// Not sent: a DEFINITE outcome — X holds nothing from this item.
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			definite = true
			out = append(out, o)
			continue
		}
		id, cerr := client.CreateKeywordCriterion(ctx, lineItemID, k)
		switch {
		case cerr == nil:
			o.Outcome, o.CriterionID = model.KeywordOutcomeApplied, id
			// A later keyword equal to this one under the whitespace collapse is now present.
			present[key] = id
			definite = true
		case errors.Is(cerr, twitter.ErrWriteNotSent):
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrNotSent
			definite = true
		case twitter.IsOutcomeUnconfirmed(cerr):
			o.Outcome = model.KeywordOutcomeUnconfirmed
			lastUnconfirmed = cerr
		default:
			o.Outcome, o.ErrorCode = model.KeywordOutcomeFailed, model.KeywordTargetingErrRejected
			definite = true
		}
		out = append(out, o)
	}
	if !definite {
		return nil, &unconfirmedKeywordLeverError{err: fmt.Errorf("%s for line item %s: %w", op, lineItemID, lastUnconfirmed)}
	}
	return out, nil
}
