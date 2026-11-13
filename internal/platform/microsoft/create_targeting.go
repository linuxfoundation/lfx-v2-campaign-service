// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
)

// ---------------------------------------------------------------------------
// Create-time excluded countries, radius targets and campaign languages (#563, gaps 5 and 6).
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com, read
// 2026-11-07):
//   - A NegativeCampaignCriterion with a LocationCriterion excludes a location; Microsoft applies
//     exclusions after inclusions. Location, negative location and radius criteria are added with
//     AddCampaignCriterions, CriterionType "Targets".
//   - RadiusCriterion: LatitudeDegrees -85..85, LongitudeDegrees -180..180, Radius a whole number
//     1-500 for Miles or 1-800 for Kilometers, RadiusUnit (DistanceUnit Miles | Kilometers), and an
//     optional Name of at most 50 characters. It goes in a BiddableCampaignCriterion.
//   - Campaign.Languages takes the Search ad languages (or the single value All). "If languages
//     are set at both the ad group and campaign level, the ad group level language will override
//     the campaign level language", and an ad group's Language is optional when its campaign has
//     languages, so an ad group under a campaign created with Languages is created without one.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_CREATE_TARGETING_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldExcludedGeoTargets, FieldProximityTargets and FieldLanguages are the microsoftConfig
	// keys, named in refusals.
	FieldExcludedGeoTargets = "excludedGeoTargets"
	FieldProximityTargets   = "proximityTargets"
	FieldLanguages          = "languages"

	// MaxProximityTargets caps the radius list; MaxLanguages the language list. Excluded
	// countries share geoTargets' cap (maxGeoTargets), so each criterion call stays within the
	// bounded id decode.
	MaxProximityTargets = 10
	MaxLanguages        = 10

	// Microsoft's RadiusCriterion bounds.
	maxRadiusLatitude    = 85.0
	maxRadiusLongitude   = 180.0
	maxRadiusMiles       = 500
	maxRadiusKilometers  = 800
	maxRadiusNameRunes   = 50
	radiusUnitMiles      = "Miles"
	radiusUnitKilometers = "Kilometers"
	criterionTypeRadius  = "RadiusCriterion"
	// languageAll is Campaign.Languages' "all languages" value, valid only alone.
	languageAll = "All"
)

// supportedCampaignLanguages is Microsoft's list of Search Campaign.Languages values, keyed by
// lower case so a caller's casing is accepted and Microsoft's spelling is sent.
var supportedCampaignLanguages = func() map[string]string {
	m := map[string]string{}
	for _, l := range []string{
		"Albanian", "Bulgarian", "Croatian", "Czech", "Danish", "Dutch", "English", "Estonian", "Finnish",
		"French", "German", "Greek", "Hungarian", "Italian", "Japanese", "Latvian", "Lithuanian", "Maltese",
		"Norwegian", "Polish", "Portuguese", "Romanian", "Slovak", "Slovenian", "Spanish", "Swedish",
		"TraditionalChinese", languageAll,
	} {
		m[strings.ToLower(l)] = l
	}
	return m
}()

// ProximityTarget is one requested radius target.
type ProximityTarget struct {
	Latitude  float64
	Longitude float64
	Radius    float64
	Unit      string
	Name      string
}

// CreateTargetingError is a refusal of one create-time targeting input; Field is the
// microsoftConfig key.
type CreateTargetingError struct {
	Field string
	Msg   string
}

func (e *CreateTargetingError) Error() string { return e.Field + ": " + e.Msg }

// CreateTargeting is the normalized excluded countries, radius targets and languages.
type CreateTargeting struct {
	ExcludedGeoTargets []string
	ProximityTargets   []ProximityTarget
	Languages          []string
}

// Empty reports whether nothing was requested.
func (t CreateTargeting) Empty() bool {
	return len(t.ExcludedGeoTargets) == 0 && len(t.ProximityTargets) == 0 && len(t.Languages) == 0
}

// ValidateCreateTargeting applies the create-time rules without contacting Microsoft, so the
// dispatcher's synchronous preflight and CreateCampaign agree. Excluded codes follow geoTargets'
// rules (trimmed, uppercased, Microsoft-supported ISO 3166-1 alpha-2, de-duplicated, at most 30)
// and may not repeat a geoTargets code; radius targets are range-checked and their unit and name
// normalized; languages are matched ignoring case, sent in Microsoft's spelling, de-duplicated,
// and All is accepted only alone. A refusal is a *CreateTargetingError naming the field and a
// 1-based position.
func ValidateCreateTargeting(geoTargets, excluded []string, proximity []ProximityTarget, languages []string) (CreateTargeting, error) {
	var out CreateTargeting
	ex, err := validateGeoTargets(excluded)
	if err != nil {
		return CreateTargeting{}, &CreateTargetingError{Field: FieldExcludedGeoTargets, Msg: strings.TrimPrefix(err.Error(), "microsoft-ads: ")}
	}
	if len(ex) > 0 {
		targeted := map[string]struct{}{}
		for _, g := range geoTargets {
			targeted[strings.ToUpper(strings.TrimSpace(g))] = struct{}{}
		}
		for i, code := range ex {
			if _, both := targeted[code]; both {
				return CreateTargeting{}, &CreateTargetingError{Field: FieldExcludedGeoTargets, Msg: fmt.Sprintf("entry %d (%s) is also in geoTargets; Microsoft applies exclusions after inclusions, so it would never serve", i+1, code)}
			}
		}
	}
	out.ExcludedGeoTargets = ex

	if len(proximity) > MaxProximityTargets {
		return CreateTargeting{}, &CreateTargetingError{Field: FieldProximityTargets, Msg: fmt.Sprintf("at most %d radius targets are accepted, got %d", MaxProximityTargets, len(proximity))}
	}
	for i, p := range proximity {
		np, msg := normalizeProximity(p)
		if msg != "" {
			return CreateTargeting{}, &CreateTargetingError{Field: FieldProximityTargets, Msg: fmt.Sprintf("entry %d %s", i+1, msg)}
		}
		out.ProximityTargets = append(out.ProximityTargets, np)
	}

	if len(languages) > MaxLanguages {
		return CreateTargeting{}, &CreateTargetingError{Field: FieldLanguages, Msg: fmt.Sprintf("at most %d languages are accepted, got %d", MaxLanguages, len(languages))}
	}
	seen := map[string]struct{}{}
	for i, raw := range languages {
		l, ok := supportedCampaignLanguages[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return CreateTargeting{}, &CreateTargetingError{Field: FieldLanguages, Msg: fmt.Sprintf("entry %d is not a Microsoft Search campaign language (for example English, Japanese, Spanish, or All)", i+1)}
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out.Languages = append(out.Languages, l)
	}
	if _, all := seen[languageAll]; all && len(out.Languages) > 1 {
		return CreateTargeting{}, &CreateTargetingError{Field: FieldLanguages, Msg: "All targets every language, so it must be the only entry"}
	}
	return out, nil
}

// normalizeProximity range-checks one radius target and returns it with its unit in Microsoft's
// spelling and its name trimmed, or a caller-safe reason it was refused.
func normalizeProximity(p ProximityTarget) (ProximityTarget, string) {
	switch {
	case math.IsNaN(p.Latitude) || p.Latitude < -maxRadiusLatitude || p.Latitude > maxRadiusLatitude:
		return p, fmt.Sprintf("latitude must be between -%g and %g", maxRadiusLatitude, maxRadiusLatitude)
	case math.IsNaN(p.Longitude) || p.Longitude < -maxRadiusLongitude || p.Longitude > maxRadiusLongitude:
		return p, fmt.Sprintf("longitude must be between -%g and %g", maxRadiusLongitude, maxRadiusLongitude)
	}
	maxRadius := 0
	switch strings.ToLower(strings.TrimSpace(p.Unit)) {
	case "miles":
		p.Unit, maxRadius = radiusUnitMiles, maxRadiusMiles
	case "kilometers":
		p.Unit, maxRadius = radiusUnitKilometers, maxRadiusKilometers
	default:
		return p, "unit must be Miles or Kilometers"
	}
	if math.IsNaN(p.Radius) || p.Radius != math.Trunc(p.Radius) || p.Radius < 1 || p.Radius > float64(maxRadius) {
		return p, fmt.Sprintf("radius must be a whole number from 1 to %d %s", maxRadius, strings.ToLower(p.Unit))
	}
	p.Name = strings.TrimSpace(p.Name)
	if utf8.RuneCountInString(p.Name) > maxRadiusNameRunes {
		return p, fmt.Sprintf("name must be at most %d characters", maxRadiusNameRunes)
	}
	for _, r := range p.Name {
		if unicode.IsControl(r) {
			return p, "name must not contain a control character"
		}
	}
	return p, ""
}

// msRadiusCriterion is a RadiusCriterion on the ADD path.
type msRadiusCriterion struct {
	Type             string  `json:"Type"`
	LatitudeDegrees  float64 `json:"LatitudeDegrees"`
	LongitudeDegrees float64 `json:"LongitudeDegrees"`
	Name             string  `json:"Name,omitempty"`
	Radius           int64   `json:"Radius"`
	RadiusUnit       string  `json:"RadiusUnit"`
}

// msTargetCampaignCriterion is a CampaignCriterion whose Criterion is any target type (a
// negative location or a radius); msCampaignCriterion stays the positive-location shape.
type msTargetCampaignCriterion struct {
	CampaignId json.Number `json:"CampaignId"`
	Criterion  any         `json:"Criterion"`
	Type       string      `json:"Type"`
}

type addTargetCriterionsRequest struct {
	CampaignCriterions []msTargetCampaignCriterion `json:"CampaignCriterions"`
	CriterionType      string                      `json:"CriterionType"`
}

// createExcludedLocationCriterions attaches each resolved LocationId as a
// NegativeCampaignCriterion, in ONE AddCampaignCriterions call with the outcome rules of
// createCampaignCriterions.
func (c *Client) createExcludedLocationCriterions(ctx context.Context, campaignID string, locationIDs []string) ([]string, error) {
	crits := make([]msTargetCampaignCriterion, 0, len(locationIDs))
	for _, id := range locationIDs {
		crits = append(crits, msTargetCampaignCriterion{
			CampaignId: json.Number(campaignID),
			Criterion:  msLocationCriterion{Type: criterionTypeLocation, LocationId: json.Number(id)},
			Type:       campaignCriterionTypeNegative,
		})
	}
	return c.postCampaignCriterions(ctx, campaignID, "geo exclusions", "excluded location criteria",
		addTargetCriterionsRequest{CampaignCriterions: crits, CriterionType: criterionTypeTargets}, len(crits))
}

// createRadiusCriterions attaches each radius target as a BiddableCampaignCriterion, in ONE
// AddCampaignCriterions call with the outcome rules of createCampaignCriterions.
func (c *Client) createRadiusCriterions(ctx context.Context, campaignID string, radii []ProximityTarget) ([]string, error) {
	crits := make([]msTargetCampaignCriterion, 0, len(radii))
	for _, p := range radii {
		crits = append(crits, msTargetCampaignCriterion{
			CampaignId: json.Number(campaignID),
			Criterion: msRadiusCriterion{
				Type: criterionTypeRadius, LatitudeDegrees: p.Latitude, LongitudeDegrees: p.Longitude,
				Name: p.Name, Radius: int64(p.Radius), RadiusUnit: p.Unit,
			},
			Type: campaignCriterionTypeBiddable,
		})
	}
	return c.postCampaignCriterions(ctx, campaignID, "radius targeting", "radius criteria",
		addTargetCriterionsRequest{CampaignCriterions: crits, CriterionType: criterionTypeTargets}, len(crits))
}

// proximityLabel renders the radius targets for a step: "25 Miles around LA Convention Center".
// The name is caller text bound for the persisted steps, so a link in it is reduced
// (redact.SnapshotText) as every other persisted caller text is; the criterion sent to Microsoft
// keeps the name as given.
func proximityLabel(radii []ProximityTarget) string {
	parts := make([]string, 0, len(radii))
	for _, p := range radii {
		where := redact.SnapshotText(p.Name)
		if where == "" {
			where = strconv.FormatFloat(p.Latitude, 'f', -1, 64) + "," + strconv.FormatFloat(p.Longitude, 'f', -1, 64)
		}
		parts = append(parts, fmt.Sprintf("%d %s around %s", int64(p.Radius), p.Unit, where))
	}
	return strings.Join(parts, "; ")
}

// criterionStepError classifies a failed exclusion or radius attach the way the positive geo
// attach is classified in CreateCampaign: ambiguous (UNCONFIRMED), a context abort, a partial
// attach naming how many landed, or a rejection. harm says what the campaign would do if enabled
// as it is. The campaign exists in every case, so the caller returns it as a partial.
func criterionStepError(campaignID, what, harm string, created, wanted int, err error) error {
	switch {
	case createOutcomeAmbiguous(err) || errors.Is(err, errNoID):
		return fmt.Errorf("microsoft-ads %s UNCONFIRMED (campaign %s exists; its criteria may or may not have been attached — verify its targeting in Microsoft Advertising before enabling or retrying): %w", what, campaignID, err)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("microsoft-ads %s aborted (campaign %s exists with incomplete targeting and %s; context done): %w", what, campaignID, harm, err)
	case errors.Is(err, errPartialFailure) && created > 0:
		return fmt.Errorf("microsoft-ads %s PARTIALLY attached (campaign %s carries %d of %d requested criteria and %s; reconcile the missing ones before enabling): %w", what, campaignID, created, wanted, harm, err)
	default:
		return fmt.Errorf("microsoft-ads %s rejected (campaign %s exists but %s; do not enable it until its targeting is fixed): %w", what, campaignID, harm, err)
	}
}
