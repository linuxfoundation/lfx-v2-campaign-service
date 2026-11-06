// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// #497: observation-only demographics on Search ad groups. Built from the v23 field names
// (AdGroup.targeting_setting.target_restrictions[].bid_only, AdGroupCriterion.age_range /
// gender / income_range); no validateOnly probe has covered the positive criteria yet.

// observedServer answers the Search cascade and records the adGroups and adGroupCriteria
// bodies in order. Ad groups are numbered 330, 331, ….
type observedServer struct {
	mu       sync.Mutex
	paths    []string
	groups   []string
	criteria []string
	seq      int
}

func (s *observedServer) client(t *testing.T) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.paths = append(s.paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			// perGroupResults, not createOps: createOps calls t.Fatalf, which must not run on
			// the handler goroutine.
			_, _ = io.WriteString(w, perGroupResults(body, "campaignCriteria", "222", 700))
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			s.groups = append(s.groups, string(body))
			id := strconv.Itoa(330 + s.seq)
			s.seq++
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/`+id+`"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, perGroupResults(body, "adGroupAds", strconv.Itoa(330+s.seq-1), 440))
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			s.criteria = append(s.criteria, string(body))
			_, _ = io.WriteString(w, criteriaEcho(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(api.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond))
}

// The exact ad group targetingSetting and criteria operations for an observed age range and
// income range, with one age range and one income range excluded: the excluded values get no
// positive criterion, so no value is both targeted and excluded.
func TestCreateCampaign_ObservedDemographicsExactPayload(t *testing.T) {
	s := &observedServer{}
	in := sampleInput()
	in.ObservedDemographics = []string{"age_range", "INCOME_RANGE", "AGE_RANGE"}
	in.ExcludedAgeRanges = []string{"18-24"}
	in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50"}
	res, err := s.client(t).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(s.groups) != 1 {
		t.Fatalf("got %d ad group creates, want 1", len(s.groups))
	}
	wantSetting := `"targetingSetting":{"targetRestrictions":[{"targetingDimension":"AGE_RANGE","bidOnly":true},{"targetingDimension":"INCOME_RANGE","bidOnly":true}]}`
	if !strings.Contains(s.groups[0], wantSetting) {
		t.Errorf("ad group create = %s\nwant it to carry %s", s.groups[0], wantSetting)
	}
	if len(s.criteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(s.criteria))
	}
	g := `"adGroup":"customers/1234567890/adGroups/330"`
	pos := func(field, v string) string {
		return `{"create":{` + g + `,"status":"ENABLED","` + field + `":{"type":"` + v + `"}}}`
	}
	ops := []string{`{"create":{` + g + `,"negative":true,"incomeRange":{"type":"INCOME_RANGE_0_50"}}}`}
	for _, a := range []string{"AGE_RANGE_25_34", "AGE_RANGE_35_44", "AGE_RANGE_45_54", "AGE_RANGE_55_64", "AGE_RANGE_65_UP", "AGE_RANGE_UNDETERMINED"} {
		ops = append(ops, pos("ageRange", a))
	}
	for _, r := range []string{"INCOME_RANGE_50_60", "INCOME_RANGE_60_70", "INCOME_RANGE_70_80", "INCOME_RANGE_80_90", "INCOME_RANGE_90_UP", "INCOME_RANGE_UNDETERMINED"} {
		ops = append(ops, pos("incomeRange", r))
	}
	want := `{"operations":[` + strings.Join(ops, ",") + `]}`
	if s.criteria[0] != want {
		t.Errorf("criteria mutate =\n%s\nwant\n%s", s.criteria[0], want)
	}
	if strings.Join(res.AdGroupDemographicCriteriaIDs, ",") != "900" {
		t.Errorf("exclusion ids = %v, want [900]", res.AdGroupDemographicCriteriaIDs)
	}
	if got := len(res.AdGroupObservedDemographicCriteriaIDs); got != 12 || res.AdGroupObservedDemographicCriteriaIDs[0] != "901" {
		t.Errorf("observed ids = %v, want the 12 after the exclusion", res.AdGroupObservedDemographicCriteriaIDs)
	}
	if strings.Join(res.AdGroups[0].ObservedDemographicCriteriaIDs, ",") != strings.Join(res.AdGroupObservedDemographicCriteriaIDs, ",") {
		t.Errorf("per-group observed ids = %v, want the scalar's", res.AdGroups[0].ObservedDemographicCriteriaIDs)
	}
	if !containsStep(res.Steps, "12 observed demographic(s)") {
		t.Errorf("steps = %v, want the observed criteria counted", res.Steps)
	}
}

// With audience segments too, AUDIENCE stays first and bidOnly, and the observed dimension
// follows it; every Search ad group gets both.
func TestCreateCampaign_ObservedDemographicsBesideAudiencesOnEveryGroup(t *testing.T) {
	s := &observedServer{}
	in := sampleInput()
	in.ObservedDemographics = []string{"GENDER"}
	in.ExcludedGenders = []string{"UNDETERMINED"}
	in.AudienceSegments = []string{"customers/1234567890/userLists/77"}
	in.AdGroups = []AdGroupSpec{{Name: "Training"}, {Name: "Conference"}}
	if _, err := s.client(t).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	wantSetting := `"targetingSetting":{"targetRestrictions":[{"targetingDimension":"AUDIENCE","bidOnly":true},{"targetingDimension":"GENDER","bidOnly":true}]}`
	if len(s.groups) != 2 || len(s.criteria) != 2 {
		t.Fatalf("got %d groups / %d criteria mutates, want 2 / 2", len(s.groups), len(s.criteria))
	}
	for i := range s.groups {
		if !strings.Contains(s.groups[i], wantSetting) {
			t.Errorf("group %d create = %s, want %s", i, s.groups[i], wantSetting)
		}
		body := s.criteria[i]
		if !strings.Contains(body, `"gender":{"type":"MALE"}`) || !strings.Contains(body, `"gender":{"type":"FEMALE"}`) || strings.Contains(body, `"gender":{"type":"UNDETERMINED"}`) {
			t.Errorf("group %d criteria = %s, want MALE and FEMALE observed and the excluded UNDETERMINED absent", i, body)
		}
	}
}

// No observed dimension and no audience: the ad group create carries no targetingSetting,
// exactly as before #497.
func TestCreateCampaign_NoObservedDemographicsSendsNoTargetingSetting(t *testing.T) {
	s := &observedServer{}
	if _, err := s.client(t).CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(s.groups) != 1 || strings.Contains(s.groups[0], "targetingSetting") {
		t.Errorf("ad group create = %v, want no targetingSetting", s.groups)
	}
	if len(s.criteria) != 0 {
		t.Errorf("criteria mutates = %v, want none", s.criteria)
	}
}

func TestValidateCampaignInputKind_ObservedDemographicsRefusals(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		edit    func(*CampaignInput)
		wantErr string
	}{
		{"search accepts", CampaignKindSearch, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE_RANGE", "GENDER", "INCOME_RANGE"} }, ""},
		{"parental on search", CampaignKindSearch, func(in *CampaignInput) { in.ObservedDemographics = []string{"PARENTAL_STATUS"} }, "OPERATION_NOT_PERMITTED_FOR_CONTEXT"},
		{"unknown value", CampaignKindSearch, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE"} }, `observed demographic "AGE" must be one of`},
		{"non-ASCII fold", CampaignKindSearch, func(in *CampaignInput) { in.ObservedDemographics = []string{"genderı"} }, "must be one of"},
		{"every age excluded", CampaignKindSearch, func(in *CampaignInput) {
			in.ObservedDemographics = []string{"AGE_RANGE"}
			in.ExcludedAgeRanges = []string{"18-24", "25-34", "35-44", "45-54", "55-64", "65+", "UNDETERMINED"}
		}, "AGE_RANGE has every value excluded by ExcludedAgeRanges"},
		{"every gender excluded", CampaignKindSearch, func(in *CampaignInput) {
			in.ObservedDemographics = []string{"GENDER"}
			in.ExcludedGenders = []string{"MALE", "FEMALE", "UNDETERMINED"}
		}, "GENDER has every value excluded by ExcludedGenders"},
		{"every income excluded", CampaignKindSearch, func(in *CampaignInput) {
			in.ObservedDemographics = []string{"INCOME_RANGE"}
			in.ExcludedIncomeRanges = []string{"INCOME_RANGE_0_50", "INCOME_RANGE_50_60", "INCOME_RANGE_60_70", "INCOME_RANGE_70_80", "INCOME_RANGE_80_90", "INCOME_RANGE_90_UP", "UNDETERMINED"}
		}, "INCOME_RANGE has every value excluded by ExcludedIncomeRanges"},
		{"display", CampaignKindDisplay, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE_RANGE"} }, "observed demographics are not supported on " + CampaignKindDisplay},
		{"demand gen", CampaignKindDemandGen, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE_RANGE"} }, "observed demographics are not supported on " + CampaignKindDemandGen},
		{"video", CampaignKindVideo, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE_RANGE"} }, "observed demographics are not supported on " + CampaignKindVideo},
		{"performance max", CampaignKindPerformanceMax, func(in *CampaignInput) { in.ObservedDemographics = []string{"AGE_RANGE"} }, "observed demographics are not supported on " + CampaignKindPerformanceMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := sampleInput()
			if tc.kind != CampaignKindSearch {
				in = demandGenInput()
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
