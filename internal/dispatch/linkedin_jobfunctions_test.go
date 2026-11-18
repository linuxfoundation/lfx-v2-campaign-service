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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestLinkedIn_JobFunctionsPreflight pins linkedInConfig.jobFunctions (#585) as a synchronous 400:
// a wrong JSON type (which the best-effort decode would read as "use the default"), an empty
// list, a malformed value, and jobFunctions sent with targeting, which would not be read.
func TestLinkedIn_JobFunctionsPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsTargetingEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, linkedInConfig, want string // want "" = not applicable (accepted)
	}{
		{"absent", `{"targetingProfile":"cloud-native"}`, ""},
		{"valid", `{"jobFunctions":["urn:li:function:4","urn:li:function:4"]}`, ""},
		{"not an array", `{"jobFunctions":"urn:li:function:4"}`, "jobFunctions must be an array"},
		{"explicit null", `{"jobFunctions":null}`, "jobFunctions must be an array"},
		{"a number entry", `{"jobFunctions":[4]}`, "jobFunctions must be an array"},
		{"empty", `{"jobFunctions":[]}`, "jobFunctions is empty"},
		{"wrong namespace", `{"jobFunctions":["urn:li:skill:4"]}`, "not urn:li:function:<id>"},
		{"with targeting", `{"jobFunctions":["urn:li:function:4"],"targeting":{"include":[{"skills":["urn:li:skill:1"]}]}}`, "not read with linkedInConfig.targeting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := json.RawMessage(`{"linkedInConfig":` + tc.linkedInConfig + `}`)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("PreflightCreate = %v, want a 400 containing %q", err, tc.want)
			}
			if _, pre := domain.AsPreflightRefusal(err); !pre {
				t.Fatalf("PreflightCreate = %v, want a pre-flight refusal", err)
			}
		})
	}
}

// TestLinkedIn_JobFunctionsReachTheCampaign drives a profile create with linkedInConfig.jobFunctions
// through Dispatch (#585): the caller's job functions, de-duplicated, are the campaign's last
// include.and clause, after (skills OR groups), and the stored snapshot carries them.
func TestLinkedIn_JobFunctionsReachTheCampaign(t *testing.T) {
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
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","skills":["urn:li:skill:1"],"groups":["urn:li:group:2"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}],
		"jobFunctions":["urn:li:function:4","urn:li:function:4","urn:li:function:25"]
	}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	got := string(criteria)
	mu.Unlock()
	wantInclude := `"include":{"and":[{"or":{"urn:li:adTargetingFacet:locations":["urn:li:geo:103644278"]}},` +
		`{"or":{"urn:li:adTargetingFacet:groups":["urn:li:group:2"],"urn:li:adTargetingFacet:skills":["urn:li:skill:1"]}},` +
		`{"or":{"urn:li:adTargetingFacet:jobFunctions":["urn:li:function:4","urn:li:function:25"]}}]}`
	if !strings.Contains(got, wantInclude) {
		t.Errorf("targetingCriteria:\n got %s\nwant include %s", got, wantInclude)
	}
	if n := strings.Count(string(camp.ConfigSnapshot), "urn:li:function:4"); n != 1 {
		t.Errorf("snapshot holds urn:li:function:4 %d times, want the normalised copy (1): %s", n, camp.ConfigSnapshot)
	}
}
