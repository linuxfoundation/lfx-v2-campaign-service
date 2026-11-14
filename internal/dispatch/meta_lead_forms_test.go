// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestMeta_PreflightCreate_LeadForm: off, metaConfig.leadFormId is a 400 naming
// META_LEAD_FORMS_ENABLED; on, the leads-only rule, the id shape, the form's buttons and manual
// bidding are 400s, and a wrong type is refused on its type rather than read as absent.
func TestMeta_PreflightCreate_LeadForm(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		t.Setenv(constants.EnvMetaLeadFormsEnabled, "TRUE")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "absent", config: `{"metaConfig":{"objective":"leads"}}`},
			{name: "blank", config: `{"metaConfig":{"objective":"leads","leadFormId":"  "}}`},
			{name: "a form", config: `{"metaConfig":{"objective":"leads","leadFormId":"5550001"}}`,
				want: "metaConfig.leadFormId is disabled on this deployment (META_LEAD_FORMS_ENABLED is not \"true\")"},
			{name: "a numeric form", config: `{"metaConfig":{"objective":"leads","LeadFormId":5550001}}`, want: `"LeadFormId" must be a string`},
		})
	})
	t.Run("on", func(t *testing.T) {
		t.Setenv(constants.EnvMetaLeadFormsEnabled, "true")
		t.Setenv(constants.EnvMetaCallToActionEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "leads", config: `{"metaConfig":{"objective":"Leads","leadFormId":"5550001","callToAction":"sign_up"}}`},
			{name: "traffic", config: `{"metaConfig":{"leadFormId":"5550001"}}`,
				want: `invalid campaign config: metaConfig.leadFormId: applies only to the leads objective (objective is "traffic")`},
			{name: "not an id", config: `{"metaConfig":{"objective":"leads","leadFormId":"https://fb.me/form"}}`,
				want: `is not a Meta Instant Form id`},
			{name: "a link button", config: `{"metaConfig":{"objective":"leads","leadFormId":"5550001","callToAction":"BUY_TICKETS"}}`,
				want: `metaConfig.callToAction: "BUY_TICKETS" is not a button an Instant Form lead ad can carry`},
			{name: "manual bidding", config: `{"metaConfig":{"objective":"leads","leadFormId":"5550001","bidding":{"strategy":"manual","bidAmount":1}}}`,
				want: "invalid campaign config: metaConfig.bidding: manual bidding is a cost-per-link-click bid cap, which an Instant Form lead ad (leadFormId) cannot carry"},
			{name: "manual bidding without a form", config: `{"metaConfig":{"objective":"leads","bidding":{"strategy":"manual","bidAmount":1}}}`},
		})
	})
}

// TestMeta_DispatchCarriesTheLeadForm: with the flag on, Dispatch reads the form and creates an
// OUTCOME_LEADS campaign whose creative carries it; off, Dispatch refuses before any request.
func TestMeta_DispatchCarriesTheLeadForm(t *testing.T) {
	t.Setenv(constants.EnvMetaLeadFormsEnabled, "true")
	var (
		mu       sync.Mutex
		requests []string
		campaign map[string]any
		creative map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		decode := func(into *map[string]any) {
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, into); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/5550001":
			_, _ = io.WriteString(w, `{"id":"5550001","page":{"id":"987654321"},"status":"ACTIVE"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			decode(&campaign)
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			decode(&creative)
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],
		"objective":"leads","leadFormId":"5550001","variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	gotObjective := campaign["objective"]
	spec, _ := creative["object_story_spec"].(map[string]any)
	sawForm := false
	for _, r := range requests {
		sawForm = sawForm || r == "GET /5550001"
	}
	mu.Unlock()
	linkData, _ := spec["link_data"].(map[string]any)
	cta, _ := linkData["call_to_action"].(map[string]any)
	value, _ := cta["value"].(map[string]any)
	if gotObjective != "OUTCOME_LEADS" || value["lead_gen_form_id"] != "5550001" || !sawForm {
		t.Fatalf("objective %v, cta value %#v, form read %v; want OUTCOME_LEADS with the form, read first", gotObjective, value, sawForm)
	}

	t.Setenv(constants.EnvMetaLeadFormsEnabled, "false")
	mu.Lock()
	requests = nil
	mu.Unlock()
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !strings.Contains(err.Error(), constants.EnvMetaLeadFormsEnabled) {
		t.Fatalf("Dispatch with the flag off = %+v, %v; want a NoUpstreamCreate refusal naming the flag", camp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 0 {
		t.Fatalf("requests with the flag off = %v, want none", requests)
	}
}
