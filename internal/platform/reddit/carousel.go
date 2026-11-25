// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Reddit CAROUSEL posts (#661, split from #549 gap 5).
//
// A carousel is authored on the same legacy POST /profiles/{account}/posts endpoint as the IMAGE
// and VIDEO posts, with "type":"CAROUSEL" and one content entry per card
// (components_schema_post): each entry's media_url (the card's image, "required for image, video
// and carousel posts"), caption (the card's headline, "a carousel image's caption", at most 180
// characters), destination_url ("required for image and carousel posts") and the post's
// call_to_action. The post's own headline is the image post's.
//
// The shape is from Reddit's public v3 OpenAPI document and has NOT been sent to a live account;
// REDDIT_CAROUSEL_ENABLED keeps it off until a paused probe in a test account confirms it.

// Carousel limits. The schema allows up to 40 content entries; 2 to 6 is Reddit's carousel ad
// specification and the range #661 asks for.
const (
	MinCarouselCards = 2
	MaxCarouselCards = 6
	// MaxCarouselHeadlineRunes is content[].caption's maxLength in components_schema_post.
	MaxCarouselHeadlineRunes = 180
)

// FieldCarouselCards is the config field the refusals name.
const FieldCarouselCards = "carouselCards"

// postTypeCarousel is components_schema_post.type for a carousel.
const postTypeCarousel = "CAROUSEL"

// CarouselCard is one card of an authored CAROUSEL post. ImageURLSource is called LATE, exactly
// like CampaignInput.ImageURLSource, immediately before the post request; its URL may be a bearer
// credential and is never logged or persisted. Headline becomes the card's caption, and
// DestinationURL (with the post's UTM parameters added) its destination_url.
type CarouselCard struct {
	ImageURLSource func(ctx context.Context) (string, error)
	Headline       string
	DestinationURL string
}

// carouselContent is one resolved card, ready for the post body.
type carouselContent struct {
	MediaURL       string
	Caption        string
	DestinationURL string
}

// ValidateCarouselCardCount refuses a card count outside MinCarouselCards..MaxCarouselCards.
func ValidateCarouselCardCount(n int) error {
	if n < MinCarouselCards || n > MaxCarouselCards {
		return domain.PreflightRefusalf("%s: %d cards supplied; a carousel takes %d to %d", FieldCarouselCards, n, MinCarouselCards, MaxCarouselCards)
	}
	return nil
}

// ValidateCarouselCardText checks one card's headline and destination URL. index is 0-based;
// refusals name the 1-based card and never echo the headline or the URL (a URL may carry a
// secret in its query). It returns the trimmed headline and destination.
func ValidateCarouselCardText(index int, headline, destinationURL string) (string, string, error) {
	card := fmt.Sprintf("%s[%d]", FieldCarouselCards, index+1)
	h := strings.TrimSpace(headline)
	if h == "" {
		return "", "", domain.PreflightRefusalf("%s: headline is required", card)
	}
	if n := utf8.RuneCountInString(h); n > MaxCarouselHeadlineRunes {
		return "", "", domain.PreflightRefusalf("%s: headline is %d characters (max %d)", card, n, MaxCarouselHeadlineRunes)
	}
	d := strings.TrimSpace(destinationURL)
	if d == "" {
		return "", "", domain.PreflightRefusalf("%s: destinationUrl is required", card)
	}
	u, err := url.Parse(d)
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return "", "", domain.PreflightRefusalf("%s: destinationUrl must be an absolute http or https URL", card)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return "", "", domain.PreflightRefusalf("%s: destinationUrl must be an absolute http or https URL", card)
	}
	if u.User != nil {
		return "", "", domain.PreflightRefusalf("%s: destinationUrl must not contain embedded credentials", card)
	}
	if _, qerr := url.ParseQuery(u.RawQuery); qerr != nil {
		// u.Query() in buildRedditUTMURLFrom would silently drop a malformed pair.
		return "", "", domain.PreflightRefusalf("%s: destinationUrl has a malformed query string", card)
	}
	return h, d, nil
}

// validateCarouselInput applies every carousel rule the client can check before any request:
// the count, each card's source, headline and destination, and that the carousel is the only
// creative (no post URL, image or video beside it).
func validateCarouselInput(in CampaignInput) error {
	if strings.TrimSpace(in.PostURL) != "" || strings.TrimSpace(in.ImageURL) != "" || in.ImageURLSource != nil ||
		in.VideoURLSource != nil || in.ThumbnailURLSource != nil {
		return domain.PreflightRefusalf("a Reddit carousel post cannot be combined with a post URL, an image or a video")
	}
	if err := ValidateCarouselCardCount(len(in.CarouselCards)); err != nil {
		return err
	}
	for i, c := range in.CarouselCards {
		if c.ImageURLSource == nil {
			return domain.PreflightRefusalf("%s[%d]: an image is required", FieldCarouselCards, i+1)
		}
		if _, _, err := ValidateCarouselCardText(i, c.Headline, c.DestinationURL); err != nil {
			return err
		}
	}
	return nil
}

// resolveCarouselCards mints every card's late image URL, in card order, and builds the post
// content. It is called immediately before the one post request; nothing has been created when
// it fails, so the caller returns (nil, err).
func resolveCarouselCards(ctx context.Context, in CampaignInput) ([]carouselContent, error) {
	out := make([]carouselContent, len(in.CarouselCards))
	for i, c := range in.CarouselCards {
		mediaURL, err := lateMediaURL(ctx, c.ImageURLSource, fmt.Sprintf("carousel card %d image", i+1))
		if err != nil {
			return nil, err
		}
		h, d, err := ValidateCarouselCardText(i, c.Headline, c.DestinationURL)
		if err != nil {
			return nil, err
		}
		out[i] = carouselContent{MediaURL: mediaURL, Caption: h, DestinationURL: buildRedditUTMURLFrom(d, in, 0)}
	}
	return out, nil
}
