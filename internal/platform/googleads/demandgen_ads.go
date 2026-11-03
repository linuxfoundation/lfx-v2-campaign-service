// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// More than one ad per Demand Gen ad group (#474)
// ---------------------------------------------------------------------------
//
// A Demand Gen ad group used to carry exactly one ad, from one image or one video
// creative. CampaignInput.DemandGenAds (the single group) and DemandGenAdGroup.Ads (a
// named group) let a caller give it several, each its own ad — the shape of a live
// display campaign running several ad variants. Two DemandGenVideoResponsiveAd ads in
// one Demand Gen ad group were accepted by a validateOnly probe on 2026-11-03.
//
// Each ad is created by its own requests, exactly as the single ad is (createDemandGenAd,
// createDemandGenVideoAd), so a failure on ad j returns ads 1..j-1 complete and what
// exists of ad j.

// maxDemandGenAdsPerGroup caps the list. A SERVICE bound, not Google's: every ad's images
// are fetched and held before the budget mutate, and every ad adds one or two sequential
// mutates after the campaign exists — each a point where a failure leaves a partial
// campaign to reconcile. It matches maxDemandGenAdGroups for the same reason; the whole
// campaign's images stay under one maxCreativeTotalImageBytes either way.
const maxDemandGenAdsPerGroup = 5

// DemandGenAd is one ad of a Demand Gen ad group that carries several.
type DemandGenAd struct {
	// Name labels the ad. It is sanitized like every other name part and appended to the
	// ad group's name ("<ad group name> - <Name>"); two equal ignoring case in one group
	// are refused. A video ad sends that composition as Google's required ad.name, which
	// a validateOnly probe on 2026-11-03 capped at 255 characters (maxAdNameRunes), so a
	// longer one is refused; an image ad sends no ad.name, as before, and the label names
	// it in the step lines.
	Name string
	// Creative and VideoCreative are the ad: exactly one of them.
	Creative      DemandGenCreative
	VideoCreative DemandGenVideoCreative
}

// demandGenAdPlan is one validated Demand Gen ad. label is empty for the single unnamed ad
// built from the campaign-level or group-level creative fields; adName is the video ad's
// ad.name.
type demandGenAdPlan struct {
	label    string
	adName   string
	creative demandGenCreativePlan
	video    demandGenVideoPlan
}

// singleDemandGenAd is the one unnamed ad a group's creative fields describe, nil when
// neither creative was supplied (the no-ad campaign that predates the creative).
func singleDemandGenAd(groupName string, creative demandGenCreativePlan, video demandGenVideoPlan) []demandGenAdPlan {
	if !creative.present && !video.present {
		return nil
	}
	return []demandGenAdPlan{{adName: demandGenVideoAdName(groupName), creative: creative, video: video}}
}

// validateDemandGenAds resolves a group's named ads. owner names the group in an error;
// groupName is the composed ad group name each ad's name is built on. Pure, and run in
// preflightCampaignKind, so every refusal lands before the budget mutate.
func validateDemandGenAds(owner, groupName string, ads []DemandGenAd, resolved map[string][]byte) ([]demandGenAdPlan, error) {
	if len(ads) > maxDemandGenAdsPerGroup {
		return nil, fmt.Errorf("google-ads: %s names %d Demand Gen ads, limit %d", owner, len(ads), maxDemandGenAdsPerGroup)
	}
	plans := make([]demandGenAdPlan, 0, len(ads))
	seen := make(map[string]struct{}, len(ads))
	for i, a := range ads {
		label := sanitizeNamePart(a.Name)
		if label == "" {
			return nil, fmt.Errorf("google-ads: Demand Gen ad %d of %s has no name; each ad needs a distinct label", i+1, owner)
		}
		adName := groupName + " - " + label
		key := strings.ToLower(label)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads: Demand Gen ad name %q is used more than once in %s; each ad in a group needs a distinct name", label, owner)
		}
		seen[key] = struct{}{}

		adOwner := fmt.Sprintf("Demand Gen ad %q of %s", label, owner)
		plan := demandGenAdPlan{label: label, adName: adName}
		var err error
		switch image, video := !a.Creative.empty(), !a.VideoCreative.empty(); {
		case image && video:
			return nil, errDemandGenTwoCreatives(adOwner)
		case image:
			if plan.creative, err = validateDemandGenCreativeSpec(a.Creative, resolved); err != nil {
				return nil, fmt.Errorf("google-ads: %s: %w", adOwner, err)
			}
		case video:
			if plan.video, err = validateDemandGenVideoSpec(a.VideoCreative, resolved); err != nil {
				return nil, fmt.Errorf("google-ads: %s: %w", adOwner, err)
			}
			if n := utf8.RuneCountInString(adName); n > maxAdNameRunes {
				return nil, fmt.Errorf("google-ads: Demand Gen video ad name %q is %d characters, over Google's %d; shorten the ad's name, EventName or the ad group's name", capForError(adName), n, maxAdNameRunes)
			}
		default:
			return nil, fmt.Errorf("google-ads: %s has no creative; set its Creative (image) or VideoCreative", adOwner)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// demandGenAdStep tags an ad's step line with its label, so a group with several ads says
// which line is which ad. Empty for the single unnamed ad, whose step line is unchanged.
func demandGenAdStep(label string) string {
	if label == "" {
		return ""
	}
	return fmt.Sprintf(" [ad %q]", label)
}
