// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// More than one Demand Gen ad group (#460)
// ---------------------------------------------------------------------------
//
// A Demand Gen campaign used to get exactly one ad group, built from the campaign-level
// DemandGenCreative and DemandGenAudience. CampaignInput.DemandGenAdGroups lets a caller
// name several, each with its own creative and its own Audience — the shape of a live
// campaign split into, say, Retargeting and Prospecting. Two Demand Gen ad groups under
// one campaign, each with its own video ad, were accepted by a validateOnly probe on
// 2026-11-03.
//
// The cascade walks ONE shape either way: without the list, the preflight builds a
// single group from the campaign-level fields, named and targeted exactly as before, so
// a caller that omits the list sends the same requests and gets the same result.

// maxDemandGenAdGroups caps the list. A SERVICE bound, not Google's: every group's images
// are fetched and held in memory before the budget mutate, and every group adds four to
// five sequential mutates after the campaign exists — each one a point where a failure
// leaves a partial campaign to reconcile. Five covers the audience splits an event
// campaign uses (the live one has two) with room to spare.
const maxDemandGenAdGroups = 5

// DemandGenAdGroup is one named Demand Gen ad group in CampaignInput.DemandGenAdGroups.
//
// Geo and languages are NOT per group: CampaignInput.GeoTargets/ExcludedGeoTargets and
// Languages are attached to EVERY group, because Demand Gen takes both only at the ad
// group level (geo.go, demandgen_targeting.go) and a group created without them would
// serve worldwide in every language — a split by audience is not meant to also be a
// split by geography. Per-group targeting is a separate feature with its own refusals.
type DemandGenAdGroup struct {
	// Name labels the group. It is sanitized like every other name part and appended to
	// the campaign's ad group name ("<event> - Display | <Name>"), as the Search
	// ad-group list does; two names equal ignoring case are refused, because Google
	// refuses DUPLICATE_ADGROUP_NAME only after the earlier groups exist.
	Name string
	// Audience is this group's single Google Ads Audience, under the same rules as
	// CampaignInput.DemandGenAudience. Optional.
	Audience string
	// Creative and VideoCreative are this group's ad: exactly one of them. A named group
	// with no ad cannot serve, and the list is new, so no caller depends on that shape.
	Creative      DemandGenCreative
	VideoCreative DemandGenVideoCreative
}

// demandGenGroupPlan is one validated Demand Gen ad group.
type demandGenGroupPlan struct {
	name     string
	audience string
	creative demandGenCreativePlan
	video    demandGenVideoPlan
}

// validateDemandGenAdGroups resolves the groups the Demand Gen cascade creates. base is
// the single group the campaign-level fields describe; it is the whole answer when the
// list is absent. Pure, and run in preflightCampaignKind, so every refusal lands before
// the budget mutate.
func validateDemandGenAdGroups(kind, customerID string, in CampaignInput, base demandGenGroupPlan) ([]demandGenGroupPlan, error) {
	if len(in.DemandGenAdGroups) == 0 {
		if kind != campaignKindDemandGen {
			return nil, nil
		}
		if base.video.present {
			if err := validateDemandGenVideoAdName(base.name); err != nil {
				return nil, err
			}
		}
		return []demandGenGroupPlan{base}, nil
	}
	if kind != campaignKindDemandGen {
		return nil, fmt.Errorf("google-ads: Demand Gen ad groups are not supported on %s; omit DemandGenAdGroups (Search takes its own ad-group list, AdGroups)", kind)
	}
	// The campaign-level creative and Audience describe the ONE group the list replaces;
	// with a list there is no single group for them to go on, and guessing would put an ad
	// or an Audience on a group the caller did not name.
	if !in.DemandGenCreative.empty() || !in.DemandGenVideoCreative.empty() || strings.TrimSpace(in.DemandGenAudience) != "" {
		return nil, fmt.Errorf("google-ads: DemandGenAdGroups replaces the single ad group, so DemandGenCreative, DemandGenVideoCreative and DemandGenAudience must be empty; set the creative and Audience on each ad group instead")
	}
	if len(in.DemandGenAdGroups) > maxDemandGenAdGroups {
		return nil, fmt.Errorf("google-ads: %d Demand Gen ad groups requested, limit %d", len(in.DemandGenAdGroups), maxDemandGenAdGroups)
	}

	plans := make([]demandGenGroupPlan, 0, len(in.DemandGenAdGroups))
	seen := make(map[string]struct{}, len(in.DemandGenAdGroups))
	for i, g := range in.DemandGenAdGroups {
		label := sanitizeNamePart(g.Name)
		if label == "" {
			return nil, fmt.Errorf("google-ads: Demand Gen ad group %d has no name; each ad group needs a distinct label", i+1)
		}
		name := base.name + " | " + label
		if err := validateEntityName("ad group", name, utf8.RuneCountInString(name), maxAdGroupNameRunes, "characters"); err != nil {
			return nil, err
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads: Demand Gen ad group name %q is used more than once; Google refuses duplicate ad group names (DUPLICATE_ADGROUP_NAME)", name)
		}
		seen[key] = struct{}{}

		plan := demandGenGroupPlan{name: name}
		var err error
		// The same check as the campaign-level DemandGenAudience: shape and THIS account's
		// customer id only. It is not a tenancy check, so the dispatch adapter refuses a
		// group's Audience under DemandGenAudience's gate (GOOGLE_ADS_DEMAND_GEN_AUDIENCE_ENABLED,
		// see dispatch.googleAdsDemandGenAdGroupAudiencesSet) until #468 adds one.
		if plan.audience, err = validateDemandGenAudienceField(kind, customerID, fmt.Sprintf("the Audience of Demand Gen ad group %q", name), g.Audience); err != nil {
			return nil, err
		}
		owner := fmt.Sprintf("Demand Gen ad group %q", name)
		switch image, video := !g.Creative.empty(), !g.VideoCreative.empty(); {
		case image && video:
			return nil, errDemandGenTwoCreatives(owner)
		case image:
			if plan.creative, err = validateDemandGenCreativeSpec(g.Creative, in.ResolvedImages); err != nil {
				return nil, fmt.Errorf("google-ads: Demand Gen ad group %q: %w", name, err)
			}
		case video:
			if plan.video, err = validateDemandGenVideoSpec(g.VideoCreative, in.ResolvedImages); err != nil {
				return nil, fmt.Errorf("google-ads: Demand Gen ad group %q: %w", name, err)
			}
			if err := validateDemandGenVideoAdName(name); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("google-ads: %s has no creative; set its Creative (image) or VideoCreative, because an ad group with no ad cannot serve", owner)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// demandGenGroupImages is one group's fetched images: the image ad's, slot-indexed into
// demandGenImageSlots, or the video ad's logo.
type demandGenGroupImages struct {
	images []fetchedImage
	logos  []fetchedImage
}

// fetchDemandGenGroupImages fetches every group's images in ONE fetchSlotImages walk, so
// the whole campaign shares one wall-clock budget and one maxCreativeTotalImageBytes total
// rather than one per group — N groups must not multiply what one create may hold in
// memory or spend fetching. Runs before the budget mutate, like every image fetch.
//
// For a single image-ad group the walk is exactly the one this cascade made before #460:
// the same five slots and the same URLs.
func (c *Client) fetchDemandGenGroupImages(ctx context.Context, groups []demandGenGroupPlan) ([]demandGenGroupImages, error) {
	type owner struct {
		group int
		base  int
		logo  bool
	}
	var (
		slots  []imageSlot
		urls   [][]string
		owners []owner
		// Every plan carries the same CampaignInput.ResolvedImages, so any present one's
		// map is the map.
		resolved map[string][]byte
	)
	for gi, g := range groups {
		if g.creative.present {
			resolved = g.creative.resolved
			base := len(slots)
			for si, s := range demandGenImageSlots {
				slots = append(slots, s)
				urls = append(urls, g.creative.urls[si])
				owners = append(owners, owner{group: gi, base: base})
			}
		}
		if g.video.present {
			resolved = g.video.resolved
			slots = append(slots, demandGenVideoLogoSlot)
			urls = append(urls, g.video.logos)
			owners = append(owners, owner{group: gi, base: len(slots) - 1, logo: true})
		}
	}
	out := make([]demandGenGroupImages, len(groups))
	if len(slots) == 0 {
		return out, nil
	}
	fetched, err := c.fetchSlotImages(ctx, slots, urls, resolved)
	if err != nil {
		return nil, err
	}
	for _, img := range fetched {
		o := owners[img.slot]
		// Re-indexed to the slot's position within ITS creative, which is what
		// createDemandGenAd's per-slot bucketing reads.
		img.slot -= o.base
		if o.logo {
			out[o.group].logos = append(out[o.group].logos, img)
		} else {
			out[o.group].images = append(out[o.group].images, img)
		}
	}
	return out, nil
}

// demandGenGroupStep tags a step line with its ad group when the caller named several, so
// the step list says which group each line is about. Empty for the single group, whose
// step lines are unchanged.
func demandGenGroupStep(multi bool, name string) string {
	if !multi {
		return ""
	}
	return fmt.Sprintf(" [ad group %q]", name)
}
