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

// scopeOf builds a campaign scope carrying NO provenance, the legacy shape: rows written
// before the creating customer was recorded. Those are read under the current connection
// (unknown cannot prove a mismatch), so it is the right default for tests about everything
// other than the account-identity filter itself — which uses scopeUnderCustomer.
func scopeOf(ids ...string) []model.ProjectCampaignScope {
	out := make([]model.ProjectCampaignScope, 0, len(ids))
	for _, id := range ids {
		out = append(out, model.ProjectCampaignScope{PlatformCampaignID: id})
	}
	return out
}

// scopeUnderCustomer builds a scope entry whose recorded provenance names customerID, the
// shape a campaign dispatched after provenance tracking has.
func scopeUnderCustomer(id, customerID string) model.ProjectCampaignScope {
	return model.ProjectCampaignScope{
		PlatformCampaignID: id,
		Result:             json.RawMessage(`{"customerId":"` + customerID + `"}`),
	}
}

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

// A legacy row recording no creating customer must FAIL CLOSED on this path, unlike the
// reads, which proceed.
//
// Google Ads is one customer shared across every foundation and criterion ids are
// account-scoped bare numerics, so an unrecorded tenant cannot prove the criteria belong to
// the campaign the caller addressed. A read that guesses wrong shows wrong numbers; a REMOVE
// that guesses wrong irreversibly deletes another project's keyword. The platform must not be
// contacted at all.
func TestGoogleAdsKeywordActions_UnknownCreationAccountFailsClosed(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	// Result carries the ad group but NO customerId.
	blob, _ := json.Marshal(map[string]any{"adGroupId": "333", "adId": "444"})
	campaign := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "555", Result: blob}

	_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds, campaign, pauseAction("333", "777"))
	if err == nil {
		t.Fatal("expected an unrecorded creating account to be refused, got nil")
	}
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Errorf("error is not ErrCampaignProvenanceUnknown: %v", err)
	}
	if *reached {
		t.Error("the platform was contacted for a campaign whose ad account is unrecorded")
	}
}

func TestGoogleAdsKeywordActions_HappyPathReturnsOutcomes(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The client resolves each criterion's TYPE before mutating; 333~777 is a positive
		// keyword, so the mutate proceeds.
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"777","negative":false},"adGroup":{"id":"333"}}]}`)
			return
		}
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

// TestGoogleAdsInsightReads_ForeignAccountCampaignsAreNotQueried is the insight reads' half of
// the account-identity invariant ReadMetrics and ApplyKeywordActions already enforce.
//
// A platform_campaign_id is a bare numeric unique only WITHIN its customer, and
// UpdateGoogleAds can re-point a project's connection between create and read. An id carried
// over from the old customer either matches nothing — an empty read indistinguishable from a
// campaign with no activity — or, on a numeric collision, selects ANOTHER account's campaign.
// On a customer shared across every foundation that means reporting a different project's
// keyword text and spend as this project's own.
//
// A MIXED-provenance scope fails the WHOLE read rather than returning the matching subset.
//
// Dropping only the mismatched entries returns a silent partial result: both endpoints report
// success, and neither response carries an omitted-campaign signal of any kind, so a caller
// cannot tell a project whose other campaigns were filtered out from one that genuinely has
// none. For the audience read a distribution computed over half a project's campaigns looks
// exactly like a complete one, and is what a re-targeting decision is then made on.
//
// The assertion is on BOTH halves — the sentinel AND that no query was issued at all. A test
// checking only that "999" is absent from the query would pass against the partial-result bug,
// because under that bug the query goes out carrying just "555".
func TestGoogleAdsInsightReads_MixedProvenanceScopeFailsClosed(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	// 555 was created under the customer this connection resolves to; 999 under another.
	scope := []model.ProjectCampaignScope{
		scopeUnderCustomer("555", "1234567890"),
		scopeUnderCustomer("999", "5555555555"),
	}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"keywords", func() error {
			_, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scope)
			return err
		}},
		{"audience", func() error {
			_, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scope)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*reached = false
			err := tc.call()
			if err == nil {
				t.Fatal("a mixed-provenance scope returned success; a partial result was reported as complete")
			}
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("error is not ErrCampaignAccountMismatch (the sentinel the 409 arm maps): %v", err)
			}
			if *reached {
				t.Error("a GAQL query was issued for a partially-mismatched scope; the read must refuse before querying")
			}
		})
	}
}

// A scope that is non-empty on the way in but empty after the provenance filter must be an
// ERROR, never a query. Passing an empty id list on would make campaignScopePredicate refuse
// anyway, but relying on that leaves the account-wide read one dropped guard away.
func TestGoogleAdsInsightReads_AllForeignScopeIsRefusedWithoutQuerying(t *testing.T) {
	opts, reached := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	scope := []model.ProjectCampaignScope{scopeUnderCustomer("999", "5555555555")}
	_, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scope)
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("error is not ErrCampaignAccountMismatch: %v", err)
	}
	if *reached {
		t.Error("the platform was queried after every campaign was filtered out; an empty " +
			"scope is exactly when an unscoped read exposes every other project")
	}
}

// A row with NO recorded provenance is READ, matching googleAdsCreationCustomerID's contract
// ("empty means unknown, and the caller must treat that as permission to proceed") and the two
// sibling read paths. Dropping them would silently empty the results of every project whose
// campaigns predate provenance tracking. The asymmetry against ApplyKeywordActions — which
// fails CLOSED on unknown provenance — is deliberate: a misleading read is recoverable, an
// irreversible REMOVE is not.
func TestGoogleAdsInsightReads_UnknownProvenanceIsStillRead(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	if _, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scopeOf("555")); err != nil {
		t.Fatalf("ReadAudienceInsights: %v", err)
	}
	mu.Lock()
	got := append([]string{}, bodies...)
	mu.Unlock()
	if len(got) == 0 || !strings.Contains(got[0], "555") {
		t.Errorf("a legacy row with no recorded customer was dropped from the scope; every "+
			"project whose campaigns predate provenance tracking would read empty: %v", got)
	}
}

func TestGoogleAdsReadKeywordPerformance_MapsRowsAndKeepsRequestWindow(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"777","status":"ENABLED","keyword":{"text":"kw","matchType":"EXACT"}},`+
			`"adGroup":{"id":"333"},"campaign":{"id":"555"},"metrics":{"impressions":"200","clicks":"10","costMicros":"1000"}}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	kp, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scopeOf("555"))
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

	_, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindow("next_tuesday"), scopeOf("555"))
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

	ai, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast7Days, scopeOf("555"))
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

// The platform package and the model package each declare a dimension vocabulary, and the
// dispatcher copies the token straight through without mapping. Nothing else pins them
// together: changing googleads.DimensionGender alone left the whole suite green while the
// response would have violated the design's Enum("age","gender","device"). Assert all three
// pairs so a divergence fails here rather than at a consumer.
func TestGoogleAdsReadAudienceInsights_DimensionTokensMatchTheModelVocabulary(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(b), "FROM age_range_view"):
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"ageRange":{"type":"AGE_RANGE_25_34"}},"metrics":{"impressions":"10","clicks":"1","costMicros":"5"}}]}`)
		case strings.Contains(string(b), "FROM gender_view"):
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"gender":{"type":"MALE"}},"metrics":{"impressions":"20","clicks":"2","costMicros":"10"}}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[{"segments":{"device":"MOBILE"},"metrics":{"impressions":"30","clicks":"3","costMicros":"15"}}]}`)
		}
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	ai, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days, scopeOf("555"))
	if err != nil {
		t.Fatalf("ReadAudienceInsights: %v", err)
	}
	got := map[string]bool{}
	for _, b := range ai.Buckets {
		got[b.Dimension] = true
	}
	// These are the exact three tokens design/brief.go declares in Enum("age","gender","device").
	for _, want := range []string{model.AudienceDimensionAge, model.AudienceDimensionGender, model.AudienceDimensionDevice} {
		if !got[want] {
			t.Errorf("no bucket carried dimension %q; got %v — the platform and model vocabularies have drifted, and the response would violate the design Enum", want, got)
		}
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

	if _, err := d.ReadKeywordPerformance(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days, scopeOf("555")); !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("keyword read: error is not ErrConnectionNotUsable: %v", err)
	}
	if _, err := d.ReadAudienceInsights(context.Background(), "p1", model.ProviderGoogleAds, model.MetricsWindowLast30Days, scopeOf("555")); !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("audience read: error is not ErrConnectionNotUsable: %v", err)
	}
}

// A criterion that is not a POSITIVE KEYWORD must reach the service as a PERMANENT input
// fault (ErrKeywordActionInvalid → 400), not as a retryable upstream failure.
//
// This covers the real client→dispatcher path rather than a fake: the guard lives in the
// client, and the folding onto the domain sentinel lives here, so only running both together
// proves a caller is told "these actions are not valid" instead of being invited to retry an
// action that can never succeed.
func TestGoogleAdsKeywordActions_NonKeywordCriterionIsAPermanentFault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		search string
	}{
		// A NEGATIVE keyword: acting on it would remove an exclusion and WIDEN spend.
		{"negative keyword", `{"adGroupCriterion":{"criterionId":"777","negative":true},"adGroup":{"id":"333"}}`},
		// A userList/audience criterion: keyword_view returns no row for it at all.
		{"userList criterion", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := false
			opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "googleAds:search") {
					_, _ = io.WriteString(w, `{"results":[`+tc.search+`]}`)
					return
				}
				mutated = true
				_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~777"}]}`)
			})
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

			_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds,
				provisionedGoogleAdsCampaign("1234567890"), pauseAction("333", "777"))
			if err == nil {
				t.Fatal("a non-keyword criterion was accepted; this endpoint can then widen delivery")
			}
			if !errors.Is(err, domain.ErrKeywordActionInvalid) {
				t.Errorf("error is not ErrKeywordActionInvalid, so the service answers 503-retry rather than 400: %v", err)
			}
			if mutated {
				t.Error("the mutate was issued for a criterion that is not a positive keyword")
			}
		})
	}
}

// An UNCONFIRMED client outcome must survive the dispatcher as an Unconfirmed()-marked error.
//
// The dispatcher previously returned the client error raw, unlike the toggle path. Since the
// service detects ambiguity ONLY through that interface, anything the client marks structurally
// must still be detectable here — otherwise an atomic batch Google may already have applied
// (including an irreversible REMOVE) is reported as a clean failure and a retry is invited.
func TestGoogleAdsKeywordActions_UnconfirmedOutcomeSurvivesTheDispatcher(t *testing.T) {
	opts, _ := keywordActionServers(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"adGroupCriterion":{"criterionId":"777","negative":false},"adGroup":{"id":"333"}}]}`)
			return
		}
		// A 2xx naming a criterion the batch never addressed: the mutate MAY have applied.
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/333~888"}]}`)
	})
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	_, err := d.ApplyKeywordActions(context.Background(), "p1", model.ProviderGoogleAds,
		provisionedGoogleAdsCampaign("1234567890"), pauseAction("333", "777"))
	if err == nil {
		t.Fatal("expected an UNCONFIRMED outcome to surface as an error, got nil")
	}
	// Detected exactly as classifyKeywordActionError detects it — by behaviour, not by text.
	var unconfirmed interface{ Unconfirmed() bool }
	if !errors.As(err, &unconfirmed) || !unconfirmed.Unconfirmed() {
		t.Errorf("the dispatcher dropped the unconfirmed marker; the service will report this "+
			"possibly-applied batch as a definite failure and invite a retry: %v", err)
	}
}
