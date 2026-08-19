// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Geo targeting (LFXV2-3283). Google Ads addresses locations by NUMERIC geo
// target constant, not by country code: a location criterion carries
// `geoTargetConstants/{id}`, where the id comes from Google's published
// geo-targets table. Callers here speak ISO 3166-1 alpha-2 (the vocabulary the
// meta and reddit clients already take), so this file owns the one mapping
// between the two.
//
// WHY A CURATED MAP RATHER THAN THE FULL TABLE: Google's geo-target table has
// ~100k rows spanning countries, regions, cities and postal codes, and it is a
// data file that changes. This map is the country subset the legacy Express
// implementation shipped (`lfx-self-serve` `campaign-proxy.service.ts`'s
// GEO_TARGET_MAP), ported verbatim so the two paths target the SAME places
// during the cutover. An unmapped code is REFUSED, not dropped — see
// validateGeoTargets.
// ---------------------------------------------------------------------------

// geoTargetConstants maps an ISO 3166-1 alpha-2 country code to its Google Ads
// geo target constant id.
//
// These ids are Google's, not ours, and they are NOT derived from the ISO code
// by any rule — 2840 is the United States and 2826 the United Kingdom, with no
// arithmetic relation to "US"/"GB". They are therefore transcribed, and a wrong
// transcription targets the WRONG COUNTRY while looking perfectly valid, which
// is exactly the failure this ticket exists to fix. Ported from the legacy
// GEO_TARGET_MAP, which is the implementation serving this channel today.
var geoTargetConstants = map[string]string{
	"US": "2840", // United States
	"CA": "2124", // Canada
	"GB": "2826", // United Kingdom
	"DE": "2276", // Germany
	"FR": "2250", // France
	"JP": "2392", // Japan
	"AU": "2036", // Australia
	"IN": "2356", // India
	"BR": "2076", // Brazil
	"CN": "2156", // China
	"KR": "2410", // South Korea
	"NL": "2528", // Netherlands
	"SE": "2752", // Sweden
	"CH": "2756", // Switzerland
	"IL": "2376", // Israel
	"SG": "2702", // Singapore
	"IE": "2372", // Ireland
	"ES": "2724", // Spain
	"IT": "2380", // Italy
	"AT": "2040", // Austria
	"FI": "2246", // Finland
	"NO": "2578", // Norway
	"DK": "2208", // Denmark
	"BE": "2056", // Belgium
	"PL": "2616", // Poland
	"CZ": "2203", // Czechia
	"NZ": "2554", // New Zealand
	"TW": "2158", // Taiwan
	"HK": "2344", // Hong Kong
	"MX": "2484", // Mexico
}

// maxGeoTargets bounds caller input so the location criteria stay within one
// mutate call. Not a Google Ads platform limit — a sanity cap on this broker's
// input, mirroring maxKeywords/maxAudienceSegments. The map above has 30
// entries, so this cannot be reached without duplicates.
const maxGeoTargets = 30

// validateGeoTargets upper-cases, trims and de-duplicates caller-supplied
// country codes, resolving each to its Google geo target constant id. It
// returns the resolved ids in caller order.
//
// An empty input returns (nil, nil): geo targeting is OPTIONAL at this layer,
// and a campaign created with none behaves exactly as it did before this
// ticket. The decision about whether an untargeted campaign is acceptable
// belongs to the caller that knows the campaign's purpose, not to this
// validator — see CampaignInput.GeoTargets.
//
// An UNMAPPED or malformed code is a HARD ERROR rather than a silent drop, and
// that asymmetry is deliberate. Dropping "USA" (a plausible typo for "US")
// would create a campaign that spends worldwide while reporting success —
// which is the exact defect LFXV2-3283 fixes. Refusing it fails the create
// BEFORE the first mutate, so nothing paid exists. This is the same choice
// validateKeywords makes and the opposite of meta's default-to-US, which is
// safe there only because Meta's criteria attach during creation.
func validateGeoTargets(geoTargets []string) ([]string, error) {
	if len(geoTargets) == 0 {
		return nil, nil
	}
	if len(geoTargets) > maxGeoTargets {
		return nil, fmt.Errorf("google-ads: at most %d geo targets are supported, got %d", maxGeoTargets, len(geoTargets))
	}
	seen := make(map[string]struct{}, len(geoTargets))
	out := make([]string, 0, len(geoTargets))
	for _, g := range geoTargets {
		code := strings.ToUpper(strings.TrimSpace(g))
		if code == "" {
			return nil, fmt.Errorf("google-ads: geo target must not be empty")
		}
		id, ok := geoTargetConstants[code]
		if !ok {
			return nil, fmt.Errorf("google-ads: geo target %q is not a supported country code (want an ISO 3166-1 alpha-2 code present in the geo target map, e.g. US, GB, DE)", code)
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// geoTargetResource renders a geo target constant id as the resource name a
// location criterion references.
func geoTargetResource(id string) string {
	return "geoTargetConstants/" + id
}

// ---------------------------------------------------------------------------
// Location criteria. The LEVEL differs per channel and that is the whole trap
// this ticket warns about: Search takes campaign-level location criteria, and
// Demand Gen REJECTS them — it takes the same criterion on the AD GROUP. A
// single implementation attaching at the campaign level works on Search and is
// refused on Demand Gen, after the budget and campaign have already been
// created and spend real money. Hence two payload types and two functions,
// named for their level, rather than one with a level parameter.
// ---------------------------------------------------------------------------

// locationInfo is the "location" criterion payload, shared by both levels.
type locationInfo struct {
	GeoTargetConstant string `json:"geoTargetConstant"`
}

// campaignCriterionCreate is the create payload for campaignCriteria:mutate
// (the SEARCH path's level).
type campaignCriterionCreate struct {
	Campaign string        `json:"campaign"`
	Location *locationInfo `json:"location,omitempty"`
}

// adGroupCriterionLocationCreate is the create payload for
// adGroupCriteria:mutate carrying a LOCATION (the DEMAND GEN path's level).
//
// It is separate from adGroupCriterionCreate (targeting.go) rather than a
// widened version of it: that type sets `status` on every operation, which the
// keyword/audience criteria want, and it carries the keyword/userList oneof
// arms a location criterion must never emit alongside a location.
type adGroupCriterionLocationCreate struct {
	AdGroup  string        `json:"adGroup"`
	Location *locationInfo `json:"location,omitempty"`
}

// createCampaignGeoTargeting attaches location criteria to a just-created
// SEARCH campaign as a single campaignCriteria:mutate call, one operation per
// location. Batched into one call so the whole set shares one atomic outcome
// (partialFailure stays false, as everywhere else in this client): either all
// the requested locations are targeted or none are, never a half-targeted
// campaign that quietly spends outside its region.
//
// Called AFTER the campaign create and reported as a partial-result failure by
// the caller: the campaign exists and is PAUSED regardless, so a failure here
// must never be a (nil, err) that discards the claim.
func (c *Client) createCampaignGeoTargeting(ctx context.Context, campaignResource, campaignID string, geoConstantIDs []string) ([]string, error) {
	if len(geoConstantIDs) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads geo targeting aborted before any request (context already done; campaign %s has no location criteria yet): %w", campaignID, ctxErr)
	}

	ops := make([]mutateOperation, 0, len(geoConstantIDs))
	for _, id := range geoConstantIDs {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Location: &locationInfo{GeoTargetConstant: geoTargetResource(id)},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (campaign %s; location criteria may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads geo targeting failed (campaign %s created; it has NO location criteria and would serve worldwide if enabled): %w", campaignID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — location criteria may exist — verify in Google Ads before retrying)", campaignID)
	}

	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}

// createAdGroupGeoTargeting attaches location criteria to a just-created
// DEMAND GEN ad group as a single adGroupCriteria:mutate call.
//
// Demand Gen rejects campaign-level location criteria, so the criterion goes
// on the ad group — the level the legacy Express implementation uses for this
// channel, and the reason this is not the same function as the Search path's.
func (c *Client) createAdGroupGeoTargeting(ctx context.Context, adGroupResource, adGroupID string, geoConstantIDs []string) ([]string, error) {
	if len(geoConstantIDs) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads geo targeting aborted before any request (context already done; ad group %s has no location criteria yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, len(geoConstantIDs))
	for _, id := range geoConstantIDs {
		ops = append(ops, mutateOperation{Create: adGroupCriterionLocationCreate{
			AdGroup:  adGroupResource,
			Location: &locationInfo{GeoTargetConstant: geoTargetResource(id)},
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("adGroupCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (ad group %s; location criteria may exist — verify in Google Ads before retrying): %w", adGroupID, err)
		}
		return nil, fmt.Errorf("google-ads geo targeting failed (ad group %s created; it has NO location criteria and would serve worldwide if enabled): %w", adGroupID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (ad group %s; 2xx with a malformed/short mutate response — location criteria may exist — verify in Google Ads before retrying)", adGroupID)
	}

	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedAdGroupID, critID := c.adGroupCriterionID(r.ResourceName)
		if critID == "" || returnedAdGroupID == "" {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (ad group %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", adGroupID, r.ResourceName, i)
		}
		if returnedAdGroupID != adGroupID {
			return nil, fmt.Errorf("google-ads geo targeting UNCONFIRMED (ad group %s; adGroupCriterion resource name %q reports a different ad group id %q — verify in Google Ads before retrying)", adGroupID, r.ResourceName, returnedAdGroupID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
