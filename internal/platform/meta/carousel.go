// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Meta carousel ads (#550 gap 10).
//
// metaConfig.format "carousel" replaces one ad per variant with ONE ad whose cards are the variants,
// in order: object_story_spec.link_data.child_attachments (Ad Creative Link Data
// `child_attachments`, "2-10 cards"). Each card carries its variant's image (`picture` for an
// imageUrl, `image_hash` for a stored asset uploaded through the same per-campaign /adimages
// cache), its headline as `name`, its description, its own UTM-tagged link (utm_content=variant-N)
// and the call-to-action button. The ad's message is variant 1's primaryText (a carousel has one),
// and link_data.link — the destination of the end card and of the message — is card 1's link.
// Meta's default card ordering (multi_share_optimized) is left as Meta sets it.
//
// The ad set, budget, targeting and objectives are unchanged; only the creative step differs, and
// the result records AdCount 1, Format "carousel" and the per-card image hashes.

// Creative formats of metaConfig.format.
const (
	FormatSingle   = "single"
	FormatCarousel = "carousel"
)

// Carousel card bounds (Ad Creative Link Data child_attachments: 2-10).
const (
	MinCarouselCards = 2
	MaxCarouselCards = 10
)

// NormalizeFormat returns the creative format to build: FormatSingle for an empty or blank value,
// else the lower-cased value when it is single or carousel. Anything else is a refusal raised
// before any request.
func NormalizeFormat(format string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(format))
	switch v {
	case "":
		return FormatSingle, nil
	case FormatSingle, FormatCarousel:
		return v, nil
	}
	return "", creativeOptionInvalid("format", "%s is not one of %s, %s", domain.CallerValue(format), FormatSingle, FormatCarousel)
}

// carouselInvalid refuses a carousel config before any request (400, PreflightRefusal).
func carouselInvalid(field, format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: metaConfig.%s: %s", domain.ErrCreateConfigInvalid, field, fmt.Sprintf(format, args...)))
}

// ValidateCarousel checks variants as the cards of one carousel ad: 2-10 cards, each with an image
// (imageUrl, or imageAssetId / resolved bytes) and no video, and one message — variant 1's
// primaryText, which every later card leaves empty or repeats verbatim (nothing is silently
// dropped). Copy limits, the image URL rules and the url/asset exclusion are the per-variant checks
// CreateCampaign already applies.
func ValidateCarousel(variants []AdVariant) error {
	if n := len(variants); n < MinCarouselCards || n > MaxCarouselCards {
		return carouselInvalid("format", "a carousel needs %d to %d variants, one per card; got %d", MinCarouselCards, MaxCarouselCards, n)
	}
	message := strings.TrimSpace(variants[0].PrimaryText)
	if message == "" {
		return carouselInvalid("variants[0].primaryText", "a carousel's message is variant 1's primaryText, which is required")
	}
	for i, v := range variants {
		if variantHasVideo(v) {
			return carouselInvalid(fmt.Sprintf("variants[%d].videoAssetId", i), "a carousel card carries an image, not a video")
		}
		if strings.TrimSpace(v.ImageURL) == "" && strings.TrimSpace(v.ImageAssetID) == "" && len(v.ImageBytes) == 0 {
			return carouselInvalid(fmt.Sprintf("variants[%d]", i), "a carousel card needs an image (imageUrl or imageAssetId)")
		}
		if i > 0 {
			if p := strings.TrimSpace(v.PrimaryText); p != "" && p != message {
				return carouselInvalid(fmt.Sprintf("variants[%d].primaryText", i), "a carousel has one message, variant 1's primaryText; leave the other cards' primaryText empty or identical")
			}
		}
	}
	return nil
}

// carouselCreativeName is the carousel creative's name; CreateCampaign bounds it up front.
func carouselCreativeName(eventName string) string {
	return fmt.Sprintf("%s - Carousel", strings.TrimSpace(eventName))
}

// carouselStorySpec builds the carousel ad's object_story_spec from its cards (each already carrying
// its image), the shared message and the ad link.
func carouselStorySpec(pageID, link, message string, cards []map[string]any) map[string]any {
	return map[string]any{
		"page_id": pageID,
		"link_data": map[string]any{
			"link":              link,
			"message":           message,
			"child_attachments": cards,
		},
	}
}

// createCarouselAd uploads every stored card image (through the per-campaign cache), then creates
// the one carousel creative and its ad. It returns the per-card image hashes ("" for an imageUrl
// card). An image upload failure is an *uploadStageError naming the card: no creative or ad request
// was sent. A creative created before the ad failed is returned with the error, as createVariantAd
// does.
func (c *Client) createCarouselAd(ctx context.Context, in CampaignInput, adSetID string, uploads *uploadCache) (adID, creativeID string, hashes []string, err error) {
	cta := in.CallToAction
	if cta == "" {
		cta = DefaultCallToAction
	}
	cards := make([]map[string]any, 0, len(in.Variants))
	hashes = make([]string, len(in.Variants))
	for i, v := range in.Variants {
		link := buildUTMURL(in, i)
		card := map[string]any{
			"link": link,
			"name": v.Headline,
			"call_to_action": map[string]any{
				"type":  cta,
				"value": map[string]any{"link": link},
			},
		}
		if v.Description != "" {
			card["description"] = v.Description
		}
		if len(v.ImageBytes) > 0 {
			h, uerr := uploads.upload(ctx, c, v.ImageBytes, v.ImageMIME)
			if uerr != nil {
				return "", "", hashes, &uploadStageError{variant: i + 1, err: uerr}
			}
			hashes[i] = h
			card["image_hash"] = h
		} else {
			card["picture"] = strings.TrimSpace(v.ImageURL)
		}
		cards = append(cards, card)
	}

	creativeBody := map[string]any{
		"name":              carouselCreativeName(in.EventName),
		"object_story_spec": carouselStorySpec(c.account.PageID, buildUTMURL(in, 0), strings.TrimSpace(in.Variants[0].PrimaryText), cards),
	}
	if in.InstagramUserID != "" {
		creativeBody["instagram_user_id"] = in.InstagramUserID
	}
	var creativeResp createResponse
	if err = c.doCreate(ctx, "/"+c.account.AccountID+"/adcreatives", creativeBody, &creativeResp); err != nil {
		return "", "", hashes, err
	}
	if creativeResp.ID == "" {
		return "", "", hashes, &transportError{Method: http.MethodPost, Path: "/" + c.account.AccountID + "/adcreatives", Err: fmt.Errorf("creative creation returned no ID")}
	}
	var adResp createResponse
	if err = c.doCreate(ctx, "/"+c.account.AccountID+"/ads", map[string]any{
		"name":     fmt.Sprintf("%s - Carousel Ad", in.EventName),
		"adset_id": adSetID,
		"creative": map[string]any{"creative_id": creativeResp.ID},
		"status":   "PAUSED",
	}, &adResp); err != nil {
		return "", creativeResp.ID, hashes, err
	}
	if adResp.ID == "" {
		return "", creativeResp.ID, hashes, &transportError{Method: http.MethodPost, Path: "/" + c.account.AccountID + "/ads", Err: fmt.Errorf("ad creation returned no ID")}
	}
	return adResp.ID, creativeResp.ID, hashes, nil
}

// scrubCarouselErr renders err for a persisted Step with every card's image URL redacted, exactly as
// scrubURLFromErr does for one variant: when any card URL carries a query or fragment (where a
// pre-signed URL keeps its signature) the upstream text is withheld altogether.
func scrubCarouselErr(err error, variants []AdVariant) string {
	msg := err.Error()
	for _, v := range variants {
		raw := strings.TrimSpace(v.ImageURL)
		if raw == "" {
			continue
		}
		if urlHasSecretMaterial(raw) {
			return "(message withheld: a card image URL carries credentials that upstream text may echo)"
		}
		msg = strings.ReplaceAll(msg, raw, redactURL(raw))
	}
	return truncate(msg, 300)
}

// carouselFailureStep records the carousel ad's failure in Steps, worded as the per-variant loop
// words a variant's (upload stage, UNCONFIRMED, definite failure).
func carouselFailureStep(err error, creativeID string, variants []AdVariant) string {
	detail := scrubCarouselErr(err, variants)
	var upErr *uploadStageError
	if errors.As(err, &upErr) {
		msg := fmt.Sprintf("Carousel ad failed: card %d image upload did not complete, so no creative or ad was created: %s", upErr.variant, detail)
		if createOutcomeAmbiguous(upErr.err) {
			msg += " (the library image upload may have landed; it is content-addressed, so it needs no cleanup — but re-dispatching will NOT recreate this ad: the campaign persists as created_degraded and is reused as-is, so create it in Meta Ads Manager)"
		}
		return msg
	}
	orphan := ""
	if creativeID != "" {
		orphan = fmt.Sprintf(" (orphaned creative: %s)", creativeID)
	}
	if createOutcomeAmbiguous(err) {
		return fmt.Sprintf("Carousel ad/creative creation outcome UNCONFIRMED; it may have been created — verify in Meta Ads Manager before recreating%s: %s", orphan, detail)
	}
	return fmt.Sprintf("Carousel ad failed%s: %s", orphan, detail)
}
