// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn carousel Sponsored Content (#665): linkedInConfig.format "carousel" with
// linkedInConfig.carouselCards (2 to 10 stored images of the brief, each with a headline and an
// optional destination) and linkedInConfig.carouselIntroText creates a CAROUSEL campaign whose one
// creative is a carousel dark post. See linkedin/carousel.go for the LinkedIn contract and its
// sources. The card images are resolved, sized and checked by resolveLinkedInImages before any
// LinkedIn request, like every other stored image.

// linkedinCarouselEnabled reports whether this deployment opted in to linkedInConfig.format
// "carousel" (#665). Read per call so it flips without a rebuild. Off by default: the CAROUSEL
// campaign and the carousel post shape come from LinkedIn's documentation and have not been
// exercised against a live account.
func linkedinCarouselEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsCarouselEnabled) == "true"
}

// carouselCardsShapeRefusal is the one sentence a wrong-typed carouselCards gets.
func carouselCardsShapeRefusal() error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.carouselCards must be an array of objects {\"imageAssetId\": string, \"headline\": string, \"destinationUrl\"?: string}", domain.ErrCreateConfigInvalid))
}

// checkCarouselFieldShape refuses a carouselIntroText that is not a JSON string, and a
// carouselCards that is not an array of objects whose fields are the three strings CarouselCard
// names: an explicit null, a number headline, an unknown key (a misspelled "destination" would
// otherwise send every click to the registration page). The best-effort pre-flight decode skips a
// wrong-typed field, so without this a malformed card would pass the synchronous check. checkFormatShape
// calls it for either key, matched case-insensitively as encoding/json does.
func checkCarouselFieldShape(key string, raw json.RawMessage) error {
	if strings.EqualFold(key, "carouselIntroText") {
		var str string
		if isJSONNull(raw) || json.Unmarshal(raw, &str) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.carouselIntroText must be a string", domain.ErrCreateConfigInvalid))
		}
		return nil
	}
	var list []json.RawMessage
	if isJSONNull(raw) || json.Unmarshal(raw, &list) != nil {
		return carouselCardsShapeRefusal()
	}
	for _, el := range list {
		// Field by field, so a null field is refused too: a string decode turns null into ""
		// without an error.
		var fields map[string]json.RawMessage
		if isJSONNull(el) || json.Unmarshal(el, &fields) != nil {
			return carouselCardsShapeRefusal()
		}
		for _, v := range fields {
			var str string
			if isJSONNull(v) || json.Unmarshal(v, &str) != nil {
				return carouselCardsShapeRefusal()
			}
		}
		var card linkedin.CarouselCard
		dec := json.NewDecoder(bytes.NewReader(el))
		dec.DisallowUnknownFields()
		if dec.Decode(&card) != nil {
			return carouselCardsShapeRefusal()
		}
	}
	return nil
}

// linkedinCarouselFormat is linkedinFormat's carousel arm: it returns linkedin.FormatCarousel, or
// an ErrCreateConfigInvalid pre-flight refusal for
//
//   - format "carousel" while LINKEDIN_ADS_CAROUSEL_ENABLED is off (checked first);
//   - variants beside the cards: the carousel IS the one creative;
//   - a card whose imageAssetId is not a uuid;
//   - what linkedin.ValidateCarousel refuses: 2 to 10 cards, a headline of at most 45 characters,
//     intro text of at most 255, a destination that is not an absolute http(s) URL without
//     userinfo or a malformed query, or over 2,000 characters.
//
// Whether each asset exists for this brief, and is an image LinkedIn accepts for a carousel, is
// decided at dispatch by resolveLinkedInImages, still before any LinkedIn request. A lead
// generation carousel is refused by linkedin.ValidateLeadGen (linkedinLeadGen).
func linkedinCarouselFormat(cfg linkedinConfig) (string, error) {
	if !linkedinCarouselEnabled() {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.format %q is disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, linkedin.FormatCarousel, constants.EnvLinkedInAdsCarouselEnabled, constants.EnvLinkedInAdsCarouselEnabled))
	}
	refuse := func(err error) (string, error) {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	if len(cfg.Variants) > 0 {
		return refuse(fmt.Errorf("format %q takes its creative from carouselCards; omit variants", linkedin.FormatCarousel))
	}
	for i, card := range cfg.CarouselCards {
		raw := strings.TrimSpace(card.ImageAssetID)
		if raw == "" {
			continue // ValidateCarousel names the missing id
		}
		if _, err := uuid.Parse(raw); err != nil {
			return refuse(fmt.Errorf("carouselCards[%d].imageAssetId %s is not a valid asset id", i, domain.CallerValue(raw)))
		}
	}
	if err := linkedin.ValidateCarousel(cfg.CarouselCards, cfg.CarouselIntroText); err != nil {
		return refuse(err)
	}
	return linkedin.FormatCarousel, nil
}
