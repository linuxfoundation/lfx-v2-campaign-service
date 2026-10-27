// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The budget-reuse arm is written out once per creating channel rather than shared, so a
// wiring proof on Search and Performance Max says nothing about the other three. These
// tests are the same proof for Display, Demand Gen and Video: Google refuses the budget
// create as a duplicate, the lookup finds the orphan adoptable, and the ADOPTED resource
// name is what reaches that channel's campaigns:mutate — not a name from nowhere, and not
// a dead end.

// reusableBudgetHandler builds the half of a channel server that the reuse path owns: the
// budget create always refuses as a duplicate, the first search returns one adoptable
// orphan carrying the brief's own amount (so no alignment mutate is expected), the second
// — the campaign-side attachment check — returns nothing, and campaigns:mutate is captured.
// Everything else is handed to the channel's own cascade.
//
// mu guards the captures: httptest serves each request on its own goroutine.
func reusableBudgetHandler(t *testing.T, budgetName string, cascade http.HandlerFunc) (http.HandlerFunc, func() map[string]any) {
	t.Helper()
	var (
		mu           sync.Mutex
		searches     int
		campaignBody map[string]any
	)
	h := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			// Reuse changes nothing about what Google says — and this handler failing the
			// test on an alignment mutate is what proves none was attempted.
			gaqlError(http.StatusBadRequest, "campaignBudgetError", "DUPLICATE_NAME")(w, r)
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			mu.Lock()
			n := searches
			searches++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 0 {
				_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{
					budgetRow(budgetRowOpts{name: budgetName, amountMicros: "500000000"}),
				}})
				return
			}
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []json.RawMessage{}})
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			body := decode(t, r)
			mu.Lock()
			campaignBody = body
			mu.Unlock()
			okCampaign(w, r)
		default:
			cascade(w, r)
		}
	}
	return h, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return campaignBody
	}
}

// assertAttachedToReusedBudget fails unless the captured campaign create points at the
// adopted budget. The id is the lookup's 777, never the okBudget fixture's 111 — so a
// channel that silently fell back to a created budget cannot pass.
func assertAttachedToReusedBudget(t *testing.T, campaignBody map[string]any) {
	t.Helper()
	ops, _ := campaignBody["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("campaign body = %v", campaignBody)
	}
	op, _ := ops[0].(map[string]any)
	create, _ := op["create"].(map[string]any)
	if got, _ := create["campaignBudget"].(string); got != "customers/1234567890/campaignBudgets/777" {
		t.Errorf("campaignBudget = %q, want the reused budget's resource name", got)
	}
}

func assertReuseStep(t *testing.T, steps []string) {
	t.Helper()
	for _, s := range steps {
		if strings.Contains(s, "Campaign budget reused: 777") {
			return
		}
	}
	t.Errorf("the result must say the budget was reused rather than created: %v", steps)
}

func TestCreateDisplayCampaign_DuplicateBudgetReusesOrphan(t *testing.T) {
	adGroupH, _ := capturedMutate(1, displayAdGroupResource)
	assetH, _ := displayAssets()
	adH, _ := capturedMutate(1, displayAdResource)

	images := displayImages(t)
	budgetName := ComposeName(budgetKindFor(campaignKindDisplay), demandGenInput())
	h, readCampaign := reusableBudgetHandler(t, budgetName, displayCascade(t, images, adGroupH, assetH, adH))
	srv := demandGenTLSServer(t, h)
	c := demandGenClient(t, srv)

	res, err := c.CreateDisplayCampaign(context.Background(), displayInput(srv.URL))
	if err != nil {
		t.Fatalf("an adoptable orphan budget must not fail the Display dispatch: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the reused 777", res.CampaignBudgetID)
	}
	if res.CampaignID == "" {
		t.Error("the campaign step must be reached once the budget is adopted")
	}
	assertAttachedToReusedBudget(t, readCampaign())
	assertReuseStep(t, res.Steps)
}

func TestCreateDemandGenCampaign_DuplicateBudgetReusesOrphan(t *testing.T) {
	assetH, _ := okCreativeAssets()
	adH, _ := capturedMutate(1, func(int) string {
		return "customers/1234567890/adGroupAds/333~444"
	})

	budgetName := ComposeName(budgetKindFor(campaignKindDemandGen), demandGenInput())
	h, readCampaign := reusableBudgetHandler(t, budgetName, demandGenCascade(t, twoImages(t), assetH, adH))
	srv := demandGenTLSServer(t, h)
	c := demandGenClient(t, srv)

	res, err := c.CreateDemandGenCampaign(context.Background(), creativeInput(creativeAt(srv.URL)))
	if err != nil {
		t.Fatalf("an adoptable orphan budget must not fail the Demand Gen dispatch: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the reused 777", res.CampaignBudgetID)
	}
	if res.CampaignID == "" {
		t.Error("the campaign step must be reached once the budget is adopted")
	}
	assertAttachedToReusedBudget(t, readCampaign())
	assertReuseStep(t, res.Steps)
}

// Video is exercised through createVideoCampaignCascade for the same reason every other
// Video test is: the exported entry point refuses before sending anything, and the cascade
// is retained against the day Google opens Video creation. The reuse arm inside it still
// has to be right when that day comes.
func TestCreateVideoCampaignCascade_DuplicateBudgetReusesOrphan(t *testing.T) {
	adGroupH, _ := capturedMutate(1, videoAdGroupResource)
	assetH, _ := capturedMutate(1, videoAssetResource)
	adH, _ := capturedMutate(1, videoAdResource)

	budgetName := ComposeName(budgetKindFor(campaignKindVideo), demandGenInput())
	h, readCampaign := reusableBudgetHandler(t, budgetName, videoCascade(t, adGroupH, assetH, adH))
	srv := demandGenTLSServer(t, h)
	c := demandGenClient(t, srv)

	res, err := c.createVideoCampaignCascade(context.Background(), videoInput())
	if err != nil {
		t.Fatalf("an adoptable orphan budget must not fail the Video cascade: %v", err)
	}
	if res.CampaignBudgetID != "777" {
		t.Errorf("CampaignBudgetID = %q, want the reused 777", res.CampaignBudgetID)
	}
	if res.CampaignID == "" {
		t.Error("the campaign step must be reached once the budget is adopted")
	}
	assertAttachedToReusedBudget(t, readCampaign())
	assertReuseStep(t, res.Steps)
}
