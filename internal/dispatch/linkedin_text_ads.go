// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn Text Ads (#586): linkedInConfig.format "text" with linkedInConfig.textAds creates a
// TEXT_AD campaign whose creatives are text ads instead of single-image dark posts. Omitted (or
// "single_image") format is today's behaviour, byte for byte. See linkedin/text_ads.go for the
// LinkedIn contract and its sources.

// linkedinTextAdsEnabled reports whether this deployment opted in to linkedInConfig.format
// "text" (#586). Read per call so it flips without a rebuild. Off by default: the TEXT_AD
// campaign and textAd creative shapes come from LinkedIn's documentation and have not been
// exercised against a live account, and a wrong guess fails after the campaign group exists.
func linkedinTextAdsEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsTextAdsEnabled) == "true"
}

// textAdsShapeRefusal is the one sentence a wrong-typed textAds gets.
func textAdsShapeRefusal() error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.textAds must be an array of objects {\"headline\": string, \"description\": string, \"imageUrn\"?: string, \"imageAssetId\"?: string}", domain.ErrCreateConfigInvalid))
}

// checkFormatShape refuses a linkedInConfig.format that is not a JSON string, and a
// linkedInConfig.textAds that is not an array of objects whose fields are the four strings
// TextAd names (#586): an explicit null, a number headline, an unknown key (a misspelled
// "descripton" would otherwise create an ad with no description). The best-effort pre-flight
// decode skips a wrong-typed field, so without this a malformed textAds would pass the
// synchronous check and fail the job later. Anything it cannot attribute to these keys is left
// to the ordinary decode.
func checkFormatShape(config json.RawMessage) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["linkedInConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		switch {
		case strings.EqualFold(k, "format"):
			var str string
			if isJSONNull(raw) || json.Unmarshal(raw, &str) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.format must be the string %q or %q", domain.ErrCreateConfigInvalid, linkedin.FormatSingleImage, linkedin.FormatText))
			}
		case strings.EqualFold(k, "textAds"):
			var list []json.RawMessage
			if isJSONNull(raw) || json.Unmarshal(raw, &list) != nil {
				return textAdsShapeRefusal()
			}
			for _, el := range list {
				// Field by field, so a null field is refused too: a string decode turns null
				// into "" without an error.
				var fields map[string]json.RawMessage
				if isJSONNull(el) || json.Unmarshal(el, &fields) != nil {
					return textAdsShapeRefusal()
				}
				for _, v := range fields {
					var str string
					if isJSONNull(v) || json.Unmarshal(v, &str) != nil {
						return textAdsShapeRefusal()
					}
				}
				var ad linkedin.TextAd
				dec := json.NewDecoder(bytes.NewReader(el))
				dec.DisallowUnknownFields()
				if dec.Decode(&ad) != nil {
					return textAdsShapeRefusal()
				}
			}
		}
	}
	return nil
}

// linkedinFormat checks linkedInConfig.format and textAds before any job or request (#586) and
// returns the format Dispatch stores: linkedin.FormatText for a text create, and "" for the
// default (omitted or "single_image"), so a default create's snapshot and client input stay
// byte for byte what they were before #586. Every refusal is an ErrCreateConfigInvalid pre-flight refusal,
// so PreflightCreate answers it with a synchronous 400 and Dispatch fails the job with the same
// sentence:
//
//   - an unknown format;
//   - textAds without format "text";
//   - format "text" while LINKEDIN_ADS_TEXT_ADS_ENABLED is off;
//   - format "text" without bidding.strategy "manual" — LinkedIn offers Text Ads no automated
//     bidding, so the default automatic plan cannot be created as one;
//   - format "text" with variants, or with textAds that linkedin.ValidateTextAds refuses
//     (missing or over-long headline/description, a malformed imageUrn, both image fields).
func linkedinFormat(cfg linkedinConfig, plan bidPlan) (string, error) {
	refuse := func(err error) (string, error) {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	format, err := linkedin.NormalizeFormat(cfg.Format)
	if err != nil {
		return refuse(err)
	}
	if format != linkedin.FormatText {
		if cfg.TextAds != nil {
			return refuse(fmt.Errorf("textAds are accepted only with format %q", linkedin.FormatText))
		}
		return "", nil
	}
	if !linkedinTextAdsEnabled() {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.format %q is disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, linkedin.FormatText, constants.EnvLinkedInAdsTextAdsEnabled, constants.EnvLinkedInAdsTextAdsEnabled))
	}
	if !plan.manual() {
		return refuse(fmt.Errorf("format %q requires bidding {\"strategy\": \"manual\", \"bidAmount\": <max CPC>}: LinkedIn Text Ads support only manual bidding", linkedin.FormatText))
	}
	if len(cfg.Variants) > 0 {
		return refuse(fmt.Errorf("format %q takes its creatives from textAds; omit variants", linkedin.FormatText))
	}
	if err := linkedin.ValidateTextAds(cfg.TextAds); err != nil {
		return refuse(err)
	}
	return format, nil
}

// linkedinCreativeCount is how many creatives the create asks for: the text ads of a text
// create, the variants otherwise.
func linkedinCreativeCount(cfg linkedinConfig) int {
	if cfg.Format == linkedin.FormatText {
		return len(cfg.TextAds)
	}
	return len(cfg.Variants)
}
