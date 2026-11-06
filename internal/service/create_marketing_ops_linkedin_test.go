// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

func TestLinkedInMarketingOpsInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   []string
	}{
		{"empty", ``, nil},
		{"no linkedin config", `{"googleAdsConfig":{"conversionActions":["1"]}}`, nil},
		{"no conversions", `{"linkedInConfig":{"targetingProfile":"cloud-native"}}`, nil},
		{"conversions", `{"linkedInConfig":{"conversions":["104012"]}}`, []string{"linkedInConfig.conversions"}},
		{"a blank conversion counts", `{"linkedInConfig":{"conversions":[""]}}`, []string{"linkedInConfig.conversions"}},
		{"an empty list does not count", `{"linkedInConfig":{"conversions":[]}}`, nil},
		// encoding/json matches struct field names case-insensitively, as the dispatcher's decode does.
		{"case variant key", `{"linkedInConfig":{"Conversions":["1"]}}`, []string{"linkedInConfig.conversions"}},
		{"envelope key is exact", `{"LinkedInConfig":{"conversions":["1"]}}`, nil},
		{"around a type error", `{"linkedInConfig":{"budgetUsd":"x","conversions":["1"]}}`, []string{"linkedInConfig.conversions"}},
		{"malformed envelope", `{`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LinkedInMarketingOpsInputs(json.RawMessage(tc.config)); !slices.Equal(got, tc.want) {
				t.Fatalf("LinkedInMarketingOpsInputs(%s) = %q, want %q", tc.config, got, tc.want)
			}
		})
	}
}

// TestBriefService_CreateCampaigns_LinkedInConversionsNeedMarketingOps drives the #552 gate through
// CreateCampaigns: linkedInConfig.conversions needs global_marketing_ops, and a request carrying
// both Google and LinkedIn gated inputs is checked once and names both.
func TestBriefService_CreateCampaigns_LinkedInConversionsNeedMarketingOps(t *testing.T) {
	conv := map[string]any{"linkedInConfig": map[string]any{"conversions": []any{"104012"}}}
	noConv := map[string]any{"linkedInConfig": map[string]any{"targetingProfile": "cloud-native"}}
	both := map[string]any{
		"googleAdsConfig": map[string]any{"channel": "search", "conversionActions": []any{"customers/1/conversionActions/2"}},
		"linkedInConfig":  map[string]any{"conversions": []any{"104012"}},
	}
	for _, tc := range []struct {
		name      string
		platforms []string
		config    map[string]any
		checker   *fakeMarketingOps
		wantErr   string
		wantCalls int
		wantMsg   string
	}{
		{"marketing ops may", []string{"linkedin-ads"}, conv, &fakeMarketingOps{allow: true}, "", 1, ""},
		{"an executive director may not", []string{"linkedin-ads"}, conv, &fakeMarketingOps{}, "403", 1,
			linkedInMarketingOpsForbiddenMessage([]string{"linkedInConfig.conversions"})},
		{"no conversions is not checked", []string{"linkedin-ads"}, noConv, &fakeMarketingOps{}, "", 0, ""},
		{"another platform is not checked", []string{"reddit-ads"}, conv, &fakeMarketingOps{}, "", 0, ""},
		{"a failed check is unavailable", []string{"linkedin-ads"}, conv, &fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, "503", 1, ""},
		{"both platforms, one check, both named", []string{"google-ads", "linkedin-ads"}, both, &fakeMarketingOps{}, "403", 1,
			marketingOpsForbiddenMessage([]string{"conversionActions"}) + "; " +
				linkedInMarketingOpsForbiddenMessage([]string{"linkedInConfig.conversions"})},
		{"google only keeps its message", []string{"google-ads"}, both, &fakeMarketingOps{}, "403", 1,
			marketingOpsForbiddenMessage([]string{"conversionActions"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, jobs, _ := approvedPaidBriefService(t)
			s.SetMarketingOpsChecker(tc.checker)
			_, err := s.CreateCampaigns(ctxWithActor(testActor), &briefs.CreateCampaignsPayload{
				ProjectID: "cncf", BriefID: "b1",
				Input: &briefs.CampaignCreateInput{Platforms: tc.platforms, Config: tc.config},
			})
			switch tc.wantErr {
			case "":
				if err != nil {
					t.Fatalf("CreateCampaigns = %v, want accepted", err)
				}
			case "403":
				var forbidden *briefs.ForbiddenError
				if !errors.As(err, &forbidden) || forbidden.Message != tc.wantMsg {
					t.Fatalf("CreateCampaigns = %T (%v), want a 403 %q", err, err, tc.wantMsg)
				}
			case "503":
				var unavailable *briefs.ConnServiceUnavailableError
				if !errors.As(err, &unavailable) || !strings.Contains(unavailable.Message, "linkedInConfig") {
					t.Fatalf("CreateCampaigns = %T (%v), want a 503 naming linkedInConfig", err, err)
				}
			}
			if tc.wantErr != "" && len(jobs.jobs) != 0 {
				t.Fatalf("%d job(s) created for a refused request", len(jobs.jobs))
			}
			if got := len(tc.checker.calls); got != tc.wantCalls {
				t.Fatalf("checker called %d time(s), want %d", got, tc.wantCalls)
			}
		})
	}
}
