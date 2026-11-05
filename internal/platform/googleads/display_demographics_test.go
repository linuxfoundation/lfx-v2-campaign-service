// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"strings"
	"testing"
)

// #499: positive demographic targeting on Display ad groups. Built from the v23 field names
// (AdGroup.targeting_setting.target_restrictions[].bid_only, AdGroupCriterion.age_range /
// gender / income_range / parental_status); no validateOnly probe has covered the positive
// criteria yet.

// Every Display group gets the exact targetingSetting — the targeted dimensions bidOnly false,
// the rest true, AUDIENCE false only where the group has audiences — and one criteria mutate
// carrying its exclusions then its positive targeting.
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
	demo := `{"targetingDimension":"AGE_RANGE","bidOnly":false},{"targetingDimension":"GENDER","bidOnly":true},` +
		`{"targetingDimension":"PARENTAL_STATUS","bidOnly":true},{"targetingDimension":"INCOME_RANGE","bidOnly":false}`
	sameJSON(t, groups[0], `{"operations":[{"create":{"name":"`+displayGroupBase+` | Retargeting",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"ENABLED","type":"DISPLAY_STANDARD",`+
		`"targetingSetting":{"targetRestrictions":[`+demo+`,{"targetingDimension":"AUDIENCE","bidOnly":false}]}}}]}`)
	sameJSON(t, groups[1], `{"operations":[{"create":{"name":"`+displayGroupBase+` | Prospecting",`+
		`"campaign":"customers/1234567890/campaigns/222","status":"ENABLED","type":"DISPLAY_STANDARD",`+
		`"targetingSetting":{"targetRestrictions":[`+demo+`]}}}]}`)

	criteria := rig.sent("adGroupCriteria:mutate")
	// Group 1: demographics, then its audiences. Group 2: demographics only (its exclusion
	// list is a separate mutate after it).
	for i, group := range map[int]string{0: "333", 2: "334"} {
		g := `"adGroup":"customers/1234567890/adGroups/` + group + `"`
		sameJSON(t, criteria[i], `{"operations":[
			{"create":{`+g+`,"negative":true,"incomeRange":{"type":"INCOME_RANGE_0_50"}}},
			{"create":{`+g+`,"status":"ENABLED","ageRange":{"type":"AGE_RANGE_25_34"}}},
			{"create":{`+g+`,"status":"ENABLED","ageRange":{"type":"AGE_RANGE_35_44"}}},
			{"create":{`+g+`,"status":"ENABLED","incomeRange":{"type":"INCOME_RANGE_90_UP"}}}]}`)
	}
	if got := strings.Join(res.AdGroupDemographicCriteriaIDs, ","); got != "800" {
		t.Errorf("AdGroupDemographicCriteriaIDs = %s, want the first group's exclusion", got)
	}
	if got := strings.Join(res.AdGroupDemographicTargetingCriteriaIDs, ","); got != "801,802,803" {
		t.Errorf("AdGroupDemographicTargetingCriteriaIDs = %s, want the first group's three", got)
	}
	if len(res.AdGroups) != 2 || strings.Join(res.AdGroups[1].DemographicTargetingCriteriaIDs, ",") != "821,822,823" {
		t.Errorf("AdGroups = %+v, want group 2's own targeting ids", res.AdGroups)
	}
	if !containsStep(res.Steps, "Demographic targeting applied: 3 ad group criteria (2 age range(s); 1 income range(s))") {
		t.Errorf("steps = %v, want the targeting counted", res.Steps)
	}
}

// The single Display group with targeting and no audiences: the four demographic dimensions
// alone, no AUDIENCE entry, and the targeting alone in its criteria mutate.
func TestCreateDisplayCampaign_SingleGroupDemographicTargeting(t *testing.T) {
	rig, c, base := newDisplayRig(t, displayImages(t), nil)
	in := displayInput(base)
	in.DisplayDemographics = DisplayDemographics{Genders: []string{"female"}, ParentalStatuses: []string{"NOT_A_PARENT"}}
	if _, err := c.CreateDisplayCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateDisplayCampaign: %v", err)
	}
	groups := rig.sent("adGroups:mutate")
	if len(groups) != 1 || !strings.Contains(groups[0], `"targetRestrictions":[{"targetingDimension":"AGE_RANGE","bidOnly":true},{"targetingDimension":"GENDER","bidOnly":false},{"targetingDimension":"PARENTAL_STATUS","bidOnly":false},{"targetingDimension":"INCOME_RANGE","bidOnly":true}]}`) {
		t.Errorf("ad group create = %v", groups)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	g := `"adGroup":"customers/1234567890/adGroups/333"`
	if len(criteria) != 1 {
		t.Fatalf("got %d criteria mutates, want 1", len(criteria))
	}
	sameJSON(t, criteria[0], `{"operations":[
		{"create":{`+g+`,"status":"ENABLED","gender":{"type":"FEMALE"}}},
		{"create":{`+g+`,"status":"ENABLED","parentalStatus":{"type":"NOT_A_PARENT"}}}]}`)
}

// Without demographic targeting the ad group payloads are unchanged: the audience group keeps
// the probed five-entry setting and a group without audiences sends none.
func TestDisplayAdGroupTargetingSetting_Unchanged(t *testing.T) {
	if got := displayAdGroupTargetingSetting(false, adGroupDemographicTargetingPlan{}); got != nil {
		t.Errorf("no audiences and no targeting = %+v, want nil", got)
	}
	got := displayAdGroupTargetingSetting(true, adGroupDemographicTargetingPlan{})
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
