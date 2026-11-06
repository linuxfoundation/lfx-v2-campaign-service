// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import "github.com/linuxfoundation/lfx-v2-campaign-service/internal/creative"

// platformGoogleAds is this platform's identifier in the rest of the service — the value
// every CampaignResult reports and the value a slot plan carries, so a plan row joins to
// a campaign config without a translation table. It is a constant because the catalog
// would otherwise be a sixth hand-typed copy of a string five create paths already
// depend on.
const platformGoogleAds = "google-ads"

// ImageSlotCatalog publishes this platform's image rules in the platform-agnostic form
// the creative classifier consumes.
//
// It is BUILT FROM the same private tables and constants the create path validates
// against — performanceMaxImageSlots, demandGenImageSlots, displayImageSlots,
// searchImageSlots and the
// count ceilings beside them — rather than restating Google's published geometry a second
// time. That direction is the whole point of the file. A plan that promised an operator a
// 4:5 portrait at 480x600 while the create path demanded something else would be worse
// than no plan at all: the operator would spend a designer's afternoon producing an image
// the campaign then refuses. Restating the numbers here is how that drift starts, so the
// numbers are not restated, and slot_catalog_test.go pins this output against the private
// tables so a future edit to either side cannot separate them silently.
//
// The dependency runs googleads -> creative and never the reverse: the classifier knows
// nothing about Google, so classifying one asset does not link every platform client into
// the binary.
//
// Demand Gen's entries model the IMAGE creative's counts only. The Demand Gen video ad's
// logo (#460) shares the image logo's geometry but takes exactly one, which no entry here
// states.
func ImageSlotCatalog() creative.Catalog {
	pmax := performanceMaxCatalog()
	demandGen := demandGenCatalog()
	display := displayCatalog()
	search := searchCatalog()
	return creative.Merge(pmax, demandGen, display, search)
}

// shapeOf converts one row of a channel's private slot table into the classifier's shape
// rule. Google states every image slot the same way — one target ratio within its
// documented ±1%, plus a floor and no ceiling — so one converter serves all three
// channels and no channel gets to disagree about what its own table means.
//
// The shape carries NO label. Every Google slot has exactly one shape, and SlotSpec.Describe
// already opens with the slot's label, so naming the shape as well renders the operator's
// sentence with its subject twice over ("Demand Gen logo image: Demand Gen logo image 1:1 …").
// ShapeRule.Label earns its place only where a slot accepts several geometries and a gap has
// to say which one was missing.
func shapeOf(s imageSlot) creative.ShapeRule {
	return creative.ShapeRule{
		RatioW:         s.ratioW,
		RatioH:         s.ratioH,
		RatioTolerance: demandGenAspectTolerance,
		MinWidth:       s.minW,
		MinHeight:      s.minH,
	}
}

// slotFrom builds a spec from a table row plus the counts that row's channel states.
// Taking label, wire key and geometry from the row keeps every one of them single-sourced.
func slotFrom(channel string, s imageSlot, minCount, maxCount int, group string, groupMax int) creative.SlotSpec {
	return creative.SlotSpec{
		Platform: platformGoogleAds,
		Channel:  channel,
		Slot:     s.jsonKey,
		Label:    s.label,
		Shapes:   []creative.ShapeRule{shapeOf(s)},
		MaxBytes: MaxImageBytes,
		MinCount: minCount,
		MaxCount: maxCount,
		Group:    group,
		GroupMax: groupMax,
	}
}

// marketingImageGroup is the shared-ceiling name every channel's marketing shapes carry.
// The ceiling VALUE differs per channel — twenty on Performance Max and Demand Gen,
// fifteen on Display — and the classifier scopes a group by platform AND channel, so one
// name is correct and cannot leak a channel's budget into another's.
const marketingImageGroup = "marketing image"

// logoImageGroup is the shared-ceiling name for logo shapes that Google caps TOGETHER.
// Only Display uses it: its landscape and square logos are five between them. The
// Performance Max and Demand Gen logos keep per-slot ceilings, because those are the
// limits their create paths enforce.
const logoImageGroup = "logo image"

// performanceMaxCatalog publishes the five Performance Max slots.
//
// Both marketing shapes are REQUIRED here, individually — Google requires the landscape
// and the square on every asset group, which validatePerformanceMaxCreative enforces as
// two separate refusals — so they carry per-slot minimums and this channel states no
// either-or rule. Display requires both shapes in the same way. Demand Gen, whose
// identically named shapes are reciprocal, does state one; that contrast is a real
// difference in Google's rules, not an inconsistency in this file.
func performanceMaxCatalog() creative.Catalog {
	s := performanceMaxImageSlots
	return creative.Catalog{
		Slots: []creative.SlotSpec{
			slotFrom(advertisingChannelPerformanceMax, s[pmaxSlotMarketing], 1, 0, marketingImageGroup, maxPerformanceMaxMarketingImages),
			slotFrom(advertisingChannelPerformanceMax, s[pmaxSlotSquare], 1, 0, marketingImageGroup, maxPerformanceMaxMarketingImages),
			slotFrom(advertisingChannelPerformanceMax, s[pmaxSlotPortrait], 0, 0, marketingImageGroup, maxPerformanceMaxMarketingImages),
			slotFrom(advertisingChannelPerformanceMax, s[pmaxSlotLogo], minPerformanceMaxLogos, maxPerformanceMaxLogos, "", 0),
			slotFrom(advertisingChannelPerformanceMax, s[pmaxSlotLandscape], 0, maxPerformanceMaxLandscapeLogos, "", 0),
		},
	}
}

// demandGenCatalog publishes the five Demand Gen slots.
//
// The logo is required outright. The landscape and square marketing images are required
// only of each other, which is the AnyOfRule below; the portrait and tall portrait are
// optional and sit under the same twenty-image ceiling.
func demandGenCatalog() creative.Catalog {
	s := demandGenImageSlots
	ch := advertisingChannelDemandGen
	landscape := slotFrom(ch, s[0], 0, 0, marketingImageGroup, maxDemandGenMarketingImages)
	square := slotFrom(ch, s[1], 0, 0, marketingImageGroup, maxDemandGenMarketingImages)
	return creative.Catalog{
		Slots: []creative.SlotSpec{
			landscape,
			square,
			slotFrom(ch, s[2], 0, 0, marketingImageGroup, maxDemandGenMarketingImages),
			slotFrom(ch, s[3], 0, 0, marketingImageGroup, maxDemandGenMarketingImages),
			slotFrom(ch, s[logoSlotIndex], minDemandGenLogos, maxDemandGenLogos, "", 0),
		},
		AnyOf: []creative.AnyOfRule{{
			Slots: []string{landscape.Key(), square.Key()},
			Min:   1,
			Label: "Demand Gen marketing image, landscape or square",
		}},
	}
}

// displayCatalog publishes the four responsive-display slots.
//
// Both marketing shapes are REQUIRED, individually, as on Performance Max — Google's
// ResponsiveDisplayAdInfo requires at least one landscape and at least one square
// marketing image — so each carries a per-slot minimum under a FIFTEEN-image shared
// ceiling, and the channel states no either-or rule. Both logo arrays are optional and
// share one five-image ceiling, which is how Google states it, so they sit in their own
// group with no per-slot maximum (#437).
func displayCatalog() creative.Catalog {
	s := displayImageSlots
	ch := advertisingChannelDisplay
	return creative.Catalog{
		Slots: []creative.SlotSpec{
			slotFrom(ch, s[displayMarketingSlotIndex], 1, 0, marketingImageGroup, maxDisplayMarketingImages),
			slotFrom(ch, s[displaySquareMarketingSlotIndex], 1, 0, marketingImageGroup, maxDisplayMarketingImages),
			slotFrom(ch, s[displayLogoSlotIndex], 0, 0, logoImageGroup, maxDisplayLogos),
			slotFrom(ch, s[displaySquareLogoSlotIndex], 0, 0, logoImageGroup, maxDisplayLogos),
		},
	}
}

// searchCatalog publishes the three Search campaign asset slots (#477, #538).
//
// Every one is optional: a Search campaign serves on its ads alone, and validateSearchAssets
// accepts any of the three without the others. The square and landscape ad images both link
// as AD_IMAGE and Google counts them as one twenty-image ceiling (RESOURCE_LIMIT on the 21st,
// whatever the mix), so they share the marketing-image group under maxSearchAdImages, and the
// business logo carries its own ceiling of one.
func searchCatalog() creative.Catalog {
	s := searchImageSlots
	ch := advertisingChannelSearch
	return creative.Catalog{
		Slots: []creative.SlotSpec{
			slotFrom(ch, s[searchAdImageSlotIndex], 0, 0, marketingImageGroup, maxSearchAdImages),
			slotFrom(ch, s[searchLandscapeAdImageSlotIndex], 0, 0, marketingImageGroup, maxSearchAdImages),
			slotFrom(ch, s[searchBusinessLogoSlotIndex], 0, maxSearchBusinessLogos, "", 0),
		},
	}
}
