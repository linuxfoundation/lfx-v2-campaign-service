// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package creative

import (
	"fmt"
	"sort"
	"strings"
)

// Assignment is one slot and the assets the classifier placed in it, in the order they
// should be attached.
type Assignment struct {
	Slot     SlotSpec
	AssetIDs []string
}

// Gap is one requirement the library could not meet, with the sentence the operator takes
// back to their designer.
//
// A gap is first-class output, not an error: a plan with gaps is a correct and complete
// answer about a library that is not yet complete, and it is the most useful thing this
// endpoint produces. Only REQUIRED shortfalls are gaps — an optional slot left empty is
// simply an optional slot left empty, and reporting it would bury the shortfalls that
// actually block a launch.
//
// The fields are flat rather than a nested SlotSpec because a gap may span several slots
// (see AnyOfRule), and because this is what the API serves.
type Gap struct {
	Platform string
	Channel  string
	// SlotKey is the slot's Key(), or empty for a requirement spanning several slots.
	SlotKey string
	Label   string
	// Need is how many MORE images the requirement needs beyond what was assigned.
	Need   int
	Reason string
}

// Plan is the whole computed grid for one brief against one set of platforms.
type Plan struct {
	// Assignments covers every slot in the catalog that received at least one asset.
	Assignments []Assignment
	// Gaps covers every requirement left short.
	Gaps []Gap
	// Unclassified names assets whose dimensions were never recorded (uploaded before the
	// service stored them). They are NOT counted against any slot and NOT reported as
	// failing one: their shape is unknown, which is a different answer from "wrong shape",
	// and a backfill makes them classifiable without the operator doing anything.
	Unclassified []string
	// Unused names assets whose dimensions ARE known and which this plan did not PLACE in
	// any slot. Not placed is wider than not eligible, deliberately: an asset also lands
	// here when every slot that would accept it was already filled to its minimum, or when
	// a sibling slot had already spent the shared group ceiling. The question it answers is
	// the mirror of Gaps — after a folder upload, not only "what am I missing" but "which
	// of the files I just dropped is this plan not using". Whether an unused asset is
	// unusable or merely surplus is read off Gaps: a plan with no gaps has nothing missing,
	// so what is here is surplus.
	Unused []string
}

// Classify matches a brief's assets against a slot catalog.
//
// It is a PURE FUNCTION of its inputs — no reads, no writes, no clock — so the same
// library and catalog always produce the same plan, and the endpoint that serves it can
// be a GET an operator may call as often as they like before committing anything.
//
// Three rules decide the result:
//
//   - ELIGIBILITY is the slot's own: the asset satisfies one of the slot's shape rules
//     and the slot's byte cap. A gate failure is never silent — the slot either fills or
//     produces a gap naming what it wanted.
//   - RANKING among eligible assets prefers the closest fit to the slot's target ratio,
//     breaks ties by larger pixel area, and breaks remaining ties by the library's own
//     order, so an unchanged library yields an unchanged plan.
//   - REUSE is allowed across slots and refused within one. The same square image
//     legitimately serves Google's SQUARE_MARKETING_IMAGE and a 1:1 slot on another
//     platform — one pool feeding every platform is the entire point — but listing it
//     twice in one slot would ask a platform to attach the same image to itself.
//
// Slots are filled in catalog order, which matters only where a shared Group ceiling runs
// out: the earlier slot in the table wins the last place in the group. The googleads
// tables list the required marketing shapes before the optional ones for that reason.
func Classify(catalog Catalog, assets []Asset) Plan {
	plan := Plan{}
	used := make(map[string]bool, len(assets))

	for _, a := range assets {
		if !a.known() {
			plan.Unclassified = append(plan.Unclassified, a.ID)
		}
	}

	// groupUsed tracks consumption of each shared ceiling, keyed by platform+channel+group
	// so two channels' identically-named groups never draw on one another's budget.
	groupUsed := make(map[string]int)
	// placedBySlot feeds the cross-slot rules below, which are evaluated against what the
	// fill actually achieved rather than against eligibility — a slot whose eligible asset
	// lost the last place in a group contributes nothing to an AnyOfRule either.
	placedBySlot := make(map[string]int, len(catalog.Slots))
	specByKey := make(map[string]SlotSpec, len(catalog.Slots))

	for _, slot := range catalog.Slots {
		specByKey[slot.Key()] = slot

		ranked := rank(slot, assets)
		want := slot.MinCount
		if want < 1 {
			want = 1
		}
		if slot.MaxCount > 0 && want > slot.MaxCount {
			want = slot.MaxCount
		}

		groupKey := ""
		groupLeft := -1
		if slot.Group != "" && slot.GroupMax > 0 {
			groupKey = slot.Platform + "/" + slot.Channel + "/" + slot.Group
			groupLeft = slot.GroupMax - groupUsed[groupKey]
		}

		var placed []string
		for _, a := range ranked {
			if len(placed) >= want || groupLeft == 0 {
				break
			}
			placed = append(placed, a.ID)
			used[a.ID] = true
			if groupLeft > 0 {
				groupLeft--
			}
		}
		if groupKey != "" {
			groupUsed[groupKey] += len(placed)
		}
		placedBySlot[slot.Key()] = len(placed)

		if len(placed) > 0 {
			plan.Assignments = append(plan.Assignments, Assignment{Slot: slot, AssetIDs: placed})
		}
		if short := slot.MinCount - len(placed); short > 0 {
			plan.Gaps = append(plan.Gaps, Gap{
				Platform: slot.Platform,
				Channel:  slot.Channel,
				SlotKey:  slot.Key(),
				Label:    slot.Label,
				Need:     short,
				Reason:   slotGapReason(slot, len(ranked), len(placed), short, groupLeft == 0),
			})
		}
	}

	plan.Gaps = append(plan.Gaps, anyOfGaps(catalog, specByKey, placedBySlot)...)

	for _, a := range assets {
		if a.known() && !used[a.ID] {
			plan.Unused = append(plan.Unused, a.ID)
		}
	}
	return plan
}

// anyOfGaps reports the cross-slot requirements the fill did not satisfy.
//
// A member slot that the catalog does not contain is skipped rather than counted as zero:
// a caller who asked for one channel must not be told it is missing another channel's
// image.
func anyOfGaps(catalog Catalog, specByKey map[string]SlotSpec, placedBySlot map[string]int) []Gap {
	var out []Gap
	for _, rule := range catalog.AnyOf {
		have, known := 0, 0
		var members []SlotSpec
		for _, key := range rule.Slots {
			spec, ok := specByKey[key]
			if !ok {
				continue
			}
			known++
			members = append(members, spec)
			have += placedBySlot[key]
		}
		if known == 0 || have >= rule.Min {
			continue
		}
		out = append(out, Gap{
			Platform: members[0].Platform,
			Channel:  members[0].Channel,
			Label:    rule.Label,
			Need:     rule.Min - have,
			Reason:   anyOfGapReason(rule, members, have),
		})
	}
	return out
}

// slotGapReason says which of the three ways a slot can come up short actually happened,
// since the remedy differs: upload a different shape, upload more of that shape, or free
// a place in the shared ceiling first.
func slotGapReason(slot SlotSpec, eligible, placed, short int, groupExhausted bool) string {
	switch {
	case groupExhausted && eligible == 0:
		// Both blockers are real and freeing a place would not fix this slot, so naming only
		// the ceiling would send the operator to delete an image and leave the gap standing.
		return fmt.Sprintf("%s — nothing in this brief's library qualifies, and the combined ceiling of %d images across the %s slots is already filled; both have to be dealt with",
			slot.Describe(), slot.GroupMax, slot.Group)
	case groupExhausted:
		return fmt.Sprintf("%s — %d more needed, but the combined ceiling of %d images across the %s slots is already filled; free a place there first",
			slot.Describe(), short, slot.GroupMax, slot.Group)
	case eligible == 0:
		return fmt.Sprintf("%s — nothing in this brief's library qualifies", slot.Describe())
	default:
		return fmt.Sprintf("%s — %d more needed; %d of the uploaded assets qualify, %d already placed here",
			slot.Describe(), short, eligible, placed)
	}
}

// anyOfGapReason spells out every shape that would satisfy the requirement, because an
// either-or gap is satisfiable several ways and naming only one of them would send the
// operator to make an image they did not need.
func anyOfGapReason(rule AnyOfRule, members []SlotSpec, have int) string {
	parts := make([]string, 0, len(members))
	for _, m := range members {
		parts = append(parts, m.Describe())
	}
	return fmt.Sprintf("%s — %d needed across these shapes and %d placed; any of them satisfies it: %s",
		rule.Label, rule.Min, have, strings.Join(parts, "; or "))
}

// rank returns the slot's eligible assets, best fit first.
func rank(slot SlotSpec, assets []Asset) []Asset {
	type scored struct {
		asset Asset
		drift float64
	}
	var in []scored
	for _, a := range assets {
		if ok, drift := slot.Eligible(a); ok {
			in = append(in, scored{asset: a, drift: drift})
		}
	}
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].drift != in[j].drift {
			return in[i].drift < in[j].drift
		}
		ai := in[i].asset.Width * in[i].asset.Height
		aj := in[j].asset.Width * in[j].asset.Height
		if ai != aj {
			return ai > aj
		}
		return in[i].asset.Ordinal < in[j].asset.Ordinal
	})
	out := make([]Asset, 0, len(in))
	for _, s := range in {
		out = append(out, s.asset)
	}
	return out
}
