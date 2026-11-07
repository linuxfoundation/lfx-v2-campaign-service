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

func linkedInConversionsConfig(conversions string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}],
		"conversions":` + conversions + `
	}}`)
}

func TestLinkedIn_ConversionsRefusedWhileTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))
	cfg := linkedInConversionsConfig(`["104012"]`)

	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), constants.EnvLinkedInAdsConversionsEnabled) {
		t.Fatalf("PreflightCreate = %v, want a 400 naming %s", err, constants.EnvLinkedInAdsConversionsEnabled)
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

func TestLinkedIn_ConversionsPreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct{ name, conversions, want string }{
		{"valid", `["104012","107620"]`, ""},
		{"urn", `["urn:lla:llaPartnerConversion:1"]`, "is a URN"},
		{"empty", `[]`, "omit conversions"},
		{"not an id", `["abc"]`, "not a conversion rule id"},
		{"number entries", `[104012]`, "entries must be strings"},
		{"not an array", `"104012"`, "must be an array"},
		{"null", `null`, "must be an array"},
		{"a long list is refused before any per-entry work", `[` + strings.TrimSuffix(strings.Repeat(`"1",`, 41), ",") + `]`, "at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInConversionsConfig(tc.conversions))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.conversions") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_ConversionsReachTheCampaign pins the dispatcher wiring: the de-duplicated ids are
// associated with the created campaign, a failed association makes the row created_degraded, and
// the result blob records the split.
func TestLinkedIn_ConversionsReachTheCampaign(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	for _, tc := range []struct {
		name       string
		failID     string
		wantStatus string
	}{
		{"all associated", "", campaignStatusCreated},
		{"one fails", "107620", campaignStatusCreatedDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var puts []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				switch {
				case strings.Contains(r.URL.Path, "campaignConversions"):
					mu.Lock()
					puts = append(puts, r.URL.EscapedPath())
					mu.Unlock()
					if tc.failID != "" && strings.HasSuffix(r.URL.EscapedPath(), "%3A"+tc.failID+")") {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "adCampaigns"):
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

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInConversionsConfig(`["104012","107620","104012"]`))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			mu.Lock()
			n := len(puts)
			mu.Unlock()
			if n != 2 {
				t.Errorf("%d associations, want 2 (one per distinct id)", n)
			}
			if camp.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", camp.Status, tc.wantStatus)
			}
			var res linkedin.CampaignResult
			_ = json.Unmarshal(camp.Result, &res)
			if tc.failID != "" && strings.Join(res.ConversionsFailed, ",") != tc.failID {
				t.Errorf("result conversionsFailed = %v", res.ConversionsFailed)
			}
			if strings.Count(string(camp.ConfigSnapshot), "104012") != 1 {
				t.Errorf("snapshot does not hold the de-duplicated list: %s", camp.ConfigSnapshot)
			}
		})
	}
}
