// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"fmt"
	"slices"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Placement, platform and expansion choices at create time (#549 gap 7): the ad group's
// targeting `locations`, `platforms` and `expand_targeting`. Until now the create fixed them at
// FEED + COMMENTS_PAGE, ALL and true. The values come from the gap review's reading of Reddit's
// public v3 schema; no live create has sent anything but the fixed defaults, which is why the
// dispatcher keeps the choices behind REDDIT_PLACEMENT_TARGETING_ENABLED.

const (
	// FieldPlacements, FieldPlatforms and FieldExpandTargeting are the redditConfig keys a
	// PlacementError names.
	FieldPlacements      = "placements"
	FieldPlatforms       = "platforms"
	FieldExpandTargeting = "expandTargeting"

	// platformAll is the platforms value sent when the caller names none.
	platformAll = "ALL"
)

var (
	// placementValues are the accepted CampaignInput.Placements, sent as targeting.locations. They
	// are exactly the two the create has always sent, so the default is every placement.
	placementValues = []string{"FEED", "COMMENTS_PAGE"}
	// platformValues are the accepted CampaignInput.Platforms. Omitted, the create sends ALL.
	platformValues = []string{"DESKTOP", "MOBILE_NATIVE", "MOBILE_WEB"}
)

// Placements returns the accepted CampaignInput.Placements values, in a fixed order.
func Placements() []string { return slices.Clone(placementValues) }

// Platforms returns the accepted CampaignInput.Platforms values, in a fixed order.
func Platforms() []string { return slices.Clone(platformValues) }

// PlacementError is a refused placement or platform input. Field is the redditConfig key it
// belongs to and Msg the service-authored reason; any caller value in Msg went through
// domain.CallerValue.
type PlacementError struct {
	Field string
	Msg   string
}

func (e *PlacementError) Error() string { return e.Field + ": " + e.Msg }

// PlacementTargeting is the ad group's placement, platform and expansion targeting, ready to send.
type PlacementTargeting struct {
	Locations       []string
	Platforms       []string
	ExpandTargeting bool
}

// IsDefault reports whether t is exactly what the create sent before #549 gap 7.
func (t PlacementTargeting) IsDefault() bool {
	return t.ExpandTargeting && slices.Equal(t.Locations, placementValues) && slices.Equal(t.Platforms, []string{platformAll})
}

// NormalizePlacementTargeting validates the three choices and returns what to send. It is the ONE
// rule set: CreateCampaign applies it before any upstream call, and the dispatcher applies it in
// PreflightCreate so a bad value is a synchronous 400.
//
//   - placements: trimmed and uppercased; each must be FEED or COMMENTS_PAGE; a blank entry is
//     refused; duplicates are dropped. Empty means both, the create's long-standing default.
//   - platforms: trimmed and uppercased; each must be DESKTOP, MOBILE_NATIVE or MOBILE_WEB; a
//     blank entry is refused; duplicates are dropped. Empty means ALL. ALL itself is not an
//     accepted entry: omit the list for it, so one request cannot spell the default two ways.
//   - expandTargeting: nil means true, the create's long-standing default.
//
// Accepted values are returned in the fixed order of Placements() and Platforms(), so the same
// choice always builds the same request whatever order the caller listed it in.
func NormalizePlacementTargeting(placements, platforms []string, expandTargeting *bool) (PlacementTargeting, error) {
	out := PlacementTargeting{ExpandTargeting: true}
	if expandTargeting != nil {
		out.ExpandTargeting = *expandTargeting
	}
	locs, err := normalizeEnumList(FieldPlacements, placements, placementValues)
	if err != nil {
		return PlacementTargeting{}, err
	}
	if len(locs) == 0 {
		locs = slices.Clone(placementValues)
	}
	out.Locations = locs
	plats, err := normalizeEnumList(FieldPlatforms, platforms, platformValues)
	if err != nil {
		return PlacementTargeting{}, err
	}
	if len(plats) == 0 {
		plats = []string{platformAll}
	}
	out.Platforms = plats
	return out, nil
}

// normalizeEnumList uppercases and de-duplicates values against allowed, returning them in
// allowed's order. A blank or unknown entry is refused, naming its 1-based position.
func normalizeEnumList(field string, values, allowed []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(values))
	for i, v := range values {
		canonical := strings.ToUpper(strings.TrimSpace(v))
		if canonical == "" {
			return nil, &PlacementError{Field: field, Msg: fmt.Sprintf("entry %d is blank", i+1)}
		}
		if !slices.Contains(allowed, canonical) {
			return nil, &PlacementError{Field: field, Msg: fmt.Sprintf("entry %d, %s, is not one of %s", i+1, domain.CallerValue(v), strings.Join(allowed, ", "))}
		}
		seen[canonical] = true
	}
	out := make([]string, 0, len(seen))
	for _, a := range allowed {
		if seen[a] {
			out = append(out, a)
		}
	}
	return out, nil
}
