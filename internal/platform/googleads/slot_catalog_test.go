// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/creative"
)

// ---------------------------------------------------------------------------
// The catalog against the create path
// ---------------------------------------------------------------------------
//
// These tests exist because the catalog is a PROMISE and the validators are the
// enforcement. The promise is made to an operator who will act on it — a gap saying
// "Demand Gen needs a 4:5 portrait of at least 480x600" sends someone to produce that
// image — so a catalog that drifts from what the create path actually refuses does not
// produce a wrong field in a response, it wastes a designer's afternoon and then fails
// the launch anyway.
//
// Comparing the catalog's numbers against the same table it was built from would prove
// nothing. What these tests do instead is DRIVE THE REAL VALIDATORS: whatever the catalog
// calls required must be refused when absent, and whatever it calls optional must be
// accepted when absent. That pins the two sides to each other rather than to a literal.

// slotFiller builds a channel's CampaignInput with exactly the named slots carrying one
// image each, and asks that channel's own validator about it.
type slotFiller struct {
	channel  string
	build    func(filled map[string]bool) CampaignInput
	validate func(in CampaignInput) error
}

const probeImageURL = "https://cdn.example.org/probe.png"

func oneImage(on bool) []string {
	if !on {
		return nil
	}
	return []string{probeImageURL}
}

func googleSlotFillers() []slotFiller {
	return []slotFiller{
		{
			channel: advertisingChannelPerformanceMax,
			build: func(filled map[string]bool) CampaignInput {
				c := fullPMaxCreative() // text fields only; every image slot is reset below
				c.MarketingImages = oneImage(filled[assetFieldMarketingImage])
				c.SquareMarketingImages = oneImage(filled[assetFieldSquareMarketingImage])
				c.PortraitImages = oneImage(filled[assetFieldPortraitMarketingImg])
				c.LogoImages = oneImage(filled[assetFieldLogo])
				c.LandscapeLogoImages = oneImage(filled[assetFieldLandscapeLogo])
				return pmaxInput(c)
			},
			validate: func(in CampaignInput) error {
				_, err := validatePerformanceMaxCreative(campaignKindPerformanceMax, in)
				return err
			},
		},
		{
			channel: advertisingChannelDemandGen,
			build: func(filled map[string]bool) CampaignInput {
				c := fullCreative()
				c.MarketingImages = oneImage(filled[demandGenImageSlots[0].jsonKey])
				c.SquareMarketingImages = oneImage(filled[demandGenImageSlots[1].jsonKey])
				c.PortraitImages = oneImage(filled[demandGenImageSlots[2].jsonKey])
				c.TallPortraitImages = oneImage(filled[demandGenImageSlots[3].jsonKey])
				c.LogoImages = oneImage(filled[demandGenImageSlots[logoSlotIndex].jsonKey])
				return creativeInput(c)
			},
			validate: func(in CampaignInput) error {
				_, err := validateDemandGenCreative(campaignKindDemandGen, in)
				return err
			},
		},
		{
			channel: advertisingChannelDisplay,
			build: func(filled map[string]bool) CampaignInput {
				c := fullDisplayCreative()
				c.MarketingImages = oneImage(filled[displayImageSlots[displayMarketingSlotIndex].jsonKey])
				c.SquareMarketingImages = oneImage(filled[displayImageSlots[displaySquareMarketingSlotIndex].jsonKey])
				c.LogoImages = oneImage(filled[displayImageSlots[displayLogoSlotIndex].jsonKey])
				c.SquareLogoImages = oneImage(filled[displayImageSlots[displaySquareLogoSlotIndex].jsonKey])
				return displayCreativeInput(c)
			},
			validate: func(in CampaignInput) error {
				_, err := validateDisplayCreative(campaignKindDisplay, in)
				return err
			},
		},
	}
}

// slotsOfChannel returns the catalog rows for one channel, keyed by wire name.
func slotsOfChannel(channel string) map[string]creative.SlotSpec {
	out := map[string]creative.SlotSpec{}
	for _, s := range ImageSlotCatalog().Slots {
		if s.Channel == channel {
			out[s.Slot] = s
		}
	}
	return out
}

func allFilled(slots map[string]creative.SlotSpec) map[string]bool {
	filled := map[string]bool{}
	for key := range slots {
		filled[key] = true
	}
	return filled
}

// Every slot the catalog publishes for a channel must be one that channel's creative can
// actually carry, and the whole set together must validate. A catalog naming a slot the
// create path has no field for would promise an operator a place to put an image that
// does not exist.
func TestImageSlotCatalog_FullSetValidates(t *testing.T) {
	for _, f := range googleSlotFillers() {
		t.Run(f.channel, func(t *testing.T) {
			slots := slotsOfChannel(f.channel)
			if len(slots) == 0 {
				t.Fatalf("the catalog publishes no slots for %s", f.channel)
			}
			if err := f.validate(f.build(allFilled(slots))); err != nil {
				t.Fatalf("every catalog slot filled must validate, got: %v", err)
			}
		})
	}
}

// A slot the catalog marks REQUIRED must be one the create path refuses to go without.
// This is the direction that wastes an operator's time in the other failure mode — a
// catalog that under-states a requirement lets them believe a library is complete right
// up until the launch fails.
func TestImageSlotCatalog_RequiredSlotsAreEnforcedUpstream(t *testing.T) {
	for _, f := range googleSlotFillers() {
		slots := slotsOfChannel(f.channel)
		for key, spec := range slots {
			if spec.MinCount == 0 {
				continue
			}
			t.Run(f.channel+"/"+key, func(t *testing.T) {
				filled := allFilled(slots)
				filled[key] = false
				if err := f.validate(f.build(filled)); err == nil {
					t.Fatalf("the catalog requires %q (MinCount %d), but the create path accepts a creative without it",
						spec.Label, spec.MinCount)
				}
			})
		}
	}
}

// And the mirror: a slot the catalog marks OPTIONAL must be one the create path really
// does accept without. A catalog that over-states a requirement reports a gap for an
// image Google never asked for.
//
// Removing one optional slot at a time, with every other slot present, is what isolates
// the claim — an either-or pair is two optional slots whose sibling is still filled here,
// which is exactly the reading the catalog's AnyOfRule makes.
func TestImageSlotCatalog_OptionalSlotsAreReallyOptional(t *testing.T) {
	for _, f := range googleSlotFillers() {
		slots := slotsOfChannel(f.channel)
		for key, spec := range slots {
			if spec.MinCount > 0 {
				continue
			}
			t.Run(f.channel+"/"+key, func(t *testing.T) {
				filled := allFilled(slots)
				filled[key] = false
				if err := f.validate(f.build(filled)); err != nil {
					t.Fatalf("the catalog marks %q optional, but the create path refuses a creative without it: %v",
						spec.Label, err)
				}
			})
		}
	}
}

// An either-or rule must be enforced upstream as an either-or: refused when every member
// is absent, accepted when any ONE of them is present. Both halves matter — the first is
// the rule existing at all, the second is the rule not secretly being two requirements.
func TestImageSlotCatalog_AnyOfRulesMatchTheCreatePath(t *testing.T) {
	cat := ImageSlotCatalog()
	byKey := map[string]creative.SlotSpec{}
	for _, s := range cat.Slots {
		byKey[s.Key()] = s
	}

	for _, rule := range cat.AnyOf {
		if len(rule.Slots) == 0 {
			t.Fatalf("an either-or rule with no member slots can never be satisfied: %q", rule.Label)
		}
		first, ok := byKey[rule.Slots[0]]
		if !ok {
			t.Fatalf("either-or rule %q names slot %q, which the catalog does not publish", rule.Label, rule.Slots[0])
		}
		var filler slotFiller
		for _, f := range googleSlotFillers() {
			if f.channel == first.Channel {
				filler = f
			}
		}
		slots := slotsOfChannel(first.Channel)

		t.Run(rule.Label+"/none", func(t *testing.T) {
			filled := allFilled(slots)
			for _, key := range rule.Slots {
				filled[byKey[key].Slot] = false
			}
			if err := filler.validate(filler.build(filled)); err == nil {
				t.Fatalf("the catalog requires %d across %v, but the create path accepts a creative with none of them",
					rule.Min, rule.Slots)
			}
		})

		for _, member := range rule.Slots {
			t.Run(rule.Label+"/only "+member, func(t *testing.T) {
				filled := allFilled(slots)
				for _, key := range rule.Slots {
					filled[byKey[key].Slot] = false
				}
				filled[byKey[member].Slot] = true
				if err := filler.validate(filler.build(filled)); err != nil {
					t.Fatalf("%q alone must satisfy %q, but the create path refuses it: %v", member, rule.Label, err)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// The catalog against the geometry tables
// ---------------------------------------------------------------------------

// Every published shape must be the channel table's own row, unchanged. This is the
// cheap half of the guarantee — it catches a hand-typed ratio or minimum being
// introduced here later, which is precisely how the two sides would start to drift.
func TestImageSlotCatalog_GeometryComesFromTheChannelTables(t *testing.T) {
	tables := map[string][]imageSlot{
		advertisingChannelPerformanceMax: performanceMaxImageSlots,
		advertisingChannelDemandGen:      demandGenImageSlots,
		advertisingChannelDisplay:        displayImageSlots,
	}

	for _, spec := range ImageSlotCatalog().Slots {
		table, ok := tables[spec.Channel]
		if !ok {
			t.Fatalf("slot %q names channel %q, which has no geometry table", spec.Key(), spec.Channel)
		}
		var row imageSlot
		found := false
		for _, r := range table {
			if r.jsonKey == spec.Slot {
				row, found = r, true
			}
		}
		if !found {
			t.Fatalf("slot %q is not a row of its channel's table", spec.Key())
		}
		if len(spec.Shapes) != 1 {
			t.Fatalf("slot %q publishes %d shapes; every Google slot states exactly one", spec.Key(), len(spec.Shapes))
		}
		got := spec.Shapes[0]
		// No Label on the shape: the slot already carries it, and Describe would otherwise
		// print it twice — see TestImageSlotCatalog_DescribesEachSlotOnce.
		want := creative.ShapeRule{
			RatioW:         row.ratioW,
			RatioH:         row.ratioH,
			RatioTolerance: demandGenAspectTolerance,
			MinWidth:       row.minW,
			MinHeight:      row.minH,
		}
		if got != want {
			t.Errorf("slot %q publishes %+v, but its table row is %+v", spec.Key(), got, want)
		}
		if spec.Label != row.label {
			t.Errorf("slot %q is labelled %q, but its table row is labelled %q", spec.Key(), spec.Label, row.label)
		}
		if spec.MaxBytes != maxDemandGenImageBytes {
			t.Errorf("slot %q caps at %d bytes; every googleads image is fetched under maxDemandGenImageBytes (%d)",
				spec.Key(), spec.MaxBytes, maxDemandGenImageBytes)
		}
		if spec.Platform != platformGoogleAds {
			t.Errorf("slot %q reports platform %q, want %q", spec.Key(), spec.Platform, platformGoogleAds)
		}
	}
}

// The shared ceilings must be the ones the create path enforces, and must cover the same
// slots. A plan that filled twenty marketing images under a fifteen-image Display ceiling
// would be refused at the ad mutate — after the budget and the campaign exist.
func TestImageSlotCatalog_SharedCeilingsMatchTheCreatePath(t *testing.T) {
	want := map[string]int{
		advertisingChannelPerformanceMax: maxPerformanceMaxMarketingImages,
		advertisingChannelDemandGen:      maxDemandGenMarketingImages,
		advertisingChannelDisplay:        maxDisplayMarketingImages,
	}
	// The slots each channel counts toward that ceiling, as its validator counts them:
	// Performance Max sums its three marketing shapes, Demand Gen all four, Display its
	// landscape and square pair.
	wantMembers := map[string]int{
		advertisingChannelPerformanceMax: 3,
		advertisingChannelDemandGen:      4,
		advertisingChannelDisplay:        2,
	}

	members := map[string]int{}
	for _, spec := range ImageSlotCatalog().Slots {
		if spec.Group == "" {
			continue
		}
		if spec.Group != marketingImageGroup {
			t.Errorf("slot %q is in unexpected group %q", spec.Key(), spec.Group)
			continue
		}
		members[spec.Channel]++
		if spec.GroupMax != want[spec.Channel] {
			t.Errorf("slot %q shares a ceiling of %d, but %s enforces %d",
				spec.Key(), spec.GroupMax, spec.Channel, want[spec.Channel])
		}
	}
	for channel, n := range wantMembers {
		if members[channel] != n {
			t.Errorf("%s counts %d slots toward its marketing ceiling, but the catalog groups %d",
				channel, n, members[channel])
		}
	}
}

// Per-slot ceilings must be the create path's own, and a slot with no documented per-slot
// ceiling must publish none rather than borrow its group's.
func TestImageSlotCatalog_PerSlotCeilingsMatchTheCreatePath(t *testing.T) {
	want := map[string]int{
		advertisingChannelPerformanceMax + "/" + assetFieldLogo:                                 maxPerformanceMaxLogos,
		advertisingChannelPerformanceMax + "/" + assetFieldLandscapeLogo:                        maxPerformanceMaxLandscapeLogos,
		advertisingChannelDemandGen + "/" + demandGenImageSlots[logoSlotIndex].jsonKey:          maxDemandGenLogos,
		advertisingChannelDisplay + "/" + displayImageSlots[displayLogoSlotIndex].jsonKey:       maxDisplayLogos,
		advertisingChannelDisplay + "/" + displayImageSlots[displaySquareLogoSlotIndex].jsonKey: maxDisplayLogos,
	}
	for _, spec := range ImageSlotCatalog().Slots {
		if got := spec.MaxCount; got != want[spec.Channel+"/"+spec.Slot] {
			t.Errorf("slot %q publishes MaxCount %d, want %d", spec.Key(), got, want[spec.Channel+"/"+spec.Slot])
		}
	}
}

// Describe() is the sentence an operator hands a designer, so it must read like one. Every
// Google slot has a single shape and SlotSpec.Describe already opens with the slot's label,
// so labelling the shape too would render the subject twice ("Demand Gen logo image: Demand
// Gen logo image 1:1 ..."). That is not a cosmetic complaint: the label names the CHANNEL,
// and a sentence that stutters its channel is the one an operator stops reading.
func TestImageSlotCatalog_DescribesEachSlotOnce(t *testing.T) {
	for _, spec := range ImageSlotCatalog().Slots {
		got := spec.Describe()
		if strings.Count(got, spec.Label) != 1 {
			t.Errorf("slot %q describes itself as %q; the label must appear exactly once", spec.Key(), got)
		}
		// The floor is the half a designer actually acts on, so it travels with every slot.
		if !strings.Contains(got, "at least ") {
			t.Errorf("slot %q describes itself as %q, naming no pixel floor", spec.Key(), got)
		}
	}
}
