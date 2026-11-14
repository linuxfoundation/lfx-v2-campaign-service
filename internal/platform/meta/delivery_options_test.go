// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// TestCreateCampaign_OptimizationGoalReachesTheAdSet: under traffic the ad set optimizes for the
// chosen goal, LINK_CLICKS when omitted (the pre-#550 payload); other objectives keep their own.
func TestCreateCampaign_OptimizationGoalReachesTheAdSet(t *testing.T) {
	for _, tc := range []struct{ objective, goal, want string }{
		{"traffic", "", "LINK_CLICKS"},
		{"traffic", "landing_page_views", "LANDING_PAGE_VIEWS"},
		{"", " Landing_Page_Views ", "LANDING_PAGE_VIEWS"},
		{"traffic", "LINK_CLICKS", "LINK_CLICKS"},
		{"awareness", "", "REACH"},
	} {
		t.Run(tc.objective+"/"+tc.goal, func(t *testing.T) {
			adsetCap := newBodyCapture()
			srv := metaFieldServer(t, adsetCap, newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.OptimizationGoal = tc.objective, tc.goal
			if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if got := adsetCap.get()["optimization_goal"]; got != tc.want {
				t.Fatalf("optimization_goal = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestCreateCampaign_OptimizationGoalRefusalsSendNothing: a goal on an objective other than
// traffic, or outside the list, is a config-invalid pre-flight refusal with no request sent.
func TestCreateCampaign_OptimizationGoalRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct{ name, objective, goal, want string }{
		{"on awareness", "awareness", "LANDING_PAGE_VIEWS", `metaConfig.optimizationGoal: applies only to the traffic objective (objective is "awareness")`},
		{"on leads", "leads", "LANDING_PAGE_VIEWS", `objective is "leads"`},
		{"on conversions", "conversions", "LINK_CLICKS", `objective is "conversions"`},
		{"unknown goal", "traffic", "THRUPLAY", `metaConfig.optimizationGoal: "THRUPLAY" is not one of LINK_CLICKS, LANDING_PAGE_VIEWS`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.PixelID, in.OptimizationGoal = tc.objective, "1234567890", tc.goal
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
}

// TestCreateCampaign_ManualBidWithLandingPageViewsCreatesNothing: a link-click bid cap on an ad
// set that optimizes for landing page views would cap a click while Meta optimizes for a page
// load, so it is refused after the account read and before any write.
func TestCreateCampaign_ManualBidWithLandingPageViewsCreatesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("no write may be sent, got %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
	}))
	defer srv.Close()
	in := metaFieldInput()
	in.BidCap, in.OptimizationGoal = 1.25, "LANDING_PAGE_VIEWS"
	res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
	if res != nil || err == nil || !strings.Contains(err.Error(), "manual bidding needs an ad set that optimizes for link clicks") {
		t.Fatalf("CreateCampaign = %+v, %v; want the link-click bid cap refusal", res, err)
	}
}
