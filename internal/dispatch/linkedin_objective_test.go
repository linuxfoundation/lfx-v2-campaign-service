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

// linkedInObjectiveConfig is a full linkedInConfig with extra raw members (objective, bidding,
// conversions) spliced in, e.g. `"objective":"conversions","conversions":["104012"]`.
func linkedInObjectiveConfig(extra string) json.RawMessage {
	if extra != "" {
		extra = "," + extra
	}
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]` + extra + `
	}}`)
}

// TestLinkedIn_ObjectivePreflight pins linkedInConfig.objective's synchronous 400s (#582), and
// that every accepted shape is "not applicable" (no refusal).
func TestLinkedIn_ObjectivePreflight(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	const manual = `"bidding":{"strategy":"manual","bidAmount":4.5}`
	for _, tc := range []struct{ name, extra, want string }{
		{"omitted", ``, ""},
		{"traffic automatic", `"objective":"traffic"`, ""},
		{"conversions automatic with conversions", `"objective":"conversions","conversions":["104012"]`, ""},
		{"conversions manual without conversions", `"objective":"conversions",` + manual, ""},
		{"conversions automatic without conversions", `"objective":"conversions"`, "requires linkedInConfig.conversions"},
		{"conversions explicit automatic without conversions", `"objective":"conversions","bidding":{"strategy":"automatic"}`, "requires linkedInConfig.conversions"},
		{"traffic manual", `"objective":"traffic",` + manual, "automatic bidding only"},
		{"unknown value", `"objective":"leads"`, `must be "traffic" or "conversions"`},
		{"case variant value", `"objective":"Conversions","conversions":["104012"]`, `must be "traffic" or "conversions"`},
		{"empty", `"objective":""`, "must be the string"},
		{"null", `"objective":null`, "must be the string"},
		{"number", `"objective":1`, "must be the string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.objective") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// TestLinkedIn_ObjectiveConversionsNamesTheConversionsGate: with the conversions flag off, the
// refusal of automatic conversions says why conversions cannot simply be added.
func TestLinkedIn_ObjectiveConversionsNamesTheConversionsGate(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "")
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInObjectiveConfig(`"objective":"conversions"`))
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "requires linkedInConfig.conversions") ||
		!strings.Contains(err.Error(), constants.EnvLinkedInAdsConversionsEnabled) {
		t.Fatalf("PreflightCreate = %v, want a 400 naming the conversions requirement and %s", err, constants.EnvLinkedInAdsConversionsEnabled)
	}
}

// TestLinkedIn_ObjectiveRefusedByDispatchSendsNothing: the objective refusals, reached through
// Dispatch (a path that skipped the preflight), fail not-created with no LinkedIn request. The null
// and "" cases pin Dispatch's own checkObjectiveShape: without it both decode to "" — no objective —
// and would create a paid WEBSITE_VISIT campaign.
func TestLinkedIn_ObjectiveRefusedByDispatchSendsNothing(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	for _, tc := range []struct{ name, extra string }{
		{"conversions without conversions", `"objective":"conversions"`},
		{"null objective", `"objective":null`},
		{"empty objective", `"objective":""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

// TestLinkedIn_ObjectiveReachesTheWire pins the dispatcher wiring end to end against an httptest
// LinkedIn (#582): each linkedInConfig shape produces the campaign POST body, name segment and
// recorded strategy it promises, and the snapshot keeps the objective the settings readback reads.
func TestLinkedIn_ObjectiveReachesTheWire(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsConversionsEnabled, "true")
	for _, tc := range []struct {
		name, extra                     string
		objective, target, costType     string
		nameSegment                     string
		wantStrategy                    model.BiddingStrategy
		wantConversionPuts              int
		wantRecordedObjectiveOnReadback string
	}{
		{"omitted automatic stays traffic", ``, "WEBSITE_VISIT", "MAX_CLICK", "CPM", "Traffic", model.BiddingAutomatic, 0, "WEBSITE_VISIT"},
		{"explicit traffic", `"objective":"traffic"`, "WEBSITE_VISIT", "MAX_CLICK", "CPM", "Traffic", model.BiddingAutomatic, 0, "WEBSITE_VISIT"},
		{"automatic conversions", `"objective":"conversions","conversions":["104012"]`, "WEBSITE_CONVERSION", "MAX_CONVERSION", "CPM", "Conversions", model.BiddingAutomatic, 1, "WEBSITE_CONVERSION"},
		{"manual conversions", `"objective":"conversions","bidding":{"strategy":"manual","bidAmount":4.5}`, "WEBSITE_CONVERSION", "NONE", "CPC", "Conversions", model.BiddingManual, 0, "WEBSITE_CONVERSION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var campaignBody map[string]any
			conversionPuts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.Contains(r.URL.Path, "campaignConversions"):
					conversionPuts++
					w.WriteHeader(http.StatusCreated)
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "adCampaigns"):
					raw, _ := io.ReadAll(r.Body)
					_ = json.Unmarshal(raw, &campaignBody)
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

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, linkedInObjectiveConfig(tc.extra))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			mu.Lock()
			b, puts := campaignBody, conversionPuts
			mu.Unlock()
			if b["objectiveType"] != tc.objective || b["optimizationTargetType"] != tc.target || b["costType"] != tc.costType {
				t.Fatalf("campaign POST objectiveType=%v optimizationTargetType=%v costType=%v, want %s/%s/%s",
					b["objectiveType"], b["optimizationTargetType"], b["costType"], tc.objective, tc.target, tc.costType)
			}
			if _, has := b["unitCost"]; has != (tc.wantStrategy == model.BiddingManual) {
				t.Errorf("unitCost present=%v for strategy %s", has, tc.wantStrategy)
			}
			if puts != tc.wantConversionPuts {
				t.Errorf("%d conversion associations, want %d", puts, tc.wantConversionPuts)
			}
			if !strings.Contains(camp.CampaignName, "| LinkedIn | "+tc.nameSegment+" |") {
				t.Errorf("campaign name %q lacks the %s segment", camp.CampaignName, tc.nameSegment)
			}
			if camp.BiddingStrategy == nil || *camp.BiddingStrategy != tc.wantStrategy {
				t.Fatalf("recorded strategy = %v, want %s", camp.BiddingStrategy, tc.wantStrategy)
			}
			if got := linkedinRecordedObjective(context.Background(), camp); got == nil || *got != tc.wantRecordedObjectiveOnReadback {
				t.Errorf("settings readback's recorded objective = %v, want %s (snapshot %s)", got, tc.wantRecordedObjectiveOnReadback, camp.ConfigSnapshot)
			}
		})
	}
}
