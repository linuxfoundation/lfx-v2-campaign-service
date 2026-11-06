// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"strings"
	"testing"
)

// #499: demographic targeting on Display ad groups. Google requires every demographic
// dimension to target all (a validateOnly probe on 2026-11-05 refused bidOnly false with
// TARGETING_SETTING_DEMOGRAPHIC_CRITERION_TYPE_GROUPS_MUST_BE_SET_TO_TARGET_ALL), so a targeted
// dimension is sent as positive criteria for the named values plus negative criteria for every
// other value, and the targetingSetting is exactly what it was before #499.

// Every Display group gets one criteria mutate carrying its exclusions, its positive targeting,
// then the complement negatives (less the value already excluded). The targetingSetting is
// untouched: the audience group keeps the probed five-entry list, the other sends none.
func TestCreateDisplayCampaign_DemographicTargetingOnEveryGroup(t *testing.T) {
	rig, c, base := newDisplayRig(t, twoDisplayGroupImages(t), nil)
	in := twoDisplayGroupInput(base)
	in.DisplayAdGroups[1].CustomAffinities, in.DisplayAdGroups[1].UserInterests = nil, nil
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	in.DisplayDemographics = DisplayDemographics{
		AgeRanges:    []string{"25-34", "AGE_RANGE_35_44", "25-34"},
		IncomeRanges: []string{"income_range_90_up"},
	}
	res, err := c.CreateDisplayCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	groups := rig.sent("adGroups:mutate")
	if len(groups) != 2 {
		t.Fatalf("got %d ad group creates, want 2", len(groups))
	}
	sameJSON(t, groups[0], `{"operations":[{"create":{"name":"`+displayGroupBase+` | Retargeting",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"PAUSED","type":"DISPLAY_STANDARD",`+
		`"targetingSetting":{"targetRestrictions":[{"targetingDimension":"AGE_RANGE","bidOnly":true},`+
		`{"targetingDimension":"GENDER","bidOnly":true},{"targetingDimension":"PARENTAL_STATUS","bidOnly":true},`+
		`{"targetingDimension":"INCOME_RANGE","bidOnly":true},{"targetingDimension":"AUDIENCE","bidOnly":false}]}}}]}`)
	sameJSON(t, groups[1], `{"operations":[{"create":{"name":"`+displayGroupBase+` | Prospecting",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"PAUSED","type":"DISPLAY_STANDARD"}}]}`)

	criteria := rig.sent("adGroupCriteria:mutate")
	// Group 1: demographics, then its audiences. Group 2: demographics only (its exclusion
	// list is a separate mutate after it).
	for i, group := range map[int]string{0: "333", 2: "334"} {
		g := `"adGroup":"customers/1234567890/adGroups/` + group + `"`
		neg := func(field, v string) string {
			return `{"create":{` + g + `,"negative":true,"` + field + `":{"type":"` + v + `"}}}`
		}
		pos := func(field, v string) string {
			return `{"create":{` + g + `,"status":"ENABLED","` + field + `":{"type":"` + v + `"}}}`
		}
		ops := []string{neg("incomeRange", "INCOME_RANGE_0_50"),
			pos("ageRange", "AGE_RANGE_25_34"), pos("ageRange", "AGE_RANGE_35_44"), pos("incomeRange", "INCOME_RANGE_90_UP")}
		for _, a := range []string{"AGE_RANGE_18_24", "AGE_RANGE_45_54", "AGE_RANGE_55_64", "AGE_RANGE_65_UP", "AGE_RANGE_UNDETERMINED"} {
			ops = append(ops, neg("ageRange", a))
		}
		// INCOME_RANGE_0_50 is already excluded above, so the complement leaves it out.
		for _, r := range []string{"INCOME_RANGE_50_60", "INCOME_RANGE_60_70", "INCOME_RANGE_70_80", "INCOME_RANGE_80_90", "INCOME_RANGE_UNDETERMINED"} {
			ops = append(ops, neg("incomeRange", r))
		}
		sameJSON(t, criteria[i], `{"operations":[`+strings.Join(ops, ",")+`]}`)
	}
	if got := strings.Join(res.AdGroupDemographicCriteriaIDs, ","); got != "800" {
		t.Errorf("AdGroupDemographicCriteriaIDs = %s, want the first group's exclusion", got)
	}
	if got := res.AdGroupDemographicTargetingCriteriaIDs; len(got) != 13 || got[0] != "801" || got[12] != "813" {
		t.Errorf("AdGroupDemographicTargetingCriteriaIDs = %v, want the first group's 13 (801..813)", got)
	}
	if len(res.AdGroups) != 2 || len(res.AdGroups[1].DemographicTargetingCriteriaIDs) != 13 || res.AdGroups[1].DemographicTargetingCriteriaIDs[0] != "821" {
		t.Errorf("AdGroups = %+v, want group 2's own 13 targeting ids", res.AdGroups)
	}
	if !containsStep(res.Steps, "Demographic targeting applied: 13 ad group criteria (2 age range(s); 1 income range(s); 10 other value(s) excluded)") {
		t.Errorf("steps = %v, want the targeting counted", res.Steps)
	}
}

// The single Display group with targeting and no audiences sends no targetingSetting at all,
// and its criteria mutate carries the positives then the complement negatives.
func TestCreateDisplayCampaign_SingleGroupDemographicTargeting(t *testing.T) {
	rig, c, base := newDisplayRig(t, displayImages(t), nil)
	in := displayInput(base)
	in.DisplayDemographics = DisplayDemographics{Genders: []string{"female"}, ParentalStatuses: []string{"NOT_A_PARENT"}}
	if _, err := c.CreateDisplayCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	groups := rig.sent("adGroups:mutate")
	if len(groups) != 1 || strings.Contains(groups[0], "targetingSetting") {
		t.Errorf("ad group create = %v, want no targetingSetting", groups)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	g := `"adGroup":"customers/1234567890/adGroups/333"`
	if len(criteria) != 1 {
		t.Fatalf("got %d criteria mutates, want 1", len(criteria))
	}
	sameJSON(t, criteria[0], `{"operations":[
		{"create":{`+g+`,"status":"ENABLED","gender":{"type":"FEMALE"}}},
		{"create":{`+g+`,"status":"ENABLED","parentalStatus":{"type":"NOT_A_PARENT"}}},
		{"create":{`+g+`,"negative":true,"gender":{"type":"MALE"}}},
		{"create":{`+g+`,"negative":true,"gender":{"type":"UNDETERMINED"}}},
		{"create":{`+g+`,"negative":true,"parentalStatus":{"type":"PARENT"}}},
		{"create":{`+g+`,"negative":true,"parentalStatus":{"type":"UNDETERMINED"}}}]}`)
}

// The complement leaves out a value the campaign-level exclusion already excludes, and a
// dimension that is not targeted gets no complement at all.
func TestValidateDisplayDemographics_ComplementSkipsExcluded(t *testing.T) {
	in := demandGenInput()
	in.ExcludedAgeRanges = []string{"18-24", "65+"}
	in.DisplayDemographics = DisplayDemographics{AgeRanges: []string{"25-34", "35-44", "45-54", "55-64"}}
	pf, err := NewClient(testCreds(), testAccount()).preflightCampaignKind(campaignKindDisplay, in)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	plan := pf.displayDemographics
	if len(plan.negAgeRanges) != 1 || plan.negAgeRanges[0].Type != "AGE_RANGE_UNDETERMINED" {
		t.Errorf("age complement = %+v, want only AGE_RANGE_UNDETERMINED", plan.negAgeRanges)
	}
	if len(plan.negGenders)+len(plan.negIncomeRanges)+len(plan.negParentalStatuses) != 0 {
		t.Errorf("untargeted dimensions got a complement: %+v", plan)
	}
}

// The targetingSetting never carries a demographic dimension bidOnly false: with audiences it
// is the probed five-entry list, without them it is absent.
func TestDisplayAdGroupTargetingSetting_NeverRestrictsDemographics(t *testing.T) {
	if got := displayAdGroupTargetingSetting(false); got != nil {
		t.Errorf("no audiences = %+v, want nil", got)
	}
	got := displayAdGroupTargetingSetting(true)
	want := []targetRestriction{
		{"AGE_RANGE", true}, {"GENDER", true}, {"PARENTAL_STATUS", true}, {"INCOME_RANGE", true}, {"AUDIENCE", false},
	}
	if len(got.TargetRestrictions) != len(want) {
		t.Fatalf("restrictions = %+v, want %+v", got.TargetRestrictions, want)
	}
	for i := range want {
		if got.TargetRestrictions[i] != want[i] {
			t.Errorf("restriction %d = %+v, want %+v", i, got.TargetRestrictions[i], want[i])
		}
	}
}

func TestValidateCampaignInputKind_DisplayDemographicsRefusals(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		edit    func(*CampaignInput)
		wantErr string
	}{
		{"display accepts", CampaignKindDisplay, func(in *CampaignInput) {
			in.DisplayDemographics = DisplayDemographics{AgeRanges: []string{"25-34"}, Genders: []string{"MALE"}, IncomeRanges: []string{"UNDETERMINED"}, ParentalStatuses: []string{"PARENT"}}
			in.ExcludedAgeRanges = []string{"18-24"}
		}, ""},
		{"age targeted and excluded", CampaignKindDisplay, func(in *CampaignInput) {
			in.DisplayDemographics.AgeRanges = []string{"AGE_RANGE_18_24"}
			in.ExcludedAgeRanges = []string{"18-24"}
		}, "AGE_RANGE_18_24 is both targeted (DisplayDemographics.AgeRanges) and excluded (ExcludedAgeRanges)"},
		{"gender targeted and excluded", CampaignKindDisplay, func(in *CampaignInput) {
			in.DisplayDemographics.Genders = []string{"MALE"}
			in.ExcludedGenders = []string{"male"}
		}, "MALE is both targeted (DisplayDemographics.Genders) and excluded (ExcludedGenders)"},
		{"income targeted and excluded", CampaignKindDisplay, func(in *CampaignInput) {
			in.DisplayDemographics.IncomeRanges = []string{"UNDETERMINED"}
			in.ExcludedIncomeRanges = []string{"INCOME_RANGE_UNDETERMINED"}
		}, "INCOME_RANGE_UNDETERMINED is both targeted (DisplayDemographics.IncomeRanges) and excluded (ExcludedIncomeRanges)"},
		{"parental targeted and excluded", CampaignKindDisplay, func(in *CampaignInput) {
			in.DisplayDemographics.ParentalStatuses = []string{"PARENT"}
			in.ExcludedParentalStatuses = []string{"PARENT"}
		}, "PARENT is both targeted (DisplayDemographics.ParentalStatuses) and excluded (ExcludedParentalStatuses)"},
		{"unknown age", CampaignKindDisplay, func(in *CampaignInput) { in.DisplayDemographics.AgeRanges = []string{"30-40"} }, `targeted age range "30-40" must be one of`},
		{"unknown gender", CampaignKindDisplay, func(in *CampaignInput) { in.DisplayDemographics.Genders = []string{"OTHER"} }, `targeted gender "OTHER" must be one of`},
		{"unknown income", CampaignKindDisplay, func(in *CampaignInput) { in.DisplayDemographics.IncomeRanges = []string{"50-60"} }, `targeted income range "50-60" must be one of`},
		{"unknown parental", CampaignKindDisplay, func(in *CampaignInput) { in.DisplayDemographics.ParentalStatuses = []string{"PARENTS"} }, `targeted parental status "PARENTS" must be one of`},
		{"search", CampaignKindSearch, func(in *CampaignInput) { in.DisplayDemographics.AgeRanges = []string{"25-34"} }, "display demographic targeting is not supported on " + CampaignKindSearch},
		{"demand gen", CampaignKindDemandGen, func(in *CampaignInput) { in.DisplayDemographics.Genders = []string{"MALE"} }, "display demographic targeting is not supported on " + CampaignKindDemandGen},
		{"video", CampaignKindVideo, func(in *CampaignInput) { in.DisplayDemographics.IncomeRanges = []string{"UNDETERMINED"} }, "display demographic targeting is not supported on " + CampaignKindVideo},
		{"performance max", CampaignKindPerformanceMax, func(in *CampaignInput) { in.DisplayDemographics.ParentalStatuses = []string{"PARENT"} }, "display demographic targeting is not supported on " + CampaignKindPerformanceMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := demandGenInput()
			if tc.kind == CampaignKindSearch {
				in = sampleInput()
			}
			tc.edit(&in)
			err := NewClient(testCreds(), testAccount()).ValidateCampaignInputKind(tc.kind, in)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected refusal %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
