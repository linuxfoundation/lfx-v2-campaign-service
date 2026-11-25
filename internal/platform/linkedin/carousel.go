// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"fmt"
	"strings"
)

// LinkedIn carousel Sponsored Content (#665).
//
// A carousel create (CampaignInput.Format FormatCarousel) is a SPONSORED_UPDATES campaign of
// format CAROUSEL whose one creative references one dark post carrying content.carousel. Sources,
// li-lms-2026-11, consulted 2026-11-25 (the live paused probe owed in #665 confirms the shape):
//
//   - "Carousel Ads API"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/advertising-targeting/version/carousel-ads-integrations?view=li-lms-2026-11):
//     the post's content is {"carousel": {"cards": [{"media": {"id": <ImageUrn>, "title": ...},
//     "landingPage": <URL>}]}}; card landingPage is required, media "Only supports images", media
//     title is required and "Must be less than 400 characters"; "Carousel posts must be sponsored
//     and are not intended to be a regular post". Its sample also sends a post-level
//     contentLandingPage. Images come from the Images API, exactly as a single-image thumbnail.
//   - "Create and Manage Campaigns": "Dynamic, Carousel, and Video Ad Campaigns must have their
//     format set during creation"; "To create a Carousel Ad campaign, set type = SPONSORED_UPDATES
//     and format = CAROUSEL".
//   - "Campaign Objectives": CAROUSEL is listed under Website Visits (MAX_CLICK) and Website
//     Conversions (MAX_CONVERSION, or manual), so every bid plan this service creates applies.
//   - Carousel ad specifications (https://www.linkedin.com/help/lms/answer/88137): "a minimum of
//     two cards and a maximum of ten cards"; intro text "a maximum of 255 characters"; headline
//     45 characters; "Max file size: 10 MB"; "Max image dimension: 4320 x 4320 pixels"; JPG or
//     PNG (the asset store holds only those); destination link "up to 2,000 characters" with an
//     http:// or https:// prefix.
//
// Limits are REFUSED before anything exists, never truncated: a cut card headline is copy nobody
// approved. Each card's landing page is its destination (the brief's registration URL when
// omitted) with the create's UTM parameters and utm_content=card-<k>, k the card's 1-based
// position, so each card is attributed on its own (the Reddit carousel's convention, #661).

// FormatCarousel is a CAROUSEL campaign whose one creative carries CampaignInput.CarouselCards.
const FormatCarousel = "carousel"

// campaignFormatCarousel is the campaign format of a carousel Sponsored Content campaign.
const campaignFormatCarousel = "CAROUSEL"

// Carousel limits (sources above).
const (
	MinCarouselCards = 2
	MaxCarouselCards = 10
	// CarouselHeadlineMaxChars is a card headline's limit (the media title).
	CarouselHeadlineMaxChars = 45
	// CarouselIntroTextMaxChars is the post commentary's limit.
	CarouselIntroTextMaxChars = 255
	// MaxCarouselImageBytes reads "10 MB" decimally, so no file LinkedIn could count as over
	// 10 MB is ever sent.
	MaxCarouselImageBytes = 10_000_000
	// carouselImageMaxSide is the documented "4320 x 4320 pixels" ceiling on either side.
	carouselImageMaxSide = 4320
	// MaxCarouselLandingPageChars is the destination link's limit, checked on the TAGGED URL.
	MaxCarouselLandingPageChars = 2000
)

// sponsoredFormatKind groups a SPONSORED_UPDATES campaign format by the creatives it takes:
// SINGLE_VIDEO and CAROUSEL each their own, every other format (STANDARD_UPDATE, or none sent)
// the single-image one. findCampaignByNameInGroup refuses to reuse a campaign of another kind.
func sponsoredFormatKind(format string) string {
	switch format {
	case campaignFormatSingleVideo, campaignFormatCarousel:
		return format
	}
	return ""
}

// CarouselCard is one card of a carousel create.
type CarouselCard struct {
	// ImageAssetID names a stored image of the brief, uploaded through the Images API; a key into
	// CampaignInput.Images. Required.
	ImageAssetID string `json:"imageAssetId"`
	// Headline is the card's media title. Required, at most CarouselHeadlineMaxChars.
	Headline string `json:"headline"`
	// DestinationURL is the page the card opens. Optional: empty is the brief's registration URL.
	DestinationURL string `json:"destinationUrl,omitempty"`
}

// ValidateCarousel checks a carousel create's cards and intro text before anything is created:
// the card count, each card's image reference, headline and destination, and the intro text, after
// the normalisation the wire copy gets. It does not read images (see ValidateCarouselImage).
// Sentences name cards by their 1-based position and never quote caller text.
func ValidateCarousel(cards []CarouselCard, introText string) error {
	if n := len(cards); n < MinCarouselCards || n > MaxCarouselCards {
		return fmt.Errorf("%w: carouselCards has %d entries; a LinkedIn carousel takes %d to %d cards", ErrFormatInvalid, n, MinCarouselCards, MaxCarouselCards)
	}
	intro := normalizeCreativeText(introText)
	if intro == "" {
		return fmt.Errorf("%w: carouselIntroText is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid)
	}
	if c := len([]rune(intro)); c > CarouselIntroTextMaxChars {
		return fmt.Errorf("%w: carouselIntroText is %d characters; LinkedIn allows at most %d", ErrFormatInvalid, c, CarouselIntroTextMaxChars)
	}
	for i, card := range cards {
		n := i + 1
		if strings.TrimSpace(card.ImageAssetID) == "" {
			return fmt.Errorf("%w: carousel card %d needs an imageAssetId; a LinkedIn carousel card is an image", ErrFormatInvalid, n)
		}
		head := normalizeCreativeText(card.Headline)
		if head == "" {
			return fmt.Errorf("%w: carousel card %d headline is required and must not be empty, whitespace-only, or dash-only after normalization", ErrFormatInvalid, n)
		}
		if c := len([]rune(head)); c > CarouselHeadlineMaxChars {
			return fmt.Errorf("%w: carousel card %d headline is %d characters; LinkedIn allows at most %d", ErrFormatInvalid, n, c, CarouselHeadlineMaxChars)
		}
		if dest := strings.TrimSpace(card.DestinationURL); dest != "" {
			if err := validateLandingURL(dest, fmt.Sprintf("carousel card %d destinationUrl", n)); err != nil {
				return fmt.Errorf("%w: %w", ErrFormatInvalid, err)
			}
			if c := len(dest); c > MaxCarouselLandingPageChars {
				return fmt.Errorf("%w: carousel card %d destinationUrl is %d characters; LinkedIn allows at most %d", ErrFormatInvalid, n, c, MaxCarouselLandingPageChars)
			}
		}
	}
	return nil
}

// ValidateCarouselImage reports whether in can be uploaded as a carousel card image: PNG or JPEG,
// at most MaxCarouselImageBytes, neither side over 4320 pixels, and under the Images API's pixel
// ceiling. It decodes only the header and never quotes bytes.
func ValidateCarouselImage(in ImageUpload) error {
	cfg, err := decodeImageHeader(in, MaxCarouselImageBytes, "carousel ads")
	if err != nil {
		return err
	}
	if cfg.Width > carouselImageMaxSide || cfg.Height > carouselImageMaxSide {
		return fmt.Errorf("%w: the image is %dx%d; LinkedIn carousel ads accept at most %dx%d", ErrImageInvalid, cfg.Width, cfg.Height, carouselImageMaxSide, carouselImageMaxSide)
	}
	if int64(cfg.Width)*int64(cfg.Height) >= maxImagePixels {
		return fmt.Errorf("%w: the image is %dx%d; LinkedIn accepts fewer than %d pixels", ErrImageInvalid, cfg.Width, cfg.Height, maxImagePixels)
	}
	return nil
}

// carouselLandingPages returns each card's tagged landing page: its destination (reg when empty)
// with the create's UTM parameters and utm_content=card-<k>. It refuses a tagged URL over
// LinkedIn's limit, so CreateCampaign can check it before the campaign group exists.
func carouselLandingPages(cards []CarouselCard, reg, hsToken, campaignName string) ([]string, error) {
	out := make([]string, 0, len(cards))
	for i, card := range cards {
		dest := strings.TrimSpace(card.DestinationURL)
		if dest == "" {
			dest = reg
		}
		tagged := buildUTMURLWithContent(dest, hsToken, campaignName, fmt.Sprintf("card-%d", i+1))
		if c := len(tagged); c > MaxCarouselLandingPageChars {
			return nil, fmt.Errorf("%w: carousel card %d landing page is %d characters with its UTM parameters; LinkedIn allows at most %d, so shorten its destination", ErrFormatInvalid, i+1, c, MaxCarouselLandingPageChars)
		}
		out = append(out, tagged)
	}
	return out, nil
}

// createCarouselDarkPost creates a dark post (feedDistribution NONE) whose content is the
// carousel and returns its post URN. The envelope is createDarkPost's; the post-level
// contentLandingPage is the first card's, as the documented sample carries one.
func (c *Client) createCarouselDarkPost(ctx context.Context, accountID, introText string, cards []CarouselCard, landingPages []string, imageURNs map[string]string) (string, error) {
	author, err := c.orgURN(accountID)
	if err != nil {
		return "", err
	}
	wireCards := make([]any, 0, len(cards))
	for i, card := range cards {
		wireCards = append(wireCards, map[string]any{
			"media":       map[string]any{"id": imageURNs[card.ImageAssetID], "title": normalizeCreativeText(card.Headline)},
			"landingPage": landingPages[i],
		})
	}
	body := darkPostEnvelope(author, normalizeCreativeText(introText), accountID)
	body["content"] = map[string]any{"carousel": map[string]any{"cards": wireCards}}
	body["contentLandingPage"] = landingPages[0]
	return c.postDarkPost(ctx, body)
}

// createCarousel creates the carousel's dark post and the one DRAFT creative referencing it, and
// returns how many creatives were created (0 or 1), the Steps, and the first failure. NOT
// idempotent, exactly as the single-image loop: a dark post and a creative have no name-based
// lookup, so an ambiguous failure is reported UNCONFIRMED and the error says not to retry blindly.
func (c *Client) createCarousel(ctx context.Context, accountID, campaignID, eventName string, in CampaignInput, landingPages []string, imageURNs map[string]string) (int, []string, error) {
	var steps []string
	shareURN, err := c.createCarouselDarkPost(ctx, accountID, in.CarouselIntroText, in.CarouselCards, landingPages, imageURNs)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, "Carousel dark post outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating (a dark post has no idempotency lookup)")
			return 0, steps, fmt.Errorf("carousel dark post UNCONFIRMED: %w; the dark post MAY have been created and has no idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err)
		}
		return 0, steps, fmt.Errorf("carousel dark post failed: %w; inspect the returned partial result", err)
	}
	steps = append(steps, fmt.Sprintf("Carousel dark post: %s", shareURN))
	adName := fmt.Sprintf("%s | carousel-1", eventName)
	creativeID, err := c.createCreative(ctx, accountID, campaignID, shareURN, adName, nil)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Carousel creative outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating (orphaned dark post: %s)", shareURN))
			return 0, steps, fmt.Errorf("carousel creative UNCONFIRMED: %w; the dark post %q was already created and the creative MAY have been, and neither has an idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err, shareURN)
		}
		return 0, steps, fmt.Errorf("carousel creative failed: %w; the dark post %q was already created and has no idempotency lookup, so do NOT blindly retry; inspect the returned partial result", err, shareURN)
	}
	steps = append(steps, fmt.Sprintf("Carousel creative (DRAFT): %s", creativeID))
	return 1, steps, nil
}
