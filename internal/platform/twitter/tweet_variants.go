// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// One promoted tweet per variant (#564 gap 10): twitterConfig.variants, each {"tweetText": ...},
// authors one nullcast tweet per variant under the one line item this create makes and promotes
// each, so X rotates them (an A/B test). The shape mirrors how the other platforms take variants
// (metaConfig / redditConfig / linkedInConfig.variants), and each variant's destination carries
// utm_content=variant-N as theirs do, so registrations attribute to the variant that drove them.
//
// OUTCOMES. Each variant is authored and then promoted before the next one starts, and each has
// its own outcome recorded in CampaignResult.PromotedTweets. A published tweet cannot be
// unpublished, so nothing is rolled back: when any variant fails (authoring refused, UNCONFIRMED,
// or its promotion not confirmed) the remaining variants still run and the create is a degraded
// success — PromotedTweetWarning names the failed variants, and the dispatcher persists the row as
// created_degraded (owner decision 2026-11-14).
//
// PAUSE, RESUME AND LEVERS. All the variants' promoted tweets hang off the ONE line item, which is
// X's delivery gate: the status toggle (campaign + line item), the budget (campaign), the bid and
// the keyword levers (line item) therefore act on every variant at once, with no per-tweet call.
// The promoted-tweet associations themselves are always ACTIVE (the endpoint takes no
// entity_status), exactly as for a single tweet.

const (
	// FieldTweetVariants is the twitterConfig key, named in refusals.
	FieldTweetVariants = "variants"
	// MaxTweetVariants caps the variants of one create. Each costs two paced writes (the tweet and
	// its promotion) inside the two-minute dispatch budget;
	// TestCreateTargetingCriteriaFitTheDispatchBudget pins the margin with the criteria cap.
	MaxTweetVariants = 5
)

// PromotedTweetResult is one promoted tweet of a create, recorded per variant (#564 gap 10). No
// json tags, like CampaignResult, so the persisted keys are the Go field names.
type PromotedTweetResult struct {
	// Variant is 1-based, matching utm_content=variant-N; 0 for an association reused from an
	// earlier attempt, whose variant cannot be told.
	Variant int
	// TweetID is the tweet promoted (or, for a failed promotion, the tweet authored).
	TweetID string
	// Authored reports that THIS create published TweetID — the artifact a retry never reuses.
	Authored bool
	// PromotedTweetID is the promoted_tweet entity id; empty when the promotion was not confirmed.
	PromotedTweetID string
	// Warning is non-empty when this variant was not fully promoted.
	Warning string `json:",omitempty"`
}

// NormalizeTweetVariants applies the gap 10 rules (#564): 1..MaxTweetVariants entries, each a
// non-blank tweetText (trimmed), no two the same ignoring case, and neither tweetId nor tweetText
// alongside — variants author their own tweets, so an explicit tweet or a single text would be
// silently ignored otherwise. Pure, so the dispatcher's synchronous preflight and CreateCampaign
// apply the same rules. An empty list stays nil (no variants).
func NormalizeTweetVariants(texts []string, tweetID, tweetText string) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(tweetID) != "" {
		return nil, &TargetingError{Field: FieldTweetVariants, Msg: "author one new tweet per variant, so they cannot be combined with tweetId"}
	}
	if strings.TrimSpace(tweetText) != "" {
		return nil, &TargetingError{Field: FieldTweetVariants, Msg: "carry each variant's tweetText, so they cannot be combined with a top-level tweetText"}
	}
	if len(texts) > MaxTweetVariants {
		return nil, &TargetingError{Field: FieldTweetVariants, Msg: fmt.Sprintf("at most %d variants are accepted, got %d", MaxTweetVariants, len(texts))}
	}
	out := make([]string, 0, len(texts))
	seen := make(map[string]int, len(texts))
	for i, raw := range texts {
		t := strings.TrimSpace(raw)
		if t == "" {
			return nil, &TargetingError{Field: FieldTweetVariants, Msg: fmt.Sprintf("entry %d tweetText is blank", i)}
		}
		key := foldKey(t)
		if j, dup := seen[key]; dup {
			return nil, &TargetingError{Field: FieldTweetVariants, Msg: fmt.Sprintf("entry %d repeats entry %d's tweetText", i, j)}
		}
		seen[key] = i
		out = append(out, t)
	}
	return out, nil
}

// variantUTMContent is a variant's utm_content, as the other platforms' variants spell it.
func variantUTMContent(variant int) string { return "variant-" + strconv.Itoa(variant) }

// variantList renders 1-based variant numbers for a step or warning ("2, 3").
func variantList(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// authoredTweetsStatus is authoredTweetStatus for several tweets, for an abort part-way through
// the variants: every tweet this create published is named, with what to do about each.
func authoredTweetsStatus(results []PromotedTweetResult) string {
	var ids []string
	for _, r := range results {
		if r.Authored {
			ids = append(ids, r.TweetID)
		}
	}
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return authoredTweetStatus(ids[0])
	}
	return fmt.Sprintf(" / authored tweets %s PUBLISHED, cannot be unpublished — promote or delete each in X Ads Manager; retrying the variants publishes them a SECOND time", strings.Join(ids, ", "))
}

// promoteTweetVariants is Step 4 of CreateCampaign for tweet variants: it authors and promotes one
// tweet per composed text on lineItemID and returns each variant's outcome, the
// PromotedTweetWarning summarizing the failed ones ("" when every variant was promoted), and a
// non-nil aborted error only when the context ended between writes (the caller then returns the
// partial, naming every tweet already published).
//
// REUSE. On a REUSED line item the associations an earlier attempt made are read first, as the
// single-tweet path does: none — author every variant; exactly as many as there are variants — an
// earlier attempt promoted them all, so they are reused and nothing is published (their copy is
// not compared, and a step says so); any other count, or a read that fails — which variants are
// already live cannot be told, so nothing is authored or promoted and the operator verifies.
//
// FAILURES never stop the loop and never roll back (a published tweet cannot be unpublished): a
// definite authoring refusal publishes nothing; an UNCONFIRMED authoring may have published; a
// promotion that is refused, UNCONFIRMED, reported duplicate or answered without an id leaves the
// variant's tweet published but not (provably) promoted. Each is recorded on its variant and named
// in the summary, and the dispatcher persists the row as created_degraded.
func (c *Client) promoteTweetVariants(ctx context.Context, texts []string, pinnedUser, mediaKey string, ensureCard func() (string, error), lineItemID string, lineItemReused bool, steps *[]string) ([]PromotedTweetResult, string, error) {
	if lineItemReused {
		existing, err := c.listPromotedTweets(ctx, lineItemID)
		switch {
		case err != nil:
			*steps = append(*steps, fmt.Sprintf("Tweet variants skipped: the reused line item's existing promoted tweets could not be read (%s) — verify in X Ads Manager", err.Error()))
			return nil, fmt.Sprintf("could not read the promoted tweets already on reused line item %s (%s) — no variant was authored or promoted, because associations from an earlier attempt cannot be ruled out; verify in X Ads Manager before retrying", lineItemID, err.Error()), nil
		case len(existing) == len(texts):
			out := make([]PromotedTweetResult, 0, len(existing))
			for _, e := range existing {
				out = append(out, PromotedTweetResult{TweetID: e.TweetID, PromotedTweetID: e.ID})
			}
			*steps = append(*steps, fmt.Sprintf("Reusing the %d promoted tweets already on reused line item %s — no variant authored", len(existing), lineItemID))
			*steps = append(*steps, "Warning: reused existing promoted tweets; their copy/image were NOT compared with this request's variants — verify in X Ads Manager")
			return out, "", nil
		case len(existing) > 0:
			*steps = append(*steps, fmt.Sprintf("Tweet variants refused: reused line item %s already promotes %d tweets for %d variants — verify in X Ads Manager", lineItemID, len(existing), len(texts)))
			return nil, fmt.Sprintf("reused line item %s already promotes %d tweets but %d variants were requested, so which variants an earlier attempt published cannot be told — no variant was authored or promoted; verify in X Ads Manager", lineItemID, len(existing), len(texts)), nil
		}
	}

	// Resolved once, before the first write, for the reason the single path resolves before pace.
	asUserID, err := c.resolvePromotableUser(ctx, pinnedUser)
	if err != nil {
		*steps = append(*steps, fmt.Sprintf("Tweet variants skipped: %s", err.Error()))
		return nil, fmt.Sprintf("could not author the tweet variants: %s — post them manually, then add each as a promoted tweet in X Ads Manager", err.Error()), nil
	}
	// One website card (#564 gap 6) for every variant, created before the first authoring write. A
	// card that cannot be created publishes nothing.
	var cardURI string
	if ensureCard != nil {
		if cardURI, err = ensureCard(); err != nil {
			why := websiteCardFailure(err)
			*steps = append(*steps, "Tweet variants skipped: "+why)
			return nil, why, nil
		}
	}

	var out []PromotedTweetResult
	var failed []int
	var reasons []string
	fail := func(n int, why string) {
		failed = append(failed, n)
		reasons = append(reasons, fmt.Sprintf("variant %d: %s", n, why))
	}
	for i, text := range texts {
		n := i + 1
		if err := c.pace(ctx); err != nil {
			return out, "", err
		}
		resp, authorErr := c.createNullcastTweet(ctx, text, asUserID, mediaKey, cardURI)
		var tweetID string
		switch {
		case authorErr != nil && createOutcomeAmbiguous(authorErr):
			why := fmt.Sprintf("authoring UNCONFIRMED (%s) — a tweet MAY have been published; verify in X Ads Manager and delete any stray tweet not attached to an ad", authorErr.Error())
			*steps = append(*steps, fmt.Sprintf("Variant %d %s", n, why))
			out = append(out, PromotedTweetResult{Variant: n, Warning: why})
			fail(n, why)
			continue
		case authorErr != nil:
			why := fmt.Sprintf("authoring failed (%s) — nothing was published; post it manually, then add it as a promoted tweet in X Ads Manager", authorErr.Error())
			*steps = append(*steps, fmt.Sprintf("Variant %d %s", n, why))
			out = append(out, PromotedTweetResult{Variant: n, Warning: why})
			fail(n, why)
			continue
		default:
			tweetID = extractTweetID(resp)
			if tweetID == "" {
				why := "authoring UNCONFIRMED (X returned a 2xx with no tweet ID) — a tweet MAY have been published; verify in X Ads Manager and delete any stray tweet not attached to an ad"
				*steps = append(*steps, fmt.Sprintf("Variant %d %s", n, why))
				out = append(out, PromotedTweetResult{Variant: n, Warning: why})
				fail(n, why)
				continue
			}
		}
		out = append(out, PromotedTweetResult{Variant: n, TweetID: tweetID, Authored: true})
		cur := &out[len(out)-1]
		*steps = append(*steps, fmt.Sprintf("Variant %d: authored promoted-only tweet %s (nullcast — not visible on the public timeline or to followers)", n, tweetID))

		if err := c.pace(ctx); err != nil {
			return out, "", err
		}
		presp, perr := c.createRequest(ctx, "promoted_tweets", map[string]string{
			"line_item_id": lineItemID,
			"tweet_ids":    tweetID,
		}, false /* NOT idempotent: DUPLICATE_PROMOTABLE_ENTITY does not name the holder */)
		var why string
		switch {
		case perr != nil && isDuplicatePromotedTweetErr(perr):
			why = fmt.Sprintf("promotion of tweet %s reported as duplicate (DUPLICATE_PROMOTABLE_ENTITY), possibly on a different line item — verify in X Ads Manager", tweetID)
		case perr != nil && createOutcomeAmbiguous(perr):
			why = fmt.Sprintf("promotion of tweet %s UNCONFIRMED (%s) — it MAY have been created; verify in X Ads Manager before adding it", tweetID, perr.Error())
		case perr != nil:
			why = fmt.Sprintf("promotion of tweet %s failed (%s) — the tweet is published; add it as a promoted tweet in X Ads Manager", tweetID, perr.Error())
		default:
			if id := extractPromotedTweetID(presp); id != "" {
				cur.PromotedTweetID = id
				*steps = append(*steps, fmt.Sprintf("Variant %d: promoted tweet created: %s (tweet: %s; held from serving by the PAUSED line item)", n, id, tweetID))
				continue
			}
			why = fmt.Sprintf("promotion of tweet %s UNCONFIRMED (X returned a 2xx with no promoted-tweet ID) — verify in X Ads Manager before adding it", tweetID)
		}
		cur.Warning = why
		*steps = append(*steps, fmt.Sprintf("Variant %d %s", n, why))
		fail(n, why)
	}
	if len(failed) == 0 {
		return out, "", nil
	}
	published := 0
	for _, r := range out {
		if r.Authored {
			published++
		}
	}
	*steps = append(*steps, fmt.Sprintf("Tweet variants NOT fully promoted: variant(s) %s of %d; %d tweet(s) published by this call are kept (a published tweet cannot be unpublished, so nothing was rolled back)", variantList(failed), len(texts), published))
	return out, fmt.Sprintf("variant(s) %s of %d not fully promoted — %s", variantList(failed), len(texts), strings.Join(reasons, "; ")), nil
}
