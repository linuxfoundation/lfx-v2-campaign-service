// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Lead Gen Form campaigns (#664).

const ownedLeadForm = `{"id":6755260984438374400,"name":"CFP","state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}}`

func TestValidateLeadGen(t *testing.T) {
	lg := func(id, cta string) *LeadGen { return &LeadGen{FormID: id, CallToAction: cta} }
	for _, tc := range []struct {
		name      string
		in        *LeadGen
		objective string
		format    string
		delivery  DeliveryOptions
		variants  []CreativeVariant
		want      *LeadGen
		refused   string
	}{
		{"not lead gen", nil, ObjectiveWebsiteVisit, "", DeliveryOptions{}, nil, nil, ""},
		{"bare id, default label", lg(" 42 ", ""), ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, lg("42", "REGISTER"), ""},
		{"adForm URN, own label", lg("urn:li:adForm:42", "APPLY"), ObjectiveLeadGeneration, FormatSingleImage, DeliveryOptions{AudienceExpansionEnabled: boolp(true), OffsiteDeliveryEnabled: boolp(false)}, nil, lg("42", "APPLY"), ""},
		{"lead gen without a form", nil, ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, nil, "requires a Lead Gen Form"},
		{"form without lead gen", lg("42", ""), ObjectiveWebsiteVisit, "", DeliveryOptions{}, nil, nil, "used only with objective LEAD_GENERATION"},
		{"not a number", lg("abc", ""), ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, nil, "must be a Lead Gen Form id"},
		{"leading zero", lg("042", ""), ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, nil, "must be a Lead Gen Form id"},
		{"another URN type", lg("urn:li:leadGenForm:42", ""), ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, nil, "must be a Lead Gen Form id"},
		{"unknown label", lg("42", "register"), ObjectiveLeadGeneration, "", DeliveryOptions{}, nil, nil, "leadGenCallToAction must be one of"},
		{"text format", lg("42", ""), ObjectiveLeadGeneration, FormatText, DeliveryOptions{}, nil, nil, "Sponsored Content"},
		{"audience network", lg("42", ""), ObjectiveLeadGeneration, "", DeliveryOptions{OffsiteDeliveryEnabled: boolp(true)}, nil, nil, "offsiteDeliveryEnabled true is not available"},
		{"variant call to action", lg("42", ""), ObjectiveLeadGeneration, "", DeliveryOptions{}, []CreativeVariant{{}, {CallToAction: "REGISTER"}}, nil, "variants[1].callToAction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateLeadGen(tc.in, tc.objective, tc.format, tc.delivery, tc.variants)
			if tc.refused != "" {
				if !errors.Is(err, ErrLeadGenInvalid) || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("ValidateLeadGen = %v, want ErrLeadGenInvalid containing %q", err, tc.refused)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ValidateLeadGen = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

// TestCampaignBidding_LeadGeneration: LEAD_GENERATION is MAX_LEAD on CPM with automatic bidding,
// and a manual bid is refused.
func TestCampaignBidding_LeadGeneration(t *testing.T) {
	plan, err := campaignBidding(0, ObjectiveLeadGeneration)
	if err != nil || plan.objectiveType != ObjectiveLeadGeneration || plan.optimizationTarget != OptimizationTargetMaxLead ||
		plan.nameObjective != "Leads" || !reflect.DeepEqual(plan.fields, map[string]any{"costType": "CPM", "optimizationTargetType": "MAX_LEAD"}) {
		t.Errorf("automatic = %+v, %v; want LEAD_GENERATION / MAX_LEAD / CPM / Leads", plan, err)
	}
	if _, err := campaignBidding(4.5, ObjectiveLeadGeneration); err == nil || !strings.Contains(err.Error(), "automatic bidding") {
		t.Errorf("manual = %v, want the automatic-only refusal", err)
	}
}

// leadGenServer serves a single-image create, answering the form read with form, and records
// the campaign, post and creative bodies and every mutating request.
type leadGenServer struct {
	mu                 sync.Mutex
	formStatus         int
	form               string
	campaign           map[string]any
	posts, creatives   []map[string]any
	mutating, formRead int
}

func (s *leadGenServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if strings.HasPrefix(r.URL.Path, "/leadForms/") {
				s.formRead++
				if s.formStatus != 0 {
					w.WriteHeader(s.formStatus)
				}
				_, _ = io.WriteString(w, s.form)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		s.mutating++
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			s.campaign = body
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			s.posts = append(s.posts, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			s.creatives = append(s.creatives, body)
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
}

func leadGenInput() CampaignInput {
	in := biddingCreateInput(0)
	in.Objective = ObjectiveLeadGeneration
	in.LeadGen = &LeadGen{FormID: "6755260984438374400", CallToAction: "APPLY"}
	return in
}

// TestCreateCampaign_LeadGenWireBodies pins a lead gen create end to end: the form is read first,
// the campaign is LEAD_GENERATION / MAX_LEAD with the Audience Network off, the post carries no
// post-level call to action, and the creative opens the form.
func TestCreateCampaign_LeadGenWireBodies(t *testing.T) {
	s := &leadGenServer{form: ownedLeadForm}
	res, err := s.start(t).CreateCampaign(context.Background(), leadGenInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.formRead != 1 {
		t.Errorf("form read %d times, want once", s.formRead)
	}
	c := s.campaign
	if c["objectiveType"] != "LEAD_GENERATION" || c["optimizationTargetType"] != "MAX_LEAD" || c["costType"] != "CPM" || c["offsiteDeliveryEnabled"] != false {
		t.Errorf("campaign body = %v, want LEAD_GENERATION / MAX_LEAD / CPM, offsiteDeliveryEnabled false", c)
	}
	if res.ObjectiveType != ObjectiveLeadGeneration || !strings.Contains(res.CampaignName, "| Leads |") {
		t.Errorf("result objective %q, name %q; want LEAD_GENERATION and the Leads segment", res.ObjectiveType, res.CampaignName)
	}
	if len(s.posts) != 1 || s.posts[0]["contentCallToActionLabel"] != nil {
		t.Errorf("posts = %v, want one with no contentCallToActionLabel", s.posts)
	}
	want := map[string]any{"destination": "urn:li:adForm:6755260984438374400", "label": "APPLY"}
	if len(s.creatives) != 1 || !reflect.DeepEqual(s.creatives[0]["leadgenCallToAction"], want) || s.creatives[0]["intendedStatus"] != "DRAFT" {
		t.Errorf("creatives = %v, want one DRAFT creative with leadgenCallToAction %v", s.creatives, want)
	}
}

// TestCreateCampaign_LeadFormCheckSendsNothing: a form that is not the account's, an archived
// one or one LinkedIn does not find is a pre-flight refusal; a failed read is a plain error.
// Either way nothing is created.
func TestCreateCampaign_LeadFormCheckSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name, form string
		status     int
		refusal    string // "" = not a pre-flight refusal
	}{
		{"another account's form", strings.Replace(ownedLeadForm, "123456789", "999", 1), 0, "does not belong to ad account 123456789"},
		{"an organic form", `{"id":1,"state":"PUBLISHED","owner":{"organization":"urn:li:organization:1"}}`, 0, "does not belong"},
		{"archived", strings.Replace(ownedLeadForm, "PUBLISHED", "ARCHIVED", 1), 0, "is archived"},
		{"not found", `{}`, http.StatusNotFound, "was not found"},
		{"server error", `{}`, http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &leadGenServer{form: tc.form, formStatus: tc.status}
			res, err := s.start(t).CreateCampaign(context.Background(), leadGenInput())
			msg, pre := domain.AsPreflightRefusal(err)
			if res != nil || err == nil || pre != (tc.refusal != "") || !strings.Contains(msg, tc.refusal) {
				t.Fatalf("CreateCampaign = %+v, %v; want nil and refusal %q", res, err, tc.refusal)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.mutating != 0 {
				t.Errorf("%d mutating requests for a refused create", s.mutating)
			}
		})
	}
}

// TestListLeadForms pins the owner finder: the pre-encoded owner, archived forms left out, the
// sort, and a form of another owner failing the read.
func TestListLeadForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elements string
		want     []LeadForm
		fails    bool
	}{
		{"owned", `{"id":3,"name":"b","state":"DRAFT","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}},
			{"id":2,"name":"A","state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}},
			{"id":1,"name":"old","state":"ARCHIVED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}}`,
			[]LeadForm{{ID: "2", Name: "A", State: "PUBLISHED"}, {ID: "3", Name: "b", State: "DRAFT"}}, false},
		{"another owner", `{"id":2,"name":"A","state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:9"}}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var query string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[`+tc.elements+`],"metadata":{}}`)
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			got, err := c.ListLeadForms(context.Background())
			if tc.fails {
				if err == nil {
					t.Fatalf("ListLeadForms = %v, want an error", got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ListLeadForms = %+v, %v; want %+v", got, err, tc.want)
			}
			if !strings.Contains(query, "owner=(sponsoredAccount:urn%3Ali%3AsponsoredAccount%3A123456789)") || !strings.Contains(query, "q=owner") {
				t.Errorf("query = %q, want q=owner and the pre-encoded owner", query)
			}
		})
	}
}
