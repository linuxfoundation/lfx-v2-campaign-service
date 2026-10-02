// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func floatPtr(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// Languages
// ---------------------------------------------------------------------------

// Each code must resolve to ITS OWN constant. A map that returned 1000 for
// everything would satisfy a bare "resolves without error" assertion while running
// every campaign in the world in English.
func TestResolveLanguageList_ResolvesEachCodeToItsOwnConstant(t *testing.T) {
	got, err := resolveLanguageList([]string{"EN", "de", " ja "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1000", "1001", "1005"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A raw numeric language constant id is the escape hatch for every language the
// curated map does not carry, and it must pass through byte-identical. Dedupe is
// by the RESOLVED id, so "EN" and "1000" collapse.
func TestResolveLanguageList_AcceptsNumericIDsAndDedupesAcrossSpellings(t *testing.T) {
	got, err := resolveLanguageList([]string{"EN", "1000", "1088"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"1000", "1088"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveLanguageList_RejectsUnknownAndMalformed(t *testing.T) {
	for _, entry := range []string{"ENG", "XX", "", "0", "000"} {
		if _, err := resolveLanguageList([]string{entry}); err == nil {
			t.Errorf("expected language %q to be rejected", entry)
		}
	}
}

func TestResolveLanguageList_CapsTheList(t *testing.T) {
	ids := make([]string, 0, maxLanguages+1)
	for i := 0; i < maxLanguages+1; i++ {
		ids = append(ids, strconv.Itoa(2000+i))
	}
	if _, err := resolveLanguageList(ids); err == nil {
		t.Fatalf("expected %d languages to exceed the cap of %d", len(ids), maxLanguages)
	}
	if _, err := resolveLanguageList(ids[:maxLanguages]); err != nil {
		t.Fatalf("exactly %d languages must be accepted, got %v", maxLanguages, err)
	}
}

func TestLanguageResource_RendersConstantPath(t *testing.T) {
	if got := languageResource("1000"); got != "languageConstants/1000" {
		t.Fatalf("got %q, want languageConstants/1000", got)
	}
}

// ---------------------------------------------------------------------------
// Ad schedules
// ---------------------------------------------------------------------------

// Minutes are an ENUM upstream, not an integer. A payload carrying 30 where
// Google expects "THIRTY" is rejected after the campaign exists, so the mapping
// is pinned on a value that is neither the zero nor the first enum member.
func TestValidateAdSchedules_MapsMinutesToTheEnum(t *testing.T) {
	got, mods, err := validateAdSchedules([]AdSchedule{{
		DayOfWeek: "monday", StartHour: 9, StartMinute: 30, EndHour: 17, EndMinute: 45,
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || len(mods) != 1 {
		t.Fatalf("got %d schedules and %d modifiers, want 1 of each", len(got), len(mods))
	}
	want := adScheduleInfo{DayOfWeek: "MONDAY", StartHour: 9, StartMinute: "THIRTY", EndHour: 17, EndMinute: "FORTY_FIVE"}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if mods[0] != nil {
		t.Errorf("an unset bid modifier must stay nil, got %v", *mods[0])
	}
}

// Midnight at each end of the day. Hour 0 is a valid START and hour 24 a valid
// END; a bounds check that treated 0 as unset or 24 as out of range would refuse
// the two most ordinary schedules there are.
func TestValidateAdSchedules_AcceptsMidnightAtBothEnds(t *testing.T) {
	got, _, err := validateAdSchedules([]AdSchedule{{
		DayOfWeek: "SUNDAY", StartHour: 0, StartMinute: 0, EndHour: 24, EndMinute: 0,
	}})
	if err != nil {
		t.Fatalf("a full-day schedule must be accepted, got %v", err)
	}
	if got[0].StartHour != 0 || got[0].EndHour != 24 {
		t.Errorf("got %+v, want 0..24", got[0])
	}
	if got[0].StartMinute != "ZERO" || got[0].EndMinute != "ZERO" {
		t.Errorf("minute ZERO must be sent explicitly, got %+v", got[0])
	}
}

func TestValidateAdSchedules_RejectsMalformedIntervals(t *testing.T) {
	bad := map[string]AdSchedule{
		"unknown day":       {DayOfWeek: "FUNDAY", StartHour: 9, EndHour: 17},
		"empty day":         {DayOfWeek: "", StartHour: 9, EndHour: 17},
		"start hour 24":     {DayOfWeek: "MONDAY", StartHour: 24, EndHour: 24},
		"negative start":    {DayOfWeek: "MONDAY", StartHour: -1, EndHour: 17},
		"end hour 0":        {DayOfWeek: "MONDAY", StartHour: 0, EndHour: 0},
		"end hour 25":       {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 25},
		"off-enum minute":   {DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 10, EndHour: 17},
		"off-enum end min":  {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, EndMinute: 20},
		"end before start":  {DayOfWeek: "MONDAY", StartHour: 17, EndHour: 9},
		"empty window":      {DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 30, EndHour: 9, EndMinute: 30},
		"past end of day":   {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 24, EndMinute: 15},
		"NaN bid modifier":  {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(math.NaN())},
		"bid modifier high": {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(maxBidModifier + 0.1)},
		"bid modifier low":  {DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(0.05)},
	}
	for name, schedule := range bad {
		if _, _, err := validateAdSchedules([]AdSchedule{schedule}); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// 0 is the -100% opt-out and is NOT the same as "no modifier": both must be
// accepted, and they must stay distinguishable all the way to the wire.
func TestValidateAdSchedules_ZeroBidModifierIsNotUnset(t *testing.T) {
	_, mods, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(0)},
		{DayOfWeek: "TUESDAY", StartHour: 9, EndHour: 17},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mods[0] == nil || *mods[0] != 0 {
		t.Errorf("an explicit zero modifier must survive as 0, got %v", mods[0])
	}
	if mods[1] != nil {
		t.Errorf("an unset modifier must stay nil, got %v", *mods[1])
	}
}

func TestValidateAdSchedules_CapsTheList(t *testing.T) {
	schedules := make([]AdSchedule, 0, maxAdSchedules+1)
	for i := 0; i < maxAdSchedules+1; i++ {
		schedules = append(schedules, AdSchedule{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17})
	}
	if _, _, err := validateAdSchedules(schedules); err == nil {
		t.Fatalf("expected %d ad schedules to exceed the cap of %d", len(schedules), maxAdSchedules)
	}
	if _, _, err := validateAdSchedules(schedules[:maxAdSchedules]); err != nil {
		t.Fatalf("exactly %d ad schedules must be accepted, got %v", maxAdSchedules, err)
	}
}

// ---------------------------------------------------------------------------
// Device bid modifiers
// ---------------------------------------------------------------------------

// The accepted range is Google's own (0 to opt out, else 0.1..10.0), so both
// boundaries must be INSIDE it — a guard that refuses its own boundary refuses a
// create upstream would have taken.
func TestValidateDeviceBidModifiers_AcceptsGooglesOwnRange(t *testing.T) {
	devices, mods, err := validateDeviceBidModifiers([]DeviceBidModifier{
		{Device: "mobile", BidModifier: minBidModifier},
		{Device: "DESKTOP", BidModifier: maxBidModifier},
		{Device: " tablet ", BidModifier: 0},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantTypes := []string{"MOBILE", "DESKTOP", "TABLET"}
	for i, want := range wantTypes {
		if devices[i].Type != want {
			t.Errorf("devices[%d].Type = %q, want %q", i, devices[i].Type, want)
		}
	}
	if mods[2] != 0 {
		t.Errorf("the tablet opt-out must stay 0, got %v", mods[2])
	}
}

func TestValidateDeviceBidModifiers_RejectsBadInput(t *testing.T) {
	bad := map[string][]DeviceBidModifier{
		"unknown device":    {{Device: "WATCH", BidModifier: 1}},
		"empty device":      {{Device: "", BidModifier: 1}},
		"below the range":   {{Device: "MOBILE", BidModifier: 0.05}},
		"above the range":   {{Device: "MOBILE", BidModifier: maxBidModifier + 0.1}},
		"negative modifier": {{Device: "MOBILE", BidModifier: -1}},
		"NaN modifier":      {{Device: "MOBILE", BidModifier: math.NaN()}},
		"Inf modifier":      {{Device: "MOBILE", BidModifier: math.Inf(1)}},
		// Two criteria for one device is a conflict Google rejects after the campaign
		// exists, and the caller plainly meant one of the two values.
		"duplicate device": {{Device: "MOBILE", BidModifier: 1.5}, {Device: "mobile", BidModifier: 2}},
	}
	for name, devices := range bad {
		if _, _, err := validateDeviceBidModifiers(devices); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Demographic exclusions
// ---------------------------------------------------------------------------

// Both spellings reach the same enum, and the dedupe is by the RESOLVED enum so
// "18-24" and "AGE_RANGE_18_24" collapse into one criterion rather than two
// conflicting ones.
func TestValidateExcludedAgeRanges_AcceptsBothSpellingsAndDedupes(t *testing.T) {
	got, err := validateExcludedAgeRanges([]string{"18-24", "AGE_RANGE_18_24", "65+", " undetermined "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"AGE_RANGE_18_24", "AGE_RANGE_65_UP", "AGE_RANGE_UNDETERMINED"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Type != want[i] {
			t.Errorf("got[%d].Type = %q, want %q", i, got[i].Type, want[i])
		}
	}
}

func TestValidateExcludedAgeRanges_RejectsUnknown(t *testing.T) {
	for _, entry := range []string{"18_24", "0-17", "", "ALL"} {
		if _, err := validateExcludedAgeRanges([]string{entry}); err == nil {
			t.Errorf("expected age range %q to be rejected", entry)
		}
	}
}

func TestValidateExcludedGenders_NormalisesAndDedupes(t *testing.T) {
	got, err := validateExcludedGenders([]string{"male", "MALE", " female "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Type != "MALE" || got[1].Type != "FEMALE" {
		t.Fatalf("got %+v, want MALE then FEMALE", got)
	}
	for _, entry := range []string{"M", "NONBINARY", ""} {
		if _, err := validateExcludedGenders([]string{entry}); err == nil {
			t.Errorf("expected gender %q to be rejected", entry)
		}
	}
}

// ---------------------------------------------------------------------------
// validateCriteriaPlan
// ---------------------------------------------------------------------------

// Demand Gen attaches its targeting at the ad group level, where this client has
// verified none of these criteria. Each field is refused on its own, so a caller
// is told which one it was rather than being refused for a field it did not set.
func TestValidateCriteriaPlan_RefusesEveryCriterionOnDemandGen(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"languages": func(in *CampaignInput) { in.Languages = []string{"EN"} },
		"ad schedules": func(in *CampaignInput) {
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17}}
		},
		"device bid modifiers": func(in *CampaignInput) {
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "MOBILE", BidModifier: 1.2}}
		},
		"excluded age ranges": func(in *CampaignInput) { in.ExcludedAgeRanges = []string{"18-24"} },
		"excluded genders":    func(in *CampaignInput) { in.ExcludedGenders = []string{"UNDETERMINED"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := sampleInput()
			mutate(&in)
			if _, err := validateCriteriaPlan(campaignKindDemandGen, in); err == nil {
				t.Fatalf("%s must be refused on Demand Gen", name)
			}
			if _, err := validateCriteriaPlan(campaignKindSearch, in); err != nil {
				t.Fatalf("the same input must be accepted on Search, got %v", err)
			}
		})
	}
}

// An input with none of these fields must produce an empty plan, so the cascade
// sends no extra mutate at all and every caller predating the feature is
// unaffected.
func TestValidateCriteriaPlan_EmptyInputIsAnEmptyPlan(t *testing.T) {
	for _, kind := range []string{campaignKindSearch, campaignKindDemandGen} {
		plan, err := validateCriteriaPlan(kind, sampleInput())
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", kind, err)
		}
		if !plan.empty() || plan.count() != 0 {
			t.Errorf("%s: plan = %+v, want empty", kind, plan)
		}
	}
}

// ---------------------------------------------------------------------------
// The wire
// ---------------------------------------------------------------------------

// All five kinds in ONE campaignCriteria:mutate, each in its own criterion arm.
//
// The assertions are against raw JSON rather than a typed struct because three of
// the defects this feature can have are invisible to a typed decode: a device
// opt-out whose `bidModifier: 0` was dropped by omitempty (which turns "do not
// serve on tablets" into "bid normally on tablets"), a demographic exclusion that
// lost its `negative` (which TARGETS the age range the caller excluded), and a
// language criterion that gained one.
func TestCreateCampaign_SendsEveryCampaignCriterionInOneMutate(t *testing.T) {
	h, readBody := capturedMutate(5, campaignCriterionName)
	c := newGeoClient(t, h, failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.Languages = []string{"EN"}
	in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 30, EndHour: 17, EndMinute: 0, BidModifier: floatPtr(1.25)}}
	in.DeviceBidModifiers = []DeviceBidModifier{{Device: "TABLET", BidModifier: 0}}
	in.ExcludedAgeRanges = []string{"18-24"}
	in.ExcludedGenders = []string{"UNDETERMINED"}

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
	if len(req.Operations) != 5 {
		t.Fatalf("got %d operations, want 5 in ONE mutate (body=%s)", len(req.Operations), body)
	}
	for i, op := range req.Operations {
		if op.Create["campaign"] != "customers/1234567890/campaigns/222" {
			t.Errorf("operation %d: campaign = %v, want the created campaign resource", i, op.Create["campaign"])
		}
	}

	// 0: language, positive, no bid modifier.
	language, _ := req.Operations[0].Create["language"].(map[string]any)
	if language == nil || language["languageConstant"] != "languageConstants/1000" {
		t.Errorf("operation 0: language = %v, want languageConstants/1000", req.Operations[0].Create["language"])
	}
	if _, present := req.Operations[0].Create["negative"]; present {
		t.Errorf("operation 0 is a positive criterion and must carry no negative key, got %v", req.Operations[0].Create["negative"])
	}
	if _, present := req.Operations[0].Create["bidModifier"]; present {
		t.Errorf("operation 0 must carry no bidModifier, got %v", req.Operations[0].Create["bidModifier"])
	}

	// 1: ad schedule, with its enum minutes and its modifier.
	schedule, _ := req.Operations[1].Create["adSchedule"].(map[string]any)
	if schedule == nil {
		t.Fatalf("operation 1 carries no adSchedule (body=%s)", body)
	}
	for key, want := range map[string]any{
		"dayOfWeek": "MONDAY", "startHour": float64(9), "startMinute": "THIRTY",
		"endHour": float64(17), "endMinute": "ZERO",
	} {
		if schedule[key] != want {
			t.Errorf("operation 1: adSchedule.%s = %v, want %v", key, schedule[key], want)
		}
	}
	if req.Operations[1].Create["bidModifier"] != float64(1.25) {
		t.Errorf("operation 1: bidModifier = %v, want 1.25", req.Operations[1].Create["bidModifier"])
	}

	// 2: device opt-out. The zero MUST be on the wire.
	device, _ := req.Operations[2].Create["device"].(map[string]any)
	if device == nil || device["type"] != "TABLET" {
		t.Errorf("operation 2: device = %v, want TABLET", req.Operations[2].Create["device"])
	}
	modifier, present := req.Operations[2].Create["bidModifier"]
	if !present {
		t.Errorf("operation 2: the -100%% opt-out must be sent as an explicit bidModifier:0, not omitted (body=%s)", body)
	} else if modifier != float64(0) {
		t.Errorf("operation 2: bidModifier = %v, want 0", modifier)
	}

	// 3 and 4: demographic EXCLUSIONS, both explicitly negative.
	ageRange, _ := req.Operations[3].Create["ageRange"].(map[string]any)
	if ageRange == nil || ageRange["type"] != "AGE_RANGE_18_24" {
		t.Errorf("operation 3: ageRange = %v, want AGE_RANGE_18_24", req.Operations[3].Create["ageRange"])
	}
	gender, _ := req.Operations[4].Create["gender"].(map[string]any)
	if gender == nil || gender["type"] != "UNDETERMINED" {
		t.Errorf("operation 4: gender = %v, want UNDETERMINED", req.Operations[4].Create["gender"])
	}
	for _, i := range []int{3, 4} {
		if req.Operations[i].Create["negative"] != true {
			t.Errorf("operation %d is a demographic EXCLUSION and must carry negative:true, got %v", i, req.Operations[i].Create["negative"])
		}
	}

	if len(res.TargetingCriterionIDs) != 5 {
		t.Errorf("TargetingCriterionIDs = %v, want 5 ids", res.TargetingCriterionIDs)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Campaign targeting applied: 5 criteria") {
		t.Errorf("steps should report the criteria, got:\n%s", strings.Join(res.Steps, "\n"))
	}
}

// Geo and these criteria are SEPARATE mutates on purpose: a shared batch would
// make either set's failure discard the other, and geo's failure sentence ("it has
// NO location criteria and would serve worldwide") would be false for a dropped
// language criterion. Two requests, each carrying only its own operations.
func TestCreateCampaign_GeoAndTargetingCriteriaAreSeparateMutates(t *testing.T) {
	type capture struct {
		bodies []string
	}
	var (
		got   capture
		calls = make(chan string, 4)
	)
	c := newGeoClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			body, _ := decodeRequest(r)
			raw, _ := json.Marshal(body)
			calls <- string(raw)
			ops, _ := body["operations"].([]any)
			parts := make([]string, 0, len(ops))
			for i := range ops {
				parts = append(parts, `{"resourceName":"`+campaignCriterionName(i)+`"}`)
			}
			_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.GeoTargets = []string{"US"}
	in.Languages = []string{"EN"}

	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	close(calls)
	for body := range calls {
		got.bodies = append(got.bodies, body)
	}

	if len(got.bodies) != 2 {
		t.Fatalf("got %d campaignCriteria:mutate calls, want 2 (geo and targeting are separate)", len(got.bodies))
	}
	if !strings.Contains(got.bodies[0], "geoTargetConstant") || strings.Contains(got.bodies[0], "languageConstant") {
		t.Errorf("the first mutate must carry only the geo criteria, got %s", got.bodies[0])
	}
	if !strings.Contains(got.bodies[1], "languageConstant") || strings.Contains(got.bodies[1], "geoTargetConstant") {
		t.Errorf("the second mutate must carry only the targeting criteria, got %s", got.bodies[1])
	}
}

// Every one of these is validated inside preflightCampaignKind, BEFORE the budget
// mutate. A server that errors on ANY request is the only way to prove a bad local
// input cannot orphan a paid resource.
func TestCreateCampaign_BadCampaignCriteriaFailBeforeAnyMutate(t *testing.T) {
	cases := map[string]func(in *CampaignInput){
		"unknown language": func(in *CampaignInput) { in.Languages = []string{"ENG"} },
		"inverted schedule": func(in *CampaignInput) {
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 17, EndHour: 9}}
		},
		"off-enum minute": func(in *CampaignInput) {
			in.AdSchedules = []AdSchedule{{DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 10, EndHour: 17}}
		},
		"duplicate device": func(in *CampaignInput) {
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "MOBILE", BidModifier: 1.5}, {Device: "MOBILE", BidModifier: 2}}
		},
		"out-of-range bid modifier": func(in *CampaignInput) {
			in.DeviceBidModifiers = []DeviceBidModifier{{Device: "MOBILE", BidModifier: 11}}
		},
		"unknown age range": func(in *CampaignInput) { in.ExcludedAgeRanges = []string{"0-17"} },
		"unknown gender":    func(in *CampaignInput) { in.ExcludedGenders = []string{"NONBINARY"} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
			t.Cleanup(tokenSrv.Close)
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no upstream call may be made for an invalid campaign criterion, got %s", r.URL.Path)
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

// A criteria failure after the campaign exists returns the campaign ALONGSIDE the
// error — the campaign is real and spends, so the claim must stay reconcilable.
func TestCreateCampaign_TargetingCriteriaFailureKeepsCampaignPartial(t *testing.T) {
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT"}}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.Languages = []string{"EN"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error")
	}
	if res == nil {
		t.Fatal("expected the campaign to be returned alongside the error, got nil")
	}
	if res.CampaignID == "" {
		t.Errorf("the partial result must carry the campaign id, got %+v", res)
	}
	if len(res.TargetingCriterionIDs) != 0 {
		t.Errorf("no criterion ids may be recorded from a failed mutate, got %v", res.TargetingCriterionIDs)
	}
}

// A 2xx with fewer results than operations is UNCONFIRMED, not a success: criteria
// may exist upstream with ids this run could not read, and persisting a short list
// would make a retry double-create the rest.
func TestCreateCampaign_ShortTargetingMutateResponseIsUnconfirmed(t *testing.T) {
	c := newGeoClient(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+campaignCriterionName(0)+`"}]}`)
		},
		failHandler(t, "adGroupCriteria:mutate"))

	in := sampleInput()
	in.Languages = []string{"EN", "DE"}

	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for a short mutate response")
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("error should say UNCONFIRMED, got %v", err)
	}
	if res == nil || len(res.TargetingCriterionIDs) != 0 {
		t.Errorf("no ids may be persisted from an unconfirmed mutate, got %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Sweep fixes (LFXV2-2665): canonical language ids, duplicate ad schedules
// ---------------------------------------------------------------------------

// The geo twin of this check was caught in review; this is the same defect on the
// language side. Digits-only is not the id test — "0" names nothing, "01000" is a
// non-canonical spelling that would send TWO criteria for English, and a 21-digit
// run overflows the int64 Google exposes these ids as. All three are decidable
// locally, and all three would otherwise be refused only at the
// campaignCriteria:mutate that runs AFTER the budget and campaign are committed.
func TestResolveLanguageList_RejectsNonCanonicalNumericIDs(t *testing.T) {
	for _, entry := range []string{"0", "000", "01000", "0000000000000000000", "9999999999999999999999"} {
		if _, err := resolveLanguageList([]string{entry}); err == nil {
			t.Errorf("language %q is not a canonical positive id and must be rejected", entry)
		}
	}
}

// The over-refusal guard for the tightening above. A canonical numeric id is the
// escape hatch for every language the curated map does not carry, and refusing one
// Google would have accepted is the failure mode this whole family must not have.
func TestResolveLanguageList_StillAcceptsCanonicalNumericIDs(t *testing.T) {
	for _, entry := range []string{"1000", "1088", "9223372036854775807"} {
		got, err := resolveLanguageList([]string{entry})
		if err != nil {
			t.Fatalf("canonical language id %q must be accepted, got %v", entry, err)
		}
		if len(got) != 1 || got[0] != entry {
			t.Errorf("language %q must pass through byte-identical, got %v", entry, got)
		}
	}
}

// Two identical intervals are one criterion written twice. Google refuses the
// second as an overlapping ad schedule AFTER the campaign exists, so the repeat is
// collapsed here — the way languages, age ranges and genders already collapse.
func TestValidateAdSchedules_CollapsesExactDuplicates(t *testing.T) {
	got, mods, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 30, EndHour: 17, EndMinute: 0},
		{DayOfWeek: "monday", StartHour: 9, StartMinute: 30, EndHour: 17, EndMinute: 0},
	})
	if err != nil {
		t.Fatalf("an exact repeat must collapse, not fail: %v", err)
	}
	if len(got) != 1 || len(mods) != 1 {
		t.Fatalf("got %d schedules and %d modifiers, want 1 of each", len(got), len(mods))
	}
}

// The same interval carrying two DIFFERENT bid modifiers is a disagreement, not a
// repeat: one of the two values would be the one silently dropped. Refused, which
// is the rule devices already apply and for the same reason.
func TestValidateAdSchedules_RefusesSameIntervalWithDifferentBidModifier(t *testing.T) {
	_, _, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(1.2)},
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(1.5)},
	})
	if err == nil {
		t.Fatal("a repeated interval with a conflicting bid modifier must be refused")
	}
	// An unset modifier and an explicit one are also a disagreement — the pointer
	// polarity is load-bearing everywhere else in this file, so it must be here too.
	if _, _, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17},
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(1.5)},
	}); err == nil {
		t.Fatal("unset vs explicit on the same interval must be refused")
	}
	// Two equal modifiers written as separate pointers ARE the same thing, and must
	// collapse rather than trip the conflict check.
	got, _, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(1.2)},
		{DayOfWeek: "MONDAY", StartHour: 9, EndHour: 17, BidModifier: floatPtr(1.2)},
	})
	if err != nil {
		t.Fatalf("equal modifiers on a repeated interval must collapse: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d schedules, want 1", len(got))
	}
}

// The over-refusal guard for the dedupe. Two DIFFERENT windows on the same day are
// two legitimate criteria — a morning and an evening slot is the most ordinary
// split there is — and a dedupe keyed too coarsely (on the day alone) would drop
// one of them.
func TestValidateAdSchedules_DistinctWindowsOnOneDayBothSurvive(t *testing.T) {
	got, _, err := validateAdSchedules([]AdSchedule{
		{DayOfWeek: "MONDAY", StartHour: 6, EndHour: 9},
		{DayOfWeek: "MONDAY", StartHour: 17, EndHour: 21},
		{DayOfWeek: "MONDAY", StartHour: 9, StartMinute: 0, EndHour: 9, EndMinute: 30},
	})
	if err != nil {
		t.Fatalf("distinct windows on one day must all be accepted: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d schedules, want 3", len(got))
	}
}
