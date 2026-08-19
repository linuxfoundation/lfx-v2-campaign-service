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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// keywordActionServers wires a token endpoint plus an API server for the mutate path, and
// records whether the API was reached at all — which is what the "never contacted" guards
// below assert.
func keywordActionServers(t *testing.T, apiH http.HandlerFunc) ([]googleads.Option, *bool) {
	t.Helper()
	var mu sync.Mutex
	reached := false
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		apiH(w, r)
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, &reached
}

// provisionedGoogleAdsCampaign is a campaign whose Result blob carries the ad group id the
// keyword-action guard requires, plus the customer id the identity guard compares against.
func provisionedGoogleAdsCampaign(customerID string) *model.Campaign {
	blob, _ := json.Marshal(map[string]any{
		"adGroupId":  "333",
		"adId":       "444",
		"customerId": customerID,
	})
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "555",
		Result:             blob,
	}
}

// adGroupOnlyResult is a Result blob carrying a provisioned ad group but nothing else, so a
// test can isolate the PlatformCampaignID guard from the ad-group guard.
func adGroupOnlyResult() json.RawMessage {
	blob, _ := json.Marshal(map[string]any{"adGroupId": "333", "adId": "444"})
	return blob
}

func pauseAction(adGroupID, criterionID string) []model.KeywordAction {
	return []model.KeywordAction{{AdGroupID: adGroupID, CriterionID: criterionID, Action: model.KeywordActionPause}}
}

// The identity guard is the reason this endpoint cannot pause another account's keywords.
// Criterion ids are bare numerics unique only within their customer, and a connection can be
// re-pointed between create and action — so without this, a mutate would REMOVE a real
// keyword belonging to somebody else.
func TestGoogleAdsKeywordActions_AccountMismatchIsRefusedBeforeContact(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	// The connection resolves customer 1234567890; the campaign records a different one.
	campaign := provisionedGoogleAdsCampaign("9999999999")
	_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign, pauseAction("333", "777"))
	if err == nil {
		t.Fatal("expected an account-mismatch refusal, got nil")
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("error is not ErrCampaignAccountMismatch: %v", err)
	}
	if *reached {
		t.Error("the platform was contacted despite an account mismatch")
	}
}

// A criterion from another ad group must not be actionable through a campaign the caller
// happens to own: the path is permission-evaluated on the CAMPAIGN, so the campaign is what
// bounds which criteria may be touched.
func TestGoogleAdsKeywordActions_ForeignAdGroupIsRefusedBeforeContact(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/888~777"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	campaign := provisionedGoogleAdsCampaign("1234567890") // ad group 333
	_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign, pauseAction("888", "777"))
	if err == nil {
		t.Fatal("expected a refusal for a criterion in another ad group, got nil")
	}
	if !errors.Is(err, domain.ErrKeywordActionInvalid) {
		t.Errorf("error is not ErrKeywordActionInvalid: %v", err)
	}
	if *reached {
		t.Error("the platform was contacted for a criterion outside this campaign's ad group")
	}
}

func TestGoogleAdsKeywordActions_UnprovisionedCampaignIsRefusedBeforeContact(t *testing.T) {
	for _, tc := range []struct {
		name     string
		campaign *model.Campaign
	}{
		// This case pins the PlatformCampaignID guard SPECIFICALLY: the Result blob carries a
		// valid ad group, so the ad-group guard below cannot fire and only the missing
		// platform campaign id can produce the refusal. Without it, both cases were caught by
		// the ad-group check and deleting the PlatformCampaignID guard broke no test.
		{"no platform campaign id", &model.Campaign{Platform: model.ProviderGoogleAds, Result: adGroupOnlyResult()}},
		{"no ad group", &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "555"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[]}`)
			})
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, tc.campaign, pauseAction("333", "777"))
			if err == nil {
				t.Fatal("expected a not-provisioned refusal, got nil")
			}
			if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
				t.Errorf("error is not ErrCampaignNotProvisioned: %v", err)
			}
			if *reached {
				t.Error("the platform was contacted for an unprovisioned campaign")
			}
		})
	}
}

// A malformed batch must be refused before credentials are even decrypted, so a permanent
// input fault never masquerades as a connection problem.
func TestGoogleAdsKeywordActions_InvalidBatchRefusedBeforeCredentials(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	// An encryptor that would FAIL if it were reached. The batch is invalid, so validation
	// must short-circuit before any credential work happens.
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, errEncryptor{}, opts...)

	campaign := provisionedGoogleAdsCampaign("1234567890")
	_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign,
		[]model.KeywordAction{{AdGroupID: "333", CriterionID: "not-numeric", Action: model.KeywordActionPause}})
	if err == nil {
		t.Fatal("expected an invalid-batch refusal, got nil")
	}
	if !errors.Is(err, domain.ErrKeywordActionInvalid) {
		t.Errorf("error is not ErrKeywordActionInvalid (it may have failed on credentials instead): %v", err)
	}
	if *reached {
		t.Error("the platform was contacted for an invalid batch")
	}
}

// A legacy row that records no customer id means "unknown", and every other guard on this
// dispatcher reads it as permission to proceed. Treating it as a mismatch would break
// keyword actions for every campaign created before provenance tracking existed.
func TestGoogleAdsKeywordActions_UnknownCreationAccountProceeds(t *testing.T) {
	var gotPath string
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	// Result carries the ad group but NO customerId.
	blob, _ := json.Marshal(map[string]any{"adGroupId": "333", "adId": "444"})
	campaign := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "555", Result: blob}

	out, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign, pauseAction("333", "777"))
	if err != nil {
		t.Fatalf("a legacy row with no recorded customer id must proceed: %v", err)
	}
	if len(out) != 1 || out[0].CriterionID != "777" {
		t.Fatalf("outcomes = %+v", out)
	}
	if !strings.HasSuffix(gotPath, "adGroupCriteria:mutate") {
		t.Errorf("unexpected upstream path %q", gotPath)
	}
}

func TestGoogleAdsKeywordActions_HappyPathReturnsOutcomes(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	campaign := provisionedGoogleAdsCampaign("1234567890")
	out, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign, pauseAction("333", "777"))
	if err != nil {
		t.Fatalf("ApplyKeywordActions: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("outcomes = %d, want 1", len(out))
	}
	if out[0].AdGroupID != "333" || out[0].CriterionID != "777" || out[0].Action != model.KeywordActionPause {
		t.Errorf("outcome = %+v", out[0])
	}
}

// ─── reads ───

func TestGoogleAdsReadKeywordPerformance_MapsRowsAndKeepsRequestWindow(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"777","status":"ENABLED","keyword":{"text":"kw","matchType":"EXACT"}},`+
			`"adGroup":{"id":"333"},"campaign":{"id":"555"},"metrics":{"impressions":"200","clicks":"10","costMicros":"1000"}}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	kp, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadKeywordPerformance: %v", err)
	}
	// The REQUEST window must come back, not the platform's GAQL literal — translating back
	// would reintroduce the platform dialect into the API contract.
	if kp.Window != model.MetricsWindowLast7Days {
		t.Errorf("Window = %q, want the request window %q", kp.Window, model.MetricsWindowLast7Days)
	}
	if len(kp.Rows) != 1 || kp.Rows[0].CriterionID != "777" || kp.Rows[0].AdGroupID != "333" {
		t.Fatalf("rows = %+v", kp.Rows)
	}
	if kp.Rows[0].Impressions != 200 || kp.Rows[0].Clicks != 10 {
		t.Errorf("metrics not mapped: %+v", kp.Rows[0])
	}
}

// An unsupported window is a permanent input fault and must be refused before credentials
// are touched, so it cannot be masked by a contingent connection failure.
func TestGoogleAdsReadKeywordPerformance_BadWindowRefusedBeforeCredentials(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, errEncryptor{}, opts...)

	_, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindow("next_tuesday"))
	if err == nil {
		t.Fatal("expected an unsupported-window error, got nil")
	}
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Errorf("error is not ErrMetricsWindowUnsupported (it may have failed on credentials): %v", err)
	}
	if *reached {
		t.Error("the platform was contacted for an unsupported window")
	}
}

func TestGoogleAdsReadAudienceInsights_MapsBucketsAndKeepsRequestWindow(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), "FROM age_range_view") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},"metrics":{"impressions":"100","clicks":"10","costMicros":"500"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	ai, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days)
	if err != nil {
		t.Fatalf("ReadAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowLast7Days {
		t.Errorf("Window = %q, want %q", ai.Window, model.MetricsWindowLast7Days)
	}
	if len(ai.Buckets) != 1 || ai.Buckets[0].Value != "AGE_RANGE_25_34" || ai.Buckets[0].Dimension != model.AudienceDimensionAge {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
}

// An unusable connection must surface as ErrConnectionNotUsable so the service layer answers
// "repair your connection" rather than "retry later".
func TestGoogleAdsKeywordInsights_UnusableConnectionIsTagged(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	// Inactive connection: a defect the caller must edit, never retry.
	inactive := &model.Connection{
		Provider:             model.ProviderGoogleAds,
		AccountID:            "1234567890",
		EncryptedCredentials: []byte(goodGoogleAdsCreds),
		Status:               model.StatusInactive,
	}
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: inactive}, identityEncryptor{}, opts...)

	if _, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("keyword read: error is not ErrConnectionNotUsable: %v", err)
	}
	if _, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days); !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("audience read: error is not ErrConnectionNotUsable: %v", err)
	}
}
