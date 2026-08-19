// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// ---- fakes ----------------------------------------------------------------

// fakeConnReader returns a preset connection (or error) regardless of args.
type fakeConnReader struct {
	conn *model.Connection
	err  error
}

func (f fakeConnReader) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	return f.conn, f.err
}

// Disconnected: these tests are about a project that HAS a connection, so no tombstone exists.
func (f fakeConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// identityEncryptor treats ciphertext as plaintext, so tests can put readable JSON in
// EncryptedCredentials. errEncryptor always fails Decrypt.
type identityEncryptor struct{}

func (identityEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (identityEncryptor) Decrypt(c []byte) ([]byte, error) { return c, nil }

// errEncryptor always fails Decrypt with an UNCLASSIFIED error — one carrying neither
// domain sentinel. That is deliberate: it is the default arm of creds.resolve's
// classification, and the arm that must not guess the row is at fault.
type errEncryptor struct{}

func (errEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (errEncryptor) Decrypt([]byte) ([]byte, error)   { return nil, errors.New("bad key") }

// malformedEncryptor fails Decrypt the way a structurally invalid blob fails: the
// ciphertext is too short to contain a nonce, so nothing is ever authenticated. This is
// the ONLY decrypt failure that proves the stored row is bad.
type malformedEncryptor struct{}

func (malformedEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (malformedEncryptor) Decrypt([]byte) ([]byte, error) {
	return nil, fmt.Errorf("%w: ciphertext too short", domain.ErrCredentialsMalformed)
}

func activeRedditConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderRedditAds,
		AccountID:            "t2_acct",
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
		// Reddit requires a conversion pixel on every campaign create, so a connection
		// without one cannot dispatch at all. A usable fixture carries it, the same way a
		// real connection does. TestReddit_MissingConversionPixelIsRefused covers the
		// absent case, which is a connection saved before the column existed.
		ProviderConfig: map[string]string{"conversion_pixel_id": "t2_pixel"},
	}
}

func testBrief() *model.CampaignBrief {
	return &model.CampaignBrief{
		ID:           "brief-1",
		ProjectID:    "cncf",
		EventSlug:    "kubecon-na-2026",
		EventDetails: json.RawMessage(`{"eventName":"KubeCon NA 2026","registrationUrl":"https://events.example/kc","project":"cncf"}`),
	}
}

const goodRedditCreds = `{"ClientID":"cid","ClientSecret":"sec","RefreshToken":"rt"}`

// ---- pre-create paths: must be NoUpstreamCreate (claim released) -----------

func TestReddit_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"repo error", fakeConnReader{err: errors.New("db down")}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderRedditAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, errEncryptor{}},
		{"incomplete credentials", fakeConnReader{conn: activeRedditConn(`{"ClientID":"cid"}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderRedditAds, AccountID: "t2_a", EncryptedCredentials: []byte(goodRedditCreds), Status: model.StatusInactive}}, identityEncryptor{}},
		{"no account id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderRedditAds, EncryptedCredentials: []byte(goodRedditCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewRedditDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate (claim released), got %T: %v", err, err)
			}
		})
	}
}

func TestReddit_BadConfigIsPreCreate(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, json.RawMessage(`{not json`))
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

func TestReddit_BriefWithoutEventNameIsPreCreate(t *testing.T) {
	b := testBrief()
	b.EventDetails = json.RawMessage(`{"project":"cncf"}`) // no eventName
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), b, model.ProviderRedditAds, nil)
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a brief with no eventName must be a pre-create error, got %T: %v", err, err)
	}
}

// ---- happy path through an httptest reddit API ----------------------------

func TestReddit_AmbiguousCreateRetainsClaim(t *testing.T) {
	// An ambiguous campaign create (5xx) makes the reddit client return a NON-NIL
	// name-only partial (empty CampaignID) + error. The adapter must return that
	// campaign + a non-NoUpstreamCreate error so the orchestrator RETAINS the claim
	// (a released claim would let a retry duplicate the maybe-created campaign).
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}}) // no existing-by-name
			return
		}
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the campaign POST
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err == nil {
		t.Fatal("expected an error from an ambiguous create")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("an ambiguous create must NOT be NoUpstreamCreate — the claim must be retained")
	}
	if camp == nil {
		t.Fatal("an ambiguous create must return a non-nil campaign so the orchestrator retains the claim")
	}
	// The adapter builds the reconcile signal on the returned campaign — the
	// deterministic name (so it can be looked up) and the provider result blob — even
	// though the upstream id is empty on an ambiguous create. This asserts the ADAPTER's
	// output in isolation; the orchestrator PERSISTS this id-less partial because Result
	// is non-empty (and classifies it on retry) — see
	// TestOrchestrator_IDlessOrphanWithResultIsNotASkipSuccess.
	if camp.CampaignName == "" {
		t.Error("the retained campaign must carry the deterministic name for reconciliation")
	}
	if camp.PlatformCampaignID != "" {
		t.Errorf("an ambiguous create yields no upstream id yet, got %q", camp.PlatformCampaignID)
	}
	if len(camp.Result) == 0 {
		t.Error("the retained campaign should carry the provider result blob (steps) for reconciliation")
	}
}

func TestReddit_DispatchSuccessMapsResult(t *testing.T) {
	// A minimal Reddit API: OAuth token + campaign create (+ ad group). We only need
	// the campaign create to return an id for the adapter's mapping assertion.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	// Per-platform config is nested under the platform key in the envelope.
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp_123" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" {
		t.Error("campaign name should be populated from the result")
	}
	if len(camp.Result) == 0 {
		t.Error("the provider result blob should be captured")
	}
	// A successful create must carry a non-empty status (the orchestrator doesn't set
	// one on success, and UpsertCampaign writes it verbatim).
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	// The persistence-contract columns (budget/schedule/config) must be populated from
	// the config, not left NULL (UpsertCampaign writes them verbatim).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 50 {
		t.Errorf("BudgetAmount = %v, want 50", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetLifetime {
		t.Errorf("BudgetType = %v, want lifetime (reddit client uses goal_type LIFETIME_SPEND)", camp.BudgetType)
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2099-08-01" {
		t.Errorf("StartDate = %v, want 2099-08-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2099-08-31" {
		t.Errorf("EndDate = %v, want 2099-08-31", camp.EndDate)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot should capture the validated reddit config")
	}
	// The campaign name must carry the AUTHENTICATED project slug (brief.ProjectID
	// "cncf"), stamped by the adapter — not free text from the brief JSON.
	if !strings.Contains(camp.CampaignName, "cncf") {
		t.Errorf("campaign name must include the authenticated project slug, got %q", camp.CampaignName)
	}
}

func TestReddit_ConfigHSTokenTakesPrecedence(t *testing.T) {
	// hsToken is a documented top-level config field. A request supplying config.hsToken
	// must be honored (it drives utm_campaign for HubSpot attribution) and take
	// precedence over any token in the brief blobs — not be silently ignored.
	var gotClickURL string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ads"):
			body, _ := io.ReadAll(r.Body)
			gotClickURL = string(body) // the ad body carries the utm click_url
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	// hsToken is a TOP-LEVEL envelope field (sibling to redditConfig, per api-catalog).
	// A postUrl/variant drives the ad step so the utm click_url is emitted.
	cfg := json.RawMessage(`{"hsToken":"HS-FROM-CONFIG","redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"],"postUrl":"t3_abc123","variants":[{"headline":"Join us"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(gotClickURL, "HS-FROM-CONFIG") {
		t.Errorf("config.hsToken must drive utm_campaign; ad click_url did not carry it: %q", gotClickURL)
	}
}

func TestReddit_DegradedSuccessSetsCreatedDegraded(t *testing.T) {
	// The campaign + ad group are created, but the promoted-post ad step fails (Reddit
	// rejects the /ads POST with a 4xx). The reddit client returns (result, nil) with a
	// non-empty AdWarning — a DEGRADED success. The adapter must NOT persist a clean
	// "created" status (which would let idempotency block re-dispatch while the missing
	// ad is visible only inside the result blob); it must persist "created_degraded".
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ads"):
			// Definite rejection of the promoted-post ad -> AdWarning, but the campaign
			// (already created) still returns (result, nil).
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "rejected"})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	// postUrl (a t3_-prefixed raw id is accepted) + a variant drive the ad step so the
	// /ads failure produces an AdWarning.
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"],"postUrl":"t3_abc123","variants":[{"headline":"Join us"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err != nil {
		t.Fatalf("a degraded success (campaign created, ad failed) must NOT return an error: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp_123" {
		t.Fatalf("the created campaign must still be mapped, got %+v", camp)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("status = %q, want %q (the failed ad must surface as a degraded, not clean, success)", camp.Status, campaignStatusCreatedDegraded)
	}
}

// The config_snapshot stored for a reddit campaign must NOT contain a PostURL's
// query/fragment (which can carry secrets) — it is persisted unencrypted. Per the
// Copilot #36 security finding.
func TestReddit_ConfigSnapshotRedactsPostURL(t *testing.T) {
	camp := campaignFromReddit(context.Background(),
		&reddit.CampaignResult{CampaignID: "cmp_1", CampaignName: "n"},
		redditConfig{BudgetUSD: 10, PostURL: "https://example.com/reg?token=SECRET#f"},
	)
	if camp.ConfigSnapshot == nil {
		t.Fatal("expected a config snapshot")
	}
	s := string(camp.ConfigSnapshot)
	if strings.Contains(s, "SECRET") {
		t.Errorf("config snapshot must not carry the PostURL query/fragment secret, got: %s", s)
	}
	if !strings.Contains(s, "https://example.com/reg") {
		t.Errorf("config snapshot should retain the sanitized post URL, got: %s", s)
	}
}

// toggleCampaign builds a persisted *model.Campaign carrying the child ids in Result, as the
// reddit create path stores them.
func toggleCampaign(campaignID, adGroupID, adID string) *model.Campaign {
	return &model.Campaign{
		PlatformCampaignID: campaignID,
		Result:             []byte(`{"adGroupId":"` + adGroupID + `","adId":"` + adID + `"}`),
	}
}

// TestReddit_ToggleStatus_PatchesPlatform verifies the dispatcher resolves creds and
// PATCHes configured_status through the reddit client — cascading to the campaign AND its
// child ad group + ad (all three are PAUSED at creation, so a partial toggle would not serve).
func TestReddit_ToggleStatus_PatchesPlatform(t *testing.T) {
	type patch struct{ method, path, status string }
	var got []patch
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				ConfiguredStatus string `json:"configured_status"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, patch{r.Method, r.URL.Path, body.Data.ConfiguredStatus})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	camp := toggleCampaign("t3_c", "t5_ag", "t6_ad")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	// On PAUSE the cascade flips the campaign gate FIRST, then the ad group, then the ad — all
	// to PAUSED (ACTIVATE uses the reverse, children-first order; see the client-level tests).
	want := []patch{
		{http.MethodPatch, "/api/v3/ad_accounts/t2_acct/campaigns/t3_c", "PAUSED"},
		{http.MethodPatch, "/api/v3/ad_accounts/t2_acct/ad_groups/t5_ag", "PAUSED"},
		{http.MethodPatch, "/api/v3/ad_accounts/t2_acct/ads/t6_ad", "PAUSED"},
	}
	if len(got) != len(want) {
		t.Fatalf("issued %d PATCHes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("PATCH[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	// An unsupported run state is rejected before any call.
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, "RUNNING"); err == nil {
		t.Error("expected an error for an unsupported run status")
	}
}

// TestReddit_ToggleStatus_NoChildIDsPausesCampaignOnly verifies that PAUSING a campaign whose
// persisted Result carries no child ids (a degraded create) PATCHes only the campaign —
// pausing the parent already halts delivery, so no child id is needed.
func TestReddit_ToggleStatus_NoChildIDsPausesCampaignOnly(t *testing.T) {
	var count int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"t3_c"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	camp := &model.Campaign{PlatformCampaignID: "t3_c"} // no Result blob → no child ids
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus (pause): %v", err)
	}
	if count != 1 {
		t.Errorf("issued %d PATCHes, want 1 (campaign only when no child ids)", count)
	}
}

// TestReddit_ToggleStatus_ActivateWithoutChildIDsRejected verifies that ACTIVATING a campaign
// with no known ad group id is refused before any PATCH — activating only the campaign would
// leave the ad group/ad PAUSED and the tree unable to serve, so the caller must not persist
// "active".
func TestReddit_ToggleStatus_ActivateWithoutChildIDsRejected(t *testing.T) {
	var count int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"t3_c"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	camp := &model.Campaign{PlatformCampaignID: "t3_c"} // no child ids
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error activating a campaign with no ad group id")
	}
	// It must be ErrCampaignNotProvisioned so the service maps it to a 409 state error, NOT a
	// 503 platform failure — the platform is never contacted.
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("error = %v, want ErrCampaignNotProvisioned (a client/state error → 409, not 503)", err)
	}
	if count != 0 {
		t.Errorf("issued %d PATCHes, want 0 (rejected before any PATCH)", count)
	}
}

// TestReddit_ToggleStatus_ActivateWithAdGroupButNoAdRejected covers the case a clean reddit
// create can produce: a campaign + ad group but NO ad (the no-PostURL path returns AdCount 0 /
// empty AdID), persisted as "created" and thus toggleable. Activating it would PATCH the
// campaign + ad group, skip the absent ad, and report "active" though the campaign can't serve.
// The dispatcher must refuse (ErrCampaignNotProvisioned) before any PATCH — ACTIVATE requires
// BOTH child ids.
func TestReddit_ToggleStatus_ActivateWithAdGroupButNoAdRejected(t *testing.T) {
	var count int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"t3_c"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	camp := toggleCampaign("t3_c", "t5_ag", "") // ad group present, ad id MISSING
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error activating a campaign with an ad group but no ad")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("error = %v, want ErrCampaignNotProvisioned", err)
	}
	if count != 0 {
		t.Errorf("issued %d PATCHes, want 0 (rejected before any PATCH)", count)
	}
}

// TestReddit_ToggleStatus_PartialCascadeIsUnconfirmed verifies that on a PAUSE — where the
// campaign gate is flipped FIRST — a subsequent child PATCH failure is UNCONFIRMED (partially
// applied), not a clean failure: the caller must verify/retry rather than report "not modified".
// (On PAUSE this is a state-consistency concern only; the campaign gate has already stopped
// delivery. The ACTIVATE path is children-first, so a child failure there never opens the gate —
// covered by TestReddit_ToggleStatus_ActivateChildFailureIsCleanNotServing.)
func TestReddit_ToggleStatus_PartialCascadeIsUnconfirmed(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// On PAUSE the campaign PATCH is first (succeeds); the ad-group PATCH then fails 400.
		if strings.Contains(r.URL.Path, "/ad_groups/") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, toggleCampaign("t3_c", "t5_ag", "t6_ad"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error when a child PATCH fails after the campaign PATCH")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a partial cascade (campaign applied, child failed) must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestReddit_ToggleStatus_ActivateChildFailureIsCleanNotServing verifies that on ACTIVATE
// (children-first, campaign gate LAST) a child PATCH failure returns an error but the campaign
// gate is never opened — so nothing serves and the failure is NOT a serving/unconfirmed partial.
func TestReddit_ToggleStatus_ActivateChildFailureIsCleanNotServing(t *testing.T) {
	var campaignPatched bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/campaigns/") {
			campaignPatched = true
		}
		if strings.Contains(r.URL.Path, "/ad_groups/") {
			w.WriteHeader(http.StatusInternalServerError) // child fails before the gate flip
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, toggleCampaign("t3_c", "t5_ag", "t6_ad"), model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when a child PATCH fails during activate")
	}
	if campaignPatched {
		t.Error("the campaign gate must NOT be opened when a child activate fails (nothing should serve)")
	}
}

// TestReddit_ToggleStatus_5xxIsUnconfirmed verifies a 5xx on the PATCH surfaces as an
// error whose Unconfirmed() is true (the change may have applied upstream).
func TestReddit_ToggleStatus_5xxIsUnconfirmed(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the PATCH
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, toggleCampaign("t3_c", "t5_ag", "t6_ad"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error on a 5xx toggle")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx toggle must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestReddit_ReadMetrics_UnsupportedWindowIs400 verifies ReadMetrics wraps the platform
// client's window-rejection error with domain.ErrMetricsWindowUnsupported, so brief.go's
// GetCampaignMetrics maps it to 400 (caller input) instead of falling through to 503
// (upstream failure) — Reddit's reporting endpoint has no "yesterday" or "last_14_days"
// date-range mapping (see dateRangeForWindow), and the platform is never contacted.
func TestReddit_ReadMetrics_UnsupportedWindowIs400(t *testing.T) {
	// Reddit metrics are default-OFF while the reporting contract is unverified;
	// opt this test in so it exercises the read path rather than the gate.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
	)
	_, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderRedditAds,
		toggleCampaign("t3_c", "t5_ag", "t6_ad"),
		model.MetricsWindowYesterday,
	)
	if err == nil {
		t.Fatal("expected an error for a window Reddit cannot map to a date range")
	}
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Errorf("expected err to wrap domain.ErrMetricsWindowUnsupported (so brief.go maps it to 400), got: %v", err)
	}
	if !errors.Is(err, reddit.ErrUnsupportedWindow) {
		t.Errorf("expected err to still wrap reddit.ErrUnsupportedWindow, got: %v", err)
	}
}

// TestReddit_ReadMetrics_Success verifies the happy path: resolveRedditClient succeeds
// and ReadMetrics delegates to the client, returning its result unmodified.
func TestReddit_ReadMetrics_Success(t *testing.T) {
	// Reddit metrics are default-OFF while the reporting contract is unverified;
	// opt this test in so it exercises the read path rather than the gate.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"campaign_id":"t3_c","impressions":1000,"clicks":10,"spend":"5.00"}]}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
	)
	metrics, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderRedditAds,
		toggleCampaign("t3_c", "t5_ag", "t6_ad"),
		model.MetricsWindowToday,
	)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if metrics.Impressions != 1000 || metrics.Clicks != 10 || metrics.CostMicros != 5_000_000 {
		t.Errorf("unexpected metrics: %+v", metrics)
	}
}

// TestReddit_ReadMetrics_MissingPlatformCampaignID verifies the guard against dispatching
// to a campaign that was never provisioned (or is otherwise missing its platform id) —
// resolveRedditClient is never reached, and the connection is never contacted.
func TestReddit_ReadMetrics_MissingPlatformCampaignID(t *testing.T) {
	// Reddit metrics are default-OFF while the reporting contract is unverified;
	// opt this test in so it exercises the read path rather than the gate.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
	)
	camp := toggleCampaign("", "t5_ag", "t6_ad")
	_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds, camp, model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected an error for a campaign with no platform campaign ID")
	}
}

// TestReddit_ReadMetrics_ResolutionErrorPropagates verifies that a connection-resolution
// failure (e.g. no connection on file) is returned to the caller instead of being masked.
func TestReddit_ReadMetrics_ResolutionErrorPropagates(t *testing.T) {
	// Reddit metrics are default-OFF while the reporting contract is unverified;
	// opt this test in so it exercises the read path rather than the gate.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	wantErr := errors.New("connection lookup failed")
	d := NewRedditDispatcher(fakeConnReader{err: wantErr}, identityEncryptor{})
	_, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderRedditAds,
		toggleCampaign("t3_c", "t5_ag", "t6_ad"),
		model.MetricsWindowToday,
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected the resolution error to propagate, got: %v", err)
	}
}

// TestReddit_ReadMetrics_DisabledByDefault pins the gate. Reddit's reporting contract is a
// guess (LFXV2-2995): a 200 from a guessed shape looks authoritative and the response carries
// none of the caveats, so the capability must stay off until the shape is verified against a
// live ad account. This asserts BOTH halves — the default is off with no env set, and any
// value other than "true" is also off, so a typo'd "1"/"yes" fails closed rather than open.
func TestReddit_ReadMetrics_DisabledByDefault(t *testing.T) {
	for _, val := range []string{"", "1", "yes", "TRUE", "false"} {
		t.Run("REDDIT_METRICS_ENABLED="+val, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, val)
			d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{})
			camp := &model.Campaign{PlatformCampaignID: "camp_123"}
			_, err := d.ReadMetrics(context.Background(), "p1", model.ProviderRedditAds, camp, model.MetricsWindowLast7Days)
			if !errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Fatalf("expected ErrMetricsUnsupported so the endpoint answers 400, got %v", err)
			}
		})
	}
}

// TestReddit_ToggleStatus_ForeignAccountIs409AndNeverMutates pins the account-provenance
// guard on the TOGGLE path. Reddit campaign ids are unique only WITHIN an ad account, so once
// a project's connection is re-pointed the stored id addressed against the new account can
// collide with an unrelated campaign and PAUSE OR ACTIVATE something this project does not
// own. The refusal must be a non-retryable ErrCampaignAccountMismatch (409) raised before any
// Reddit API call, and it must sit above BOTH branches.
//
// The ACTIVATE case pins the ORDERING against the child-id guard: a foreign-account campaign
// with NO stored children must answer the MISMATCH, not "has no fully-created ad group + ad
// to serve". The latter is a fact about a campaign in a different account.
//
// Unlike google-ads/microsoft/linkedin/meta there is NO url fallback to cover: RedditURL is
// the bare ads-manager constant, so only the explicit accountId is checkable — which is
// exactly what redditCreationAccountID documents.
func TestReddit_ToggleStatus_ForeignAccountIs409AndNeverMutates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"full child ids", `{"accountId":"t2_other","adGroupId":"t5_ag","adId":"t6_ad"}`},
		// The ordering probe: no child ids, so the not-provisioned guard would fire on
		// ACTIVATE if it ran first.
		{"no child ids", `{"accountId":"t2_other"}`},
	} {
		for _, status := range []string{model.CampaignRunPaused, model.CampaignRunActive} {
			t.Run(tc.name+"/status="+status, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					t.Errorf("Reddit must not be mutated for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
				}))
				defer api.Close()
				// The token exchange happens while BUILDING the client, before the campaign
				// row is consulted, so it is not part of what the guard prevents.
				tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
				}))
				defer tok.Close()
				// activeRedditConn resolves to account t2_acct; the rows record t2_other.
				d := NewRedditDispatcher(
					fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
					reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
					reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
				)
				camp := &model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(tc.result)}
				err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, status)
				if err == nil {
					t.Fatal("expected a mismatch error")
				}
				if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
					t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
				}
				if errors.Is(err, domain.ErrCampaignNotProvisioned) {
					t.Errorf("a foreign-account campaign must answer the mismatch, not a provisioning verdict: %v", err)
				}
				if !strings.Contains(err.Error(), "t2_other") || !strings.Contains(err.Error(), "t2_acct") {
					t.Errorf("error must name the created account (t2_other) and the resolved one (t2_acct), got %v", err)
				}
			})
		}
	}
}

// TestReddit_ToggleStatus_MatchingOrUnknownAccountStillToggles is the guard's other half: a
// row recording the SAME account, and a row recording none at all, must still toggle. Absence
// means "unknown, proceed" — and on Reddit that covers EVERY row written before the accountId
// field existed, because there is no URL fallback to recover the account from.
func TestReddit_ToggleStatus_MatchingOrUnknownAccountStillToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching accountId", `{"accountId":"t2_acct","adGroupId":"t5_ag","adId":"t6_ad"}`},
		{"no provenance recorded", `{"adGroupId":"t5_ag","adId":"t6_ad"}`},
		{"unparseable result blob", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var patches int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					mu.Lock()
					patches++
					mu.Unlock()
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{}}`)
			}))
			defer api.Close()
			tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tok.Close()
			d := NewRedditDispatcher(
				fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
				reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
				reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
			)
			camp := &model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(tc.result)}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunPaused); err != nil {
				t.Fatalf("ToggleStatus must proceed for a matching/unknown account, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			// The guard let the mutation THROUGH. Asserting only "no error" would also pass
			// if the toggle silently did nothing.
			if patches == 0 {
				t.Error("a matching/unknown-account campaign must actually be toggled, but no PATCH reached Reddit")
			}
		})
	}
}

// TestReddit_ReadMetrics_ForeignAccountIs409AndNeverQueries pins the same guard on the READ
// path: the stored campaign id read under a re-pointed connection returns either nothing — a
// false "no data" — or an unrelated campaign's numbers rendered as this campaign's
// measurement.
func TestReddit_ReadMetrics_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("Reddit must not be queried for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
	)
	camp := &model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_other","adGroupId":"t5_ag","adId":"t6_ad"}`)}
	got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds, camp, model.MetricsWindowToday)
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
	}
	if got != nil {
		t.Errorf("a refused read must return no metrics, got %+v", got)
	}
	if !strings.Contains(err.Error(), "t2_other") || !strings.Contains(err.Error(), "t2_acct") {
		t.Errorf("error must name the created account (t2_other) and the resolved one (t2_acct), got %v", err)
	}
}

// TestReddit_ReadMetrics_MatchingOrUnknownAccountStillReads is the read guard's other half: a
// row that cannot PROVE a mismatch must still be read.
func TestReddit_ReadMetrics_MatchingOrUnknownAccountStillReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching accountId", `{"accountId":"t2_acct","adGroupId":"t5_ag","adId":"t6_ad"}`},
		{"no provenance recorded", `{"adGroupId":"t5_ag","adId":"t6_ad"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			var mu sync.Mutex
			var queried bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				queried = true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"campaign_id":"t3_c","impressions":1000,"clicks":10,"spend":"5.00"}]}`))
			}))
			defer api.Close()
			tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
			}))
			defer tok.Close()
			d := NewRedditDispatcher(
				fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
				reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
			)
			camp := &model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(tc.result)}
			got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds, camp, model.MetricsWindowToday)
			if err != nil {
				t.Fatalf("ReadMetrics must proceed for a matching/unknown account, got %v", err)
			}
			if got == nil || got.Impressions != 1000 {
				t.Errorf("want the platform's metrics (1000 impressions), got %+v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if !queried {
				t.Error("a matching/unknown-account campaign must actually be read, but Reddit was never queried")
			}
		})
	}
}

// TestReddit_DispatchStampsCreatingAccount closes the loop the guard depends on: it drives a
// REAL create through the client and asserts the persisted Result blob records the account,
// readable by the very function the guard calls.
//
// Without this the guard is untestably inert on Reddit: redditCreationAccountID has no URL
// fallback (RedditURL is a bare constant), so if the create path stopped stamping accountId
// every row would answer "unknown, proceed" and the mismatch could never fire — while every
// hand-written-blob guard test kept passing. Asserting through redditCreationAccountID rather
// than a literal key also pins reader and writer to the SAME persisted shape.
func TestReddit_DispatchStampsCreatingAccount(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// activeRedditConn resolves to account t2_acct, so that is what a created row must record.
	if got := redditCreationAccountID(camp); got != "t2_acct" {
		t.Errorf("a created row must record its creating account: redditCreationAccountID = %q, want %q (blob: %s)", got, "t2_acct", camp.Result)
	}
}

// TestReddit_NoAccountSelectedIsRefusedBeforeTheProvenanceGuard pins the PRECONDITION the
// provenance guard's `current == ""` arm rests on.
//
// verifyRedditAccountMatch treats an empty CURRENT account as "unknown, proceed", because an
// absence cannot prove a mismatch. On reddit that arm is unreachable today: resolveRedditClient
// refuses an account-less connection with ErrAccountNotSelected before any campaign row is
// consulted. That is a precondition, not a coincidence — and an unpinned precondition is one
// somebody relaxes later, silently turning the unreachable arm into live behaviour with nothing
// failing. This test is what fails in that case.
//
// Both entry points, because the guard is shared by both and they resolve independently.
func TestReddit_NoAccountSelectedIsRefusedBeforeTheProvenanceGuard(t *testing.T) {
	noAccount := activeRedditConn(goodRedditCreds)
	noAccount.AccountID = ""
	for _, ep := range []struct {
		name string
		call func(d *RedditDispatcher) error
	}{
		{"ToggleStatus", func(d *RedditDispatcher) error {
			return d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds,
				&model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_other"}`)},
				model.CampaignRunPaused)
		}},
		{"ReadMetrics", func(d *RedditDispatcher) error {
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderRedditAds,
				&model.Campaign{PlatformCampaignID: "t3_c", Result: json.RawMessage(`{"accountId":"t2_other"}`)},
				model.MetricsWindowToday)
			return err
		}},
	} {
		t.Run(ep.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			d := NewRedditDispatcher(fakeConnReader{conn: noAccount}, identityEncryptor{})
			err := ep.call(d)
			if err == nil {
				t.Fatal("an account-less connection must be refused")
			}
			// The connection defect is the answer, NOT the account mismatch: the guard is never
			// reached, so a row recording a foreign account is beside the point here.
			if !errors.Is(err, domain.ErrAccountNotSelected) {
				t.Errorf("want ErrAccountNotSelected (the precondition the guard's empty-current arm rests on), got %T: %v", err, err)
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("want ErrConnectionNotUsable so the response maps to 409, got %v", err)
			}
		})
	}
}
