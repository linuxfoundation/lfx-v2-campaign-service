// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
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
		t.Fatal("an ambiguous create must return a non-nil campaign so the orchestrator records the orphan")
	}
	// The retained orphan carries the reconcile signal: the deterministic campaign
	// name (so it can be looked up) and the provider result blob — even though the
	// upstream id is empty on an ambiguous create.
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
	// The campaign name must carry the AUTHENTICATED project slug (brief.ProjectID
	// "cncf"), stamped by the adapter — not free text from the brief JSON.
	if !strings.Contains(camp.CampaignName, "cncf") {
		t.Errorf("campaign name must include the authenticated project slug, got %q", camp.CampaignName)
	}
}
