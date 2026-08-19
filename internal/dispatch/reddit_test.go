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
	// Reddit metrics are default-OFF until the contract is exercised on a live account;
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

// TestReddit_ReadMetrics_UnsupportedWindowBeatsAResolutionFailure pins the ORDER, which
// the previous window test could not: it used a healthy connection, so it passed whether
// the window was checked before or after resolution.
//
// Here both are broken. An unsupported window is a permanent 400 that names the one thing
// the caller can change, while a connection error would send them to repair a connection
// on a request that would still be rejected on the window. So the window must win. Same
// property linkedin.go and twitter.go pin, and the reason ValidateMetricsWindow is
// package-level and clock-free.
func TestReddit_ReadMetrics_UnsupportedWindowBeatsAResolutionFailure(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	resolveErr := errors.New("connection lookup failed")
	d := NewRedditDispatcher(fakeConnReader{err: resolveErr}, identityEncryptor{})

	_, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderRedditAds,
		toggleCampaign("t3_c", "t5_ag", "t6_ad"),
		model.MetricsWindowYesterday,
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Errorf("the unsupported window must be reported (400) even though the connection is also broken, got: %v", err)
	}
	if errors.Is(err, resolveErr) {
		t.Errorf("the connection error masked the window rejection; the window is checked before resolution, got: %v", err)
	}
}

// TestReddit_ReadMetrics_Success verifies the happy path: resolveRedditClient succeeds
// and ReadMetrics delegates to the client, returning its result unmodified.
func TestReddit_ReadMetrics_Success(t *testing.T) {
	// Reddit metrics are default-OFF until the contract is exercised on a live account;
	// opt this test in so it exercises the read path rather than the gate.
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Shape per Reddit's public OpenAPI spec (LFXV2-3282): data is an OBJECT carrying a
		// metrics array, and spend is an int64 already in microcurrency — not the bare array
		// and decimal string this fixture used before the contract was verified.
		_, _ = w.Write([]byte(`{"data":{"metrics":[{"campaign_id":"t3_c","impressions":1000,"clicks":10,"spend":5000000}]},"pagination":{}}`))
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
	// Reddit metrics are default-OFF until the contract is exercised on a live account;
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
	// Reddit metrics are default-OFF until the contract is exercised on a live account;
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

// TestReddit_ReadMetrics_DisabledByDefault pins the gate. The reporting contract now follows
// Reddit's official public OpenAPI document (LFXV2-3282), but no request has been made against
// a live ad account: a 200 looks authoritative and the response carries none of the caveats,
// so the capability must stay off until behaviour the schema cannot express — zero-activity
// rows, the account's attribution window — is confirmed live. This asserts BOTH halves — the default is off with no env set, and any
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
