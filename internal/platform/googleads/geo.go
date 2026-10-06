// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// maxGeoTargets bounds EACH caller-supplied location list (included, excluded)
// so the criteria stay within one mutate call. Not a Google Ads platform limit —
// a sanity cap on this broker's input, mirroring maxKeywords/maxAudienceSegments.
//
// It used to be 30 with the note "the map above has 30 entries, so this cannot be
// reached without duplicates". That stopped being true when resolveGeoList began
// accepting raw geo target constant ids as well as country codes: a city-level
// campaign targets the metro areas an event draws from, and there are far more
// than 30 of those. The cap is now sized like maxKeywords, for the same reason
// the 2026-08-13 keyword incident gives — a cap the product's own callers exceed
// by default refuses creates Google would have accepted.
const maxGeoTargets = 60

// maxProximityTargets bounds the radius-targeting list. Lower than maxGeoTargets
// on purpose: a proximity criterion carries a point and a radius rather than one
// id, so the same number of them is a much larger payload, and a campaign needing
// dozens of overlapping radii wants a location list instead.
const maxProximityTargets = 20

// maxRadiusMiles / maxRadiusKilometers are Google Ads' documented ceilings for a
// proximity radius. They are PLATFORM limits, not broker sanity caps, so the
// messages say so — a caller over them is asking for something Google refuses.
const (
	maxRadiusMiles      = 500.0
	maxRadiusKilometers = 800.0
)

// radiusUnitMiles / radiusUnitKilometers are the only ProximityInfo.radiusUnits
// values Google accepts.
const (
	radiusUnitMiles      = "MILES"
	radiusUnitKilometers = "KILOMETERS"
)

// microDegreesPerDegree converts a decimal degree to the microdegrees a GeoPoint
// carries, the same way microsPerUnit converts currency to micros.
const microDegreesPerDegree = 1_000_000

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
	return resolveGeoList("geo target", geoTargets)
}

// validateExcludedGeoTargets is the EXCLUSION analogue of validateGeoTargets: the
// same vocabulary and the same rules, with "excluded geo target" in every message
// so an operator reading a rejection knows which of the two lists was refused.
// Separate entry point for the reason validateNegativeKeywords is separate from
// validateKeywords — reusing the positive function would report a bad exclusion as
// a bad inclusion and send whoever fixes it to the wrong half of the config.
func validateExcludedGeoTargets(geoTargets []string) ([]string, error) {
	return resolveGeoList("excluded geo target", geoTargets)
}

// resolveGeoList is the shared implementation behind both. `noun` names the list
// in every error message.
//
// Each entry is EITHER an ISO 3166-1 alpha-2 country code, resolved through
// geoTargetConstants above, OR a raw numeric geo target constant id taken straight
// from Google's published geo-targets table — which is how a caller addresses a
// CITY, region, metro or postal code, none of which the curated country map can
// express and none of which could be curated here without shipping ~100k rows that
// Google revises.
//
// The two are told apart by SHAPE, not by a flag, and the shapes cannot collide: an
// ISO alpha-2 code is two letters and a constant id is all digits, so no input is
// ambiguous and no caller has to say which kind it meant. Country codes keep
// working exactly as before this existed.
//
// A numeric id is checked only for SHAPE — this client cannot know whether 1014044
// is a real location without asking Google, and a lookup here would make
// ValidateCampaignInput send a request, which its contract forbids. A well-formed
// id naming nothing is refused by Google at the criteria mutate, AFTER the campaign
// exists; that is the honest cost of letting callers reach past the curated map,
// and it is why the country path (which this client CAN verify locally) still fails
// before any mutate.
//
// Dedupe is per list and by RESOLVED ID, not by the caller's spelling: "US" and
// "2840" are the same criterion, and Google rejects an exact duplicate.
func resolveGeoList(noun string, geoTargets []string) ([]string, error) {
	if len(geoTargets) == 0 {
		return nil, nil
	}
	if len(geoTargets) > maxGeoTargets {
		return nil, fmt.Errorf("google-ads: at most %d %ss are supported, got %d", maxGeoTargets, noun, len(geoTargets))
	}
	seen := make(map[string]struct{}, len(geoTargets))
	out := make([]string, 0, len(geoTargets))
	for _, g := range geoTargets {
		entry := strings.ToUpper(strings.TrimSpace(g))
		if entry == "" {
			return nil, fmt.Errorf("google-ads: %s must not be empty", noun)
		}
		id, err := resolveGeoEntry(noun, entry)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// resolveGeoEntry maps one already-trimmed, already-upper-cased entry to a geo
// target constant id.
func resolveGeoEntry(noun, entry string) (string, error) {
	if numericID(entry) {
		// Digits-only is not enough. "0" names nothing, "02840" is a non-canonical
		// spelling of 2840, and a 21-digit run overflows the int64 Google exposes these
		// ids as — all three are shaped like an id, all three are PERMANENT local input
		// faults, and all three would be refused only at the campaignCriteria:mutate,
		// which runs AFTER the budget and campaign are committed. Catching them here is
		// what keeps a typo from stranding a paid campaign, which is the whole point of
		// the preflight.
		//
		// canonicalCampaignID is reused rather than reimplemented — it is the package's
		// answer for exactly this class of value (see its use on ad group and criterion
		// ids in ValidateKeywordActions) and collapses every spelling to one. This is
		// NOT the lookup the comment above declines to do: whether 1014044 names a real
		// place still needs Google, and a well-formed id naming nothing is still refused
		// upstream. Only the locally-decidable faults move earlier.
		if canonicalCampaignID(entry) == "" {
			return "", fmt.Errorf("google-ads: %s %q is not the canonical base-10 spelling of a positive geo target constant id", noun, entry)
		}
		return entry, nil
	}
	if id, ok := geoTargetConstants[entry]; ok {
		return id, nil
	}
	return "", fmt.Errorf("google-ads: %s %q is neither a supported country code (an ISO 3166-1 alpha-2 code present in the geo target map, e.g. US, GB, DE) nor a numeric geo target constant id from Google's geo-targets table (e.g. 1014044 for a city)", noun, entry)
}

// ProximityTarget is radius targeting around a point: "everyone within N miles of
// the venue". It is the one location shape that is not a place in Google's table,
// which is why it is a struct rather than another entry in a geo list.
type ProximityTarget struct {
	// Latitude/Longitude are decimal degrees (e.g. 37.7749, -122.4194), the form
	// every mapping tool emits. They are converted to the microdegrees GeoPoint
	// actually carries — see validateProximityTargets.
	Latitude  float64
	Longitude float64
	// Radius is the distance in RadiusUnit. Must be > 0 and within Google's
	// documented ceiling for that unit.
	Radius float64
	// RadiusUnit is "MILES" or "KILOMETERS". Required — there is deliberately no
	// default. A radius of 50 means two very different campaigns depending on the
	// unit, and guessing one would silently buy ~2.5x the intended area (or 0.4x);
	// refusing costs the caller one explicit word, before anything paid exists.
	RadiusUnit string
}

// geoPoint is the ProximityInfo.geoPoint payload. Google carries the point in
// MICRODEGREES as integers, not decimal degrees as floats.
type geoPoint struct {
	LatitudeInMicroDegrees  int64 `json:"latitudeInMicroDegrees"`
	LongitudeInMicroDegrees int64 `json:"longitudeInMicroDegrees"`
}

// proximityInfo is the "proximity" criterion payload, valid at both the campaign
// and ad group level.
type proximityInfo struct {
	GeoPoint    geoPoint `json:"geoPoint"`
	Radius      float64  `json:"radius"`
	RadiusUnits string   `json:"radiusUnits"`
}

// validateProximityTargets validates the caller's radius targets and renders each
// into the wire payload.
//
// NaN/Inf are rejected FIRST, before any range comparison, for the reason
// validateCPCBid gives: every ordered comparison against NaN is false, so a NaN
// latitude would pass both bounds and then round to a garbage int64 that places the
// campaign somewhere real.
//
// The degrees→microdegrees conversion uses math.Round rather than truncation, the
// same rule the currency conversions follow: truncating 37.7749 degrees would shift
// the point, and at these scales a consistent rounding error is a consistent
// displacement of every radius.
//
// An EXACT repeat is COLLAPSED, the rule resolveGeoList already follows for the two
// id lists that share this criterion mutate. Google refuses a duplicate proximity
// criterion, and it refuses it only AFTER the campaign exists — so a locally
// decidable input error would otherwise cost a real paid campaign, which is the whole
// reason this validator runs before the budget mutate.
//
// Collapsing is the right answer here rather than the refusal that adSchedules and
// DeviceBidModifiers give a repeat, and the difference is in the payload, not in the
// policy: those carry a bid modifier, so two entries naming the same slot can
// DISAGREE and one of the two values would be the one silently dropped. A proximity
// target carries no such field — two entries that render to the same wire tuple say
// exactly the same thing, so there is nothing to lose by keeping one.
//
// The key is the RENDERED tuple, not the caller's spelling, for the reason
// resolveGeoList dedupes by resolved id: 37.7749 and 37.77490 are the same
// microdegrees, and "MILES", " miles " and "Miles" are the same unit — those are
// different spellings of one criterion, and Google sees only the rendered form. A
// mile radius and its kilometre equivalent are deliberately NOT collapsed: that is a
// conversion, not a spelling, and whether Google treats the two as one criterion is
// not locally decidable — the same line the ad-schedule overlap check draws.
//
// proximityInfo is comparable (two int64, a float64 and a string), so it is the map
// key directly. A NaN key, which would never equal itself, cannot arise: NaN is
// refused earlier in this same loop iteration, before anything is rendered.
//
// The cap is checked against the SUBMITTED count, before any collapsing, which is
// where resolveGeoList checks its own. It is a limit on the request a caller may
// send, not on the criteria that survive: a caller handing over a list longer than
// this has lost track of what it is asking for, and silently shrinking it into the
// cap would hide that rather than report it.
func validateProximityTargets(targets []ProximityTarget) ([]proximityInfo, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	if len(targets) > maxProximityTargets {
		return nil, fmt.Errorf("google-ads: at most %d proximity targets are supported, got %d", maxProximityTargets, len(targets))
	}
	seen := make(map[proximityInfo]struct{}, len(targets))
	out := make([]proximityInfo, 0, len(targets))
	for i, t := range targets {
		if math.IsNaN(t.Latitude) || math.IsInf(t.Latitude, 0) || math.IsNaN(t.Longitude) || math.IsInf(t.Longitude, 0) {
			return nil, fmt.Errorf("google-ads: proximity target %d must have a finite latitude and longitude, got %v/%v", i, t.Latitude, t.Longitude)
		}
		if t.Latitude < -90 || t.Latitude > 90 {
			return nil, fmt.Errorf("google-ads: proximity target %d latitude %v is outside -90..90", i, t.Latitude)
		}
		if t.Longitude < -180 || t.Longitude > 180 {
			return nil, fmt.Errorf("google-ads: proximity target %d longitude %v is outside -180..180", i, t.Longitude)
		}
		if math.IsNaN(t.Radius) || math.IsInf(t.Radius, 0) || t.Radius <= 0 {
			return nil, fmt.Errorf("google-ads: proximity target %d radius must be a finite number greater than 0, got %v", i, t.Radius)
		}
		unit := strings.ToUpper(strings.TrimSpace(t.RadiusUnit))
		var maxRadius float64
		switch unit {
		case radiusUnitMiles:
			maxRadius = maxRadiusMiles
		case radiusUnitKilometers:
			maxRadius = maxRadiusKilometers
		default:
			return nil, fmt.Errorf("google-ads: proximity target %d radius unit must be %s or %s, got %q", i, radiusUnitMiles, radiusUnitKilometers, t.RadiusUnit)
		}
		if t.Radius > maxRadius {
			return nil, fmt.Errorf("google-ads: proximity target %d radius %v exceeds Google's maximum of %.0f %s", i, t.Radius, maxRadius, unit)
		}
		info := proximityInfo{
			GeoPoint: geoPoint{
				LatitudeInMicroDegrees:  int64(math.Round(t.Latitude * microDegreesPerDegree)),
				LongitudeInMicroDegrees: int64(math.Round(t.Longitude * microDegreesPerDegree)),
			},
			Radius:      t.Radius,
			RadiusUnits: unit,
		}
		if _, dup := seen[info]; dup {
			continue
		}
		seen[info] = struct{}{}
		out = append(out, info)
	}
	return out, nil
}

// geoPlan is the whole resolved location intent for one campaign: the places it
// should serve in, the places it must not, and the radii around points.
//
// It is one value rather than three parameters because all three become operations
// in the SAME campaignCriteria:mutate, and that is load-bearing — see
// createCampaignGeoTargeting for why the exclusions in particular must share the
// inclusions' atomic outcome.
type geoPlan struct {
	included  []string
	excluded  []string
	proximity []proximityInfo
}

// empty reports whether the plan would produce no criteria at all, in which case
// the caller sends no mutate.
func (p geoPlan) empty() bool {
	return len(p.included) == 0 && len(p.excluded) == 0 && len(p.proximity) == 0
}

// count is how many operations the plan becomes.
func (p geoPlan) count() int {
	return len(p.included) + len(p.excluded) + len(p.proximity)
}

// validateGeoPlan resolves every location input on a CampaignInput for the given
// campaign kind.
//
// PROXIMITY IS REFUSED ON DEMAND GEN, before any mutate. Demand Gen takes its
// location criteria on the AD GROUP (that is this file's central warning), and
// whether an ad group criterion accepts a proximity on that channel is not
// something this client has verified against a real account. The two honest
// options were to send it and find out after a paid campaign exists, or to refuse
// it locally where refusing is free; silently DROPPING it was not an option, since
// a campaign that serves nationwide when the caller asked for a 25-mile radius is
// the same defect LFXV2-3283 fixed for countries. Lift this the moment someone
// confirms the behaviour live.
func validateGeoPlan(kind string, in CampaignInput) (geoPlan, error) {
	included, err := validateGeoTargets(in.GeoTargets)
	if err != nil {
		return geoPlan{}, err
	}
	excluded, err := validateExcludedGeoTargets(in.ExcludedGeoTargets)
	if err != nil {
		return geoPlan{}, err
	}
	// A location that is both targeted and excluded is a contradiction Google
	// resolves by letting the exclusion win, which means the campaign silently does
	// not serve where the caller plainly asked it to. Refuse it here, where the
	// caller still has the chance to mean something.
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, id := range excluded {
		excludedSet[id] = struct{}{}
	}
	for _, id := range included {
		if _, clash := excludedSet[id]; clash {
			return geoPlan{}, fmt.Errorf("google-ads: geo target constant %s is both targeted and excluded — the exclusion would win and the campaign would not serve there at all", id)
		}
	}
	// Still named on Demand Gen alone, and still deliberately: Search, Performance Max
	// and Video all attach location criteria at the CAMPAIGN level, where proximity is
	// the ordinary radius target this client already sends, so a fence spelled
	// `!= campaignKindSearch` would refuse two channels that take it.
	if len(in.ProximityTargets) > 0 && kind == campaignKindDemandGen {
		return geoPlan{}, fmt.Errorf("google-ads: proximity targeting is not supported on %s (Demand Gen attaches location criteria at the ad group level, where this client has not verified proximity); use country or city geo targets instead", kind)
	}
	proximity, err := validateProximityTargets(in.ProximityTargets)
	if err != nil {
		return geoPlan{}, err
	}
	return geoPlan{included: included, excluded: excluded, proximity: proximity}, nil
}

// geoStep renders the location intent for the campaign-created step log.
//
// It reports the CALLER'S spellings, not the resolved constant ids: the step log is
// what an operator reconciles a campaign against the brief with, and "US, GB" is
// the half of that comparison a human can check. Each part appears only when it was
// asked for, so a campaign with no exclusions does not carry a confusing
// "excluding: none" the reader has to parse past.
func geoStep(in CampaignInput, plan geoPlan) string {
	parts := make([]string, 0, 3)
	if len(plan.included) > 0 {
		parts = append(parts, "targeting "+strings.Join(in.GeoTargets, ", "))
	}
	if len(plan.excluded) > 0 {
		parts = append(parts, "excluding "+strings.Join(in.ExcludedGeoTargets, ", "))
	}
	if n := len(plan.proximity); n > 0 {
		parts = append(parts, fmt.Sprintf("%d radius target(s)", n))
	}
	return strings.Join(parts, "; ")
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
//
// Negative carries `omitempty`, and here that is CORRECT rather than the trap it
// would be on campaignNegativeKeywordCreate (targeting.go). That type exists only
// to create exclusions, so an omitted `negative` would turn every operation into a
// positive keyword buying the traffic the caller excluded — which is why it has no
// omitempty. This type carries BOTH polarities, and for it "absent" is exactly
// Google's default of a positive criterion, so omitting the field on a false is the
// accurate encoding, not a dropped one.
//
// It models the whole CampaignCriterion resource, not just a location: the arms
// past Proximity belong to campaign_criteria.go and each is a separate member of
// Google's criterion oneof, so exactly one is ever set per operation.
type campaignCriterionCreate struct {
	Campaign  string         `json:"campaign"`
	Negative  bool           `json:"negative,omitempty"`
	Location  *locationInfo  `json:"location,omitempty"`
	Proximity *proximityInfo `json:"proximity,omitempty"`
	// BidModifier is a POINTER so the -100% opt-out survives serialisation: a plain
	// float64 with omitempty would drop an explicit 0, turning "do not serve on
	// tablets" into "bid on tablets normally" — the exact inverse of the request.
	// nil means the criterion carries no bid adjustment at all.
	BidModifier *float64        `json:"bidModifier,omitempty"`
	Language    *languageInfo   `json:"language,omitempty"`
	AdSchedule  *adScheduleInfo `json:"adSchedule,omitempty"`
	Device      *deviceInfo     `json:"device,omitempty"`
	AgeRange    *ageRangeInfo   `json:"ageRange,omitempty"`
	Gender      *genderInfo     `json:"gender,omitempty"`
}

// adGroupCriterionLocationCreate is the create payload for
// adGroupCriteria:mutate carrying a LOCATION (the DEMAND GEN path's level).
//
// It is separate from adGroupCriterionCreate (targeting.go) rather than a
// widened version of it: that type sets `status` on every operation, which the
// keyword/audience criteria want, and it carries the keyword/userList oneof
// arms a location criterion must never emit alongside a location.
//
// Negative carries omitempty for the same reason campaignCriterionCreate's does.
// There is no Proximity arm: validateGeoPlan refuses proximity on the Demand Gen
// path before any mutate, so a field here would advertise a shape this type can
// never be asked to emit.
type adGroupCriterionLocationCreate struct {
	AdGroup  string        `json:"adGroup"`
	Negative bool          `json:"negative,omitempty"`
	Location *locationInfo `json:"location,omitempty"`
}

// createCampaignGeoTargeting attaches location criteria to a just-created
// SEARCH campaign as a single campaignCriteria:mutate call, one operation per
// location. Batched into one call so the whole set shares one atomic outcome
// (partialFailure stays false, as everywhere else in this client): either all
// the requested locations are targeted or none are, never a half-targeted
// campaign that quietly spends outside its region.
//
// THE EXCLUSIONS SHARE THAT BATCH, and must. Splitting them into a second mutate
// would create the one ordering where a failure is worse than doing nothing: the
// inclusions commit, the exclusions do not, and the campaign serves across exactly
// the places the caller named as the ones to stay out of. Proximity rides along for
// the same reason — a radius without its exclusions is not the campaign that was
// asked for.
//
// Called AFTER the campaign create and reported as a partial-result failure by
// the caller: the campaign exists and is PAUSED regardless, so a failure here
// must never be a (nil, err) that discards the claim.
func (c *Client) createCampaignGeoTargeting(ctx context.Context, campaignResource, campaignID string, plan geoPlan) ([]string, error) {
	if plan.empty() {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads geo targeting aborted before any request (context already done; campaign %s has no location criteria yet): %w", campaignID, ctxErr)
	}

	ops := make([]mutateOperation, 0, plan.count())
	for _, id := range plan.included {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Location: &locationInfo{GeoTargetConstant: geoTargetResource(id)},
		}})
	}
	for _, id := range plan.excluded {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Negative: true,
			Location: &locationInfo{GeoTargetConstant: geoTargetResource(id)},
		}})
	}
	for i := range plan.proximity {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign:  campaignResource,
			Proximity: &plan.proximity[i],
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
//
// Inclusions and exclusions share one mutate here too, for the reason the Search
// path documents. Proximity never reaches this function: validateGeoPlan refuses it
// on this channel before anything paid exists.
func (c *Client) createAdGroupGeoTargeting(ctx context.Context, adGroupResource, adGroupID string, plan geoPlan) ([]string, error) {
	if plan.empty() {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads geo targeting aborted before any request (context already done; ad group %s has no location criteria yet): %w", adGroupID, ctxErr)
	}

	ops := make([]mutateOperation, 0, plan.count())
	for _, id := range plan.included {
		ops = append(ops, mutateOperation{Create: adGroupCriterionLocationCreate{
			AdGroup:  adGroupResource,
			Location: &locationInfo{GeoTargetConstant: geoTargetResource(id)},
		}})
	}
	for _, id := range plan.excluded {
		ops = append(ops, mutateOperation{Create: adGroupCriterionLocationCreate{
			AdGroup:  adGroupResource,
			Negative: true,
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
