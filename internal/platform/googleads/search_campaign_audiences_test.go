// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// #513: Search user lists observed at the campaign level.

// campaignAudienceServer answers the Search cascade, records the campaign create and every
// campaignCriteria:mutate body, and answers the user-list criteria mutate with critStatus
// (200 echoes one criterion per operation).
type campaignAudienceServer struct {
	mu         sync.Mutex
	campaigns  []string
	criteria   []string
	groups     []string
	critStatus int
	// critCampaign, when set, is the campaign id the user-list criteria answer names.
	critCampaign string
}

func (s *campaignAudienceServer) client(t *testing.T) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			okBudget(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			s.campaigns = append(s.campaigns, string(body))
			okCampaign(w, r)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			s.criteria = append(s.criteria, string(body))
			if strings.Contains(string(body), `"userList"`) && s.critStatus != 0 && s.critStatus != http.StatusOK {
				w.WriteHeader(s.critStatus)
				_, _ = io.WriteString(w, `{"error":{"code":3,"status":"INVALID_ARGUMENT","details":[`+
					`{"@type":"type.googleapis.com/google.ads.googleads.v23.errors.GoogleAdsFailure",`+
					`"errors":[{"errorCode":{"criterionError":"INVALID_USER_LIST"},"message":"boom"}]}]}}`)
				return
			}
			campaign := "222"
			if s.critCampaign != "" && strings.Contains(string(body), `"userList"`) {
				campaign = s.critCampaign
			}
			_, _ = io.WriteString(w, perGroupResults(body, "campaignCriteria", campaign, 700))
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			s.groups = append(s.groups, string(body))
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/330"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, perGroupResults(body, "adGroupAds", "330", 440))
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			_, _ = io.WriteString(w, criteriaEcho(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	return noExistingCampaign(NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(api.URL), WithClock(fixedClock()),
		withRetryBaseDelay(time.Millisecond)))
}

const (
	testUserList1 = "customers/1234567890/userLists/7474872400"
	testUserList2 = "customers/1234567890/userLists/6921247814"
)

// The campaign create carries AUDIENCE bidOnly, the lists are positive ENABLED campaign
// criteria (deduped, trimmed) in their own mutate, and the ids and a step are reported. The ad
// group is untouched: no AUDIENCE restriction there, since it observes nothing itself.
func TestCreateCampaign_CampaignAudienceSegmentsExactPayload(t *testing.T) {
	s := &campaignAudienceServer{}
	in := sampleInput()
	in.CampaignAudienceSegments = []string{testUserList1, " " + testUserList2 + " ", testUserList1}
	res, err := s.client(t).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(s.campaigns) != 1 {
		t.Fatalf("got %d campaign creates, want 1", len(s.campaigns))
	}
	wantSetting := `"targetingSetting":{"targetRestrictions":[{"targetingDimension":"AUDIENCE","bidOnly":true}]}`
	if !strings.Contains(s.campaigns[0], wantSetting) {
		t.Errorf("campaign create = %s\nwant it to carry %s", s.campaigns[0], wantSetting)
	}
	if len(s.criteria) != 1 {
		t.Fatalf("got %d campaignCriteria mutates, want 1: %v", len(s.criteria), s.criteria)
	}
	op := func(ul string) string {
		return `{"create":{"campaign":"customers/1234567890/campaigns/222","status":"ENABLED","userList":{"userList":"` + ul + `"}}}`
	}
	want := `{"operations":[` + op(testUserList1) + `,` + op(testUserList2) + `]}`
	if s.criteria[0] != want {
		t.Errorf("criteria mutate =\n%s\nwant\n%s", s.criteria[0], want)
	}
	if got := strings.Join(res.CampaignAudienceCriteriaIDs, ","); got != "700,701" {
		t.Errorf("CampaignAudienceCriteriaIDs = %q, want 700,701", got)
	}
	if !containsStep(res.Steps, "Campaign audiences observed: 2 user list(s)") {
		t.Errorf("steps = %v, want the observed lists counted", res.Steps)
	}
	if len(s.groups) != 1 || strings.Contains(s.groups[0], "targetingSetting") {
		t.Errorf("ad group create = %v, want one with no targetingSetting", s.groups)
	}
}

// Without the field, the campaign create sends no targetingSetting at all: every existing
// Search create keeps its payload.
func TestCreateCampaign_NoCampaignAudienceSegmentsSendsNoCampaignTargetingSetting(t *testing.T) {
	s := &campaignAudienceServer{}
	if _, err := s.client(t).CreateCampaign(context.Background(), sampleInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if len(s.campaigns) != 1 || strings.Contains(s.campaigns[0], "targetingSetting") {
		t.Errorf("campaign create = %v, want no targetingSetting", s.campaigns)
	}
	if len(s.criteria) != 0 {
		t.Errorf("campaignCriteria mutates = %v, want none", s.criteria)
	}
}

// A definite refusal of the criteria mutate is a partial result: the campaign is recorded,
// no ad group is built on top, and the error says the lists are missing. An ambiguous one
// is UNCONFIRMED.
func TestCreateCampaign_CampaignAudienceSegmentsFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "campaign audience observation failed (campaign 222 created; it observes NONE of its 1 user list(s)"},
		{http.StatusInternalServerError, "campaign audience observation UNCONFIRMED (campaign 222"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			s := &campaignAudienceServer{critStatus: tc.status}
			in := sampleInput()
			in.CampaignAudienceSegments = []string{testUserList1}
			res, err := s.client(t).CreateCampaign(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if res == nil || res.CampaignID != "222" {
				t.Fatalf("res = %+v, want the campaign recorded", res)
			}
			if len(res.CampaignAudienceCriteriaIDs) != 0 {
				t.Errorf("ids = %v, want none recorded", res.CampaignAudienceCriteriaIDs)
			}
			if len(s.groups) != 0 {
				t.Errorf("ad groups = %v, want none built after the failure", s.groups)
			}
		})
	}
}

func TestValidateCampaignInputKind_CampaignAudienceSegmentsRefusals(t *testing.T) {
	many := make([]string, maxAudienceSegments+1)
	for i := range many {
		many[i] = fmt.Sprintf("customers/1234567890/userLists/%d", 100+i)
	}
	cases := []struct {
		name    string
		kind    string
		edit    func(*CampaignInput)
		wantErr string
	}{
		{"search accepts", CampaignKindSearch, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{testUserList1, testUserList2} }, ""},
		{"search accepts exactly the cap", CampaignKindSearch, func(in *CampaignInput) { in.CampaignAudienceSegments = many[:maxAudienceSegments] }, ""},
		{"over the cap", CampaignKindSearch, func(in *CampaignInput) { in.CampaignAudienceSegments = many }, "at most 20 distinct CampaignAudienceSegments entries"},
		// The cap counts distinct lists: 21 entries that dedupe to 20 are accepted.
		{"duplicates within the cap", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = append(append([]string{}, many[:maxAudienceSegments]...), many[0])
		}, ""},
		{"raw input far over the cap", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = make([]string, 4*maxAudienceSegments+1)
		}, "at most 80 CampaignAudienceSegments entries (20 distinct) are supported, got 81"},
		{"empty entry", CampaignKindSearch, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{" "} }, "CampaignAudienceSegments entry must not be empty"},
		{"malformed", CampaignKindSearch, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{"userLists/1"} }, "is not a customers/{customerId}/userLists/{id} user list resource name"},
		{"custom audience", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = []string{"customers/1234567890/customAudiences/776852670"}
		}, "is not a user list; a Search campaign observes user lists only"},
		{"another customer", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = []string{"customers/999/userLists/1"}
		}, `is on customer "999", not this campaign's account "1234567890"`},
		{"non-canonical id", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = []string{"customers/1234567890/userLists/011"}
		}, "not the canonical base-10 spelling"},
		{"beside audienceSegments", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = []string{testUserList1}
			in.AudienceSegments = []string{testUserList2}
		}, "CampaignAudienceSegments and AudienceSegments cannot both be set"},
		{"beside an ad group's audienceSegments", CampaignKindSearch, func(in *CampaignInput) {
			in.CampaignAudienceSegments = []string{testUserList1}
			in.AdGroups = []AdGroupSpec{{Name: "A"}, {Name: "B", AudienceSegments: []string{testUserList2}}}
		}, "CampaignAudienceSegments and AdGroups entry 2 AudienceSegments cannot both be set"},
		{"display", CampaignKindDisplay, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{testUserList1} }, "campaign-level audience segments are not supported on " + CampaignKindDisplay},
		{"demand gen", CampaignKindDemandGen, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{testUserList1} }, "campaign-level audience segments are not supported on " + CampaignKindDemandGen},
		{"video", CampaignKindVideo, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{testUserList1} }, "campaign-level audience segments are not supported on " + CampaignKindVideo},
		{"performance max", CampaignKindPerformanceMax, func(in *CampaignInput) { in.CampaignAudienceSegments = []string{testUserList1} }, "campaign-level audience segments are not supported on " + CampaignKindPerformanceMax},
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

// A 2xx naming another campaign's criterion is not proof the list is observed here: the result
// is UNCONFIRMED and no id is recorded.
func TestCreateCampaign_CampaignAudienceSegmentsWrongCampaignResultIsUnconfirmed(t *testing.T) {
	s := &campaignAudienceServer{critCampaign: "999"}
	in := sampleInput()
	in.CampaignAudienceSegments = []string{testUserList1}
	res, err := s.client(t).CreateCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), `reports a different campaign id "999"`) {
		t.Fatalf("err = %v, want the wrong-campaign UNCONFIRMED", err)
	}
	if res == nil || len(res.CampaignAudienceCriteriaIDs) != 0 {
		t.Errorf("res = %+v, want the campaign with no list ids recorded", res)
	}
}
