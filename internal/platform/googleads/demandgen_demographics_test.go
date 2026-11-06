// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// fullDemographics is every dimension, in a scrambled order and mixed case, with repeats.
func fullDemographics() *DemandGenDemographics {
	return &DemandGenDemographics{
		AgeRanges:        []string{"45-54", "25-34", "age_range_35_44", "25-34"},
		Genders:          []string{"female", "UNDETERMINED", "MALE"},
		IncomeRanges:     []string{"INCOME_RANGE_90_UP", "income_range_80_90"},
		ParentalStatuses: []string{"NOT_A_PARENT"},
		AudienceSegments: []string{" customers/1234567890/userLists/123 ", "customers/1234567890/userLists/123"},
	}
}

// wantFullDimensions is fullDemographics as Google receives it: adjacent age brackets merged
// into one segment, values in enum order, UNDETERMINED as includeUndetermined, repeats gone.
const wantFullDimensions = `[
	{"age":{"ageRanges":[{"minAge":25,"maxAge":54}],"includeUndetermined":false}},
	{"gender":{"genders":["MALE","FEMALE"],"includeUndetermined":true}},
	{"householdIncome":{"incomeRanges":["INCOME_RANGE_80_90","INCOME_RANGE_90_UP"],"includeUndetermined":false}},
	{"parentalStatus":{"parentalStatuses":["NOT_A_PARENT"],"includeUndetermined":false}},
	{"audienceSegments":{"segments":[{"userList":{"userList":"customers/1234567890/userLists/123"}}]}}]`

// demandGenDemographicsName is the composed campaign name of demandGenInput(), and the
// Audience name its fullDemographics get. The hash is pinned so a change to what it covers
// (and so to which Audience a retry reuses) fails here rather than in production.
const (
	demandGenDemographicsCampaign = "LFX | DemandGen Campaign | tlf | KubeCon Europe 2026"
	demandGenDemographicsName     = demandGenDemographicsCampaign + " | Demographics f339a2ebe4b8c46f"
)

func demographicsInput() CampaignInput {
	in := demandGenInput()
	in.DemandGenDemographics = fullDemographics()
	return in
}

func TestValidateDemandGenDemographics_BuildsTheAudience(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, demographicsInput())
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	plan := pf.demandGenDemographics
	if plan == nil {
		t.Fatal("pf.demandGenDemographics is nil")
	}
	body, err := json.Marshal(plan.create)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, `{"name":"`+demandGenDemographicsName+`","description":"Created by LFX for `+pf.campaignName+`","dimensions":`+wantFullDimensions+`}`)
	if pf.campaignName != demandGenDemographicsCampaign {
		t.Errorf("campaign name = %q, want %q", pf.campaignName, demandGenDemographicsCampaign)
	}
	if want := "3 age range(s), 2 gender(s), 2 income range(s), 1 parental status(es), 1 user list(s)"; plan.summary != want {
		t.Errorf("summary = %q, want %q", plan.summary, want)
	}

	// Absent stays absent, and changes nothing else in the plan.
	pf, err = c.preflightCampaignKind(campaignKindDemandGen, demandGenInput())
	if err != nil || pf.demandGenDemographics != nil {
		t.Errorf("absent demographics = %+v, %v; want nil, nil", pf.demandGenDemographics, err)
	}
}

// The same choice in any order and case is the same Audience (same name, same bytes); a
// different choice, or a different campaign, is a different one.
func TestDemographicAudienceName_IsStableAndContentHashed(t *testing.T) {
	plan := func(campaign string, d *DemandGenDemographics) *demandGenDemographicPlan {
		in := demandGenInput()
		in.DemandGenDemographics = d
		p, err := validateDemandGenDemographics(campaignKindDemandGen, "1234567890", campaign, in)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		return p
	}
	a := plan("C", &DemandGenDemographics{AgeRanges: []string{"25-34", "35-44"}, Genders: []string{"MALE", "FEMALE"}})
	b := plan("C", &DemandGenDemographics{Genders: []string{"female", "male", "MALE"}, AgeRanges: []string{"AGE_RANGE_35_44", "25-34"}})
	if a.create.Name != b.create.Name {
		t.Errorf("reordered input got a different name: %q vs %q", a.create.Name, b.create.Name)
	}
	if other := plan("C", &DemandGenDemographics{AgeRanges: []string{"25-34"}, Genders: []string{"MALE", "FEMALE"}}); other.create.Name == a.create.Name {
		t.Errorf("a different choice reused the name %q", a.create.Name)
	}
	if other := plan("D", &DemandGenDemographics{AgeRanges: []string{"25-34", "35-44"}, Genders: []string{"MALE", "FEMALE"}}); strings.TrimPrefix(other.create.Name, "D") == strings.TrimPrefix(a.create.Name, "C") {
		t.Errorf("another campaign got the same hash: %q vs %q", other.create.Name, a.create.Name)
	}
}

// Audience.name is capped at 255 characters (TOO_LONG at 256, validateOnly 2026-11-05). The
// campaign part is shortened, never the hash, and the hash still tells two long campaign
// names apart that share the kept prefix.
func TestDemographicAudienceName_FitsGooglesLimit(t *testing.T) {
	dims := []audienceDimension{{Gender: &genderDimension{Genders: []string{"MALE"}}}}
	long := strings.Repeat("é", 250)
	a := demographicAudienceName(long+"a", dims)
	b := demographicAudienceName(long+"b", dims)
	if n := utf8.RuneCountInString(a); n != maxAudienceNameRunes {
		t.Errorf("name is %d characters, want %d", n, maxAudienceNameRunes)
	}
	if !strings.Contains(a, demographicAudienceNameInfix) || a == b {
		t.Errorf("names %q and %q must keep the hash and differ", a, b)
	}
	if short := demographicAudienceName("Short", dims); !strings.HasPrefix(short, "Short | Demographics ") || utf8.RuneCountInString(short) != len("Short | Demographics ")+16 {
		t.Errorf("short name = %q", short)
	}
}

func TestAgeSegments_MergeAdjacentBrackets(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []string
		want string
	}{
		"one bracket":    {[]string{"18-24"}, `[{"minAge":18,"maxAge":24}]`},
		"65+ has no max": {[]string{"65+"}, `[{"minAge":65}]`},
		"non-adjacent":   {[]string{"45-54", "18-24"}, `[{"minAge":18,"maxAge":24},{"minAge":45,"maxAge":54}]`},
		"all":            {[]string{"18-24", "25-34", "35-44", "45-54", "55-64", "65+"}, `[{"minAge":18}]`},
		"two runs":       {[]string{"25-34", "35-44", "55-64", "65+"}, `[{"minAge":25,"maxAge":44},{"minAge":55}]`},
	} {
		t.Run(name, func(t *testing.T) {
			in := demandGenInput()
			in.DemandGenDemographics = &DemandGenDemographics{AgeRanges: tc.in}
			p, err := validateDemandGenDemographics(campaignKindDemandGen, "1234567890", "C", in)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			got, _ := json.Marshal(p.create.Dimensions[0].Age.AgeRanges)
			jsonEqual(t, got, tc.want)
		})
	}
}

// Every refusal is before anything upstream: the preflight returns no plan.
func TestValidateDemandGenDemographics_Refusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*CampaignInput)
		wantSub string
	}{
		"empty object": {func(in *CampaignInput) { in.DemandGenDemographics = &DemandGenDemographics{} }, "is set but empty"},
		"beside DemandGenAudience": {func(in *CampaignInput) { in.DemandGenAudience = testDemandGenAudience },
			"DemandGenDemographics and DemandGenAudience cannot both be set"},
		"beside a group Audience": {func(in *CampaignInput) {
			in.DemandGenAdGroups = []DemandGenAdGroup{{Name: "A", Creative: creativeAt("https://img.example.com")}, {Name: "B", Audience: testDemandGenAudience, Creative: creativeAt("https://img.example.com")}}
		}, "Audience on Demand Gen ad group 2"},
		"unknown age":                 {func(in *CampaignInput) { in.DemandGenDemographics.AgeRanges = []string{"30-50"} }, `Demand Gen age range "30-50" must be one of`},
		"non-ASCII lookalike":         {func(in *CampaignInput) { in.DemandGenDemographics.IncomeRanges = []string{"ıncome_range_0_50"} }, "Demand Gen income range"},
		"unknown gender":              {func(in *CampaignInput) { in.DemandGenDemographics.Genders = []string{"OTHER"} }, `Demand Gen gender "OTHER"`},
		"unknown parental":            {func(in *CampaignInput) { in.DemandGenDemographics.ParentalStatuses = []string{"PARENTS"} }, `Demand Gen parental status "PARENTS"`},
		"age UNDETERMINED alone":      {func(in *CampaignInput) { in.DemandGenDemographics.AgeRanges = []string{"undetermined"} }, "age range list holds only UNDETERMINED"},
		"gender UNDETERMINED alone":   {func(in *CampaignInput) { in.DemandGenDemographics.Genders = []string{"UNDETERMINED"} }, "gender list holds only UNDETERMINED"},
		"income UNDETERMINED alone":   {func(in *CampaignInput) { in.DemandGenDemographics.IncomeRanges = []string{"INCOME_RANGE_UNDETERMINED"} }, "income range list holds only UNDETERMINED"},
		"parental UNDETERMINED alone": {func(in *CampaignInput) { in.DemandGenDemographics.ParentalStatuses = []string{"UNDETERMINED"} }, "parental status list holds only UNDETERMINED"},
		"too many": {func(in *CampaignInput) {
			in.DemandGenDemographics.Genders = strings.Split(strings.Repeat("MALE,", maxDemographicExclusions+1), ",")[:maxDemographicExclusions+1]
		}, "at most 20 Demand Gen genders"},
		"another account's list": {func(in *CampaignInput) {
			in.DemandGenDemographics.AudienceSegments = []string{"customers/9999999999/userLists/1"}
		}, "is on customer 9999999999"},
		"an Audience in the list": {func(in *CampaignInput) {
			in.DemandGenDemographics.AudienceSegments = []string{testDemandGenAudience}
		}, "is an Audience, which cannot go inside another Audience"},
		"a custom audience": {func(in *CampaignInput) {
			in.DemandGenDemographics.AudienceSegments = []string{"customers/1234567890/customAudiences/1"}
		}, "is not a customers/<id>/userLists/<id>"},
		"leading-zero id": {func(in *CampaignInput) {
			in.DemandGenDemographics.AudienceSegments = []string{"customers/1234567890/userLists/0123"}
		}, "not the canonical base-10 spelling"},
		"blank list entry": {func(in *CampaignInput) { in.DemandGenDemographics.AudienceSegments = []string{" "} }, "entry must not be empty"},
		"too many lists": {func(in *CampaignInput) {
			in.DemandGenDemographics.AudienceSegments = make([]string, maxAudienceSegments+1)
		}, "at most 20 DemandGenDemographics.AudienceSegments"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
			in := demographicsInput()
			tc.mutate(&in)
			pf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
			if err == nil {
				t.Fatalf("accepted; want a refusal containing %q (plan %+v)", tc.wantSub, pf.demandGenDemographics)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// Off Demand Gen the field is refused rather than dropped, through the whole preflight.
func TestPreflightRefusesDemandGenDemographicsOffDemandGen(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for _, kind := range []string{campaignKindSearch, campaignKindPerformanceMax, campaignKindVideo, campaignKindDisplay} {
		t.Run(kind, func(t *testing.T) {
			in := demandGenInput()
			if kind == campaignKindPerformanceMax {
				in.PerformanceMaxCreative = fullPMaxCreative()
			}
			if _, err := c.preflightCampaignKind(kind, in); err != nil {
				t.Fatalf("the base input must pass on %s, got %v", kind, err)
			}
			in.DemandGenDemographics = &DemandGenDemographics{AgeRanges: []string{"25-34"}}
			_, err := c.preflightCampaignKind(kind, in)
			if err == nil || !strings.Contains(err.Error(), "Demand Gen demographics are not supported on "+kind) {
				t.Fatalf("preflight(%s) = %v, want the Demand Gen demographics refusal", kind, err)
			}
		})
	}
}

// The excluded* demographic fields stay refused on Demand Gen, and the refusal now points at
// DemandGenDemographics, so a targeted value can never contradict an exclusion there.
func TestPreflightRefusesDemandGenExclusionsPointingAtDemographics(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	in := demographicsInput()
	in.ExcludedAgeRanges = []string{"18-24"}
	_, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err == nil || !strings.Contains(err.Error(), "DemandGenDemographics") || !strings.Contains(err.Error(), "to TARGET (every value except the excluded ones)") {
		t.Fatalf("err = %v, want the exclusion refusal pointing at the complement in DemandGenDemographics", err)
	}
	in = demographicsInput()
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	if _, err := c.preflightCampaignKind(campaignKindDemandGen, in); err == nil {
		t.Fatal("an income exclusion must stay refused on Demand Gen")
	}
}

// ---------------------------------------------------------------------------
// The cascade
// ---------------------------------------------------------------------------

const testDemographicAudience = "customers/1234567890/audiences/777"

func audienceCreated(_ int, _ []byte, w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"results":[{"resourceName":"`+testDemographicAudience+`"}]}`)
}

// The Audience is created FIRST, before the budget, with exactly the planned body, and then
// attached to the ad group in the audience+language criteria mutate.
func TestCreateDemandGenCampaign_CreatesAndAttachesTheDemographicAudience(t *testing.T) {
	rig, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": audienceCreated})
	in := demographicsInput()
	in.GeoTargets = []string{"US"}
	in.Languages = []string{"EN"}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if got, want := strings.Join(rig.mutatePaths(), ","), "audiences:mutate,campaignBudgets:mutate,campaigns:mutate,adGroups:mutate,adGroupCriteria:mutate,adGroupCriteria:mutate"; got != want {
		t.Errorf("mutates =\n  %s\nwant\n  %s", got, want)
	}
	jsonEqual(t, rig.sent("audiences:mutate")[0], `{"operations":[{"create":{"name":"`+demandGenDemographicsName+`",`+
		`"description":"Created by LFX for `+demandGenDemographicsCampaign+`","dimensions":`+wantFullDimensions+`}}]}`)
	jsonEqual(t, rig.sent("adGroupCriteria:mutate")[1], `{"operations":[
		{"create":{"adGroup":"customers/1234567890/adGroups/333","audience":{"audience":"`+testDemographicAudience+`"}}},
		{"create":{"adGroup":"customers/1234567890/adGroups/333","language":{"languageConstant":"languageConstants/1000"}}}]}`)
	if res.DemandGenDemographicAudience != testDemographicAudience {
		t.Errorf("DemandGenDemographicAudience = %q, want %q", res.DemandGenDemographicAudience, testDemographicAudience)
	}
	if strings.Join(res.AudienceCriteriaIDs, ",") != "800" {
		t.Errorf("AudienceCriteriaIDs = %v, want [800]", res.AudienceCriteriaIDs)
	}
	want := "Demographic Audience created: " + testDemographicAudience + " (3 age range(s), 2 gender(s), 2 income range(s), 1 parental status(es), 1 user list(s)), to be attached to every Demand Gen ad group"
	if res.Steps[0] != want {
		t.Errorf("first step = %q, want %q", res.Steps[0], want)
	}
}

// Every DemandGenAdGroups entry gets the one Audience.
func TestCreateDemandGenCampaign_DemographicAudienceOnEveryAdGroup(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": audienceCreated})
	in := twoGroupInput(base)
	for i := range in.DemandGenAdGroups {
		in.DemandGenAdGroups[i].Audience = ""
	}
	in.DemandGenDemographics = &DemandGenDemographics{Genders: []string{"FEMALE"}}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if n := len(rig.sent("audiences:mutate")); n != 1 {
		t.Fatalf("got %d audiences:mutate, want exactly one for both groups", n)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	for i, g := range map[int]string{1: "333", 3: "334"} {
		jsonEqual(t, criteria[i], `{"operations":[
			{"create":{"adGroup":"customers/1234567890/adGroups/`+g+`","audience":{"audience":"`+testDemographicAudience+`"}}},
			{"create":{"adGroup":"customers/1234567890/adGroups/`+g+`","language":{"languageConstant":"languageConstants/1000"}}}]}`)
	}
	for _, g := range res.AdGroups {
		if strings.Join(g.AudienceCriteriaIDs, ",") != "800" {
			t.Errorf("group %s AudienceCriteriaIDs = %v, want [800]", g.Name, g.AudienceCriteriaIDs)
		}
	}
}

// audienceRow is the GAQL row Google returns for the Audience fullDemographics creates.
func audienceRow(status, dimensions string) string {
	return `{"results":[{"audience":{"resourceName":"` + testDemographicAudience + `","name":"` + demandGenDemographicsName +
		`","status":"` + status + `","scope":"CUSTOMER","dimensions":` + dimensions + `}}]}`
}

// A retry lands on NAME_ALREADY_IN_USE: the Audience is looked up read-only by the exact name
// and reused only when it still holds exactly the planned dimensions. Google omits a false
// includeUndetermined, which must still compare equal.
func TestCreateDemandGenCampaign_ReusesTheDemographicAudienceOnNameInUse(t *testing.T) {
	googleShape := strings.ReplaceAll(wantFullDimensions, `,"includeUndetermined":false`, ``)
	nameInUse := func(_ int, _ []byte, w http.ResponseWriter) {
		gaqlError(http.StatusBadRequest, "audienceError", "NAME_ALREADY_IN_USE")(w, nil)
	}
	rig, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": nameInUse,
		"googleAds:search": func(_ int, _ []byte, w http.ResponseWriter) {
			_, _ = io.WriteString(w, audienceRow("ENABLED", googleShape))
		},
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	search := string(rig.sent("googleAds:search")[0])
	if !strings.Contains(search, `FROM audience WHERE audience.name = '`+demandGenDemographicsName+`'`) {
		t.Errorf("lookup = %s, want it to match the exact name", search)
	}
	if res.DemandGenDemographicAudience != testDemographicAudience || !strings.HasPrefix(res.Steps[0], "Demographic Audience reused: "+testDemographicAudience) {
		t.Errorf("audience = %q, steps[0] = %q; want the reused Audience", res.DemandGenDemographicAudience, res.Steps[0])
	}
	if !strings.Contains(string(rig.sent("adGroupCriteria:mutate")[0]), `"audience":{"audience":"`+testDemographicAudience+`"}`) {
		t.Errorf("the reused Audience was not attached: %s", rig.sent("adGroupCriteria:mutate")[0])
	}
}

// Every failure of the Audience step is before the budget: (nil, err), nothing else sent.
func TestCreateDemandGenCampaign_DemographicAudienceFailuresCreateNothingElse(t *testing.T) {
	nameInUse := func(_ int, _ []byte, w http.ResponseWriter) {
		gaqlError(http.StatusBadRequest, "audienceError", "NAME_ALREADY_IN_USE")(w, nil)
	}
	rows := func(body string) func(int, []byte, http.ResponseWriter) {
		return func(_ int, _ []byte, w http.ResponseWriter) { _, _ = io.WriteString(w, body) }
	}
	edited := strings.Replace(wantFullDimensions, `"NOT_A_PARENT"`, `"PARENT"`, 1)
	extraArm := strings.Replace(wantFullDimensions, `{"userList":{"userList":"customers/1234567890/userLists/123"}}`,
		`{"userList":{"userList":"customers/1234567890/userLists/123"}},{"customAudience":{"customAudience":"customers/1234567890/customAudiences/9"}}`, 1)
	withExclusion := strings.Replace(audienceRow("ENABLED", wantFullDimensions), `"dimensions":`, `"exclusionDimension":{"exclusions":[{"userList":{"userList":"customers/1234567890/userLists/5"}}]},"dimensions":`, 1)
	otherScope := strings.Replace(audienceRow("ENABLED", wantFullDimensions), `"scope":"CUSTOMER"`, `"scope":"ASSET_GROUP"`, 1)
	noScope := strings.Replace(audienceRow("ENABLED", wantFullDimensions), `"scope":"CUSTOMER",`, ``, 1)
	for name, tc := range map[string]struct {
		overrides map[string]func(int, []byte, http.ResponseWriter)
		wantSub   string
	}{
		"definite refusal": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
				gaqlError(http.StatusBadRequest, "audienceError", "DIMENSION_INVALID")(w, nil)
			},
		}, "creation failed (no budget or campaign was created)"},
		"name in use, exclusion added since": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(withExclusion),
		}, "carries exclusions this client never set"},
		"name in use, another scope": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(otherScope),
		}, `has scope "ASSET_GROUP", not CUSTOMER`},
		"name in use, scope missing": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(noScope),
		}, `has scope "", not CUSTOMER`},
		"name in use, edited since": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(audienceRow("ENABLED", edited)),
		}, "differ from the planned ones"},
		"name in use, an arm this client never sends": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(audienceRow("ENABLED", extraArm)),
		}, "differ from the planned ones"},
		"name in use, removed": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(audienceRow("REMOVED", wantFullDimensions)),
		}, "is REMOVED, not ENABLED"},
		"name in use, not found": {map[string]func(int, []byte, http.ResponseWriter){
			"audiences:mutate": nameInUse, "googleAds:search": rows(`{"results":[]}`),
		}, "returned 0 Audiences"},
	} {
		t.Run(name, func(t *testing.T) {
			rig, c, _ := newDGRig(t, nil, tc.overrides)
			res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			if res != nil {
				t.Errorf("result = %+v, want nil: nothing billable exists", res)
			}
			for _, p := range rig.mutatePaths() {
				if p != "audiences:mutate" {
					t.Errorf("sent %s after a failed Audience step", p)
				}
			}
		})
	}
}

// The demographic Audience is named on every partial past it, so a reconciler can find it.
func TestCreateDemandGenCampaign_PartialsNameTheDemographicAudience(t *testing.T) {
	_, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": audienceCreated,
		"campaigns:mutate": func(_ int, _ []byte, w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
	if err == nil || res == nil {
		t.Fatalf("res, err = %+v, %v; want a partial and an error", res, err)
	}
	if res.DemandGenDemographicAudience != testDemographicAudience || res.CampaignBudgetID != "111" {
		t.Errorf("partial = %+v, want the budget and the Audience named", res)
	}
}

// An Audience create that may have landed is not a pre-create failure: the result is a partial
// naming the planned Audience, so the claim is kept and the account reconciled, and nothing
// past the Audience is sent.
func TestCreateDemandGenCampaign_UnconfirmedDemographicAudienceIsAPartial(t *testing.T) {
	for name, create := range map[string]func(int, []byte, http.ResponseWriter){
		"ambiguous create": func(_ int, _ []byte, w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) },
		"wrong-kind 2xx": func(_ int, _ []byte, w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/userLists/777"}]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": create})
			res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
			if err == nil || !strings.Contains(err.Error(), "creation UNCONFIRMED") {
				t.Fatalf("err = %v, want UNCONFIRMED", err)
			}
			if res == nil || res.CampaignBudgetID != "" || len(res.Steps) != 1 || !strings.Contains(res.Steps[0], demandGenDemographicsName) ||
				res.DemandGenDemographicAudienceName != demandGenDemographicsName {
				t.Fatalf("result = %+v, want a partial naming the planned Audience", res)
			}
			if outcome, ok := CreateOutcome(res, err); !ok || outcome == "" {
				t.Errorf("CreateOutcome = %q, %v; want a partial-result outcome", outcome, ok)
			}
			if got := strings.Join(rig.mutatePaths(), ","); got != "audiences:mutate" {
				t.Errorf("mutates = %s, want only the Audience create", got)
			}
		})
	}
}

// A definite budget refusal after the Audience exists returns a partial naming the Audience.
func TestCreateDemandGenCampaign_BudgetRefusalAfterTheAudienceIsAPartial(t *testing.T) {
	_, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": audienceCreated,
		"campaignBudgets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "INVALID_AMOUNT")(w, nil)
		},
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
	if err == nil || !strings.Contains(err.Error(), "demographic Audience "+testDemographicAudience+" in place") {
		t.Fatalf("err = %v, want the budget failure naming the Audience", err)
	}
	if res == nil || res.DemandGenDemographicAudience != testDemographicAudience {
		t.Fatalf("result = %+v, want a partial naming the Audience", res)
	}
}

// Google's read-back order is not verified live, so the reuse comparison ignores order.
func TestSameAudienceDimensions_IgnoresOrder(t *testing.T) {
	in := demandGenInput()
	in.DemandGenDemographics = &DemandGenDemographics{
		AgeRanges: []string{"18-24", "45-54"}, Genders: []string{"MALE", "FEMALE"},
		AudienceSegments: []string{"customers/1234567890/userLists/2", "customers/1234567890/userLists/1"},
	}
	p, err := validateDemandGenDemographics(campaignKindDemandGen, "1234567890", "C", in)
	if err != nil {
		t.Fatal(err)
	}
	got := []json.RawMessage{
		json.RawMessage(`{"audienceSegments":{"segments":[{"userList":{"userList":"customers/1234567890/userLists/2"}},{"userList":{"userList":"customers/1234567890/userLists/1"}}]}}`),
		json.RawMessage(`{"gender":{"genders":["FEMALE","MALE"]}}`),
		json.RawMessage(`{"age":{"ageRanges":[{"minAge":45,"maxAge":54},{"minAge":18,"maxAge":24}]}}`),
	}
	if !sameAudienceDimensions(got, p.create.Dimensions) {
		t.Error("the same content in another order must compare equal")
	}
	got[1] = json.RawMessage(`{"gender":{"genders":["FEMALE"]}}`)
	if sameAudienceDimensions(got, p.create.Dimensions) {
		t.Error("different content must not compare equal")
	}
	// The same lists in another order are the same Audience.
	in.DemandGenDemographics.AudienceSegments = []string{"customers/1234567890/userLists/1", "customers/1234567890/userLists/2"}
	q, _ := validateDemandGenDemographics(campaignKindDemandGen, "1234567890", "C", in)
	if q.create.Name != p.create.Name {
		t.Errorf("reordered user lists got another name: %q vs %q", q.create.Name, p.create.Name)
	}
}

// The off-channel refusal names only fields that channel accepts: every field the hint names
// is set at once, and the preflight must pass.
func TestDemandGenDemographicsRefusalHintIsAccepted(t *testing.T) {
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	setters := map[string]func(*CampaignInput){
		"ExcludedAgeRanges":        func(in *CampaignInput) { in.ExcludedAgeRanges = []string{"18-24"} },
		"ExcludedGenders":          func(in *CampaignInput) { in.ExcludedGenders = []string{"MALE"} },
		"ExcludedIncomeRanges":     func(in *CampaignInput) { in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"} },
		"ExcludedParentalStatuses": func(in *CampaignInput) { in.ExcludedParentalStatuses = []string{"PARENT"} },
		"ObservedDemographics":     func(in *CampaignInput) { in.ObservedDemographics = []string{"GENDER"} },
	}
	for _, kind := range []string{campaignKindSearch, campaignKindDisplay, campaignKindVideo, campaignKindPerformanceMax} {
		t.Run(kind, func(t *testing.T) {
			in := demandGenInput()
			if kind == campaignKindPerformanceMax {
				in.PerformanceMaxCreative = fullPMaxCreative()
			}
			hint := demographicFieldsHint(kind)
			for field, set := range setters {
				if strings.Contains(hint, field) {
					set(&in)
				}
			}
			if _, err := c.preflightCampaignKind(kind, in); err != nil {
				t.Fatalf("the fields hint %q names must be accepted on %s: %v", hint, kind, err)
			}
			in.DemandGenDemographics = &DemandGenDemographics{AgeRanges: []string{"25-34"}}
			if _, err := c.preflightCampaignKind(kind, in); err == nil || !strings.Contains(err.Error(), hint) {
				t.Errorf("err = %v, want the hint %q", err, hint)
			}
		})
	}
}

// The Audience was created, then the budget hit DUPLICATE_NAME and its reuse lookup failed. The
// outcome must not be budget_name_taken ("nothing new was created"): this attempt created the
// Audience, which the partial names by resource and by planned name.
func TestCreateDemandGenCampaign_BudgetNameTakenAfterTheAudienceIsNotNothingCreated(t *testing.T) {
	_, c, _ := newDGRig(t, nil, map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": audienceCreated,
		"campaignBudgets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, nil)
		},
		"googleAds:search": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "requestError", "INVALID_INPUT")(w, nil)
		},
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), demographicsInput())
	if err == nil || res == nil {
		t.Fatalf("res, err = %+v, %v; want a partial and an error", res, err)
	}
	if res.DemandGenDemographicAudience != testDemographicAudience || res.DemandGenDemographicAudienceName != demandGenDemographicsName {
		t.Errorf("partial = %+v, want the Audience named", res)
	}
	outcome, ok := CreateOutcome(res, err)
	if !ok || outcome == domain.CreateOutcomeBudgetNameTaken {
		t.Errorf("CreateOutcome = %q, %v; must not claim nothing new was created", outcome, ok)
	}
	// Without the Audience the same refusal is still budget_name_taken.
	res.DemandGenDemographicAudience = ""
	if outcome, _ := CreateOutcome(res, err); outcome != domain.CreateOutcomeBudgetNameTaken {
		t.Errorf("CreateOutcome without an Audience = %q, want budget_name_taken", outcome)
	}
}
