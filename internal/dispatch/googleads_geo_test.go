// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// geoServers stands up a Google Ads API fake that serves the whole create cascade and
// records the criteria :mutate bodies per endpoint, so a test can assert WHICH endpoint
// received the location criteria as well as what they contained.
func geoServers(t *testing.T) ([]googleads.Option, *geoCapture) {
	t.Helper()
	cap := &geoCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "googleAds:search"):
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.campaignCriteria = body
			cap.mu.Unlock()
			_, _ = io.WriteString(w, criteriaResults(t, body, "campaignCriteria", "222"))
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			// The ad group receives BOTH keyword/audience criteria and (on Demand Gen)
			// location criteria, so keep them apart: a test asserting geo must not be
			// satisfied by a keyword payload that happened to hit the same endpoint.
			if strings.Contains(string(body), "geoTargetConstant") {
				cap.adGroupGeoCriteria = body
			} else {
				cap.adGroupOtherCriteria = body
			}
			cap.mu.Unlock()
			_, _ = io.WriteString(w, criteriaResults(t, body, "adGroupCriteria", "333"))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, cap
}

// criteriaResults echoes ONE result per requested operation. The client treats a
// result count that differs from the operation count as UNCONFIRMED (an unknown number
// of criteria may have committed), so a fixed-size fake would fail every test whose
// operation count it did not happen to match — and would hide a real count mismatch.
func criteriaResults(t *testing.T, body []byte, kind, parentID string) string {
	t.Helper()
	var req struct {
		Operations []json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode criteria request: %v", err)
	}
	parts := make([]string, 0, len(req.Operations))
	for i := range req.Operations {
		parts = append(parts, fmt.Sprintf(`{"resourceName":"customers/1234567890/%s/%s~%d"}`, kind, parentID, 901+i))
	}
	return `{"results":[` + strings.Join(parts, ",") + `]}`
}

type geoCapture struct {
	mu                   sync.Mutex
	campaignCriteria     []byte
	adGroupGeoCriteria   []byte
	adGroupOtherCriteria []byte
}

// geoOperations decodes the location criteria out of a captured :mutate body.
func geoOperations(t *testing.T, body []byte) []struct {
	Campaign string `json:"campaign"`
	AdGroup  string `json:"adGroup"`
	Location *struct {
		GeoTargetConstant string `json:"geoTargetConstant"`
	} `json:"location"`
} {
	t.Helper()
	var req struct {
		Operations []struct {
			Create struct {
				Campaign string `json:"campaign"`
				AdGroup  string `json:"adGroup"`
				Location *struct {
					GeoTargetConstant string `json:"geoTargetConstant"`
				} `json:"location"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode criteria body: %v (body=%s)", err, body)
	}
	out := make([]struct {
		Campaign string `json:"campaign"`
		AdGroup  string `json:"adGroup"`
		Location *struct {
			GeoTargetConstant string `json:"geoTargetConstant"`
		} `json:"location"`
	}, 0, len(req.Operations))
	for _, op := range req.Operations {
		out = append(out, op.Create)
	}
	return out
}

// The plumbing assertion: `geoTargets` in the dispatch config must reach the outbound
// campaignCriteria request as resolved Google constants. Distinctive per-country values
// (GB=2826, IN=2356) mean a mapping that collapsed both to one country, or swapped their
// order, fails here rather than passing silently.
func TestGoogleAds_SearchConfigGeoTargetsReachCampaignCriteria(t *testing.T) {
	opts, cap := geoServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"search","geoTargets":["GB","IN"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) == 0 {
		t.Fatal("no campaignCriteria:mutate request was sent — cfg.GeoTargets never reached the client")
	}
	if len(cap.adGroupGeoCriteria) != 0 {
		t.Errorf("Search geo must attach at CAMPAIGN level, but location criteria hit adGroupCriteria: %s", cap.adGroupGeoCriteria)
	}

	ops := geoOperations(t, cap.campaignCriteria)
	if len(ops) != 2 {
		t.Fatalf("got %d location operations, want 2: %s", len(ops), cap.campaignCriteria)
	}
	want := []string{"geoTargetConstants/2826", "geoTargetConstants/2356"}
	for i, op := range ops {
		if op.Location == nil || op.Location.GeoTargetConstant != want[i] {
			t.Errorf("operation %d: got %+v, want geoTargetConstant %q", i, op.Location, want[i])
		}
		if op.Campaign != "customers/1234567890/campaigns/222" {
			t.Errorf("operation %d: campaign = %q, want the created campaign resource", i, op.Campaign)
		}
		if op.AdGroup != "" {
			t.Errorf("operation %d: adGroup = %q, want empty on the Search path", i, op.AdGroup)
		}
	}
}

// The Demand Gen half: the SAME config field must route to the ad-group endpoint
// instead, because Demand Gen rejects campaign-level location criteria.
func TestGoogleAds_DemandGenConfigGeoTargetsReachAdGroupCriteria(t *testing.T) {
	opts, cap := geoServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","geoTargets":["MX"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) != 0 {
		t.Errorf("Demand Gen REJECTS campaign-level location criteria, but one was sent: %s", cap.campaignCriteria)
	}
	if len(cap.adGroupGeoCriteria) == 0 {
		t.Fatal("no ad-group location criteria were sent — cfg.GeoTargets never reached the Demand Gen path")
	}

	ops := geoOperations(t, cap.adGroupGeoCriteria)
	if len(ops) != 1 {
		t.Fatalf("got %d location operations, want 1: %s", len(ops), cap.adGroupGeoCriteria)
	}
	if ops[0].Location == nil || ops[0].Location.GeoTargetConstant != "geoTargetConstants/2484" {
		t.Errorf("got %+v, want geoTargetConstants/2484 (MX)", ops[0].Location)
	}
	if ops[0].AdGroup != "customers/1234567890/adGroups/333" {
		t.Errorf("adGroup = %q, want the created ad group resource", ops[0].AdGroup)
	}
	if ops[0].Campaign != "" {
		t.Errorf("campaign = %q, want empty on the Demand Gen path", ops[0].Campaign)
	}
}

// A config with no geoTargets must send no criteria request at all: the pre-LFXV2-3283
// behaviour is preserved for every caller predating the field.
func TestGoogleAds_NoGeoTargetsSendsNoLocationCriteria(t *testing.T) {
	opts, cap := geoServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) != 0 || len(cap.adGroupGeoCriteria) != 0 {
		t.Errorf("an untargeted create must send no location criteria; got campaign=%s adGroup=%s",
			cap.campaignCriteria, cap.adGroupGeoCriteria)
	}
}

// An unmapped country code must fail the dispatch BEFORE anything paid is created. The
// campaign id being absent is the assertion that matters: a create that reached Google
// would report 222.
func TestGoogleAds_UnmappedGeoFailsDispatchWithNoCampaign(t *testing.T) {
	opts, cap := geoServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"geoTargets":["USA"]}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil {
		t.Fatal("expected the dispatch to fail for an unmapped geo target")
	}
	if camp != nil && camp.PlatformCampaignID != "" {
		t.Errorf("no campaign may be created for an invalid geo target, got %q", camp.PlatformCampaignID)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaignCriteria) != 0 || len(cap.adGroupGeoCriteria) != 0 {
		t.Error("no criteria request may be sent when validation refuses the input")
	}
}
