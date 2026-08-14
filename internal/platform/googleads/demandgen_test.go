// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// demandGenInput is the narrowest input that reaches the mutates: Demand Gen needs no
// headlines, descriptions or keywords, which is the whole point of the separate path.
func demandGenInput() CampaignInput {
	return CampaignInput{
		EventName:       "KubeCon Europe 2026",
		EventSlug:       "kubecon-eu-2026",
		Project:         "tlf",
		RegistrationURL: "https://events.linuxfoundation.org/kubecon-eu-2026/",
		Budget:          500,
	}
}

// captureServer records every mutate path and its decoded body, answering each with a
// plausible resourceName so the cascade proceeds to the next step.
func captureServer(t *testing.T, bodies *[]map[string]any, paths *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		// A decode failure is recorded as nil rather than failing here: t.Fatalf inside a
		// handler goroutine only kills that goroutine and leaves the exchange half-done.
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		*paths = append(*paths, r.URL.Path)
		*bodies = append(*bodies, decoded)
		mu.Unlock()

		var resource string
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			resource = "customers/1234567890/campaignBudgets/111"
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			resource = "customers/1234567890/campaigns/222"
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			resource = "customers/1234567890/adGroups/333"
		default:
			resource = "customers/1234567890/unknown/999"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"resourceName":"` + resource + `"}]}`))
	}))
}

func newDemandGenClient(t *testing.T, apiURL string) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	return NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiURL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
}

// The contract that distinguishes this path from Search. A Demand Gen campaign must
// carry advertisingChannelType DEMAND_GEN and targetSpend, and must NOT carry the
// Search-only networkSettings/manualCpc — Google rejects the Search shape on this
// channel, and a swapped payload is the failure this port most plausibly ships.
func TestCreateDemandGenCampaignSendsTheDemandGenShape(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := captureServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	res, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), demandGenInput())
	if err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}
	if res.CampaignID != "222" {
		t.Errorf("CampaignID = %q, want %q", res.CampaignID, "222")
	}

	mu.Lock()
	defer mu.Unlock()
	var campaign map[string]any
	for i, p := range paths {
		if strings.HasSuffix(p, "campaigns:mutate") {
			ops, _ := bodies[i]["operations"].([]any)
			if len(ops) == 0 {
				t.Fatal("campaigns:mutate carried no operations")
			}
			op, _ := ops[0].(map[string]any)
			campaign, _ = op["create"].(map[string]any)
		}
	}
	if campaign == nil {
		t.Fatalf("no campaigns:mutate seen, paths = %v", paths)
	}
	if got := campaign["advertisingChannelType"]; got != "DEMAND_GEN" {
		t.Errorf("advertisingChannelType = %v, want DEMAND_GEN", got)
	}
	if got := campaign["status"]; got != "PAUSED" {
		t.Errorf("status = %v, want PAUSED — a created campaign must never serve until a human enables it", got)
	}
	if _, ok := campaign["targetSpend"]; !ok {
		t.Error("targetSpend missing — Demand Gen bids with targetSpend, not manual CPC")
	}
	// The Search-only fields, asserted ABSENT. Without these the test would pass on a
	// payload that merely added DEMAND_GEN to the Search shape, which the API rejects.
	if _, ok := campaign["networkSettings"]; ok {
		t.Error("networkSettings must NOT be sent for Demand Gen — there is no Search network to target")
	}
	if _, ok := campaign["manualCpc"]; ok {
		t.Error("manualCpc must NOT be sent for Demand Gen")
	}
}

// Demand Gen ads are image/video assets uploaded by a human in the Google Ads UI, so
// this path stops after the ad group. Creating a text ad here would produce an ad the
// channel cannot serve. Asserted as the ABSENCE of those mutates, and paired with the
// positive assertion that the three expected ones did happen — an absence alone would
// pass if the whole cascade had failed early.
func TestCreateDemandGenCampaignCreatesNoAdOrKeywords(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := captureServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	if _, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), demandGenInput()); err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()

	for _, want := range []string{"campaignBudgets:mutate", "campaigns:mutate", "adGroups:mutate"} {
		found := false
		for _, p := range got {
			if strings.HasSuffix(p, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a %s, got %v", want, got)
		}
	}
	for _, forbidden := range []string{"adGroupAds:mutate", "adGroupCriteria:mutate"} {
		for _, p := range got {
			if strings.HasSuffix(p, forbidden) {
				t.Errorf("Demand Gen must not call %s (no ads, no keywords), got %v", forbidden, got)
			}
		}
	}
}

// The ad group must NOT carry the Search path's SEARCH_STANDARD type — naming a Search
// ad-group type under a Demand Gen campaign is rejected.
func TestCreateDemandGenAdGroupOmitsTheSearchType(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var bodies []map[string]any
	srv := captureServer(t, &bodies, &paths, &mu)
	t.Cleanup(srv.Close)

	if _, err := newDemandGenClient(t, srv.URL).CreateDemandGenCampaign(context.Background(), demandGenInput()); err != nil {
		t.Fatalf("CreateDemandGenCampaign: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, p := range paths {
		if !strings.HasSuffix(p, "adGroups:mutate") {
			continue
		}
		ops, _ := bodies[i]["operations"].([]any)
		if len(ops) == 0 {
			t.Fatal("adGroups:mutate carried no operations")
		}
		op, _ := ops[0].(map[string]any)
		create, _ := op["create"].(map[string]any)
		if _, ok := create["type"]; ok {
			t.Errorf("Demand Gen ad group must not send a type, got %v", create["type"])
		}
		if got := create["status"]; got != "ENABLED" {
			t.Errorf("ad group status = %v, want ENABLED", got)
		}
		return
	}
	t.Fatalf("no adGroups:mutate seen, paths = %v", paths)
}

// The partial-result contract, which the legacy TS does not have. A campaign create
// that fails AFTER the budget committed must return a NON-NIL result carrying the
// budget id — returning (nil, err) would tell the orchestrator nothing was created and
// leave an orphan budget nobody reconciles.
//
// NOTE for anyone revert-verifying this: a 5xx takes the `createOutcomeAmbiguous`
// branch, NOT the `default` one. Mutating the default arm's return to nil leaves this
// test green and looks like it proves nothing — it is the ambiguous arm this drives.
func TestCreateDemandGenCampaignReturnsThePartialAfterBudgetCommits(t *testing.T) {
	var mu sync.Mutex
	var calls int
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
	res, err := c.CreateDemandGenCampaign(context.Background(), demandGenInput())

	if err == nil {
		t.Fatal("expected an error when the campaign mutate fails")
	}
	if res == nil {
		t.Fatal("expected a NON-NIL partial: the budget committed, so nil would report nothing was created")
	}
	if res.CampaignBudgetID != "111" {
		t.Errorf("CampaignBudgetID = %q, want %q — the partial must name the orphan to reconcile", res.CampaignBudgetID, "111")
	}
	if res.CampaignID != "" {
		t.Errorf("CampaignID = %q, want empty — the campaign was not created", res.CampaignID)
	}
}
