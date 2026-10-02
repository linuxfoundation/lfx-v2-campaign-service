// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// servingCapture records the request bodies the serving-readiness fields land in.
type servingCapture struct {
	mu               sync.Mutex
	campaign         []byte
	campaignCriteria []byte
	adGroup          []byte
}

// servingServers is geoServers' sibling for the three serving-readiness fields:
// it additionally captures the campaign and ad-group create bodies, which is where
// the flight window and the CPC bid land.
func servingServers(t *testing.T) ([]googleads.Option, *servingCapture) {
	t.Helper()
	cap := &servingCapture{}
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
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.campaign = body
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.adGroup = body
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaignCriteria:mutate"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.campaignCriteria = body
			cap.mu.Unlock()
			writeCriteria(t, w, body, "campaignCriteria", "222")
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			body, _ := io.ReadAll(r.Body)
			writeCriteria(t, w, body, "adGroupCriteria", "333")
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, cap
}

// writeCriteria echoes the criteria results from inside a handler goroutine, where
// t.Fatalf (FailNow) must never be called. A malformed body is reported with
// t.Errorf and answered with a 500, which fails the create loudly rather than
// hanging the server while a deferred Close runs.
func writeCriteria(t *testing.T, w http.ResponseWriter, body []byte, kind, parentID string) {
	out, err := criteriaResultsOrErr(body, kind, parentID)
	if err != nil {
		t.Errorf("decode %s request: %v (body=%s)", kind, err, body)
		http.Error(w, "bad criteria request", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, out)
}

func servingCreate(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var req struct {
		Operations []struct {
			Create map[string]any `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v (body=%s)", err, body)
	}
	if len(req.Operations) == 0 {
		t.Fatalf("no operations in body %s", body)
	}
	return req.Operations[0].Create
}

// The plumbing assertion: cpcBid / startDate / endDate / negativeKeywords in the
// dispatch config must reach the outbound requests, at the right endpoints.
// Distinctive values mean a mapping that dropped one, or crossed start with end,
// fails here rather than passing silently.
func TestGoogleAds_SearchConfigServingReadinessFieldsReachTheAPI(t *testing.T) {
	opts, cap := servingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"search",` +
		`"cpcBid":2.5,"startDate":"2026-08-01","endDate":"2026-08-31",` +
		`"negativeKeywords":[{"text":"free","matchType":"BROAD"},{"text":"jobs","matchType":"PHRASE"}]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()

	cop := servingCreate(t, cap.campaign)
	// v23 names, with the account-timezone day boundaries. The pre-v23
	// startDate/endDate spellings are rejected as unrecognized fields.
	if cop["startDateTime"] != "2026-08-01 00:00:00" {
		t.Errorf("startDateTime = %v, want 2026-08-01 00:00:00", cop["startDateTime"])
	}
	if cop["endDateTime"] != "2026-08-31 23:59:59" {
		t.Errorf("endDateTime = %v, want 2026-08-31 23:59:59", cop["endDateTime"])
	}

	if len(cap.campaignCriteria) == 0 {
		t.Fatal("no campaignCriteria:mutate was sent — cfg.NegativeKeywords never reached the client")
	}
	var req struct {
		Operations []struct {
			Create struct {
				Campaign string `json:"campaign"`
				Negative bool   `json:"negative"`
				Keyword  *struct {
					Text      string `json:"text"`
					MatchType string `json:"matchType"`
				} `json:"keyword"`
			} `json:"create"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(cap.campaignCriteria, &req); err != nil {
		t.Fatalf("decode campaignCriteria: %v", err)
	}
	if len(req.Operations) != 2 {
		t.Fatalf("got %d negative operations, want 2: %s", len(req.Operations), cap.campaignCriteria)
	}
	wantText := []string{"free", "jobs"}
	wantMatch := []string{"BROAD", "PHRASE"}
	for i, op := range req.Operations {
		if !op.Create.Negative {
			t.Errorf("operation %d is not marked negative — it would create a POSITIVE campaign keyword", i)
		}
		if op.Create.Campaign != "customers/1234567890/campaigns/222" {
			t.Errorf("operation %d: campaign = %q", i, op.Create.Campaign)
		}
		if op.Create.Keyword == nil || op.Create.Keyword.Text != wantText[i] || op.Create.Keyword.MatchType != wantMatch[i] {
			t.Errorf("operation %d: keyword = %+v, want %s/%s", i, op.Create.Keyword, wantText[i], wantMatch[i])
		}
	}

	agop := servingCreate(t, cap.adGroup)
	if agop["cpcBidMicros"] != "2500000" && agop["cpcBidMicros"] != float64(2_500_000) {
		t.Errorf("cpcBidMicros = %#v, want 2500000 micros for a 2.50 bid", agop["cpcBidMicros"])
	}
}

// Omitting the three fields must reproduce the pre-feature request exactly: no date
// fields, no bid field, no campaignCriteria call at all. Every caller predating them
// omits them, and their creates must not change shape.
func TestGoogleAds_SearchConfigWithoutServingReadinessFieldsIsUnchanged(t *testing.T) {
	opts, cap := servingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"search"}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, f := range []string{"startDateTime", "endDateTime", "startDate", "endDate"} {
		if strings.Contains(string(cap.campaign), f) {
			t.Errorf("campaign create emits %q with no flight window configured: %s", f, cap.campaign)
		}
	}
	if strings.Contains(string(cap.adGroup), "cpcBidMicros") {
		t.Errorf("ad group create emits cpcBidMicros with no bid configured: %s", cap.adGroup)
	}
	if len(cap.campaignCriteria) != 0 {
		t.Errorf("a campaignCriteria:mutate was sent with nothing to attach: %s", cap.campaignCriteria)
	}
}

// The flight window is channel-agnostic and comes from the shared preflight, so it
// must reach the Demand Gen payload too — which carries its own create shape.
func TestGoogleAds_DemandGenConfigFlightWindowReachesCampaignCreate(t *testing.T) {
	opts, cap := servingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","startDate":"2026-08-01","endDate":"2026-08-31"}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	cop := servingCreate(t, cap.campaign)
	if cop["advertisingChannelType"] != "DEMAND_GEN" {
		t.Fatalf("channel = %v, want DEMAND_GEN", cop["advertisingChannelType"])
	}
	if cop["startDateTime"] != "2026-08-01 00:00:00" || cop["endDateTime"] != "2026-08-31 23:59:59" {
		t.Errorf("flight window = %v / %v", cop["startDateTime"], cop["endDateTime"])
	}
	// Demand Gen rejects both of these, and the shared preflight must not have
	// leaked the Search-only fields into this payload.
	if _, ok := cop["manualCpc"]; ok {
		t.Error("demand gen create must not carry manualCpc")
	}
	if _, ok := cop["networkSettings"]; ok {
		t.Error("demand gen create must not carry networkSettings")
	}
}

// A bad serving-readiness value must be refused as a PRE-CREATE failure: nothing
// upstream exists, so the dispatch has nothing to reconcile.
func TestGoogleAds_BadServingReadinessConfigIsPreCreate(t *testing.T) {
	for name, cfgJSON := range map[string]string{
		"bid over the maximum":    `{"googleAdsConfig":{"budget":50,"cpcBid":100000}}`,
		"malformed start date":    `{"googleAdsConfig":{"budget":50,"startDate":"2026-8-1"}}`,
		"end before start":        `{"googleAdsConfig":{"budget":50,"startDate":"2026-08-31","endDate":"2026-08-01"}}`,
		"bad negative match type": `{"googleAdsConfig":{"budget":50,"negativeKeywords":[{"text":"free","matchType":"FUZZY"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := servingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(cfgJSON)); err == nil {
				t.Fatal("expected a dispatch error")
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if len(cap.campaign) != 0 || len(cap.adGroup) != 0 {
				t.Error("a local input error must not reach any create mutate")
			}
		})
	}
}
