// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// A created Audience per Demand Gen ad group (#518)
// ---------------------------------------------------------------------------

// The pinned names of the two groups' Audiences in groupDemographicsInput. The hash covers the
// campaign name, the group's label and the dimensions, so a change to what it covers (and so to
// which Audience a retry reuses) fails here rather than in production.
const (
	retargetingAudienceName = demandGenDemographicsCampaign + " | Retargeting | Demographics 89b48424866c5bff"
	prospectingAudienceName = demandGenDemographicsCampaign + " | Prospecting | Demographics 462aa0eaab05cefa"
)

// groupDemographicsInput is the live KubeCon NA shape: Retargeting with user lists only, and
// Prospecting with age 25-54 (and undetermined), a user list and a custom audience.
func groupDemographicsInput(base string) CampaignInput {
	in := twoGroupInput(base)
	in.DemandGenAdGroups[0].Audience = ""
	in.DemandGenAdGroups[0].Demographics = &DemandGenDemographics{
		AudienceSegments: []string{"customers/1234567890/userLists/2", "customers/1234567890/userLists/1"},
	}
	in.DemandGenAdGroups[1].Audience = ""
	in.DemandGenAdGroups[1].Demographics = &DemandGenDemographics{
		AgeRanges:        []string{"25-34", "35-44", "45-54", "UNDETERMINED"},
		AudienceSegments: []string{"customers/1234567890/userLists/3"},
		CustomAudiences:  []string{"customers/1234567890/customAudiences/4"},
	}
	return in
}

// audiencesNumbered answers each audiences:mutate with audiences/901, 902, ... in call order.
func audiencesNumbered(call int, _ []byte, w http.ResponseWriter) {
	_, _ = fmt.Fprintf(w, `{"results":[{"resourceName":"customers/1234567890/audiences/%d"}]}`, 901+call)
}

func audienceCreateName(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Operations []struct {
			Create audienceCreate `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Operations) != 1 {
		t.Fatalf("audiences:mutate body %s: %v", body, err)
	}
	return req.Operations[0].Create.Name
}

// Each group's Demographics become an Audience of its own, created before the budget in entry
// order, and each group's one audience criterion references its own Audience.
func TestCreateDemandGenCampaign_CreatesAnAudiencePerAdGroup(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": audiencesNumbered})
	res, err := c.CreateDemandGenCampaign(context.Background(), groupDemographicsInput(base))
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if got := strings.Join(rig.mutatePaths()[:3], ","); got != "audiences:mutate,audiences:mutate,campaignBudgets:mutate" {
		t.Errorf("first mutates = %s, want both Audiences before the budget", got)
	}
	sent := rig.sent("audiences:mutate")
	jsonEqual(t, sent[0], `{"operations":[{"create":{"name":"`+retargetingAudienceName+`",`+
		`"description":"Created by LFX for `+demandGenDemographicsCampaign+`","dimensions":[`+
		`{"audienceSegments":{"segments":[{"userList":{"userList":"customers/1234567890/userLists/1"}},{"userList":{"userList":"customers/1234567890/userLists/2"}}]}}]}}]}`)
	jsonEqual(t, sent[1], `{"operations":[{"create":{"name":"`+prospectingAudienceName+`",`+
		`"description":"Created by LFX for `+demandGenDemographicsCampaign+`","dimensions":[`+
		`{"age":{"ageRanges":[{"minAge":25,"maxAge":54}],"includeUndetermined":true}},`+
		`{"audienceSegments":{"segments":[{"userList":{"userList":"customers/1234567890/userLists/3"}},{"customAudience":{"customAudience":"customers/1234567890/customAudiences/4"}}]}}]}}]}`)

	criteria := rig.sent("adGroupCriteria:mutate")
	for i, g := range map[int]struct{ adGroup, audience string }{1: {"333", "901"}, 3: {"334", "902"}} {
		jsonEqual(t, criteria[i], `{"operations":[
			{"create":{"adGroup":"customers/1234567890/adGroups/`+g.adGroup+`","audience":{"audience":"customers/1234567890/audiences/`+g.audience+`"}}},
			{"create":{"adGroup":"customers/1234567890/adGroups/`+g.adGroup+`","language":{"languageConstant":"languageConstants/1000"}}}]}`)
	}
	want := []DemandGenAdGroupAudience{
		{AdGroupName: res.AdGroups[0].Name, DemographicAudience: "customers/1234567890/audiences/901", DemographicAudienceName: retargetingAudienceName},
		{AdGroupName: res.AdGroups[1].Name, DemographicAudience: "customers/1234567890/audiences/902", DemographicAudienceName: prospectingAudienceName},
	}
	if !slices.Equal(res.DemandGenAdGroupAudiences, want) {
		t.Errorf("DemandGenAdGroupAudiences = %+v, want %+v", res.DemandGenAdGroupAudiences, want)
	}
	if res.DemandGenDemographicAudience != "" || res.DemandGenDemographicAudienceName != "" {
		t.Errorf("campaign-level Audience = %q / %q, want none", res.DemandGenDemographicAudience, res.DemandGenDemographicAudienceName)
	}
	if want := `Demographic Audience created: customers/1234567890/audiences/902 (3 age range(s), 1 user list(s), 1 custom audience(s)), to be attached to Demand Gen ad group "` + res.AdGroups[1].Name + `"`; res.Steps[1] != want {
		t.Errorf("steps[1] = %q, want %q", res.Steps[1], want)
	}
}

// A group's own Demographics override the campaign-level ones for that group; every other group
// still gets the campaign-level Audience, which is created first.
func TestCreateDemandGenCampaign_GroupDemographicsOverrideTheCampaignLevel(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": audiencesNumbered})
	in := groupDemographicsInput(base)
	in.DemandGenAdGroups[0].Demographics = nil
	in.DemandGenDemographics = &DemandGenDemographics{Genders: []string{"FEMALE"}}
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	sent := rig.sent("audiences:mutate")
	if len(sent) != 2 || audienceCreateName(t, sent[1]) != prospectingAudienceName ||
		!strings.HasPrefix(audienceCreateName(t, sent[0]), demandGenDemographicsCampaign+" | Demographics ") {
		t.Fatalf("audiences:mutate = %q, want the campaign-level Audience then Prospecting's", sent)
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	if !strings.Contains(string(criteria[1]), `"audience":{"audience":"customers/1234567890/audiences/901"}`) {
		t.Errorf("group 1 criteria = %s, want the campaign-level Audience 901", criteria[1])
	}
	if !strings.Contains(string(criteria[3]), `"audience":{"audience":"customers/1234567890/audiences/902"}`) {
		t.Errorf("group 2 criteria = %s, want its own Audience 902", criteria[3])
	}
	if res.DemandGenDemographicAudience != "customers/1234567890/audiences/901" || len(res.DemandGenAdGroupAudiences) != 1 ||
		res.DemandGenAdGroupAudiences[0].DemographicAudience != "customers/1234567890/audiences/902" {
		t.Errorf("result audiences = %q, %+v", res.DemandGenDemographicAudience, res.DemandGenAdGroupAudiences)
	}
}

// Every refusal of a group's Demographics is a preflight refusal, before any request, and
// names the group.
func TestPreflightRefusesBadGroupDemographics(t *testing.T) {
	const base = "https://cdn.example.org"
	c := &Client{account: AccountConfig{CustomerID: "1234567890"}}
	for name, tc := range map[string]struct {
		mutate  func(in *CampaignInput)
		wantSub []string
	}{
		"beside the group's own Audience": {func(in *CampaignInput) {
			in.DemandGenAdGroups[1].Audience = "customers/1234567890/audiences/42"
		}, []string{`| Prospecting" sets both Audience and Demographics`}},
		"empty": {func(in *CampaignInput) {
			in.DemandGenAdGroups[0].Demographics = &DemandGenDemographics{}
		}, []string{`the Demographics of Demand Gen ad group "`, `| Retargeting" is set but empty`}},
		"campaign-level overridden by every group": {func(in *CampaignInput) {
			in.DemandGenDemographics = &DemandGenDemographics{Genders: []string{"FEMALE"}}
		}, []string{"DemandGenDemographics would be attached to no ad group", "each of the 2 Demand Gen ad groups"}},
		"campaign-level beside a group's Audience, unchanged": {func(in *CampaignInput) {
			in.DemandGenDemographics = &DemandGenDemographics{Genders: []string{"FEMALE"}}
			in.DemandGenAdGroups[0].Demographics = nil
			in.DemandGenAdGroups[0].Audience = "customers/1234567890/audiences/41"
		}, []string{"DemandGenDemographics cannot be combined with an Audience on Demand Gen ad group 1"}},
		"a user list on another account": {func(in *CampaignInput) {
			in.DemandGenAdGroups[1].Demographics.AudienceSegments = []string{"customers/999/userLists/3"}
		}, []string{`Demographics.AudienceSegments of Demand Gen ad group "`, `is on customer "999"`}},
		"a custom audience in the user lists": {func(in *CampaignInput) {
			in.DemandGenAdGroups[1].Demographics.AudienceSegments = []string{"customers/1234567890/customAudiences/3"}
		}, []string{`so move it to Demographics.CustomAudiences of Demand Gen ad group "`}},
		"an Audience in the custom audiences": {func(in *CampaignInput) {
			in.DemandGenAdGroups[1].Demographics.CustomAudiences = []string{"customers/1234567890/audiences/3"}
		}, []string{"cannot go inside another Audience", "or set that ad group's Audience to it and omit its Demographics"}},
		"an unknown age range": {func(in *CampaignInput) {
			in.DemandGenAdGroups[1].Demographics.AgeRanges = []string{"30-50"}
		}, []string{`30-50`, `(in the Demographics of Demand Gen ad group "`}},
		"restricts nobody": {func(in *CampaignInput) {
			in.DemandGenAdGroups[0].Demographics = &DemandGenDemographics{Genders: []string{"MALE", "FEMALE", "UNDETERMINED"}}
		}, []string{`the Demographics of Demand Gen ad group "`, `restricts nobody`}},
	} {
		t.Run(name, func(t *testing.T) {
			in := groupDemographicsInput(base)
			tc.mutate(&in)
			_, err := c.preflightCampaignKind(campaignKindDemandGen, in)
			if err == nil {
				t.Fatal("preflight passed, want a refusal")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("err = %v, want it to contain %q", err, sub)
				}
			}
		})
	}
	// The base input passes, and a group's dimension that names everyone is left out (#529).
	in := groupDemographicsInput(base)
	in.DemandGenAdGroups[0].Demographics.Genders = []string{"MALE", "FEMALE", "UNDETERMINED"}
	pf, err := c.preflightCampaignKind(campaignKindDemandGen, in)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if d := pf.demandGenGroups[0].demographics; d == nil || len(d.create.Dimensions) != 1 || d.create.Dimensions[0].AudienceSegments == nil {
		t.Errorf("group 1 plan = %+v, want the user lists alone, the everyone gender dimension left out", d)
	}
	// Off Demand Gen the list itself is refused, so a group's Demographics never get that far.
	in = groupDemographicsInput(base)
	if _, err := c.preflightCampaignKind(campaignKindDisplay, in); err == nil || !strings.Contains(err.Error(), "Demand Gen ad groups are not supported") {
		t.Errorf("display preflight = %v, want the Demand Gen ad groups refusal", err)
	}
}

// A retry lands on NAME_ALREADY_IN_USE for a group's Audience: it is looked up by that group's
// exact name and reused when it holds exactly the planned dimensions.
func TestCreateDemandGenCampaign_ReusesAGroupAudienceOnNameInUse(t *testing.T) {
	rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "audienceError", "NAME_ALREADY_IN_USE")(w, nil)
		},
		"googleAds:search": func(_ int, _ []byte, w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"results":[{"audience":{"resourceName":"customers/1234567890/audiences/700","name":"`+retargetingAudienceName+
				`","status":"ENABLED","scope":"CUSTOMER","dimensions":[{"audienceSegments":{"segments":[`+
				`{"userList":{"userList":"customers/1234567890/userLists/2"}},{"userList":{"userList":"customers/1234567890/userLists/1"}}]}}]}}]}`)
		},
	})
	in := groupDemographicsInput(base)
	in.DemandGenAdGroups[1].Demographics = nil
	in.DemandGenAdGroups[1].Audience = "customers/1234567890/audiences/42"
	res, err := c.CreateDemandGenCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if search := string(rig.sent("googleAds:search")[0]); !strings.Contains(search, `audience.name = '`+retargetingAudienceName+`'`) {
		t.Errorf("lookup = %s, want the group's exact name", search)
	}
	if len(res.DemandGenAdGroupAudiences) != 1 || res.DemandGenAdGroupAudiences[0].DemographicAudience != "customers/1234567890/audiences/700" ||
		!strings.HasPrefix(res.Steps[0], "Demographic Audience reused: customers/1234567890/audiences/700") {
		t.Errorf("audiences = %+v, steps[0] = %q; want the reused Audience", res.DemandGenAdGroupAudiences, res.Steps[0])
	}
	criteria := rig.sent("adGroupCriteria:mutate")
	if !strings.Contains(string(criteria[1]), `/audiences/700"`) || !strings.Contains(string(criteria[3]), `/audiences/42"`) {
		t.Errorf("criteria = %s / %s, want the reused Audience on group 1 and group 2's own", criteria[1], criteria[3])
	}
}

// A group's Audience create that fails is still before the budget. The first failing with
// nothing in place is (nil, err); once an earlier Audience exists, or when the create may have
// landed, the result is a partial naming every Audience so far.
func TestCreateDemandGenCampaign_GroupAudienceFailures(t *testing.T) {
	refused := func(_ int, _ []byte, w http.ResponseWriter) {
		gaqlError(http.StatusBadRequest, "audienceError", "DIMENSION_INVALID")(w, nil)
	}
	for name, tc := range map[string]struct {
		create   func(int, []byte, http.ResponseWriter)
		wantNil  bool
		wantErr  string
		wantAuds int
	}{
		"first refused": {refused, true, "creation failed (no budget or campaign was created)", 0},
		"second refused": {func(call int, b []byte, w http.ResponseWriter) {
			if call == 0 {
				audiencesNumbered(call, b, w)
				return
			}
			refused(call, b, w)
		}, false, "creation failed (no budget or campaign was created)", 1},
		"second unconfirmed": {func(call int, b []byte, w http.ResponseWriter) {
			if call == 0 {
				audiencesNumbered(call, b, w)
				return
			}
			http.Error(w, "boom", http.StatusInternalServerError)
		}, false, "creation UNCONFIRMED", 2},
		"first unconfirmed": {func(_ int, _ []byte, w http.ResponseWriter) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, false, "creation UNCONFIRMED", 1},
	} {
		t.Run(name, func(t *testing.T) {
			rig, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){"audiences:mutate": tc.create})
			res, err := c.CreateDemandGenCampaign(context.Background(), groupDemographicsInput(base))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			for _, p := range rig.mutatePaths() {
				if p != "audiences:mutate" {
					t.Errorf("sent %s after a failed Audience step", p)
				}
			}
			if tc.wantNil {
				if res != nil {
					t.Errorf("result = %+v, want nil: nothing exists", res)
				}
				return
			}
			if res == nil || len(res.DemandGenAdGroupAudiences) != tc.wantAuds || res.CampaignBudgetID != "" {
				t.Fatalf("result = %+v, want a partial naming %d Audience(s)", res, tc.wantAuds)
			}
			last := res.DemandGenAdGroupAudiences[tc.wantAuds-1]
			if strings.Contains(tc.wantErr, "UNCONFIRMED") {
				if last.DemographicAudience != "" || last.DemographicAudienceName == "" ||
					!strings.Contains(res.Steps[len(res.Steps)-1], "UNCONFIRMED") || !strings.Contains(res.Steps[len(res.Steps)-1], `[ad group "`) ||
					!strings.Contains(res.Steps[len(res.Steps)-1], "no budget or campaign was created") {
					t.Errorf("last = %+v, steps = %q; want the planned name, no resource, an UNCONFIRMED step for the group", last, res.Steps)
				}
			} else if last.DemographicAudience != "customers/1234567890/audiences/901" {
				t.Errorf("audiences = %+v, want the created first Audience named", res.DemandGenAdGroupAudiences)
			}
			if outcome, ok := CreateOutcome(res, err); !ok || outcome == "" {
				t.Errorf("CreateOutcome = %q, %v; want a partial-result outcome", outcome, ok)
			}
		})
	}
}

// After the group Audiences exist, a budget DUPLICATE_NAME whose reuse fails is not
// budget_name_taken: this attempt created those Audiences.
func TestCreateDemandGenCampaign_BudgetNameTakenAfterGroupAudiencesIsNotNothingCreated(t *testing.T) {
	_, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": audiencesNumbered,
		"campaignBudgets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, nil)
		},
		"googleAds:search": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "requestError", "INVALID_INPUT")(w, nil)
		},
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), groupDemographicsInput(base))
	if err == nil || res == nil || len(res.DemandGenAdGroupAudiences) != 2 {
		t.Fatalf("res, err = %+v, %v; want a partial naming both Audiences", res, err)
	}
	if outcome, ok := CreateOutcome(res, err); !ok || outcome == domain.CreateOutcomeBudgetNameTaken {
		t.Errorf("CreateOutcome = %q, %v; must not claim nothing new was created", outcome, ok)
	}
	res.DemandGenAdGroupAudiences = nil
	if outcome, _ := CreateOutcome(res, err); outcome != domain.CreateOutcomeBudgetNameTaken {
		t.Errorf("CreateOutcome without the Audiences = %q, want budget_name_taken", outcome)
	}
}

// A definite budget refusal after the group Audiences exist names them.
func TestCreateDemandGenCampaign_BudgetRefusalAfterGroupAudiencesIsAPartial(t *testing.T) {
	_, c, base := newDGRig(t, twoGroupImages(t), map[string]func(int, []byte, http.ResponseWriter){
		"audiences:mutate": audiencesNumbered,
		"campaignBudgets:mutate": func(_ int, _ []byte, w http.ResponseWriter) {
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "INVALID_AMOUNT")(w, nil)
		},
	})
	res, err := c.CreateDemandGenCampaign(context.Background(), groupDemographicsInput(base))
	if err == nil || !strings.Contains(err.Error(), "demographic Audience customers/1234567890/audiences/901, customers/1234567890/audiences/902 in place") {
		t.Fatalf("err = %v, want the budget failure naming both Audiences", err)
	}
	if res == nil || len(res.DemandGenAdGroupAudiences) != 2 {
		t.Fatalf("result = %+v, want a partial naming both Audiences", res)
	}
}
