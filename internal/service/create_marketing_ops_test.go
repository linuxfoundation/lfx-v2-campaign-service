// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeMarketingOps records each check and answers with a fixed verdict.
type fakeMarketingOps struct {
	allow bool
	err   error
	calls []string
}

func (f *fakeMarketingOps) IsGlobalMarketingOps(_ context.Context, projectSlug, username string) (bool, error) {
	f.calls = append(f.calls, projectSlug+"|"+username)
	return f.allow, f.err
}

func TestGoogleAdsDemandGenAudienceRequested(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   bool
	}{
		{"empty", ``, false},
		{"no google config", `{"microsoftConfig":{"budget":5}}`, false},
		{"google config without audience", `{"googleAdsConfig":{"channel":"demand-gen"}}`, false},
		{"campaign audience", `{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`, true},
		{"whitespace audience counts", `{"googleAdsConfig":{"demandGenAudience":" "}}`, true},
		{"group audience", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"},{"name":"b","audience":"customers/1/audiences/3"}]}}`, true},
		{"groups without audience", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"}]}}`, false},
		// encoding/json matches field names case-insensitively, and so does the dispatcher's decode.
		{"case variant key", `{"googleAdsConfig":{"DemandGenAudience":"customers/1/audiences/2"}}`, true},
		// A type error elsewhere in the object does not hide an audience decoded before it.
		{"audience before a type error", `{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2","demandGenAdGroups":"x"}}`, true},
		{"malformed envelope", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GoogleAdsDemandGenAudienceRequested(json.RawMessage(tc.config)); got != tc.want {
				t.Fatalf("GoogleAdsDemandGenAudienceRequested(%s) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}

// TestBriefService_CreateCampaigns_DemandGenAudienceNeedsMarketingOps drives the gate through
// CreateCampaigns: a refusal arrives before any job exists, and a grant creates the job.
func TestBriefService_CreateCampaigns_DemandGenAudienceNeedsMarketingOps(t *testing.T) {
	audience := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "demand-gen", "demandGenAudience": "customers/1/audiences/2"}}
	groupAudience := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "demand-gen", "demandGenAdGroups": []any{map[string]any{"name": "g", "audience": " "}}}}
	noAudience := map[string]any{"googleAdsConfig": map[string]any{"channel": "demand-gen"}}

	for _, tc := range []struct {
		name      string
		platforms []string
		config    map[string]any
		checker   *fakeMarketingOps // nil: none injected
		actor     *model.Actor
		wantErr   string // "", "403" or "503"
		wantCalls int
	}{
		{"marketing ops may", []string{"google-ads"}, audience, &fakeMarketingOps{allow: true}, testActor, "", 1},
		{"an executive director may not", []string{"google-ads"}, audience, &fakeMarketingOps{}, testActor, "403", 1},
		{"a group audience is checked", []string{"google-ads"}, groupAudience, &fakeMarketingOps{}, testActor, "403", 1},
		{"a failed check is unavailable, not a grant", []string{"google-ads"}, audience,
			&fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, testActor, "503", 1},
		{"no checker is unavailable", []string{"google-ads"}, audience, nil, testActor, "503", 0},
		{"no actor is unavailable", []string{"google-ads"}, audience, &fakeMarketingOps{allow: true}, nil, "503", 0},
		{"no audience is not checked", []string{"google-ads"}, noAudience, &fakeMarketingOps{}, testActor, "", 0},
		// googleAdsConfig is read only by the google-ads dispatcher, so another platform's create
		// carrying it is not checked.
		{"another platform is not checked", []string{"microsoft-ads"}, audience, &fakeMarketingOps{}, testActor, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, jobs, _ := approvedPaidBriefService(t)
			if tc.checker != nil {
				s.SetMarketingOpsChecker(tc.checker)
			}
			ctx := context.Background()
			if tc.actor != nil {
				ctx = ctxWithActor(tc.actor)
			}
			_, err := s.CreateCampaigns(ctx, &briefs.CreateCampaignsPayload{
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
				if !errors.As(err, &forbidden) || !strings.Contains(forbidden.Message, "global_marketing_ops") {
					t.Fatalf("CreateCampaigns = %T (%v), want a 403 naming global_marketing_ops", err, err)
				}
			case "503":
				var unavailable *briefs.ConnServiceUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("CreateCampaigns = %T (%v), want a 503", err, err)
				}
			}
			if tc.wantErr != "" {
				if n := len(jobs.jobs); n != 0 {
					t.Fatalf("%d job(s) created for a refused request", n)
				}
			}
			if tc.checker != nil {
				if got := len(tc.checker.calls); got != tc.wantCalls {
					t.Fatalf("checker called %d time(s), want %d", got, tc.wantCalls)
				}
				if tc.wantCalls > 0 && tc.checker.calls[0] != "cncf|"+testActor.Username {
					t.Fatalf("checked %q, want the path project and the caller's username", tc.checker.calls[0])
				}
			}
		})
	}
}

// The published 403 example is the sentence the service actually returns. design cannot import
// this package, so the example is a copy; every generated OpenAPI document must carry it.
func TestForbiddenErrorExampleMatchesService(t *testing.T) {
	for _, rel := range []string{
		"../../gen/http/openapi.json",
		"../../gen/http/openapi3.json",
		"../../gen/http/openapi3.2.json",
	} {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(rel) //nolint:gosec // fixed repo-relative path in a test
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var doc struct {
				Definitions map[string]forbiddenSchema `json:"definitions"`
				Components  struct {
					Schemas map[string]forbiddenSchema `json:"schemas"`
				} `json:"components"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			var found []string
			for _, schemas := range []map[string]forbiddenSchema{doc.Definitions, doc.Components.Schemas} {
				for name, s := range schemas {
					if strings.HasPrefix(name, "ForbiddenError") || strings.HasSuffix(name, "ForbiddenError") {
						found = append(found, name)
						if got := s.Properties.Message.Example; got != demandGenAudienceForbiddenMessage {
							t.Errorf("%s message example = %q, want %q", name, got, demandGenAudienceForbiddenMessage)
						}
					}
				}
			}
			if len(found) == 0 {
				t.Fatalf("%s has no ForbiddenError schema", rel)
			}
		})
	}
}

type forbiddenSchema struct {
	Properties struct {
		Message struct {
			Example string `json:"example"`
		} `json:"message"`
	} `json:"properties"`
}
