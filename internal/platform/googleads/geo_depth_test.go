// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// resolveGeoList / resolveGeoEntry — the two-shape vocabulary
// ---------------------------------------------------------------------------

// A numeric geo target constant id is the ONLY way to reach a city, region, metro or
// postal code: the curated map holds countries. It must pass through byte-identical —
// a client that "normalised" it (padded, re-based, looked it up in the country map and
// failed) would make city targeting impossible while every country test still passed.
func TestResolveGeoList_AcceptsNumericConstantIDsVerbatim(t *testing.T) {
	got, err := validateGeoTargets([]string{"1014044", "9061072"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1014044", "9061072"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Dedupe is by RESOLVED id, not by the caller's spelling. "US" and "2840" are the same
// place said two ways; sending both would create two criteria for one country, and
// Google charges nothing extra for that but the criterion-count assertions downstream
// (and any human reading the campaign) would both be wrong.
func TestResolveGeoList_DedupesAcrossSpellings(t *testing.T) {
	got, err := validateGeoTargets([]string{"US", " us ", "2840", "JP"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"2840", "2392"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// An all-zero id is numeric in shape and names nothing. It is the one digit string that
// is worth catching locally, because it is what an unset integer field serialises to —
// so it arrives from a caller bug rather than from a human typing an id.
func TestResolveGeoList_RejectsAllZeroConstantID(t *testing.T) {
	for _, entry := range []string{"0", "000"} {
		if _, err := validateGeoTargets([]string{entry}); err == nil {
			t.Errorf("expected %q to be rejected as a geo target constant id", entry)
		}
	}
}

// The noun is threaded through every message so an operator reading a 400 knows WHICH
// list to fix. Both lists reject the same way; only the wording may differ.
func TestResolveGeoList_ErrorNamesTheListItCameFrom(t *testing.T) {
	_, err := validateGeoTargets([]string{"XX"})
	if err == nil {
		t.Fatal("expected an error for an unmapped code")
	}
	if !strings.Contains(err.Error(), "geo target") || strings.Contains(err.Error(), "excluded") {
		t.Errorf("included-list error should say %q and not %q, got %v", "geo target", "excluded", err)
	}

	_, exErr := validateExcludedGeoTargets([]string{"XX"})
	if exErr == nil {
		t.Fatal("expected an error for an unmapped excluded code")
	}
	if !strings.Contains(exErr.Error(), "excluded geo target") {
		t.Errorf("excluded-list error should say %q, got %v", "excluded geo target", exErr)
	}
}

// The cap bounds EACH list independently. A shared budget across both lists would mean
// a long exclusion list silently shrinking how many places a campaign may target.
func TestResolveGeoList_CapsEachListIndependently(t *testing.T) {
	ids := make([]string, 0, maxGeoTargets+1)
	for i := 0; i < maxGeoTargets+1; i++ {
		ids = append(ids, strconv.Itoa(1000000+i))
	}
	if _, err := validateGeoTargets(ids); err == nil {
		t.Fatalf("expected %d geo targets to exceed the cap of %d", len(ids), maxGeoTargets)
	}
	if _, err := validateGeoTargets(ids[:maxGeoTargets]); err != nil {
		t.Fatalf("exactly %d geo targets must be accepted, got %v", maxGeoTargets, err)
	}
	// Both lists are full-sized at once — proof the cap is per list.
	if _, err := validateExcludedGeoTargets(ids[:maxGeoTargets]); err != nil {
		t.Fatalf("exactly %d excluded geo targets must be accepted, got %v", maxGeoTargets, err)
	}
}

// ---------------------------------------------------------------------------
// validateProximityTargets
// ---------------------------------------------------------------------------

// Degrees reach the wire as MICRO-degrees. An off-by-1000 here lands the campaign in a
// different hemisphere's worth of error while every "it was sent" assertion still passes,
// so the conversion is pinned on an asymmetric, signed, fractional coordinate.
func TestValidateProximityTargets_ConvertsDegreesToMicroDegrees(t *testing.T) {
	got, err := validateProximityTargets([]ProximityTarget{{
		Latitude: 37.7749, Longitude: -122.4194, Radius: 25, RadiusUnit: "MILES",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d proximity criteria, want 1", len(got))
	}
	if got[0].GeoPoint.LatitudeInMicroDegrees != 37774900 {
		t.Errorf("latitude = %d, want 37774900", got[0].GeoPoint.LatitudeInMicroDegrees)
	}
	if got[0].GeoPoint.LongitudeInMicroDegrees != -122419400 {
		t.Errorf("longitude = %d, want -122419400", got[0].GeoPoint.LongitudeInMicroDegrees)
	}
	if got[0].Radius != 25 || got[0].RadiusUnits != radiusUnitMiles {
		t.Errorf("radius = %v %q, want 25 MILES", got[0].Radius, got[0].RadiusUnits)
	}
}

// Case and surrounding whitespace on the unit are a caller's formatting, not a different
// unit — but anything that is not one of the two units must be refused rather than
// defaulted, because a guessed unit buys ~2.5x or ~0.4x the intended area.
func TestValidateProximityTargets_UnitIsNormalisedButNeverGuessed(t *testing.T) {
	got, err := validateProximityTargets([]ProximityTarget{{
		Latitude: 1, Longitude: 1, Radius: 10, RadiusUnit: " kilometers ",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[0].RadiusUnits != radiusUnitKilometers {
		t.Errorf("radiusUnits = %q, want %q", got[0].RadiusUnits, radiusUnitKilometers)
	}

	for _, unit := range []string{"", "KM", "MI", "miles per hour"} {
		if _, err := validateProximityTargets([]ProximityTarget{{
			Latitude: 1, Longitude: 1, Radius: 10, RadiusUnit: unit,
		}}); err == nil {
			t.Errorf("radius unit %q must be refused, not defaulted", unit)
		}
	}
}

// NaN and +Inf pass every ordered comparison, so a bounds check written as `> max` alone
// lets them straight through to a paid campaign. They are rejected first, explicitly.
func TestValidateProximityTargets_RejectsOutOfRangeAndNonFinite(t *testing.T) {
	valid := ProximityTarget{Latitude: 10, Longitude: 10, Radius: 10, RadiusUnit: "MILES"}
	bad := map[string]ProximityTarget{
		"NaN latitude":        {Latitude: math.NaN(), Longitude: 10, Radius: 10, RadiusUnit: "MILES"},
		"NaN longitude":       {Latitude: 10, Longitude: math.NaN(), Radius: 10, RadiusUnit: "MILES"},
		"NaN radius":          {Latitude: 10, Longitude: 10, Radius: math.NaN(), RadiusUnit: "MILES"},
		"+Inf radius":         {Latitude: 10, Longitude: 10, Radius: math.Inf(1), RadiusUnit: "MILES"},
		"-Inf latitude":       {Latitude: math.Inf(-1), Longitude: 10, Radius: 10, RadiusUnit: "MILES"},
		"latitude above 90":   {Latitude: 90.1, Longitude: 10, Radius: 10, RadiusUnit: "MILES"},
		"latitude below -90":  {Latitude: -90.1, Longitude: 10, Radius: 10, RadiusUnit: "MILES"},
		"longitude above 180": {Latitude: 10, Longitude: 180.1, Radius: 10, RadiusUnit: "MILES"},
		"zero radius":         {Latitude: 10, Longitude: 10, Radius: 0, RadiusUnit: "MILES"},
		"negative radius":     {Latitude: 10, Longitude: 10, Radius: -1, RadiusUnit: "MILES"},
		"miles over ceiling":  {Latitude: 10, Longitude: 10, Radius: maxRadiusMiles + 1, RadiusUnit: "MILES"},
		"km over ceiling":     {Latitude: 10, Longitude: 10, Radius: maxRadiusKilometers + 1, RadiusUnit: "KILOMETERS"},
	}
	for name, target := range bad {
		if _, err := validateProximityTargets([]ProximityTarget{valid, target}); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}

	// The ceilings themselves are INSIDE the accepted range — a cap that refuses its own
	// boundary value is the off-by-one that quietly narrows what callers may buy.
	for _, ok := range []ProximityTarget{
		{Latitude: 90, Longitude: 180, Radius: maxRadiusMiles, RadiusUnit: "MILES"},
		{Latitude: -90, Longitude: -180, Radius: maxRadiusKilometers, RadiusUnit: "KILOMETERS"},
	} {
		if _, err := validateProximityTargets([]ProximityTarget{ok}); err != nil {
			t.Errorf("boundary target %+v must be accepted, got %v", ok, err)
		}
	}
}

func TestValidateProximityTargets_CapsTheList(t *testing.T) {
	targets := make([]ProximityTarget, 0, maxProximityTargets+1)
	for i := 0; i < maxProximityTargets+1; i++ {
		targets = append(targets, ProximityTarget{Latitude: 10, Longitude: 10, Radius: 10, RadiusUnit: "MILES"})
	}
	if _, err := validateProximityTargets(targets); err == nil {
		t.Fatalf("expected %d proximity targets to exceed the cap of %d", len(targets), maxProximityTargets)
	}
	if _, err := validateProximityTargets(targets[:maxProximityTargets]); err != nil {
		t.Fatalf("exactly %d proximity targets must be accepted, got %v", maxProximityTargets, err)
	}
}

// ---------------------------------------------------------------------------
// validateGeoPlan
// ---------------------------------------------------------------------------

// Targeting and excluding the same place is not a redundancy Google resolves in the
// caller's favour — the exclusion wins and the campaign does not serve there at all. The
// clash is caught across SPELLINGS, because "US" and "2840" collide only after resolution
// and a check on the raw strings would miss exactly the inputs most likely to produce it.
func TestValidateGeoPlan_RefusesIncludeExcludeClashAcrossSpellings(t *testing.T) {
	in := sampleInput()
	in.GeoTargets = []string{"US", "JP"}
	in.ExcludedGeoTargets = []string{"2840"}

	_, err := validateGeoPlan(campaignKindSearch, in)
	if err == nil {
		t.Fatal("expected a clash between included US and excluded 2840")
	}
	if !strings.Contains(err.Error(), "2840") {
		t.Errorf("error should name the clashing constant, got %v", err)
	}
}

// Excluding a place that is NOT targeted is ordinary: target a country, carve a region out
// of it. Only an exact clash is a defect.
func TestValidateGeoPlan_AcceptsExclusionsThatDoNotClash(t *testing.T) {
	in := sampleInput()
	in.GeoTargets = []string{"US"}
	in.ExcludedGeoTargets = []string{"1014044"}

	plan, err := validateGeoPlan(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.included) != 1 || len(plan.excluded) != 1 {
		t.Fatalf("plan = %+v, want one included and one excluded", plan)
	}
	if plan.count() != 2 {
		t.Errorf("count() = %d, want 2", plan.count())
	}
}

// Demand Gen attaches location criteria at the AD GROUP level, where this client has not
// verified proximity. Refusing locally is free; the alternative is finding out after a
// paid campaign exists. Dropping it silently is what LFXV2-3283 already fixed once.
func TestValidateGeoPlan_RefusesProximityOnDemandGen(t *testing.T) {
	in := sampleInput()
	in.ProximityTargets = []ProximityTarget{{Latitude: 10, Longitude: 10, Radius: 10, RadiusUnit: "MILES"}}

	if _, err := validateGeoPlan(campaignKindDemandGen, in); err == nil {
		t.Fatal("expected proximity targeting to be refused on Demand Gen")
	}
	if _, err := validateGeoPlan(campaignKindSearch, in); err != nil {
		t.Fatalf("the same input must be accepted on Search, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// The wire: one mutate, three kinds of criterion
// ---------------------------------------------------------------------------

// The whole point of the single batch. Inclusions, exclusions and proximity must arrive in
// ONE campaignCriteria:mutate — split across two calls, a failure of the second commits the
// inclusions alone and the campaign serves across exactly the places named as off-limits.
//
// This also pins the omitempty POLARITY on campaignCriterionCreate.Negative in both
// directions: `true` on an exclusion, and the key ENTIRELY ABSENT on an inclusion. A
// `"negative": false` is Google's own default and harmless, but an inclusion that gained
// `negative: true` would exclude the country the caller paid to target, so the two shapes
// are asserted against the raw JSON rather than a typed struct that cannot tell absent
// from false.
func TestCreateCampaign_SendsInclusionsExclusionsAndProximityInOneMutate(t *testing.T) {
	h, readBody := capturedMutate(4, campaignCriterionName)
	c := newGeoClient(t, h, failHandler(t, "adGroupCriteria:mutate (Search geo attaches at CAMPAIGN level)"))

	in := sampleInput()
	in.GeoTargets = []string{"US", "JP"}
	in.ExcludedGeoTargets = []string{"1014044"}
	in.ProximityTargets = []ProximityTarget{{
		Latitude: 37.7749, Longitude: -122.4194, Radius: 25, RadiusUnit: "MILES",
	}}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	body := readBody()
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode captured body: %v (body=%s)", err, body)
	}
	if len(req.Operations) != 4 {
		t.Fatalf("got %d operations, want 4 in ONE mutate (body=%s)", len(req.Operations), body)
	}

	// Inclusions first, in the caller's order, each with NO negative key.
	for i, want := range []string{"geoTargetConstants/2840", "geoTargetConstants/2392"} {
		create := req.Operations[i].Create
		if _, present := create["negative"]; present {
			t.Errorf("operation %d is an INCLUSION and must carry no negative key, got %v", i, create["negative"])
		}
		loc, _ := create["location"].(map[string]any)
		if loc == nil || loc["geoTargetConstant"] != want {
			t.Errorf("operation %d: location = %v, want geoTargetConstant %q", i, create["location"], want)
		}
	}

	// Then the exclusion, explicitly negative.
	exclusion := req.Operations[2].Create
	if exclusion["negative"] != true {
		t.Errorf("operation 2 is an EXCLUSION and must carry negative:true, got %v", exclusion["negative"])
	}
	if loc, _ := exclusion["location"].(map[string]any); loc == nil || loc["geoTargetConstant"] != "geoTargetConstants/1014044" {
		t.Errorf("operation 2: location = %v, want geoTargetConstants/1014044", exclusion["location"])
	}

	// Then proximity, carrying a point and a radius rather than a constant.
	proximityOp := req.Operations[3].Create
	if _, present := proximityOp["location"]; present {
		t.Errorf("operation 3 is a PROXIMITY criterion and must carry no location, got %v", proximityOp["location"])
	}
	prox, _ := proximityOp["proximity"].(map[string]any)
	if prox == nil {
		t.Fatalf("operation 3 carries no proximity (body=%s)", body)
	}
	if prox["radius"] != float64(25) || prox["radiusUnits"] != radiusUnitMiles {
		t.Errorf("proximity radius = %v %v, want 25 MILES", prox["radius"], prox["radiusUnits"])
	}
	point, _ := prox["geoPoint"].(map[string]any)
	if point == nil {
		t.Fatalf("operation 3 carries no geoPoint (body=%s)", body)
	}
	if point["latitudeInMicroDegrees"] != float64(37774900) {
		t.Errorf("latitudeInMicroDegrees = %v, want 37774900", point["latitudeInMicroDegrees"])
	}
	if point["longitudeInMicroDegrees"] != float64(-122419400) {
		t.Errorf("longitudeInMicroDegrees = %v, want -122419400", point["longitudeInMicroDegrees"])
	}

	// All four criteria are recorded, in the order they were sent.
	if len(res.GeoCriterionIDs) != 4 {
		t.Errorf("GeoCriterionIDs = %v, want 4 ids", res.GeoCriterionIDs)
	}
}

// The step string is what a human reads to confirm the campaign is what they asked for, so
// it reports the CALLER'S spellings and names only the parts that were actually requested —
// an "excluding" clause on a campaign with no exclusions is a lie about a paid resource.
func TestCreateCampaign_GeoStepReportsOnlyWhatWasAskedFor(t *testing.T) {
	h, _ := capturedMutate(3, campaignCriterionName)
	c := newGeoClient(t, h, failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.GeoTargets = []string{"US", "JP"}
	in.ExcludedGeoTargets = []string{"1014044"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	step := strings.Join(res.Steps, "\n")
	if !strings.Contains(step, "targeting US, JP") {
		t.Errorf("steps should report the caller's spellings, got:\n%s", step)
	}
	if !strings.Contains(step, "excluding 1014044") {
		t.Errorf("steps should report the exclusions, got:\n%s", step)
	}
	if strings.Contains(step, "radius target") {
		t.Errorf("steps must not mention radius targets when none were asked for, got:\n%s", step)
	}
}

// Demand Gen's exclusions attach at the AD GROUP level with the same polarity contract.
// The failing campaign-level handler proves the level did not silently change.
func TestCreateDemandGenCampaign_SendsExclusionsAtAdGroupLevel(t *testing.T) {
	h, readBody := capturedMutate(2, adGroupCriterionName)
	c := newGeoClient(t,
		failHandler(t, "campaignCriteria:mutate (Demand Gen REJECTS campaign-level location criteria)"),
		h)

	in := sampleInput()
	in.GeoTargets = []string{"DE"}
	in.ExcludedGeoTargets = []string{"FR"}

	if _, err := c.CreateDemandGenCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	body := readBody()
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode captured body: %v (body=%s)", err, body)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("got %d operations, want 2 (body=%s)", len(req.Operations), body)
	}
	if _, present := req.Operations[0].Create["negative"]; present {
		t.Errorf("the inclusion must carry no negative key, got %v", req.Operations[0].Create["negative"])
	}
	if req.Operations[1].Create["negative"] != true {
		t.Errorf("the exclusion must carry negative:true, got %v", req.Operations[1].Create["negative"])
	}
	for i, op := range req.Operations {
		if op.Create["adGroup"] != "customers/1234567890/adGroups/333" {
			t.Errorf("operation %d: adGroup = %v, want the created ad group resource", i, op.Create["adGroup"])
		}
		if _, present := op.Create["campaign"]; present {
			t.Errorf("operation %d: campaign = %v, want absent (Demand Gen attaches at AD GROUP level)", i, op.Create["campaign"])
		}
	}
}

// ---------------------------------------------------------------------------
// Refusals that must happen before anything is paid for
// ---------------------------------------------------------------------------

// Every one of these is validated inside preflightCampaignKind, BEFORE the budget mutate.
// The server errors on ANY request, so reaching upstream at all fails the test — which is
// the only way to prove a bad local input cannot orphan a paid budget.
func TestCreateCampaign_BadGeoDepthInputFailsBeforeAnyMutate(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"unmapped exclusion": func(in *CampaignInput) {
			in.ExcludedGeoTargets = []string{"XX"}
		},
		"zero constant id": func(in *CampaignInput) {
			in.GeoTargets = []string{"0"}
		},
		"include/exclude clash": func(in *CampaignInput) {
			in.GeoTargets = []string{"US"}
			in.ExcludedGeoTargets = []string{"US"}
		},
		"proximity with no unit": func(in *CampaignInput) {
			in.ProximityTargets = []ProximityTarget{{Latitude: 10, Longitude: 10, Radius: 10}}
		},
		"proximity out of range": func(in *CampaignInput) {
			in.ProximityTargets = []ProximityTarget{{Latitude: 91, Longitude: 10, Radius: 10, RadiusUnit: "MILES"}}
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
			t.Cleanup(tokenSrv.Close)
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no upstream call may be made for an invalid geo input, got %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			t.Cleanup(apiSrv.Close)
			c := NewClient(testCreds(), testAccount(),
				WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
				withRetryBaseDelay(time.Millisecond))

			in := sampleInput()
			mutate(&in)

			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatal("expected an error")
			}
			if res != nil {
				t.Errorf("expected a nil result (nothing was created), got %+v", res)
			}
			// The adoption path must refuse the identical input, or the same brief is
			// accepted or rejected depending on whether a same-name campaign exists.
			if vErr := c.ValidateCampaignInput(in); vErr == nil {
				t.Error("ValidateCampaignInput must refuse the same input")
			}
		})
	}
}

// Proximity on Demand Gen is refused in preflight too, not after a campaign exists.
func TestCreateDemandGenCampaign_ProximityFailsBeforeAnyMutate(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream call may be made when proximity is refused, got %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))

	in := sampleInput()
	in.ProximityTargets = []ProximityTarget{{Latitude: 10, Longitude: 10, Radius: 10, RadiusUnit: "MILES"}}

	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected proximity targeting to be refused on Demand Gen")
	}
	if res != nil {
		t.Errorf("expected a nil result (nothing was created), got %+v", res)
	}
}
