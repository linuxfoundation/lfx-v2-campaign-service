// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// One ad per variant (#549 gap 6). With CampaignInput.AdPerVariant set and the author-a-post
// path in use (ImageURL or ImageURLSource, no PostURL), a create with two or more Variants
// authors one promoted post per variant, each with that variant's headline and its own
// utm_content=variant-N destination, and then creates one ad per authored post in the single ad
// group. A create with zero or one variant, or with an existing PostURL (whose post carries its
// own headline), is the long-standing one-ad create, byte for byte.
//
// Each variant's post and ad are classified with the same outcome table as the one-ad create
// (promo_outcome.go); its step is prefixed "Variant N:". A variant that fails does not roll back
// the others: the campaign is created_degraded (AdWarning names the failed variants) and every
// created ad id is recorded in CampaignResult.AdIDs, which the status toggle cascades over. A
// caller cancellation is still fatal, and returns a partial result that names every post and ad
// already created so the claim is retained.

const (
	// MaxAdVariants is the most variants a one-ad-per-variant create accepts. A create with more
	// is refused before any upstream call rather than silently dropping variants.
	MaxAdVariants = 5

	// FieldVariants is the redditConfig key a variant refusal names.
	FieldVariants = "variants"
)

// VariantAd records one variant's post and ad on a one-ad-per-variant create. Variant is
// 1-based. Outcome is the promo outcome-table class: PromoAdCreated for a created ad, otherwise
// the class of the step that failed. PostID is set once the post was authored, AdID once the ad
// was confirmed.
type VariantAd struct {
	Variant int          `json:"variant"`
	PostID  string       `json:"postId,omitempty"`
	AdID    string       `json:"adId,omitempty"`
	Outcome PromoOutcome `json:"outcome"`
}

// AdPerVariantApplies reports whether a create with these inputs takes the one-ad-per-variant
// path: the switch is on, there is no existing post, an image is supplied to author from, and
// there are at least two variants.
func AdPerVariantApplies(in CampaignInput) bool {
	return in.AdPerVariant && strings.TrimSpace(in.PostURL) == "" &&
		(strings.TrimSpace(in.ImageURL) != "" || in.ImageURLSource != nil) && len(in.Variants) >= 2
}

// ValidateAdVariants applies the one-ad-per-variant rules and returns each variant's trimmed
// headline: at most MaxAdVariants variants, and every headline non-blank and at most
// maxRedditHeadlineRunes characters, because each becomes its own post (the one-ad create falls
// back to the event name for a blank first headline; here a blank one would make two variants
// indistinguishable). Refusals name the 1-based variant and never echo the headline.
func ValidateAdVariants(variants []AdVariant) ([]string, error) {
	if n := len(variants); n > MaxAdVariants {
		return nil, domain.PreflightRefusalf("%s: %d variants supplied; one ad per variant accepts at most %d", FieldVariants, n, MaxAdVariants)
	}
	headlines := make([]string, len(variants))
	for i, v := range variants {
		h := strings.TrimSpace(v.Headline)
		if h == "" {
			return nil, domain.PreflightRefusalf("%s[%d]: headline is required; each variant becomes its own promoted post", FieldVariants, i+1)
		}
		if n := utf8.RuneCountInString(h); n > maxRedditHeadlineRunes {
			return nil, domain.PreflightRefusalf("%s[%d]: headline is too long: %d characters (max %d)", FieldVariants, i+1, n, maxRedditHeadlineRunes)
		}
		headlines[i] = h
	}
	return headlines, nil
}

// variantRun carries one-ad-per-variant state from post authoring (before the campaign exists)
// to ad creation (after the ad group exists).
type variantRun struct {
	headlines []string
	ads       []VariantAd
}

func newVariantRun(headlines []string) *variantRun {
	ads := make([]VariantAd, len(headlines))
	for i := range ads {
		ads[i].Variant = i + 1
	}
	return &variantRun{headlines: headlines, ads: ads}
}

// mayHavePost reports whether any variant has, or may have, a post upstream: an authored one,
// or one whose authoring was ambiguous.
func (r *variantRun) mayHavePost() bool {
	for _, a := range r.ads {
		if a.PostID != "" || a.Outcome == PromoAuthorUnconfirmed || a.Outcome == PromoAuthorCanceledInFlight {
			return true
		}
	}
	return false
}

// adIDs returns the confirmed ad ids in variant order.
func (r *variantRun) adIDs() []string {
	var ids []string
	for _, a := range r.ads {
		if a.AdID != "" {
			ids = append(ids, a.AdID)
		}
	}
	return ids
}

// failed returns the 1-based variants with no confirmed ad.
func (r *variantRun) failed() []int {
	var out []int
	for _, a := range r.ads {
		if a.AdID == "" {
			out = append(out, a.Variant)
		}
	}
	return out
}

// snapshot copies the per-variant records for a result; nil on a one-ad create (r == nil).
func (r *variantRun) snapshot() []VariantAd {
	if r == nil {
		return nil
	}
	return append([]VariantAd(nil), r.ads...)
}

func variantStep(variant int, step string) string {
	return fmt.Sprintf("Variant %d: %s", variant, step)
}

func joinVariants(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ", ")
}

// authorVariantPosts authors one post per variant (Step 1.5 of a one-ad-per-variant create). It
// returns (nil, nil) when the create should continue — every variant either has a post or a
// recorded failure — and a non-nil *CampaignResult or error when it must stop:
//   - (nil, err): nothing was or may have been created (the first variant was cancelled before
//     send, or its image URL could not be obtained);
//   - (result, err): a caller cancellation after a post was or may have been authored; the result
//     names the posts so the claim is retained.
//
// A late ImageURLSource is called once per post, immediately before that post's request, so no
// signed URL waits behind the earlier variants' round trips.
func (c *Client) authorVariantPosts(ctx context.Context, accountID string, in CampaignInput, run *variantRun, cta, staticImageURL string, steps *[]string, campaignName string) (*CampaignResult, error) {
	stopResult := func() *CampaignResult {
		return &CampaignResult{
			Platform:     "reddit-ads",
			CampaignName: campaignName,
			AccountID:    c.account.AccountID,
			RedditURL:    redditAdsManagerURL,
			Steps:        *steps,
			VariantAds:   run.snapshot(),
		}
	}
	for i, headline := range run.headlines {
		imageURL := staticImageURL
		if in.ImageURLSource != nil {
			lateURL, lerr := in.ImageURLSource(ctx)
			if lerr == nil {
				lateURL = strings.TrimSpace(lateURL)
				lerr = validateImageURL(lateURL)
			}
			if lerr != nil {
				if !run.mayHavePost() {
					return nil, fmt.Errorf("obtain the promoted-post image URL: %w", lerr)
				}
				// Nothing was sent for this variant: record it as a pre-send authoring failure.
				row := promoRow(PromoAuthorPreSend)
				run.ads[i].Outcome = PromoAuthorPreSend
				*steps = append(*steps, variantStep(i+1, row.step(0, false)))
				continue
			}
			imageURL = lateURL
		}
		postID, err := c.createPromotedPost(ctx, accountID, in, i, headline, cta, imageURL)
		if err == nil {
			run.ads[i].PostID = postID
			*steps = append(*steps, variantStep(i+1, fmt.Sprintf("Authored promoted post: %s (CTA: %s)", postID, cta)))
			continue
		}
		outcome := classifyAuthorFailure(ctx, err)
		row := promoRow(outcome)
		status, hasStatus := errStatus(err)
		run.ads[i].Outcome = outcome
		switch outcome {
		case PromoAuthorCanceledInFlight:
			*steps = append(*steps, variantStep(i+1, row.step(status, hasStatus)))
			return stopResult(), fmt.Errorf("reddit promoted-post authoring UNCONFIRMED (a post for variant %d of campaign %q may exist): %w", i+1, campaignName, err)
		case PromoAuthorCanceledBeforeSend:
			if !run.mayHavePost() {
				return nil, fmt.Errorf("reddit campaign creation aborted during post authoring: %w", ctx.Err())
			}
			return stopResult(), fmt.Errorf("reddit campaign creation aborted during post authoring for variant %d; earlier variants' posts exist and are not attached to an ad: %w", i+1, ctx.Err())
		default:
			*steps = append(*steps, variantStep(i+1, row.step(status, hasStatus)))
		}
	}
	return nil, nil
}

// createVariantAds creates one ad per authored variant post (Step 4 of a one-ad-per-variant
// create) and returns the AdWarning (empty when every variant has an ad). A caller cancellation
// stops it with (partial, err), the partial naming every ad already created.
func (c *Client) createVariantAds(ctx context.Context, accountID, adGroupID string, in CampaignInput, run *variantRun, steps *[]string, partial func() *CampaignResult) (string, *CampaignResult, error) {
	for i := range run.ads {
		postID := run.ads[i].PostID
		if postID == "" {
			continue // authoring failed; its step is already recorded
		}
		adBody := map[string]any{
			"data": map[string]any{
				"ad_group_id":       adGroupID,
				"name":              fmt.Sprintf("%s - Ad %d", replacePipes(in.EventName), i+1),
				"post_id":           postID,
				"configured_status": "PAUSED",
				"click_url":         buildRedditUTMURL(in, i),
			},
		}
		adResp, err := c.requestNoThrottleRetry(ctx, http.MethodPost, "/ad_accounts/"+accountID+"/ads", adBody)
		if err != nil {
			outcome := classifyAdFailure(ctx, err)
			row := promoRow(outcome)
			status, hasStatus := errStatus(err)
			run.ads[i].Outcome = outcome
			switch outcome {
			case PromoAdCanceledInFlight, PromoAdCanceledBeforeSend:
				pr := partial()
				pr.AdGroupID = adGroupID
				pr.AdIDs = run.adIDs()
				if len(pr.AdIDs) > 0 {
					pr.AdID = pr.AdIDs[0]
				}
				pr.AdCount = len(pr.AdIDs)
				pr.VariantAds = run.snapshot()
				pr.AdWarning = fmt.Sprintf("variant %d: %s", i+1, row.AdWarning)
				if outcome == PromoAdCanceledInFlight {
					return "", pr, fmt.Errorf("ad creation for variant %d interrupted, outcome UNCONFIRMED: %w", i+1, ctx.Err())
				}
				return "", pr, fmt.Errorf("ad creation for variant %d aborted before send: %w", i+1, ctx.Err())
			default:
				*steps = append(*steps, variantStep(i+1, row.step(status, hasStatus)))
			}
			continue
		}
		adID := decodeID(adResp)
		if adID == "" {
			row := promoRow(PromoAdNoID)
			run.ads[i].Outcome = PromoAdNoID
			*steps = append(*steps, variantStep(i+1, row.stepf(postID)))
			continue
		}
		run.ads[i].AdID = adID
		run.ads[i].Outcome = PromoAdCreated
		*steps = append(*steps, variantStep(i+1, fmt.Sprintf("Ad created: %s (post: %s, click URL: %s)", adID, postID, displayRedditUTMURL(in, i))))
	}
	failed := run.failed()
	if len(failed) == 0 {
		return "", nil, nil
	}
	created := len(run.ads) - len(failed)
	*steps = append(*steps, fmt.Sprintf("Variant ads: %d of %d created; no confirmed ad for variant(s) %s -- the campaign and ad group exist (PAUSED) and the created ads are kept; see each variant's step above, verify any UNCONFIRMED post or ad in Reddit Ads Manager BEFORE recreating it, and DELETE any post that is not attached to an ad",
		created, len(run.ads), joinVariants(failed)))
	return fmt.Sprintf("one ad per variant: %d of %d ads created; no confirmed ad for variant(s) %s (campaign and ad group created, PAUSED) — verify any UNCONFIRMED variant in Reddit Ads Manager BEFORE recreating it, and DELETE any post that is not attached to an ad",
		created, len(run.ads), joinVariants(failed)), nil, nil
}
