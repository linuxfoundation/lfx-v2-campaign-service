// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
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

// leadFormServer answers the account read, the Instant Form read (with answer, or a Graph error
// when answer is empty) and the create POSTs, recording the form reads and every POST body.
type leadFormServer struct {
	*httptest.Server
	mu        sync.Mutex
	formReads []string
	posts     []string
	bodies    map[string]map[string]any
}

func newLeadFormServer(t *testing.T, answer string) *leadFormServer {
	t.Helper()
	ls := &leadFormServer{bodies: map[string]map[string]any{}}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ls.mu.Lock()
		defer ls.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/5550001":
			ls.formReads = append(ls.formReads, r.URL.Query().Get("fields"))
			if answer == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100}}`)
				return
			}
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case r.Method == http.MethodPost:
			suffix := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			ls.posts = append(ls.posts, suffix)
			ls.bodies[suffix] = decodeBody(t, r)
			ids := map[string]string{"campaigns": "120100000000123", "adsets": "120200000000456", "adcreatives": "creative_1", "ads": "ad_1"}
			_, _ = io.WriteString(w, `{"id":"`+ids[suffix]+`"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return ls
}

func (ls *leadFormServer) snapshot() (formReads, posts []string, bodies map[string]map[string]any) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return append([]string(nil), ls.formReads...), append([]string(nil), ls.posts...), ls.bodies
}

const activeFormOfThePage = `{"id":"5550001","page":{"id":"987654321","name":"LF Events"},"status":"ACTIVE"}`

// TestCreateCampaign_LeadFormMakesAnOutcomeLeadsAd: with leads and a form, the form is read once
// before the first write, then the campaign is OUTCOME_LEADS, the ad set a LEAD_GENERATION ad set on
// the Page with destination ON_AD, and the creative's button carries lead_gen_form_id.
func TestCreateCampaign_LeadFormMakesAnOutcomeLeadsAd(t *testing.T) {
	ls := newLeadFormServer(t, activeFormOfThePage)
	defer ls.Close()
	in := metaFieldInput()
	in.Objective, in.LeadFormID, in.CallToAction = "leads", " 5550001 ", "sign_up"
	res, err := biddingClient(ls.URL, 100).CreateCampaign(context.Background(), in)
	if err != nil || res == nil || res.AdCount != 1 {
		t.Fatalf("CreateCampaign = %+v, %v", res, err)
	}
	reads, posts, bodies := ls.snapshot()
	if !reflect.DeepEqual(reads, []string{"id,page,status"}) {
		t.Errorf("form reads = %v, want one read of id,page,status", reads)
	}
	if !reflect.DeepEqual(posts, []string{"campaigns", "adsets", "adcreatives", "ads"}) {
		t.Errorf("posts = %v", posts)
	}
	if got := bodies["campaigns"]["objective"]; got != "OUTCOME_LEADS" {
		t.Errorf("campaign objective = %v, want OUTCOME_LEADS", got)
	}
	adSet := bodies["adsets"]
	if adSet["optimization_goal"] != "LEAD_GENERATION" || adSet["destination_type"] != "ON_AD" || adSet["billing_event"] != "IMPRESSIONS" {
		t.Errorf("ad set = goal %v, destination %v, billing %v; want LEAD_GENERATION, ON_AD, IMPRESSIONS", adSet["optimization_goal"], adSet["destination_type"], adSet["billing_event"])
	}
	if !reflect.DeepEqual(adSet["promoted_object"], map[string]any{"page_id": "987654321"}) {
		t.Errorf("promoted_object = %#v, want the Page", adSet["promoted_object"])
	}
	spec, _ := bodies["adcreatives"]["object_story_spec"].(map[string]any)
	linkData, _ := spec["link_data"].(map[string]any)
	want := map[string]any{"type": "SIGN_UP", "value": map[string]any{"lead_gen_form_id": "5550001"}}
	if !reflect.DeepEqual(linkData["call_to_action"], want) {
		t.Errorf("call_to_action = %#v, want %#v", linkData["call_to_action"], want)
	}
}

// TestCreateCampaign_LeadsWithoutAFormKeepsTheTrafficMapping: leads without a form is today's
// interim website-traffic campaign, and no form is read.
func TestCreateCampaign_LeadsWithoutAFormKeepsTheTrafficMapping(t *testing.T) {
	ls := newLeadFormServer(t, activeFormOfThePage)
	defer ls.Close()
	in := metaFieldInput()
	in.Objective = "leads"
	if _, err := biddingClient(ls.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	reads, _, bodies := ls.snapshot()
	if len(reads) != 0 {
		t.Errorf("form reads = %v, want none", reads)
	}
	adSet := bodies["adsets"]
	if bodies["campaigns"]["objective"] != "OUTCOME_TRAFFIC" || adSet["optimization_goal"] != "LINK_CLICKS" {
		t.Errorf("objective %v, goal %v; want OUTCOME_TRAFFIC, LINK_CLICKS", bodies["campaigns"]["objective"], adSet["optimization_goal"])
	}
	if _, has := adSet["destination_type"]; has {
		t.Errorf("destination_type = %v, want absent", adSet["destination_type"])
	}
	spec, _ := bodies["adcreatives"]["object_story_spec"].(map[string]any)
	linkData, _ := spec["link_data"].(map[string]any)
	cta, _ := linkData["call_to_action"].(map[string]any)
	if v, _ := cta["value"].(map[string]any); v["lead_gen_form_id"] != nil || v["link"] == nil {
		t.Errorf("call_to_action value = %#v, want the click link", cta["value"])
	}
}

// TestCreateCampaign_LeadFormRefusalsSendNothing: a form on another objective, a malformed id, a
// button a form cannot carry and manual bidding are refused before any request.
func TestCreateCampaign_LeadFormRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name, objective, form, cta string
		bid                        float64
		want                       string
	}{
		{"traffic", "traffic", "5550001", "", 0, `metaConfig.leadFormId: applies only to the leads objective (objective is "traffic")`},
		{"default objective", "", "5550001", "", 0, `objective is "traffic"`},
		{"not numeric", "leads", "form-1", "", 0, `"form-1" is not a Meta Instant Form id`},
		{"too long", "leads", strings.Repeat("1", 21), "", 0, "at most 20 digits"},
		{"a link button", "leads", "5550001", "BUY_TICKETS", 0, `metaConfig.callToAction: "BUY_TICKETS" is not a button an Instant Form lead ad can carry`},
		{"manual bid", "leads", "5550001", "", 1, "manual bidding needs an ad set that optimizes for link clicks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777") && tc.bid != 0 {
					_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/5550001" && tc.bid != 0 {
					_, _ = io.WriteString(w, activeFormOfThePage)
					return
				}
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.LeadFormID, in.CallToAction, in.BidCap = tc.objective, tc.form, tc.cta, tc.bid
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want an error containing %q", res, err, tc.want)
			}
			var refusal *domain.PreflightRefusal
			if tc.bid == 0 && (!errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid)) {
				t.Fatalf("err = %v, want a config-invalid pre-flight refusal", err)
			}
		})
	}
}

// TestCreateCampaign_UnusableLeadFormCreatesNothing: a form of another Page or one not ACTIVE is a
// refusal, and a failed or malformed read an error; either way no POST is sent.
func TestCreateCampaign_UnusableLeadFormCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		refusal            bool
	}{
		{"another Page", `{"id":"5550001","page":{"id":"111"},"status":"ACTIVE"}`, `Instant Form "5550001" belongs to a different Page`, true},
		{"archived", `{"id":"5550001","page":{"id":"987654321"},"status":"ARCHIVED"}`, `is not active (status "ARCHIVED")`, true},
		{"no status", `{"id":"5550001","page":{"id":"987654321"}}`, "is not active", true},
		{"no page", `{"id":"5550001","status":"ACTIVE"}`, "did not name the form and its Page", false},
		{"a page with no id", `{"id":"5550001","page":{"name":"LF Events"},"status":"ACTIVE"}`, "did not name the form and its Page", false},
		{"another node", `{"id":"999","page":{"id":"987654321"},"status":"ACTIVE"}`, "did not name the form and its Page", false},
		{"read fails", "", "could not be read, so nothing was created", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ls := newLeadFormServer(t, tc.answer)
			defer ls.Close()
			in := metaFieldInput()
			in.Objective, in.LeadFormID = "leads", "5550001"
			res, err := biddingClient(ls.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) || errors.As(err, &refusal) != tc.refusal {
				t.Fatalf("CreateCampaign = %+v, %v; want an error containing %q (refusal %v)", res, err, tc.want, tc.refusal)
			}
			if _, posts, _ := ls.snapshot(); len(posts) != 0 {
				t.Fatalf("posts = %v, want none", posts)
			}
		})
	}
}
