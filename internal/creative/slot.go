// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package creative holds the platform-agnostic description of what shape an ad platform
// demands of an image, and the classifier that matches a brief's uploaded assets against
// those shapes.
//
// It deliberately depends on NOTHING — not on domain/model, not on any platform package.
// That direction is what makes the catalog composable: each platform package publishes
// its own rows as creative.SlotSpec values (see googleads.ImageSlotCatalog), and a caller
// assembles the registry from the platforms it cares about. If this package imported the
// platform packages instead, every one of them would have to be linked in to classify a
// single asset, and adding a platform would mean editing this package.
package creative

import "fmt"

// ShapeRule is ONE acceptable geometry for a slot.
//
// A slot carries a LIST of these rather than a single ratio, because the platforms do not
// agree on how they bound an image. Google states one target aspect ratio per slot with a
// tolerance and a floor; LinkedIn accepts any of three named shapes, each with its own
// floor AND ceiling; Microsoft states a floor and a ceiling and no ratio at all. One rule
// per acceptable shape expresses all three without a per-platform branch in the matcher —
// an asset satisfies a slot when it satisfies ANY of the slot's rules.
//
// Every bound is OPTIONAL and zero means ABSENT, not zero: a slot with no ratio leaves
// RatioW/RatioH at 0, and a slot with no upper bound leaves MaxWidth/MaxHeight at 0. No
// real image has a zero dimension, so the zero value can carry "unbounded" without
// colliding with a value a platform could actually state.
type ShapeRule struct {
	// Label names this shape when the slot has more than one, so a gap can say which
	// shape was missing ("LinkedIn vertical image"). Empty when the slot has a single
	// shape and the slot's own label already says everything.
	Label string

	// RatioW/RatioH are the target aspect ratio as the platform documents it, held as two
	// integers rather than a float so the documented value IS the value in the code —
	// 1.91:1 written as 191/100 cannot drift the way a hand-rounded 1.91 can. Both zero
	// means the platform states no ratio for this shape.
	RatioW int
	RatioH int

	// RatioTolerance is the fractional slack allowed on that ratio (Google's documented
	// "+-1%" is 0.01). Meaningless, and ignored, when no ratio is set.
	RatioTolerance float64

	// MinWidth/MinHeight and MaxWidth/MaxHeight bound the pixel dimensions. Zero on either
	// pair means the platform states no bound in that direction.
	MinWidth  int
	MinHeight int
	MaxWidth  int
	MaxHeight int
}

// SlotSpec is one place an ad platform will accept an image, and everything that decides
// whether a given upload may go there.
//
// The unit is the SLOT, not the platform, because a platform may take several shapes at
// once — a Performance Max asset group wants a landscape, a square, a portrait and two
// logos — and the operator's question is per slot: "what have I got that fits here, and
// what am I missing?"
type SlotSpec struct {
	// Platform is the campaign platform identifier the rest of the service uses
	// ("google-ads", "linkedin-ads", …), so a plan row joins to a campaign config without
	// a translation table.
	Platform string

	// Channel distinguishes slots within one platform whose rules differ — Google's
	// PERFORMANCE_MAX, DEMAND_GEN and DISPLAY all have a slot called "logo" and three
	// different sets of rules for it. Empty when the platform has a single ad shape.
	Channel string

	// Slot is the wire name: the AssetFieldType for Performance Max, the payload array key
	// for Demand Gen and Display, the config field for a single-image platform. It is what
	// a caller puts back into a campaign config, so it is taken verbatim from the channel's
	// own table rather than derived from the label.
	Slot string

	// Label is the operator-facing name, and it NAMES ITS CHANNEL for the reason the
	// googleads tables give: three channels carry a slot called "logo image" under
	// different rules, so a gap reading only "logo image" would not tell an operator which
	// creative they are missing.
	Label string

	// Shapes are the acceptable geometries. An empty list means the platform documents no
	// geometry for this slot — the slot still exists and still has a byte cap, and an
	// asset with known dimensions is eligible on shape alone.
	Shapes []ShapeRule

	// MaxBytes caps one image in this slot. Zero means only the service's own upload
	// ceiling applies.
	MaxBytes int64

	// MinCount is how many images the platform REQUIRES in this slot, and MaxCount how
	// many it will accept. MinCount 0 marks an optional slot; MaxCount 0 means the
	// platform states no per-slot ceiling (a combined Group ceiling may still apply).
	MinCount int
	MaxCount int

	// Group and GroupMax express a ceiling SHARED across several slots — Google caps the
	// three Performance Max marketing shapes at twenty images between them, not twenty
	// each. Slots in the same Group on the same (Platform, Channel) draw on one GroupMax.
	// Empty Group means the slot is bounded only by its own MaxCount.
	Group    string
	GroupMax int
}

// AnyOfRule is a requirement that spans SEVERAL slots: at least Min images between them,
// with no one slot required on its own.
//
// It exists because Google states one of Demand Gen's rules that way and no per-slot
// minimum can carry it. Each of the landscape and square marketing images is required
// "if the other is not present", so a per-slot MinCount of 1 on both would report a gap
// for a shape Google does not need, and a MinCount of 0 on both would report no gap at
// all for an ad Google will refuse. Both answers are wrong in a way an operator acts on.
//
// Performance Max is the contrast that proves the distinction is real rather than
// bookkeeping: it requires BOTH of the same two shapes outright, so it carries two
// per-slot minimums and no AnyOfRule.
type AnyOfRule struct {
	// Slots are the Key() values of the member slots. A key naming no slot in the
	// catalog is ignored, so a platform that publishes a channel's rules without that
	// channel's slots cannot manufacture a gap against nothing.
	Slots []string
	// Min is how many images are needed across those slots together.
	Min int
	// Label names the requirement for the operator ("Demand Gen landscape or square
	// marketing image").
	Label string
}

// Catalog is one platform's published rules: its slots, plus any requirement that spans
// more than one of them. Platforms publish a Catalog rather than a bare slot slice so a
// cross-slot rule travels with the slots it governs and cannot be dropped by a caller
// assembling a registry.
type Catalog struct {
	Slots []SlotSpec
	AnyOf []AnyOfRule
}

// Merge combines catalogs from several platforms into one, preserving order.
func Merge(in ...Catalog) Catalog {
	var out Catalog
	for _, c := range in {
		out.Slots = append(out.Slots, c.Slots...)
		out.AnyOf = append(out.AnyOf, c.AnyOf...)
	}
	return out
}

// Key identifies a slot within a plan: platform, channel and slot name. It is the join key
// between a plan row and an operator override, so it must survive a round trip through a
// client as a string.
func (s SlotSpec) Key() string {
	if s.Channel == "" {
		return s.Platform + "/" + s.Slot
	}
	return s.Platform + "/" + s.Channel + "/" + s.Slot
}

// Asset is the classifier's view of one stored upload: an identity and a shape. It is a
// local struct rather than domain/model.CreativeAsset so that this package stays free of
// the domain, and — the part that matters at runtime — so that classifying can never
// require the image BYTES. Every field here is already on the asset row.
//
// Width and Height are a PAIR and are either both set or both zero, matching the model
// they are mapped from. Zero means UNKNOWN (an asset stored before the service recorded
// dimensions), which is why Eligible refuses it outright rather than comparing it against
// a minimum: a 0x0 read as a real size fails every slot in the catalog, and the operator
// would be told their whole pre-existing library is the wrong shape.
type Asset struct {
	ID       string
	Width    int
	Height   int
	ByteSize int64
	// Ordinal breaks ranking ties in a STABLE way. The caller fills it from the list
	// order, which the repository returns oldest first, so an unchanged library produces
	// an unchanged plan across reloads. An index rather than a timestamp because two
	// assets can share a created_at and an index cannot.
	Ordinal int
}

// known reports whether the asset's dimensions were recorded.
func (a Asset) known() bool { return a.Width > 0 && a.Height > 0 }

// fits reports whether the asset satisfies this one shape rule.
func (r ShapeRule) fits(a Asset) bool {
	if r.MinWidth > 0 && a.Width < r.MinWidth {
		return false
	}
	if r.MinHeight > 0 && a.Height < r.MinHeight {
		return false
	}
	if r.MaxWidth > 0 && a.Width > r.MaxWidth {
		return false
	}
	if r.MaxHeight > 0 && a.Height > r.MaxHeight {
		return false
	}
	if r.RatioW <= 0 || r.RatioH <= 0 {
		return true
	}
	return r.ratioDrift(a) <= r.RatioTolerance
}

// ratioDrift is how far the asset's aspect ratio sits from this rule's target, as a
// fraction of the target. It is the eligibility test AND the ranking key, which is why it
// is one function: a ranking computed differently from the gate could prefer an asset the
// gate would have refused.
//
// Returns 0 for a rule that states no ratio — every eligible asset ranks equally on shape
// there, and the area tie-break below decides.
func (r ShapeRule) ratioDrift(a Asset) float64 {
	if r.RatioW <= 0 || r.RatioH <= 0 || a.Height <= 0 {
		return 0
	}
	want := float64(r.RatioW) / float64(r.RatioH)
	got := float64(a.Width) / float64(a.Height)
	drift := (got - want) / want
	if drift < 0 {
		return -drift
	}
	return drift
}

// Eligible reports whether the asset may be placed in this slot, and how well it fits.
//
// The second return is the DRIFT of the best-fitting shape rule — the ranking key — and is
// meaningful only when the first is true. A slot with no shape rules admits any asset whose
// dimensions are known, at drift 0.
func (s SlotSpec) Eligible(a Asset) (ok bool, drift float64) {
	if !a.known() {
		return false, 0
	}
	if s.MaxBytes > 0 && a.ByteSize > s.MaxBytes {
		return false, 0
	}
	if len(s.Shapes) == 0 {
		return true, 0
	}
	best := -1.0
	for _, r := range s.Shapes {
		if !r.fits(a) {
			continue
		}
		if d := r.ratioDrift(a); best < 0 || d < best {
			best = d
		}
	}
	if best < 0 {
		return false, 0
	}
	return true, best
}

// Describe renders the slot's requirement as the sentence an operator takes back to their
// designer. It is the whole value of a gap — "Reddit needs a 1:1 of at least 300x300" is
// actionable; "no eligible asset" is not — so it states every bound the slot actually
// carries and says nothing about the ones it does not.
func (s SlotSpec) Describe() string {
	if len(s.Shapes) == 0 {
		if s.MaxBytes > 0 {
			return fmt.Sprintf("%s: any image of at most %d bytes", s.Label, s.MaxBytes)
		}
		return fmt.Sprintf("%s: any image", s.Label)
	}
	out := s.Label + ": "
	for i, r := range s.Shapes {
		if i > 0 {
			out += ", or "
		}
		out += r.describe()
	}
	if s.MaxBytes > 0 {
		out += fmt.Sprintf(", at most %d bytes", s.MaxBytes)
	}
	return out
}

func (r ShapeRule) describe() string {
	out := ""
	if r.Label != "" {
		out += r.Label + " "
	}
	switch {
	case r.RatioW > 0 && r.RatioH > 0 && r.RatioTolerance > 0:
		out += fmt.Sprintf("%d:%d (within %.0f%%)", r.RatioW, r.RatioH, r.RatioTolerance*100)
	case r.RatioW > 0 && r.RatioH > 0:
		out += fmt.Sprintf("%d:%d", r.RatioW, r.RatioH)
	default:
		out += "any ratio"
	}
	if r.MinWidth > 0 || r.MinHeight > 0 {
		out += fmt.Sprintf(", at least %dx%d", r.MinWidth, r.MinHeight)
	}
	if r.MaxWidth > 0 || r.MaxHeight > 0 {
		out += fmt.Sprintf(", at most %dx%d", r.MaxWidth, r.MaxHeight)
	}
	return out
}
