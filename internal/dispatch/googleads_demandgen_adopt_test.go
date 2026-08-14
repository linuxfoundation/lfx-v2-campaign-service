// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// Adoption looks a campaign up BY NAME, and the name it composed was hardcoded
// "Search Campaign" -- composed and looked up BEFORE the channel was ever consulted. So a
// Demand Gen dispatch with adoptExisting:true searched for the SEARCH campaign's name. Two
// ways that ends badly, both with real money attached:
//
//  1. the brief has a Search campaign -> it is adopted INTO the demand-gen slot, and the
//     brief now claims a Demand Gen campaign that is actually a Search campaign;
//  2. the brief has a real "DemandGen Campaign" -> the lookup misses it entirely and the
//     dispatcher creates a SECOND paid Demand Gen campaign.
//
// This asserts on the GAQL the dispatcher actually sends, because the bug was invisible at
// every other layer -- the create path composed the right name all along.
func TestGoogleAdsDemandGenAdoptionLooksUpTheDemandGenName(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	var mu sync.Mutex
	var queries []string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			queries = append(queries, string(body))
			mu.Unlock()
			// A clean absence, so the dispatch proceeds rather than adopting. What is under
			// test is WHICH name was searched for, not what came back.
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)},
		identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL),
		googleads.WithBaseURL(apiSrv.URL),
	)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","adoptExisting":true}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(queries) == 0 {
		t.Fatal("adoption must issue a name lookup; none was sent")
	}
	joined := strings.Join(queries, "\n")
	if !strings.Contains(joined, googleads.CampaignKindDemandGen) {
		t.Errorf("the adoption lookup never mentions %q; queries sent:\n%s", googleads.CampaignKindDemandGen, joined)
	}
	// The precise regression. "Search Campaign" is a SUBSTRING of nothing else here, and
	// "DemandGen Campaign" does not contain it, so this cannot pass by accident.
	if strings.Contains(joined, googleads.CampaignKindSearch) {
		t.Errorf("a demand-gen dispatch looked up the SEARCH campaign name %q -- it can adopt a Search campaign into the demand-gen slot, or miss the real Demand Gen campaign and create a second paid one. Queries sent:\n%s", googleads.CampaignKindSearch, joined)
	}
}

// The channel is now resolved BEFORE adoption, so an unsupported one must be refused without
// the platform being contacted at all. Previously the switch sat after adoption, so a typo
// could bind a campaign first and only then be rejected.
func TestGoogleAdsUnsupportedChannelIsRefusedBeforeAnyUpstreamCall(t *testing.T) {
	var called sync.Map
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store("token", true)
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(r.URL.Path, true)
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)},
		identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL),
		googleads.WithBaseURL(apiSrv.URL),
	)
	// adoptExisting:true is the point -- adoption is what used to run first.
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"perfrmance-max","adoptExisting":true}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil {
		t.Fatalf("an unsupported channel must be refused, got campaign %+v", camp)
	}
	if !strings.Contains(err.Error(), "unsupported channel") {
		t.Errorf("error = %v, want it to name the unsupported channel", err)
	}
	called.Range(func(k, _ any) bool {
		if strings.Contains(k.(string), "googleAds:search") || strings.Contains(k.(string), "mutate") {
			t.Errorf("an unsupported channel reached the platform at %v; it must be refused before any upstream call", k)
		}
		return true
	})
}
