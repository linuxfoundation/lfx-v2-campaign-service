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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn Audience Expansion and Audience Network choices (#667).

// linkedInDeliveryConfig is a valid single-image create with the given extra linkedInConfig
// fields, e.g. `"audienceExpansionEnabled":true`.
func linkedInDeliveryConfig(extra string) json.RawMessage {
	cfg := `{"linkedInConfig":{"budgetUsd":100,
		"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]`
	if extra != "" {
		cfg += "," + extra
	}
	return json.RawMessage(cfg + `}}`)
}

// TestLinkedIn_DeliveryOptionsPreflight pins the synchronous 400s: either field while the flag is
// off (false included, and any value but exactly "true"), a non-boolean value, and the Audience
// Network on a text create. null reads as omitted.
func TestLinkedIn_DeliveryOptionsPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, flag string
		config     json.RawMessage
		want       string // "" = not applicable (accepted)
	}{
		{"omitted, flag off", "", linkedInDeliveryConfig(""), ""},
		{"null reads as omitted, flag off", "", linkedInDeliveryConfig(`"audienceExpansionEnabled":null,"offsiteDeliveryEnabled":null`), ""},
		{"expansion true, flag off", "", linkedInDeliveryConfig(`"audienceExpansionEnabled":true`), "linkedInConfig.audienceExpansionEnabled disabled on this deployment (LINKEDIN_ADS_DELIVERY_OPTIONS_ENABLED"},
		{"offsite false, flag off", "", linkedInDeliveryConfig(`"offsiteDeliveryEnabled":false`), "linkedInConfig.offsiteDeliveryEnabled disabled"},
		{"both, flag off", "", linkedInDeliveryConfig(`"audienceExpansionEnabled":false,"offsiteDeliveryEnabled":true`), "linkedInConfig.audienceExpansionEnabled and linkedInConfig.offsiteDeliveryEnabled disabled"},
		{"flag not exactly true", "TRUE", linkedInDeliveryConfig(`"audienceExpansionEnabled":true`), "disabled on this deployment"},
		{"both true, flag on", "true", linkedInDeliveryConfig(`"audienceExpansionEnabled":true,"offsiteDeliveryEnabled":true`), ""},
		{"both false, flag on", "true", linkedInDeliveryConfig(`"audienceExpansionEnabled":false,"offsiteDeliveryEnabled":false`), ""},
		{"expansion a string", "true", linkedInDeliveryConfig(`"audienceExpansionEnabled":"true"`), "linkedInConfig.audienceExpansionEnabled: must be true or false"},
		{"offsite a number, flag off", "", linkedInDeliveryConfig(`"offsiteDeliveryEnabled":1`), "linkedInConfig.offsiteDeliveryEnabled: must be true or false"},
		{"case variant of the key", "true", linkedInDeliveryConfig(`"OffsiteDeliveryEnabled":"yes"`), "linkedInConfig.offsiteDeliveryEnabled: must be true or false"},
		{"audience network on a text create", "true", linkedInTextConfig(validTextAds + `,"offsiteDeliveryEnabled":true`), "the LinkedIn Audience Network serves Sponsored Content only"},
		{"expansion on a text create", "true", linkedInTextConfig(validTextAds + `,"audienceExpansionEnabled":true,"offsiteDeliveryEnabled":false`), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsDeliveryOptionsEnabled, tc.flag)
			t.Setenv(constants.EnvLinkedInAdsTextAdsEnabled, "true")
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, tc.config)
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_DeliveryOptionsRefusedInDispatch drives the refusals through Dispatch, the path a
// job takes: nothing may reach LinkedIn.
func TestLinkedIn_DeliveryOptionsRefusedInDispatch(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra string }{
		{"flag off", "", `"audienceExpansionEnabled":true`},
		{"mistyped", "true", `"offsiteDeliveryEnabled":"true"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsDeliveryOptionsEnabled, tc.flag)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInDeliveryConfig(tc.extra))
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Fatalf("Dispatch = %v, want a not-created config refusal", err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%d LinkedIn requests for a refused create", n)
			}
		})
	}
}

// TestLinkedIn_DeliveryOptionsReachTheCampaign pins the wire end to end: omitted keeps the
// long-standing body (offsiteDeliveryEnabled false, no audienceExpansionEnabled), and each
// chosen value is sent as given.
func TestLinkedIn_DeliveryOptionsReachTheCampaign(t *testing.T) {
	for _, tc := range []struct {
		name, extra   string
		wantOffsite   bool
		wantExpansion any // nil = key absent
	}{
		{"omitted", "", false, nil},
		{"both on", `"audienceExpansionEnabled":true,"offsiteDeliveryEnabled":true`, true, true},
		{"expansion off explicitly", `"audienceExpansionEnabled":false`, false, false},
		{"audience network only", `"offsiteDeliveryEnabled":true`, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsDeliveryOptionsEnabled, "true")
			var mu sync.Mutex
			var campaignBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				switch {
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "adCampaigns"):
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					_ = json.Unmarshal(b, &campaignBody)
					mu.Unlock()
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

			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInDeliveryConfig(tc.extra)); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if got, ok := campaignBody["offsiteDeliveryEnabled"].(bool); !ok || got != tc.wantOffsite {
				t.Errorf("offsiteDeliveryEnabled = %v, want %v", campaignBody["offsiteDeliveryEnabled"], tc.wantOffsite)
			}
			got, present := campaignBody["audienceExpansionEnabled"]
			if tc.wantExpansion == nil {
				if present {
					t.Errorf("audienceExpansionEnabled = %v, want the key absent", got)
				}
			} else if got != tc.wantExpansion {
				t.Errorf("audienceExpansionEnabled = %v (present %v), want %v", got, present, tc.wantExpansion)
			}
		})
	}
}
