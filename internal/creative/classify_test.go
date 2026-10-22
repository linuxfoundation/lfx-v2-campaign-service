// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package creative

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// landscape/square/portrait are the three Google shapes, written as a test asset.
func asset(id string, w, h int, ordinal int) Asset {
	return Asset{ID: id, Width: w, Height: h, ByteSize: 100, Ordinal: ordinal}
}

func landscapeSlot() SlotSpec {
	return SlotSpec{
		Platform: "google-ads", Channel: "DEMAND_GEN", Slot: "marketingImages",
		Label:  "Demand Gen marketing image",
		Shapes: []ShapeRule{{RatioW: 191, RatioH: 100, RatioTolerance: 0.01, MinWidth: 600, MinHeight: 314}},
		// A single-image slot: required, and the platform takes at most one.
		MinCount: 1, MaxCount: 1,
	}
}

func squareSlot() SlotSpec {
	return SlotSpec{
		Platform: "google-ads", Channel: "DEMAND_GEN", Slot: "squareMarketingImages",
		Label:    "Demand Gen square marketing image",
		Shapes:   []ShapeRule{{RatioW: 1, RatioH: 1, RatioTolerance: 0.01, MinWidth: 300, MinHeight: 300}},
		MinCount: 0,
	}
}

func assignedTo(p Plan, key string) []string {
	for _, a := range p.Assignments {
		if a.Slot.Key() == key {
			return a.AssetIDs
		}
	}
	return nil
}

func gapFor(p Plan, label string) *Gap {
	for i := range p.Gaps {
		if p.Gaps[i].Label == label {
			return &p.Gaps[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// eligibility
// ---------------------------------------------------------------------------

// The ratio gate is the platform's own tolerance, and it has to hold on BOTH sides of
// the target: an image a percent too wide is refused exactly as one a percent too narrow
// is. A one-sided comparison is the classic way this goes wrong, and it would place an
// asset Google then rejects at the ad mutate.
func TestEligible_RatioToleranceIsTwoSided(t *testing.T) {
	slot := landscapeSlot()
	cases := []struct {
		name string
		w, h int
		want bool
	}{
		{"exactly 1.91:1", 1910, 1000, true},
		{"just inside, wide", 1927, 1000, true},   // +0.89%
		{"just inside, narrow", 1893, 1000, true}, // -0.89%
		{"just outside, wide", 1940, 1000, false}, // +1.57%
		{"just outside, narrow", 1880, 1000, false},
		{"square", 1000, 1000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, _ := slot.Eligible(asset("a", c.w, c.h, 0))
			if ok != c.want {
				t.Errorf("%dx%d: eligible=%v, want %v", c.w, c.h, ok, c.want)
			}
		})
	}
}

// An asset whose dimensions were never recorded must be UNCLASSIFIED, never "wrong
// shape". Reading an unrecorded 0x0 as a real size fails every minimum in the catalog,
// which would tell an operator their entire pre-existing library is the wrong size — a
// false statement that a dimension backfill, not a designer, actually fixes.
func TestClassify_UnknownDimensionsAreUnclassifiedNotFailing(t *testing.T) {
	cat := Catalog{Slots: []SlotSpec{landscapeSlot()}}
	plan := Classify(cat, []Asset{{ID: "old", ByteSize: 100, Ordinal: 0}})

	if len(plan.Unclassified) != 1 || plan.Unclassified[0] != "old" {
		t.Fatalf("an asset with no recorded dimensions must be unclassified, got %v", plan.Unclassified)
	}
	if len(plan.Unused) != 0 {
		t.Errorf("an unclassified asset must not also be reported as unused, got %v", plan.Unused)
	}
	gap := gapFor(plan, "Demand Gen marketing image")
	if gap == nil {
		t.Fatal("the required slot is still unfilled, so it must still report a gap")
	}
	if strings.Contains(gap.Reason, "old") {
		t.Errorf("the gap must not blame the undimensioned asset: %q", gap.Reason)
	}
}

// The byte cap is part of eligibility, because an image the fetcher would refuse to
// download is not an image this slot can use.
func TestEligible_RespectsByteCap(t *testing.T) {
	slot := landscapeSlot()
	slot.MaxBytes = 1000
	if ok, _ := slot.Eligible(Asset{ID: "big", Width: 1910, Height: 1000, ByteSize: 1001}); ok {
		t.Error("an asset over the slot's byte cap must not be eligible")
	}
	if ok, _ := slot.Eligible(Asset{ID: "ok", Width: 1910, Height: 1000, ByteSize: 1000}); !ok {
		t.Error("an asset exactly at the cap must be eligible")
	}
}

// ---------------------------------------------------------------------------
// ranking
// ---------------------------------------------------------------------------

// Closest ratio wins, then larger area, then library order. The third tie-break is what
// makes the plan STABLE: an operator who reloads the page must see the same assignment,
// and two assets uploaded in one batch can share a created_at.
func TestClassify_RankingIsClosestThenLargerThenStable(t *testing.T) {
	slot := landscapeSlot()
	slot.MaxCount = 0

	t.Run("closest ratio wins over larger area", func(t *testing.T) {
		plan := Classify(Catalog{Slots: []SlotSpec{slot}}, []Asset{
			asset("huge-but-drifting", 3820, 2010, 0), // 1.9005:1, inside tolerance
			asset("small-but-exact", 1910, 1000, 1),   // exactly 1.91:1
		})
		if got := assignedTo(plan, slot.Key()); len(got) == 0 || got[0] != "small-but-exact" {
			t.Errorf("the closest ratio must rank first, got %v", got)
		}
	})

	t.Run("equal ratio falls back to area", func(t *testing.T) {
		plan := Classify(Catalog{Slots: []SlotSpec{slot}}, []Asset{
			asset("small", 1910, 1000, 0),
			asset("large", 3820, 2000, 1),
		})
		if got := assignedTo(plan, slot.Key()); len(got) == 0 || got[0] != "large" {
			t.Errorf("at an equal ratio the larger image must rank first, got %v", got)
		}
	})

	t.Run("identical assets fall back to library order", func(t *testing.T) {
		plan := Classify(Catalog{Slots: []SlotSpec{slot}}, []Asset{
			asset("second", 1910, 1000, 1),
			asset("first", 1910, 1000, 0),
		})
		if got := assignedTo(plan, slot.Key()); len(got) == 0 || got[0] != "first" {
			t.Errorf("identical assets must rank by library order, got %v", got)
		}
	})
}

// Classifying the same inputs twice must produce the same plan — the endpoint is a GET
// an operator may refresh, and a plan that reshuffled on reload would be unusable as the
// basis for an override.
func TestClassify_IsDeterministic(t *testing.T) {
	cat := Catalog{Slots: []SlotSpec{landscapeSlot(), squareSlot()}}
	assets := []Asset{
		asset("a", 1910, 1000, 0), asset("b", 1910, 1000, 1),
		asset("c", 400, 400, 2), asset("d", 400, 400, 3),
	}
	first := Classify(cat, assets)
	for i := 0; i < 5; i++ {
		again := Classify(cat, assets)
		for _, a := range first.Assignments {
			got := assignedTo(again, a.Slot.Key())
			if strings.Join(got, ",") != strings.Join(a.AssetIDs, ",") {
				t.Fatalf("run %d assigned %v to %s, first run assigned %v", i, got, a.Slot.Key(), a.AssetIDs)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// reuse
// ---------------------------------------------------------------------------

// One square image must be free to serve a square slot on every platform at once — a
// single library feeding every platform is the whole point of the feature — while never
// appearing twice inside one slot, which would ask a platform to attach an image to
// itself.
func TestClassify_ReusesAcrossSlotsButNeverWithinOne(t *testing.T) {
	google := squareSlot()
	google.MinCount = 1
	other := SlotSpec{
		Platform: "linkedin-ads", Slot: "image",
		Label:    "LinkedIn square image",
		Shapes:   []ShapeRule{{RatioW: 1, RatioH: 1, RatioTolerance: 0.01, MinWidth: 360, MinHeight: 360}},
		MinCount: 1,
	}
	// One asset, two platforms, and a slot that would take three images if it could.
	google.MaxCount = 3
	plan := Classify(Catalog{Slots: []SlotSpec{google, other}}, []Asset{asset("only", 400, 400, 0)})

	if got := assignedTo(plan, google.Key()); len(got) != 1 || got[0] != "only" {
		t.Errorf("google slot got %v, want the one asset exactly once", got)
	}
	if got := assignedTo(plan, other.Key()); len(got) != 1 || got[0] != "only" {
		t.Errorf("linkedin slot got %v, want the same asset reused across platforms", got)
	}
	if len(plan.Gaps) != 0 {
		t.Errorf("both requirements are met, so there must be no gaps: %+v", plan.Gaps)
	}
}

// ---------------------------------------------------------------------------
// filling and shared ceilings
// ---------------------------------------------------------------------------

// A slot fills to its minimum, not past it, so a plan does not silently consume a shared
// ceiling an operator wanted for another shape.
func TestClassify_FillsToTheMinimumOnly(t *testing.T) {
	slot := landscapeSlot()
	slot.MinCount = 2
	slot.MaxCount = 10
	plan := Classify(Catalog{Slots: []SlotSpec{slot}}, []Asset{
		asset("a", 1910, 1000, 0), asset("b", 1910, 1000, 1),
		asset("c", 1910, 1000, 2), asset("d", 1910, 1000, 3),
	})
	if got := assignedTo(plan, slot.Key()); len(got) != 2 {
		t.Errorf("a slot requiring 2 must take 2, got %v", got)
	}
	if len(plan.Unused) != 2 {
		t.Errorf("the two assets left over must be reported as unused, got %v", plan.Unused)
	}
}

// The shared ceiling is shared: the earlier slot in the table wins the last place, and
// the slot that loses it says so in a sentence naming the ceiling rather than claiming
// nothing fits.
func TestClassify_SharedCeilingIsSpentInCatalogOrder(t *testing.T) {
	first := landscapeSlot()
	first.Group, first.GroupMax, first.MinCount, first.MaxCount = "marketing image", 1, 1, 1
	second := squareSlot()
	second.Group, second.GroupMax, second.MinCount = "marketing image", 1, 1

	plan := Classify(Catalog{Slots: []SlotSpec{first, second}}, []Asset{
		asset("wide", 1910, 1000, 0),
		asset("sq", 400, 400, 1),
	})

	if got := assignedTo(plan, first.Key()); len(got) != 1 {
		t.Fatalf("the first slot must win the single place in the group, got %v", got)
	}
	if got := assignedTo(plan, second.Key()); len(got) != 0 {
		t.Errorf("the group ceiling is spent, so the second slot must get nothing, got %v", got)
	}
	gap := gapFor(plan, second.Label)
	if gap == nil {
		t.Fatal("the second slot is required and empty, so it must report a gap")
	}
	if !strings.Contains(gap.Reason, "combined ceiling") {
		t.Errorf("the gap must say the ceiling is why, not that nothing fits: %q", gap.Reason)
	}
	if len(plan.Unused) != 1 || plan.Unused[0] != "sq" {
		t.Errorf("the square that could not be placed must read as unused, got %v", plan.Unused)
	}
}

// Two channels' ceilings are independent even when the group carries the same name —
// Display's fifteen must never draw down Demand Gen's twenty.
func TestClassify_GroupCeilingsAreScopedPerChannel(t *testing.T) {
	demandGen := landscapeSlot()
	demandGen.Group, demandGen.GroupMax, demandGen.MinCount, demandGen.MaxCount = "marketing image", 1, 1, 1
	display := landscapeSlot()
	display.Channel = "DISPLAY"
	display.Label = "Display marketing image"
	display.Group, display.GroupMax, display.MinCount, display.MaxCount = "marketing image", 1, 1, 1

	plan := Classify(Catalog{Slots: []SlotSpec{demandGen, display}}, []Asset{asset("wide", 1910, 1000, 0)})

	if got := assignedTo(plan, demandGen.Key()); len(got) != 1 {
		t.Errorf("demand gen got %v, want the asset", got)
	}
	if got := assignedTo(plan, display.Key()); len(got) != 1 {
		t.Errorf("display got %v, want the same asset — its ceiling is its own", got)
	}
	if len(plan.Gaps) != 0 {
		t.Errorf("neither channel's ceiling is exhausted, so there must be no gaps: %+v", plan.Gaps)
	}
}

// ---------------------------------------------------------------------------
// gaps
// ---------------------------------------------------------------------------

// A gap is the sentence an operator takes to a designer, so it must state the shape and
// the floor. "No eligible asset" is not actionable; "1.91:1 (within 1%), at least
// 600x314" is.
func TestClassify_GapNamesTheShapeAndTheFloor(t *testing.T) {
	plan := Classify(Catalog{Slots: []SlotSpec{landscapeSlot()}}, []Asset{asset("square", 400, 400, 0)})

	gap := gapFor(plan, "Demand Gen marketing image")
	if gap == nil {
		t.Fatal("a required slot with nothing eligible must produce a gap")
	}
	if gap.Need != 1 {
		t.Errorf("Need = %d, want 1", gap.Need)
	}
	if gap.Platform != "google-ads" || gap.Channel != "DEMAND_GEN" {
		t.Errorf("the gap must carry its platform and channel, got %q/%q", gap.Platform, gap.Channel)
	}
	for _, want := range []string{"191:100", "within 1%", "at least 600x314"} {
		if !strings.Contains(gap.Reason, want) {
			t.Errorf("the gap must state %q, got: %q", want, gap.Reason)
		}
	}
}

// An OPTIONAL slot left empty is not a gap. Reporting one would bury the shortfalls that
// actually block a launch under a list of images nobody asked for.
func TestClassify_OptionalSlotsProduceNoGap(t *testing.T) {
	plan := Classify(Catalog{Slots: []SlotSpec{squareSlot()}}, nil)
	if len(plan.Gaps) != 0 {
		t.Errorf("an optional slot left empty must not be a gap, got %+v", plan.Gaps)
	}
}

// ---------------------------------------------------------------------------
// either-or requirements
// ---------------------------------------------------------------------------

// Google's reciprocal pair: either shape satisfies it, neither leaves a gap, and the gap
// names BOTH shapes — an operator told only about the landscape would make an image they
// did not need when the square they already had would have done.
func TestClassify_AnyOfRule(t *testing.T) {
	landscape, square := landscapeSlot(), squareSlot()
	landscape.MinCount = 0
	cat := Catalog{
		Slots: []SlotSpec{landscape, square},
		AnyOf: []AnyOfRule{{
			Slots: []string{landscape.Key(), square.Key()},
			Min:   1,
			Label: "Demand Gen marketing image, landscape or square",
		}},
	}

	t.Run("the landscape alone satisfies it", func(t *testing.T) {
		plan := Classify(cat, []Asset{asset("wide", 1910, 1000, 0)})
		if len(plan.Gaps) != 0 {
			t.Errorf("want no gaps, got %+v", plan.Gaps)
		}
	})

	t.Run("the square alone satisfies it", func(t *testing.T) {
		plan := Classify(cat, []Asset{asset("sq", 400, 400, 0)})
		if len(plan.Gaps) != 0 {
			t.Errorf("want no gaps, got %+v", plan.Gaps)
		}
	})

	t.Run("neither leaves one gap naming both shapes", func(t *testing.T) {
		plan := Classify(cat, []Asset{asset("tall", 480, 600, 0)})
		if len(plan.Gaps) != 1 {
			t.Fatalf("want exactly one gap for the pair, got %+v", plan.Gaps)
		}
		gap := plan.Gaps[0]
		if gap.SlotKey != "" {
			t.Errorf("a gap spanning several slots must not claim one slot key, got %q", gap.SlotKey)
		}
		for _, want := range []string{"191:100", "1:1", "at least 600x314", "at least 300x300"} {
			if !strings.Contains(gap.Reason, want) {
				t.Errorf("the gap must offer %q as a way to satisfy it, got: %q", want, gap.Reason)
			}
		}
	})
}

// A rule whose member slots are not in the catalog must stay silent. A caller asking for
// one channel's plan must never be told it is missing another channel's image.
func TestClassify_AnyOfRuleIgnoresSlotsNotInTheCatalog(t *testing.T) {
	plan := Classify(Catalog{
		Slots: []SlotSpec{landscapeSlot()},
		AnyOf: []AnyOfRule{{Slots: []string{"meta-ads/image"}, Min: 1, Label: "Meta image"}},
	}, []Asset{asset("wide", 1910, 1000, 0)})

	if gapFor(plan, "Meta image") != nil {
		t.Errorf("a rule over slots the catalog does not publish must produce no gap: %+v", plan.Gaps)
	}
}

// ---------------------------------------------------------------------------
// merge
// ---------------------------------------------------------------------------

// Merging preserves order and carries the cross-slot rules with their slots, so a
// registry assembled from several platforms cannot lose a requirement on the way in.
func TestMerge_KeepsOrderAndRules(t *testing.T) {
	a := Catalog{Slots: []SlotSpec{landscapeSlot()}, AnyOf: []AnyOfRule{{Label: "a", Min: 1}}}
	b := Catalog{Slots: []SlotSpec{squareSlot()}, AnyOf: []AnyOfRule{{Label: "b", Min: 1}}}

	got := Merge(a, b)
	if len(got.Slots) != 2 || got.Slots[0].Slot != "marketingImages" || got.Slots[1].Slot != "squareMarketingImages" {
		t.Errorf("merge must preserve slot order, got %+v", got.Slots)
	}
	if len(got.AnyOf) != 2 || got.AnyOf[0].Label != "a" || got.AnyOf[1].Label != "b" {
		t.Errorf("merge must carry every rule, got %+v", got.AnyOf)
	}
}
