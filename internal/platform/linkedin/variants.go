// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"fmt"
	"slices"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Per-variant creative name, call to action and description (#588, #589).
//
// The call to action is the dark post's contentCallToActionLabel, and the description its
// content.article.description (Posts API › Post Schema and Article Ads API › Schema,
// li-lms-2026-02, the version apiVersion pins:
// https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/post-api-schema?view=li-lms-2026-02
// https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/advertising-targeting/version/article-ads-integrations?view=li-lms-2026-02).
// The creative name is the creative's own name field, which createCreative already caps at 255.

// maxCreativeNameLen is LinkedIn's limit on a creative name, the same 255 as campaign names.
const maxCreativeNameLen = maxNameLen

// maxDescriptionLen caps a variant's article description. The Article Ads schema allows up to
// 4,085 characters, but LinkedIn's single-image ad specification sets a 300-character maximum
// (https://www.linkedin.com/help/lms/answer/a426534), the same source as the 200-character
// headline and 600-character intro text createDarkPost already truncates to.
const maxDescriptionLen = 300

// CallToActionLabels are the ContentCallToActionLabel values the Posts API accepts at
// li-lms-2026-02. BUY_NOW and SHOP_NOW need 202504 or later, which apiVersion is.
var CallToActionLabels = []string{
	"APPLY", "DOWNLOAD", "VIEW_QUOTE", "LEARN_MORE", "SIGN_UP", "SUBSCRIBE", "REGISTER",
	"JOIN", "ATTEND", "REQUEST_DEMO", "SEE_MORE", "BUY_NOW", "SHOP_NOW",
}

// ValidateCallToAction refuses a call to action that is not one of CallToActionLabels. The
// match is exact (upper case, no surrounding space), so there is one accepted spelling. An empty
// value means no call to action and is accepted.
func ValidateCallToAction(label string) error {
	if label == "" || slices.Contains(CallToActionLabels, label) {
		return nil
	}
	return fmt.Errorf("callToAction %s is not a LinkedIn call to action; use one of %s", domain.CallerValue(label), strings.Join(CallToActionLabels, ", "))
}

// normalizeCreativeName returns the creative name createCreative sends for a caller's name:
// every run of whitespace (a newline or tab included) collapsed to one space, surrounding space
// trimmed, and at most maxCreativeNameLen runes. Dashes are kept, unlike ad copy
// (normalizeCreativeText): the name is shown only in Campaign Manager and reporting, and LF's
// own names use them ("OSSNA 2026 – Skill Up"). "" means no name.
func normalizeCreativeName(name string) string {
	return truncateRunes(strings.Join(strings.Fields(name), " "), maxCreativeNameLen)
}

// creativeName is the name of variant i (zero-based): the caller's name when it normalizes to
// something, otherwise "<event> | variant-<i+1>" as before #589.
func creativeName(eventName string, i int, variant CreativeVariant) string {
	if name := normalizeCreativeName(variant.Name); name != "" {
		return name
	}
	return fmt.Sprintf("%s | variant-%d", eventName, i+1)
}

// normalizeDescription returns the article description createDarkPost sends: ad copy, so
// normalized like intro text and headline (normalizeCreativeText), then cut to
// maxDescriptionLen runes.
func normalizeDescription(description string) string {
	return truncateRunes(normalizeCreativeText(description), maxDescriptionLen)
}

// ValidateVariantFields checks the optional per-variant fields #588 and #589 add: every
// callToAction is a LinkedIn label, and no two variants carry the same name once normalized.
// Two creatives of one campaign with one name cannot be told apart in Campaign Manager or in
// reporting, so a repeat is refused rather than created. It compares callers' names only, since
// it runs before the event name is known; distinctCreativeNames, which has it, also catches a
// caller's name that repeats another variant's "<event> | variant-N" default.
func ValidateVariantFields(variants []CreativeVariant) error {
	seen := make(map[string]int, len(variants))
	for i, v := range variants {
		if err := ValidateCallToAction(v.CallToAction); err != nil {
			return fmt.Errorf("variant-%d %w", i+1, err)
		}
		name := normalizeCreativeName(v.Name)
		if name == "" {
			continue
		}
		if first, dup := seen[name]; dup {
			return fmt.Errorf("variant-%d name %s repeats variant-%d's; give each variant its own name", i+1, domain.CallerValue(name), first)
		}
		seen[name] = i + 1
	}
	return nil
}

// distinctCreativeNames refuses two variants whose creative names, as creativeName resolves them,
// match: ValidateVariantFields' check plus a caller's name equal to an unnamed variant's
// "<event> | variant-N" default, which only a check that knows the event name can see.
func distinctCreativeNames(eventName string, variants []CreativeVariant) error {
	seen := make(map[string]int, len(variants))
	for i, v := range variants {
		name := creativeName(eventName, i, v)
		if first, dup := seen[name]; dup {
			return fmt.Errorf("variant-%d name %s repeats variant-%d's; give each variant its own name", i+1, domain.CallerValue(name), first)
		}
		seen[name] = i + 1
	}
	return nil
}
