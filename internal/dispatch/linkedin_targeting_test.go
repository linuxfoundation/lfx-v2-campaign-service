// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// linkedInTargetingConfig is a create config with linkedInConfig.targeting and NO targeting
// profiles: with targeting set the profile is not read, so none is needed.
func linkedInTargetingConfig(targeting string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"employerExclusions":["urn:li:company:1111"],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}],
		"targeting":` + targeting + `
	}}`)
}

const validLinkedInTargeting = `{"include":[{"titles":["urn:li:title:1234","urn:li:title:1234"]},{"dynamicSegments":["urn:li:adSegment:16182906"]}],
	"exclude":{"audienceMatchingSegments":["urn:li:adSegment:11462646"]}}`

func TestLinkedIn_TargetingRefusedWhileTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingEnabled, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	cfg := linkedInTargetingConfig(validLinkedInTargeting)

	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvLinkedInAdsTargetingEnabled) {
		t.Fatalf("PreflightCreate = %v, want a 400 naming %s", err, constants.EnvLinkedInAdsTargetingEnabled)
	}
	_, err = d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests for a refused create", n)
	}
}

func TestLinkedIn_TargetingPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, targeting, want string
	}{
		{"valid", validLinkedInTargeting, ""},
		{"titles with seniorities", `{"include":[{"titles":["urn:li:title:1"]},{"seniorities":["urn:li:seniority:5"]}]}`, "titles together"},
		{"unknown facet", `{"include":[{"memberBehaviors":["urn:li:memberBehavior:2"]}]}`, "unknown facet"},
		{"no clauses", `{"include":[]}`, "at least one clause"},
		{"exclude as an array", `{"include":[{"skills":["urn:li:skill:1"]}],"exclude":[]}`, "must be an object"},
		{"exclude as a string", `{"include":[{"skills":["urn:li:skill:1"]}],"exclude":"x"}`, "must be an object"},
		{"include as an object", `{"include":{"skills":["urn:li:skill:1"]}}`, "must be an object"},
		{"a number facet value", `{"include":[{"skills":[1]}]}`, "must be an object"},
		{"null", `null`, "must be an object"},
		{"included employer in employerExclusions", `{"include":[{"employers":["urn:li:organization:1111"]}]}`, "is in employerExclusions"},
		{"locale excluded", `{"include":[{"skills":["urn:li:skill:1"]}],"exclude":{"interfaceLocales":["urn:li:locale:en_US"]}}`, "only under include"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInTargetingConfig(tc.targeting))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.targeting") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_TargetingReachesTheCampaign pins the dispatcher wiring: the validated targeting
// reaches the campaign's targetingCriteria (with the config's employer exclusions merged), no
// targeting profile is required, and the stored snapshot carries it.
func TestLinkedIn_TargetingReachesTheCampaign(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingEnabled, "true")
	var mu sync.Mutex
	var criteria json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			var body struct {
				TargetingCriteria json.RawMessage `json:"targetingCriteria"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			criteria = body.TargetingCriteria
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInTargetingConfig(validLinkedInTargeting))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := `{"exclude":{"or":{"urn:li:adTargetingFacet:audienceMatchingSegments":["urn:li:adSegment:11462646"],` +
		`"urn:li:adTargetingFacet:employers":["urn:li:company:1111"]}},` +
		`"include":{"and":[{"or":{"urn:li:adTargetingFacet:locations":["urn:li:geo:103644278"]}},` +
		`{"or":{"urn:li:adTargetingFacet:titles":["urn:li:title:1234"]}},` +
		`{"or":{"urn:li:adTargetingFacet:dynamicSegments":["urn:li:adSegment:16182906"]}}]}}`
	mu.Lock()
	got := string(criteria)
	mu.Unlock()
	if got != want {
		t.Errorf("targetingCriteria:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), `"targeting"`) || !strings.Contains(string(camp.ConfigSnapshot), "urn:li:adSegment:16182906") {
		t.Errorf("snapshot lost the targeting: %s", camp.ConfigSnapshot)
	}
	// The snapshot stores the normalised copy Dispatch validated, duplicates dropped.
	if n := strings.Count(string(camp.ConfigSnapshot), "urn:li:title:1234"); n != 1 {
		t.Errorf("snapshot holds the title %d times, want the normalised copy (1): %s", n, camp.ConfigSnapshot)
	}
}

func TestLinkedIn_NoTargetingIsUnchangedByTheFlag(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingEnabled, "")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	cfg := json.RawMessage(`{"linkedInConfig":{"budgetUsd":100,"targetingProfile":"cloud-native"}}`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v, want not applicable", err)
	}
}

// TestEveryLinkedInFacetIsClassifiedForTheMarketingOpsGate pins the agreement between the
// client's facet keys and the create path's role gate: exactly the ad-segment facets, included
// or excluded, are reported by service.LinkedInMarketingOpsInputs. A new facet that names an
// account resource must be added to both.
func TestEveryLinkedInFacetIsClassifiedForTheMarketingOpsGate(t *testing.T) {
	segment := map[string]bool{linkedin.FacetAudienceMatchingSegments: true, linkedin.FacetDynamicSegments: true}
	for _, key := range linkedin.TargetingFacetKeys() {
		for _, where := range []string{`{"include":[{"` + key + `":["v"]}]}`, `{"include":[{"skills":["v"]}],"exclude":{"` + key + `":["v"]}}`} {
			got := len(service.LinkedInMarketingOpsInputs(linkedInTargetingConfig(where))) > 0
			if got != segment[key] {
				t.Errorf("facet %s in %s: gated = %v, want %v", key, where, got, segment[key])
			}
		}
	}
}
