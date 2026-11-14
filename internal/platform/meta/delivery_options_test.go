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

// TestCreateCampaign_FrequencyCapReachesTheAwarenessAdSet: a cap is sent as one IMPRESSIONS
// frequency_control_specs entry; without one the ad set carries no such field.
func TestCreateCampaign_FrequencyCapReachesTheAwarenessAdSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  *FrequencyCap
		want any
	}{
		{"none", nil, nil},
		{"two a week", &FrequencyCap{MaxImpressions: 2, IntervalDays: 7}, []any{map[string]any{"event": "IMPRESSIONS", "interval_days": float64(7), "max_frequency": float64(2)}}},
		{"bounds", &FrequencyCap{MaxImpressions: 90, IntervalDays: 1}, []any{map[string]any{"event": "IMPRESSIONS", "interval_days": float64(1), "max_frequency": float64(90)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adsetCap := newBodyCapture()
			srv := metaFieldServer(t, adsetCap, newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.Objective, in.FrequencyCap = "awareness", tc.cap
			if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			got, has := adsetCap.get()["frequency_control_specs"]
			if (tc.want == nil) == has || (has && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("frequency_control_specs = %#v (present %v), want %#v", got, has, tc.want)
			}
		})
	}
}

// TestCreateCampaign_FrequencyCapRefusalsSendNothing: a cap on any objective but awareness, or a
// field outside 1..90 (a missing one included), is a config-invalid refusal with no request sent.
func TestCreateCampaign_FrequencyCapRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name, objective string
		cap             FrequencyCap
		want            string
	}{
		{"on traffic", "traffic", FrequencyCap{2, 7}, `metaConfig.frequencyCap: applies only to the awareness objective, whose ad set optimizes for reach (objective is "traffic")`},
		{"on the default objective", "", FrequencyCap{2, 7}, `objective is "traffic"`},
		{"on engagement", "engagement", FrequencyCap{2, 7}, `objective is "engagement"`},
		{"zero impressions", "awareness", FrequencyCap{0, 7}, "maxImpressions 0 is out of range; Meta accepts 1 to 90"},
		{"too many impressions", "awareness", FrequencyCap{91, 7}, "maxImpressions 91 is out of range"},
		{"missing interval", "awareness", FrequencyCap{MaxImpressions: 2}, "intervalDays 0 is out of range"},
		{"interval over 90", "awareness", FrequencyCap{2, 91}, "intervalDays 91 is out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			fc := tc.cap
			in.Objective, in.FrequencyCap = tc.objective, &fc
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
}

// TestCreateCampaign_AdScheduleReachesTheLifetimeAdSet: a schedule is sent as one ADVERTISER-time
// adset_schedule entry (days de-duplicated and sorted) with pacing_type day_parting; without one
// the ad set carries neither field.
func TestCreateCampaign_AdScheduleReachesTheLifetimeAdSet(t *testing.T) {
	for _, tc := range []struct {
		name     string
		schedule *AdSchedule
		want     any
	}{
		{"none", nil, nil},
		{"weekday office hours", &AdSchedule{Days: []int{5, 1, 3, 1}, StartMinute: 540, EndMinute: 1020},
			[]any{map[string]any{"start_minute": float64(540), "end_minute": float64(1020), "days": []any{float64(1), float64(3), float64(5)}, "timezone_type": "ADVERTISER"}}},
		{"whole day bounds", &AdSchedule{Days: []int{0, 6}, StartMinute: 0, EndMinute: 1440},
			[]any{map[string]any{"start_minute": float64(0), "end_minute": float64(1440), "days": []any{float64(0), float64(6)}, "timezone_type": "ADVERTISER"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adsetCap := newBodyCapture()
			srv := metaFieldServer(t, adsetCap, newBodyCapture())
			defer srv.Close()
			in := metaFieldInput()
			in.LifetimeBudget, in.AdSchedule = true, tc.schedule
			if _, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			body := adsetCap.get()
			got, has := body["adset_schedule"]
			if (tc.want == nil) == has || (has && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("adset_schedule = %#v (present %v), want %#v", got, has, tc.want)
			}
			pacing, hasPacing := body["pacing_type"]
			if has != hasPacing || (hasPacing && !reflect.DeepEqual(pacing, []any{"day_parting"})) {
				t.Fatalf("pacing_type = %#v (present %v), want day_parting exactly when scheduled", pacing, hasPacing)
			}
			if _, lifetime := body["lifetime_budget"]; !lifetime {
				t.Fatalf("ad set has no lifetime_budget: %#v", body)
			}
		})
	}
}

// TestCreateCampaign_AdScheduleRefusalsSendNothing: a schedule on a daily budget, without days, with
// a day outside 0..6, or with minutes that are not whole hours in order inside the day is a
// config-invalid refusal with no request sent.
func TestCreateCampaign_AdScheduleRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lifetime bool
		schedule AdSchedule
		want     string
	}{
		{"daily budget", false, AdSchedule{Days: []int{1}, StartMinute: 540, EndMinute: 1020}, "metaConfig.adSchedule: needs a lifetime budget"},
		{"no days", true, AdSchedule{StartMinute: 540, EndMinute: 1020}, "days must name at least one day"},
		{"day 7", true, AdSchedule{Days: []int{7}, StartMinute: 540, EndMinute: 1020}, "day 7 is out of range"},
		{"day -1", true, AdSchedule{Days: []int{-1}, StartMinute: 540, EndMinute: 1020}, "day -1 is out of range"},
		{"start after end", true, AdSchedule{Days: []int{1}, StartMinute: 1020, EndMinute: 540}, "must satisfy 0 <= startMinute < endMinute <= 1440"},
		{"empty window", true, AdSchedule{Days: []int{1}, StartMinute: 540, EndMinute: 540}, "must satisfy"},
		{"negative start", true, AdSchedule{Days: []int{1}, StartMinute: -60, EndMinute: 540}, "must satisfy"},
		{"past midnight", true, AdSchedule{Days: []int{1}, StartMinute: 0, EndMinute: 1500}, "must satisfy"},
		{"half hour start", true, AdSchedule{Days: []int{1}, StartMinute: 570, EndMinute: 1020}, "must be whole hours (multiples of 60)"},
		{"half hour end", true, AdSchedule{Days: []int{1}, StartMinute: 540, EndMinute: 1050}, "must be whole hours"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			sc := tc.schedule
			in.LifetimeBudget, in.AdSchedule = tc.lifetime, &sc
			res, err := biddingClient(srv.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid pre-flight refusal containing %q", res, err, tc.want)
			}
		})
	}
}
