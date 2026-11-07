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
		{"no linkedin config", `{"googleAdsConfig":{"demandGenAudience":"a"}}`, nil},
		{"no targeting", `{"linkedInConfig":{"targetingProfile":"cloud-native"}}`, nil},
		{"facets only are not gated", `{"linkedInConfig":{"targeting":{"include":[{"skills":["urn:li:skill:1"],"titles":["urn:li:title:2"]}],"exclude":{"employers":["urn:li:organization:3"]}}}}`, nil},
		{"included matched audience", `{"linkedInConfig":{"targeting":{"include":[{"skills":["urn:li:skill:1"]},{"audienceMatchingSegments":["urn:li:adSegment:9"]}]}}}`,
			[]string{"linkedInConfig.targeting.include[1].audienceMatchingSegments"}},
		{"included retargeting", `{"linkedInConfig":{"targeting":{"include":[{"dynamicSegments":["urn:li:adSegment:9"]}]}}}`,
			[]string{"linkedInConfig.targeting.include[0].dynamicSegments"}},
		{"excluded segments", `{"linkedInConfig":{"targeting":{"include":[{"skills":["urn:li:skill:1"]}],"exclude":{"audienceMatchingSegments":["urn:li:adSegment:9"],"dynamicSegments":["x"]}}}}`,
			[]string{"linkedInConfig.targeting.exclude.audienceMatchingSegments", "linkedInConfig.targeting.exclude.dynamicSegments"}},
		{"a blank segment counts", `{"linkedInConfig":{"targeting":{"include":[{"dynamicSegments":[""]}]}}}`,
			[]string{"linkedInConfig.targeting.include[0].dynamicSegments"}},
		{"empty segment lists do not count", `{"linkedInConfig":{"targeting":{"include":[{"dynamicSegments":[]}],"exclude":{"audienceMatchingSegments":[]}}}}`, nil},
		// encoding/json matches struct field names case-insensitively, as the dispatcher's decode does.
		{"case variant targeting key", `{"linkedInConfig":{"Targeting":{"Include":[{"dynamicSegments":["urn:li:adSegment:9"]}]}}}`,
			[]string{"linkedInConfig.targeting.include[0].dynamicSegments"}},
		{"envelope key is exact", `{"LinkedInConfig":{"targeting":{"include":[{"dynamicSegments":["urn:li:adSegment:9"]}]}}}`, nil},
		{"segments around a type error", `{"linkedInConfig":{"budgetUsd":"x","targeting":{"include":[{"dynamicSegments":["urn:li:adSegment:9"]}]}}}`,
			[]string{"linkedInConfig.targeting.include[0].dynamicSegments"}},
		{"malformed envelope", `{`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LinkedInMarketingOpsInputs(json.RawMessage(tc.config)); !slices.Equal(got, tc.want) {
				t.Fatalf("LinkedInMarketingOpsInputs(%s) = %q, want %q", tc.config, got, tc.want)
			}
		})
	}
}

// TestBriefService_CreateCampaigns_LinkedInSegmentsNeedMarketingOps drives the #552 gate through
// CreateCampaigns: an ad segment in linkedInConfig.targeting needs global_marketing_ops, facets
// alone do not, and a request carrying both Google and LinkedIn gated inputs is checked once and
// names both.
func TestBriefService_CreateCampaigns_LinkedInSegmentsNeedMarketingOps(t *testing.T) {
	segments := map[string]any{"linkedInConfig": map[string]any{"targeting": map[string]any{
		"include": []any{map[string]any{"dynamicSegments": []any{"urn:li:adSegment:9"}}}}}}
	facets := map[string]any{"linkedInConfig": map[string]any{"targeting": map[string]any{
		"include": []any{map[string]any{"skills": []any{"urn:li:skill:9"}}}}}}
	both := map[string]any{
		"googleAdsConfig": map[string]any{"channel": "search", "conversionActions": []any{"customers/1/conversionActions/2"}},
		"linkedInConfig": map[string]any{"targeting": map[string]any{
			"include": []any{map[string]any{"skills": []any{"urn:li:skill:9"}}},
			"exclude": map[string]any{"audienceMatchingSegments": []any{"urn:li:adSegment:9"}}}},
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
		{"marketing ops may", []string{"linkedin-ads"}, segments, &fakeMarketingOps{allow: true}, "", 1, ""},
		{"an executive director may not", []string{"linkedin-ads"}, segments, &fakeMarketingOps{}, "403", 1,
			linkedInMarketingOpsForbiddenMessage([]string{"linkedInConfig.targeting.include[0].dynamicSegments"})},
		{"facets alone are not checked", []string{"linkedin-ads"}, facets, &fakeMarketingOps{}, "", 0, ""},
		{"another platform is not checked", []string{"reddit-ads"}, segments, &fakeMarketingOps{}, "", 0, ""},
		{"a failed check is unavailable", []string{"linkedin-ads"}, segments, &fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, "503", 1, ""},
		{"both platforms, one check, both named", []string{"google-ads", "linkedin-ads"}, both, &fakeMarketingOps{}, "403", 1,
			marketingOpsForbiddenMessage([]string{"conversionActions"}) + "; " +
				linkedInMarketingOpsForbiddenMessage([]string{"linkedInConfig.targeting.exclude.audienceMatchingSegments"})},
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
