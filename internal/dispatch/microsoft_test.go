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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
)

const goodMicrosoftCreds = `{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`

func activeMicrosoftConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderMicrosoftAds,
		AccountID:            "1234567",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"login_customer_id": "9999999"},
		Status:               model.StatusActive,
	}
}

// msCredCapture records what the dispatcher's credential mapping put on the wire: the OAuth
// token-request form (clientId/clientSecret/refreshToken) and the API DeveloperToken +
// CustomerAccountId/CustomerId headers. Asserting these means a dropped/misrouted mapping FAILS
// a test instead of passing silently against accept-anything fakes.
type msCredCapture struct {
	mu             sync.Mutex
	clientID       string
	clientSecret   string
	refreshToken   string
	developerToken string
	accountHeader  string
	customerHeader string
	campaignPath   string
	sawToken       bool
	sawAPI         bool
}

// microsoftServers wires a fake OAuth token endpoint + a fake API server that satisfies the full
// Campaign -> AdGroup -> Ad create hierarchy (lookups return "absent", creates return ids). It
// returns the client options and a capture of the credentials/headers seen on the wire.
func microsoftServers(t *testing.T) ([]microsoft.Option, *msCredCapture) {
	t.Helper()
	cap := &msCredCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		cap.mu.Lock()
		cap.sawToken = true
		cap.clientID = r.PostFormValue("client_id")
		cap.clientSecret = r.PostFormValue("client_secret")
		cap.refreshToken = r.PostFormValue("refresh_token")
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		cap.sawAPI = true
		if dt := r.Header.Get("DeveloperToken"); dt != "" {
			cap.developerToken = dt
		}
		if a := r.Header.Get("CustomerAccountId"); a != "" {
			cap.accountHeader = a
		}
		if c := r.Header.Get("CustomerId"); c != "" {
			cap.customerHeader = c
		}
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			_, _ = io.WriteString(w, `{"Campaigns":[]}`) // absent → create runs
		case strings.HasSuffix(p, "/AdGroups/QueryByCampaignId"):
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case strings.HasSuffix(p, "/Ads/QueryByAdGroupId"):
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case strings.HasSuffix(p, "/Campaigns"):
			cap.mu.Lock()
			cap.campaignPath = p
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/AdGroups"):
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case strings.HasSuffix(p, "/Ads"):
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []microsoft.Option{microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL)}, cap
}

// ---- pre-create paths: must release the claim (NoUpstreamCreate) -----------

func TestMicrosoft_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := map[string]struct {
		repo connReader
		enc  domain.Encryptor
	}{
		"missing connection":     {fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		"decrypt fails":          {fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, errEncryptor{}},
		"incomplete credentials": {fakeConnReader{conn: activeMicrosoftConn(`{"ClientID":"cid"}`)}, identityEncryptor{}},
		"inactive connection": {fakeConnReader{conn: &model.Connection{
			Provider: model.ProviderMicrosoftAds, AccountID: "1234567",
			EncryptedCredentials: []byte(goodMicrosoftCreds), Status: model.StatusInactive,
		}}, identityEncryptor{}},
		"missing account id": {fakeConnReader{conn: &model.Connection{
			Provider: model.ProviderMicrosoftAds, EncryptedCredentials: []byte(goodMicrosoftCreds),
			Status: model.StatusActive,
		}}, identityEncryptor{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := NewMicrosoftDispatcher(tc.repo, tc.enc)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, nil)
			if err == nil {
				t.Fatalf("%s: expected a pre-create error", name)
			}
			if camp != nil {
				t.Errorf("%s: a pre-create failure must return a nil campaign (claim released), got %+v", name, camp)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("%s: a pre-create failure must be NoUpstreamCreate (claim released), got %T: %v", name, err, err)
			}
		})
	}
}

// TestMicrosoft_DispatchSuccessMapsResultAndCreds drives a full create through fake servers and
// asserts (a) the result maps to the persistence model, (b) the connection's account/customer
// ids scope the request headers, and (c) the decoded credentials reach the OAuth token form +
// DeveloperToken header — so dropping any mapping fails this test.
func TestMicrosoft_DispatchSuccessMapsResultAndCreds(t *testing.T) {
	opts, creds := microsoftServers(t)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"microsoftConfig":{"budget":50}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "321" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("success status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	// Budget/type/config must persist (applyCampaignConfig parity with the siblings).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 50 {
		t.Errorf("BudgetAmount = %v, want 50 (persisted from microsoftConfig.budget)", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetDaily {
		t.Errorf("BudgetType = %v, want %q (Microsoft uses a daily budget)", camp.BudgetType, model.BudgetDaily)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot must capture the validated microsoftConfig")
	}
	creds.mu.Lock()
	defer creds.mu.Unlock()
	if !creds.sawToken || !creds.sawAPI {
		t.Fatalf("expected both a token exchange (%v) and an API call (%v)", creds.sawToken, creds.sawAPI)
	}
	if creds.clientID != "cid" || creds.clientSecret != "csec" || creds.refreshToken != "rt" {
		t.Errorf("OAuth token form = %q/%q/%q, want cid/csec/rt from the stored credential", creds.clientID, creds.clientSecret, creds.refreshToken)
	}
	if creds.developerToken != "dev" {
		t.Errorf("DeveloperToken header = %q, want %q from the stored credential", creds.developerToken, "dev")
	}
	if creds.accountHeader != "1234567" {
		t.Errorf("CustomerAccountId header = %q, want the connection's account id 1234567", creds.accountHeader)
	}
	if creds.customerHeader != "9999999" {
		t.Errorf("CustomerId header = %q, want the connection's login_customer_id 9999999", creds.customerHeader)
	}
}

// TestMicrosoft_TrimsWhitespaceAccountID: a whitespace-padded connection AccountID must be
// trimmed before it reaches the client (else it passes the empty check then fails the client's
// digits-only validation as a confusing pre-create error). The request header must carry the
// trimmed id.
func TestMicrosoft_TrimsWhitespaceAccountID(t *testing.T) {
	opts, creds := microsoftServers(t)
	conn := activeMicrosoftConn(goodMicrosoftCreds)
	conn.AccountID = "  1234567  " // padded — must be trimmed
	d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, opts...)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(`{"microsoftConfig":{"budget":50}}`))
	if err != nil {
		t.Fatalf("Dispatch with a padded account id must succeed after trimming, got: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "321" {
		t.Fatalf("expected a created campaign, got %+v", camp)
	}
	creds.mu.Lock()
	defer creds.mu.Unlock()
	if creds.accountHeader != "1234567" || strings.Contains(creds.accountHeader, " ") {
		t.Errorf("CustomerAccountId header = %q, want the TRIMMED id 1234567 (no whitespace)", creds.accountHeader)
	}
}
