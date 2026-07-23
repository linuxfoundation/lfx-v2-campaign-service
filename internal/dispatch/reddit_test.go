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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
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

// identityEncryptor treats ciphertext as plaintext, so tests can put readable JSON in
// EncryptedCredentials. errEncryptor always fails Decrypt.
type identityEncryptor struct{}

func (identityEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (identityEncryptor) Decrypt(c []byte) ([]byte, error) { return c, nil }

type errEncryptor struct{}

func (errEncryptor) Encrypt(p []byte) ([]byte, error) { return p, nil }
func (errEncryptor) Decrypt([]byte) ([]byte, error)   { return nil, errors.New("bad key") }

func activeRedditConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderRedditAds,
		AccountID:            "t2_acct",
		EncryptedCredentials: []byte(creds),
		Status:               model.StatusActive,
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
	// Cascade PATCHes campaign, ad group, then ad — parent-first — all to PAUSED.
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

// TestReddit_ToggleStatus_NoChildIDsTogglesCampaignOnly verifies that when the persisted
// Result carries no child ids (a degraded create), only the campaign is PATCHed.
func TestReddit_ToggleStatus_NoChildIDsTogglesCampaignOnly(t *testing.T) {
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
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	if count != 1 {
		t.Errorf("issued %d PATCHes, want 1 (campaign only when no child ids)", count)
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
