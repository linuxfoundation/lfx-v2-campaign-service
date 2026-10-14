// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// redditBiddingServer answers a CreateCampaign and captures the campaign and ad group bodies;
// posts counts every mutating request.
type redditBiddingServer struct {
	mu                    sync.Mutex
	campaignBody, adGroup map[string]any
	posts                 int
}

// snapshot returns the captured bodies and the mutating-request count under the lock: the handler
// writes them on the server's goroutine.
func (s *redditBiddingServer) snapshot() (campaign, adGroup map[string]any, posts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.campaignBody, s.adGroup, s.posts
}

func (s *redditBiddingServer) client(t *testing.T) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		decode := func() map[string]any {
			var env struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&env)
			return env.Data
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method != http.MethodGet {
			s.posts++
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/ad_accounts/t2_test"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case strings.HasSuffix(path, "/campaigns"):
			s.campaignBody = decode()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_123"}})
		case strings.HasSuffix(path, "/ad_groups"):
			s.adGroup = decode()
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_456"}})
		case strings.HasSuffix(path, "/ads"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_789"}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})
	apiSrv := httptest.NewServer(mux)
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
}

func redditBiddingInput(objective string, bid float64) CampaignInput {
	return CampaignInput{
		EventName:       "Open Source Summit",
		Project:         "tlf",
		RegistrationURL: "https://events.linuxfoundation.org/oss/",
		BudgetUSD:       500,
		StartDate:       "2026-08-01",
		EndDate:         "2026-08-31",
		GeoTargets:      []string{"us"},
		Objective:       objective,
		PostURL:         "https://www.reddit.com/r/opensource/comments/abc123/great_post/",
		ManualCPCBid:    bid,
	}
}

// TestCreateCampaign_AutomaticBiddingStaysBidless pins the automatic request (LFXV2-2665):
// campaign and ad group BIDLESS, no bid_value.
func TestCreateCampaign_AutomaticBiddingStaysBidless(t *testing.T) {
	s := &redditBiddingServer{}
	res, err := s.client(t).CreateCampaign(context.Background(), redditBiddingInput("traffic", 0))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup, _ := s.snapshot()
	if campaign["bid_strategy"] != "BIDLESS" || adGroup["bid_strategy"] != "BIDLESS" {
		t.Fatalf("automatic: campaign %v / ad group %v, want BIDLESS on both", campaign["bid_strategy"], adGroup["bid_strategy"])
	}
	if _, has := adGroup["bid_value"]; has {
		t.Fatalf("an automatic ad group must not send bid_value: %v", adGroup["bid_value"])
	}
	if !res.BidApplied {
		t.Error("BidApplied = false on a run that created the campaign and ad group")
	}
}

// TestCreateCampaign_ManualBiddingIsWhatTheBidLeverAccepts pins the manual request: campaign AND
// ad group MANUAL_BIDDING (CBO requires them to match), bid_type CPC, the ad group's bid_value in
// micro-units — the strategy, type and value the bid lever checks before it re-bids.
func TestCreateCampaign_ManualBiddingIsWhatTheBidLeverAccepts(t *testing.T) {
	s := &redditBiddingServer{}
	if _, err := s.client(t).CreateCampaign(context.Background(), redditBiddingInput("traffic", 0.75)); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup, _ := s.snapshot()
	for name, body := range map[string]map[string]any{"campaign": campaign, "ad group": adGroup} {
		if body["bid_strategy"] != BidStrategyManual || body["bid_type"] != BidTypeCPC {
			t.Errorf("%s: bid_strategy=%v bid_type=%v, want MANUAL_BIDDING/CPC", name, body["bid_strategy"], body["bid_type"])
		}
	}
	if adGroup["bid_value"] != float64(750000) {
		t.Fatalf("ad group bid_value = %v, want 750000 micro-units", adGroup["bid_value"])
	}
}

// TestCreateCampaign_ManualBidRefusalsSendNoMutation: a CPM objective (the default conversions
// included) and an amount that is not whole micro-units are refused before anything is created.
func TestCreateCampaign_ManualBidRefusalsSendNoMutation(t *testing.T) {
	for name, in := range map[string]CampaignInput{
		"default objective bids CPM": redditBiddingInput("", 1),
		"awareness bids CPM":         redditBiddingInput("awareness", 1),
		"sub-micro amount":           redditBiddingInput("traffic", 1.0000001),
	} {
		t.Run(name, func(t *testing.T) {
			s := &redditBiddingServer{}
			res, err := s.client(t).CreateCampaign(context.Background(), in)
			if err == nil || !errors.Is(err, ErrBidAmountInvalid) {
				t.Fatalf("err = %v, want ErrBidAmountInvalid", err)
			}
			if _, _, posts := s.snapshot(); res != nil || posts != 0 {
				t.Fatalf("result=%+v posts=%d, want nil and 0", res, posts)
			}
		})
	}
}

func TestManualCPCBidSupported(t *testing.T) {
	for objective, want := range map[string]bool{"traffic": true, "": false, "conversions": false, "awareness": false, "video_views": false} {
		if got := ManualCPCBidSupported(objective); got != want {
			t.Errorf("ManualCPCBidSupported(%q) = %v, want %v", objective, got, want)
		}
	}
}
