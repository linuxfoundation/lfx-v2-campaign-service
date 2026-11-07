// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
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

func TestGoogleAdsMarketingOpsInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   []string
	}{
		{"empty", ``, nil},
		{"no google config", `{"microsoftConfig":{"budget":5}}`, nil},
		{"google config without gated input", `{"googleAdsConfig":{"channel":"display","userInterests":["customers/1/userInterests/2"],"excludedPlacements":["example.com"],"performanceMaxSearchThemes":["k8s"]}}`, nil},
		{"campaign audience", `{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`, []string{"demandGenAudience"}},
		{"whitespace audience counts", `{"googleAdsConfig":{"demandGenAudience":" "}}`, []string{"demandGenAudience"}},
		{"group audience", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"},{"name":"b","audience":"customers/1/audiences/3"}]}}`, []string{"demandGenAdGroups entry 2 audience"}},
		{"groups without audience", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"}]}}`, nil},
		{"display lists", `{"googleAdsConfig":{"channel":"display","audienceSegments":["customers/1/userLists/2"],"customAudiences":["x"],"customAffinities":["y"],"excludedAudienceSegments":["z"]}}`,
			[]string{"audienceSegments", "customAudiences", "customAffinities", "excludedAudienceSegments"}},
		{"empty lists do not count", `{"googleAdsConfig":{"customAudiences":[],"sharedNegativeKeywordLists":[]}}`, nil},
		{"a list of blanks counts", `{"googleAdsConfig":{"customAffinities":[""]}}`, []string{"customAffinities"}},
		{"display group lists", `{"googleAdsConfig":{"displayAdGroups":[{"name":"a","userInterests":["u"]},{"name":"b","audienceSegments":["s"],"customAudiences":["c"],"customAffinities":["f"],"excludedAudienceSegments":["e"]}]}}`,
			[]string{"displayAdGroups entry 2 audienceSegments", "displayAdGroups entry 2 customAudiences",
				"displayAdGroups entry 2 customAffinities", "displayAdGroups entry 2 excludedAudienceSegments"}},
		{"pmax signal", `{"googleAdsConfig":{"channel":"performance-max","performanceMaxAudienceSignal":"123"}}`, []string{"performanceMaxAudienceSignal"}},
		{"whitespace signal counts", `{"googleAdsConfig":{"performanceMaxAudienceSignal":"\t"}}`, []string{"performanceMaxAudienceSignal"}},
		{"shared negative lists", `{"googleAdsConfig":{"channel":"search","sharedNegativeKeywordLists":["customers/1/sharedSets/2"]}}`, []string{"sharedNegativeKeywordLists"}},
		// Search observation: not behind the Display flag, but it names the shared account's user lists.
		{"search audience segments", `{"googleAdsConfig":{"channel":"search","audienceSegments":["customers/1/userLists/2"]}}`, []string{"audienceSegments"}},
		{"search ad group audience segments", `{"googleAdsConfig":{"channel":"search","adGroups":[{"name":"a"},{"name":"b","audienceSegments":["customers/1/userLists/2"]}]}}`, []string{"adGroups entry 2 audienceSegments"}},
		{"conversion actions", `{"googleAdsConfig":{"channel":"search","conversionActions":["customers/1/conversionActions/2"]}}`, []string{"conversionActions"}},
		{"conversion actions on any channel", `{"googleAdsConfig":{"channel":"performance-max","conversionActions":["123"]}}`, []string{"conversionActions"}},
		{"a blank conversion action counts", `{"googleAdsConfig":{"conversionActions":[""]}}`, []string{"conversionActions"}},
		{"no conversion actions", `{"googleAdsConfig":{"conversionActions":[]}}`, nil},
		{"observed demographics are not gated", `{"googleAdsConfig":{"observedDemographics":["AGE_RANGE"]}}`, nil},
		// #498: the demographic Audience is new, so only the user lists put in it are gated.
		{"demand gen demographics alone are not gated", `{"googleAdsConfig":{"demandGenDemographics":{"ageRanges":["25-34"],"genders":["MALE"]}}}`, nil},
		{"empty demand gen demographics", `{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":[]}}}`, nil},
		{"demand gen demographic user lists", `{"googleAdsConfig":{"demandGenDemographics":{"ageRanges":["25-34"],"audienceSegments":["customers/1/userLists/2"]}}}`,
			[]string{"demandGenDemographics.audienceSegments"}},
		{"a blank demographic user list counts", `{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":[""]}}}`,
			[]string{"demandGenDemographics.audienceSegments"}},
		{"demand gen demographic custom audiences", `{"googleAdsConfig":{"demandGenDemographics":{"customAudiences":["customers/1/customAudiences/2"]}}}`,
			[]string{"demandGenDemographics.customAudiences"}},
		{"empty demographic custom audiences", `{"googleAdsConfig":{"demandGenDemographics":{"customAudiences":[]}}}`, nil},
		{"demographic user lists and custom audiences", `{"googleAdsConfig":{"demandGenDemographics":{"audienceSegments":["a"],"customAudiences":[" "]}}}`,
			[]string{"demandGenDemographics.audienceSegments", "demandGenDemographics.customAudiences"}},
		// #518: each demandGenAdGroups entry's own demographics, by 1-based position.
		{"group demographics alone are not gated", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a","demographics":{"ageRanges":["25-34"]}}]}}`, nil},
		{"group demographic user lists and custom audiences", `{"googleAdsConfig":{"demandGenAdGroups":[{"name":"a"},{"name":"b","demographics":{"audienceSegments":["a"],"customAudiences":[" "]}}]}}`,
			[]string{"demandGenAdGroups entry 2 demographics.audienceSegments", "demandGenAdGroups entry 2 demographics.customAudiences"}},
		{"several, in config order", `{"googleAdsConfig":{"sharedNegativeKeywordLists":["s"],"performanceMaxAudienceSignal":"1","demandGenAudience":"a","customAudiences":["c"]}}`,
			[]string{"demandGenAudience", "customAudiences", "performanceMaxAudienceSignal", "sharedNegativeKeywordLists"}},
		// encoding/json matches field names case-insensitively, and so does the dispatcher's decode.
		{"case variant key", `{"googleAdsConfig":{"DemandGenAudience":"customers/1/audiences/2","SharedNegativeKeywordLists":["s"]}}`, []string{"demandGenAudience", "sharedNegativeKeywordLists"}},
		// A type error elsewhere in the object does not hide an input decoded around it.
		{"inputs around a type error", `{"googleAdsConfig":{"demandGenAudience":"customers/1/audiences/2","demandGenAdGroups":"x","sharedNegativeKeywordLists":["s"]}}`, []string{"demandGenAudience", "sharedNegativeKeywordLists"}},
		{"envelope key is exact", `{"GoogleAdsConfig":{"demandGenAudience":"customers/1/audiences/2"}}`, nil},
		{"malformed envelope", `{`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GoogleAdsMarketingOpsInputs(json.RawMessage(tc.config)); !slices.Equal(got, tc.want) {
				t.Fatalf("GoogleAdsMarketingOpsInputs(%s) = %q, want %q", tc.config, got, tc.want)
			}
		})
	}
}

// TestBriefService_CreateCampaigns_SharedAccountInputsNeedMarketingOps drives the gate through
// CreateCampaigns: a refusal arrives before any job exists, and a grant creates the job.
func TestBriefService_CreateCampaigns_SharedAccountInputsNeedMarketingOps(t *testing.T) {
	audience := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "demand-gen", "demandGenAudience": "customers/1/audiences/2"}}
	groupAudience := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "demand-gen", "demandGenAdGroups": []any{map[string]any{"name": "g", "audience": " "}}}}
	noAudience := map[string]any{"googleAdsConfig": map[string]any{"channel": "demand-gen"}}
	display := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "display", "displayAdGroups": []any{map[string]any{"name": "g", "customAudiences": []any{"customers/1/customAudiences/2"}}}}}
	pmax := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "performance-max", "performanceMaxAudienceSignal": "customers/1/audiences/2"}}
	sharedLists := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "search", "sharedNegativeKeywordLists": []any{"customers/1/sharedSets/2"}}}
	searchObservation := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "search", "audienceSegments": []any{"customers/1/userLists/2"}}}
	conversions := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "search", "conversionActions": []any{"customers/1/conversionActions/2"}}}
	metaAudiences := map[string]any{"metaConfig": map[string]any{"targeting": map[string]any{
		"ageMin": 25, "customAudiences": []any{"111"}, "excludedCustomAudiences": []any{"222"}}}}
	metaAgeOnly := map[string]any{"metaConfig": map[string]any{"targeting": map[string]any{"ageMin": 25, "genders": []any{"male"}}}}
	googleAndMeta := map[string]any{
		"googleAdsConfig": map[string]any{"channel": "search", "conversionActions": []any{"customers/1/conversionActions/2"}},
		"metaConfig":      map[string]any{"targeting": map[string]any{"excludedCustomAudiences": []any{"222"}}}}
	several := map[string]any{"googleAdsConfig": map[string]any{
		"channel": "search", "audienceSegments": []any{"customers/1/userLists/2"},
		"sharedNegativeKeywordLists": []any{"customers/1/sharedSets/2"}}}

	// Text a 403's message must contain beyond wantNamed, by case name.
	wantContains := map[string]string{
		"google and meta, one check, both named": "googleAdsConfig inputs that name a resource on the shared Google Ads account " +
			"require the global_marketing_ops role on this project: conversionActions; metaConfig inputs",
	}
	for _, tc := range []struct {
		name      string
		platforms []string
		config    map[string]any
		checker   *fakeMarketingOps // nil: none injected
		actor     *model.Actor
		wantErr   string // "", "403" or "503"
		wantCalls int
		wantNamed string // for a 403: the fields the message must name; "" skips the check
	}{
		{"marketing ops may", []string{"google-ads"}, audience, &fakeMarketingOps{allow: true}, testActor, "", 1, ""},
		{"an executive director may not", []string{"google-ads"}, audience, &fakeMarketingOps{}, testActor, "403", 1, "demandGenAudience"},
		{"a group audience is checked", []string{"google-ads"}, groupAudience, &fakeMarketingOps{}, testActor, "403", 1, "demandGenAdGroups entry 1 audience"},
		{"a display group list is checked", []string{"google-ads"}, display, &fakeMarketingOps{}, testActor, "403", 1, "displayAdGroups entry 1 customAudiences"},
		{"a pmax audience signal is checked", []string{"google-ads"}, pmax, &fakeMarketingOps{}, testActor, "403", 1, "performanceMaxAudienceSignal"},
		{"shared negative lists are checked", []string{"google-ads"}, sharedLists, &fakeMarketingOps{}, testActor, "403", 1, "sharedNegativeKeywordLists"},
		{"search observation segments are checked", []string{"google-ads"}, searchObservation, &fakeMarketingOps{}, testActor, "403", 1, "audienceSegments"},
		{"conversion actions are checked", []string{"google-ads"}, conversions, &fakeMarketingOps{}, testActor, "403", 1, "conversionActions"},
		{"marketing ops may send conversion actions", []string{"google-ads"}, conversions, &fakeMarketingOps{allow: true}, testActor, "", 1, ""},
		{"several inputs, one check, all named", []string{"google-ads"}, several, &fakeMarketingOps{}, testActor, "403", 1, "audienceSegments, sharedNegativeKeywordLists"},
		{"marketing ops may send several", []string{"google-ads", "microsoft-ads"}, several, &fakeMarketingOps{allow: true}, testActor, "", 1, ""},
		{"a failed check on a pmax signal is unavailable", []string{"google-ads"}, pmax,
			&fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, testActor, "503", 1, ""},
		{"a failed check is unavailable, not a grant", []string{"google-ads"}, audience,
			&fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, testActor, "503", 1, ""},
		{"no checker is unavailable", []string{"google-ads"}, audience, nil, testActor, "503", 0, ""},
		{"no actor is unavailable", []string{"google-ads"}, audience, &fakeMarketingOps{allow: true}, nil, "503", 0, ""},
		{"no audience is not checked", []string{"google-ads"}, noAudience, &fakeMarketingOps{}, testActor, "", 0, ""},
		// #550: Meta custom audiences are resources of the shared Meta ad account.
		{"meta audiences are checked", []string{"meta-ads"}, metaAudiences, &fakeMarketingOps{}, testActor, "403", 1,
			"targeting.customAudiences, targeting.excludedCustomAudiences"},
		{"marketing ops may send meta audiences", []string{"meta-ads"}, metaAudiences, &fakeMarketingOps{allow: true}, testActor, "", 1, ""},
		{"meta age and gender are not checked", []string{"meta-ads"}, metaAgeOnly, &fakeMarketingOps{}, testActor, "", 0, ""},
		{"a failed check on meta audiences is unavailable", []string{"meta-ads"}, metaAudiences,
			&fakeMarketingOps{allow: true, err: errors.New("nats: timeout")}, testActor, "503", 1, ""},
		{"metaConfig is not checked unless meta-ads is selected", []string{"google-ads"}, metaAudiences, &fakeMarketingOps{}, testActor, "", 0, ""},
		{"google and meta, one check, both named", []string{"google-ads", "meta-ads"}, googleAndMeta, &fakeMarketingOps{}, testActor, "403", 1,
			"targeting.excludedCustomAudiences"},
		// googleAdsConfig is read only by the google-ads dispatcher, so another platform's create
		// carrying it is not checked.
		{"another platform is not checked", []string{"microsoft-ads"}, audience, &fakeMarketingOps{}, testActor, "", 0, ""},
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
				if tc.wantNamed != "" && !strings.HasSuffix(forbidden.Message, ": "+tc.wantNamed) {
					t.Fatalf("403 message = %q, want it to name exactly %q", forbidden.Message, tc.wantNamed)
				}
				if want := wantContains[tc.name]; want != "" && !strings.Contains(forbidden.Message, want) {
					t.Fatalf("403 message = %q, want it to contain %q", forbidden.Message, want)
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
						want := marketingOpsForbiddenMessage([]string{"demandGenAudience"})
						if got := s.Properties.Message.Example; got != want {
							t.Errorf("%s message example = %q, want %q", name, got, want)
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
