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
	"reflect"
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

// LinkedIn Lead Gen Form campaigns (#664).

const leadGenOK = `"objective":"lead_generation","leadGenForm":"6755260984438374400"`

// TestLinkedIn_LeadGenPreflight pins the synchronous 400s: any lead gen input while the flag is
// off (any value but exactly "true"), a manual bid, a missing form, a form without the objective,
// a wrong JSON type, a malformed id, and what a lead gen campaign cannot be.
func TestLinkedIn_LeadGenPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	manual := `"bidding":{"strategy":"manual","bidAmount":5}`
	for _, tc := range []struct {
		name, flag string
		config     json.RawMessage
		want       string // "" = not applicable (accepted)
	}{
		{"omitted, flag off", "", linkedInObjectiveConfig(""), ""},
		{"objective, flag off", "", linkedInObjectiveConfig(leadGenOK), `linkedInConfig.objective "lead_generation" disabled on this deployment (LINKEDIN_ADS_LEAD_GEN_ENABLED`},
		{"form alone, flag not exactly true", "TRUE", linkedInObjectiveConfig(`"leadGenForm":"1"`), "linkedInConfig.leadGenForm disabled on this deployment"},
		{"accepted", "true", linkedInObjectiveConfig(leadGenOK), ""},
		{"accepted with an adForm URN and a label", "true", linkedInObjectiveConfig(`"objective":"lead_generation","leadGenForm":"urn:li:adForm:42","leadGenCallToAction":"APPLY"`), ""},
		{"manual bid", "true", linkedInObjectiveConfig(leadGenOK + "," + manual), "automatic bidding"},
		{"no form", "true", linkedInObjectiveConfig(`"objective":"lead_generation"`), "requires a Lead Gen Form"},
		{"form without the objective", "true", linkedInObjectiveConfig(`"leadGenForm":"1"`), `used only with linkedInConfig.objective "lead_generation"`},
		{"form a number", "true", linkedInObjectiveConfig(`"objective":"lead_generation","leadGenForm":42`), "linkedInConfig.leadGenForm must be a string"},
		{"malformed id", "true", linkedInObjectiveConfig(`"objective":"lead_generation","leadGenForm":"42abc"`), "must be a Lead Gen Form id"},
		{"unknown label", "true", linkedInObjectiveConfig(leadGenOK + `,"leadGenCallToAction":"CLICK"`), "leadGenCallToAction must be one of"},
		{"audience network", "true", linkedInObjectiveConfig(leadGenOK + `,"offsiteDeliveryEnabled":true`), "offsiteDeliveryEnabled true is not available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, tc.flag)
			t.Setenv(constants.EnvLinkedInAdsDeliveryOptionsEnabled, "true")
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

// TestLinkedIn_LeadGenRefusedInDispatch drives the refusals through Dispatch, the path a job
// takes: nothing may reach LinkedIn.
func TestLinkedIn_LeadGenRefusedInDispatch(t *testing.T) {
	for _, tc := range []struct{ name, flag, extra string }{
		{"flag off", "", leadGenOK},
		{"mistyped", "true", `"objective":"lead_generation","leadGenForm":42`},
		{"malformed id", "true", `"objective":"lead_generation","leadGenForm":"1/../2"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, tc.flag)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer srv.Close()
			d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
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

// TestLinkedIn_LeadGenReachesTheCreative pins the wiring: an accepted lead gen create reaches
// LinkedIn as a LEAD_GENERATION campaign whose creative opens the named form.
func TestLinkedIn_LeadGenReachesTheCreative(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsLeadGenEnabled, "true")
	var mu sync.Mutex
	var campaign, creative map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if strings.HasPrefix(r.URL.Path, "/leadForms/") {
				_, _ = io.WriteString(w, `{"id":6755260984438374400,"state":"PUBLISHED","owner":{"sponsoredAccount":"urn:li:sponsoredAccount:123456789"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			campaign = body
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			creative = body
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInObjectiveConfig(leadGenOK)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if campaign["objectiveType"] != "LEAD_GENERATION" || campaign["optimizationTargetType"] != "MAX_LEAD" {
		t.Errorf("campaign = %v, want LEAD_GENERATION / MAX_LEAD", campaign)
	}
	want := map[string]any{"destination": "urn:li:adForm:6755260984438374400", "label": "REGISTER"}
	if !reflect.DeepEqual(creative["leadgenCallToAction"], want) {
		t.Errorf("leadgenCallToAction = %v, want %v", creative["leadgenCallToAction"], want)
	}
}
