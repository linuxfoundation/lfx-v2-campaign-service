// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestResolveConversionEvent(t *testing.T) {
	for _, tc := range []struct {
		objective, event, want, errHas string
	}{
		{"", "", "", ""},
		{"traffic", "", "", ""}, // absent never refuses, whatever the objective
		{"", "sign_up", "SIGN_UP", ""},
		{"conversions", " Lead ", "LEAD", ""},
		{"conversions", "PURCHASE", "PURCHASE", ""},
		{"traffic", "SIGN_UP", "", "objective"},
		{"awareness", "LEAD", "", "objective"},
		{"video_views", "LEAD", "", "objective"},
		{"conversions", "CLICKS", "", "is not one of"},
		{"conversions", "VIDEO_VIEW_6S", "", "is not one of"},
		{"conversions", "MOBILE_CONVERSION_PURCHASE", "", "is not one of"},
		{"conversions", "LANDING_PAGE_VISIT", "", "is not one of"},
		// An objective that is not one of Reddit's (objectives are matched exactly, so
		// "Conversions" is not one) is named as unsupported, not as the event's problem.
		{"Conversions", "SIGN_UP", "", "unsupported Reddit objective"},
		{"bogus", "LEAD", "", "unsupported Reddit objective"},
	} {
		got, err := ResolveConversionEvent(tc.objective, tc.event)
		if tc.errHas == "" {
			if err != nil || got != tc.want {
				t.Errorf("(%q,%q) = %q, %v; want %q", tc.objective, tc.event, got, err, tc.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.errHas) {
			t.Errorf("(%q,%q) err = %v; want one naming %q", tc.objective, tc.event, err, tc.errHas)
		}
	}
}

func TestConversionEventsIsACopy(t *testing.T) {
	got := ConversionEvents()
	got[0] = "MUTATED"
	if ConversionEvents()[0] == "MUTATED" {
		t.Fatal("ConversionEvents must return a copy, not the package's own slice")
	}
}

// The client refuses a conversion event that does not suit the objective before ANY request,
// account verification included, so nothing upstream exists to orphan.
func TestCreateCampaign_ConversionEventRefusedBeforeAnyRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(Credentials{ClientID: "c", ClientSecret: "s", RefreshToken: "r"},
		AccountConfig{AccountID: "t2_acct", ConversionPixelID: "t2_pixel"},
		WithBaseURL(srv.URL), WithTokenURL(srv.URL))
	for _, tc := range []struct {
		in   CampaignInput
		want string
	}{
		{CampaignInput{Objective: "traffic", ConversionEvent: "SIGN_UP"}, "conversionEvent"},
		{CampaignInput{Objective: "conversions", ConversionEvent: "REGISTER"}, "conversionEvent"},
		// A mis-cased objective is named as the problem, not the conversion event.
		{CampaignInput{Objective: "Conversions", ConversionEvent: "SIGN_UP"}, "unsupported Reddit objective"},
	} {
		in := tc.in
		in.EventName, in.Project, in.BudgetUSD = "KubeCon", "cncf", 50
		in.StartDate, in.EndDate, in.RegistrationURL = "2099-08-01", "2099-08-31", "https://events.example.org/kubecon"
		res, err := c.CreateCampaign(context.Background(), in)
		if res != nil || err == nil {
			t.Fatalf("%+v: want (nil, refusal), got %+v, %v", in, res, err)
		}
		var pr *domain.PreflightRefusal
		if !errors.As(err, &pr) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want a PreflightRefusal naming %q, got %T: %v", tc.want, err, err)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d requests sent before the refusal", n)
	}
}
