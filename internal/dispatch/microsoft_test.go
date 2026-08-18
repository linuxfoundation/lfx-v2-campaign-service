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
		ProviderConfig:       map[string]string{"customer_id": "9999999"},
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
		// Whitespace-only credentials must be treated as ABSENT, not present. Without the
		// trim these reach CreateCampaign, whose first lookup fails on the bad credential and
		// returns a non-nil partial — classified UNCONFIRMED, wrongly RETAINING the claim for
		// a local config error where nothing was created upstream. One case per field so a
		// partial revert of the trim cannot pass silently.
		"whitespace-only client id": {fakeConnReader{conn: activeMicrosoftConn(
			`{"ClientID":"   ","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`)}, identityEncryptor{}},
		"whitespace-only client secret": {fakeConnReader{conn: activeMicrosoftConn(
			`{"ClientID":"cid","ClientSecret":" ","DeveloperToken":"dev","RefreshToken":"rt"}`)}, identityEncryptor{}},
		"whitespace-only developer token": {fakeConnReader{conn: activeMicrosoftConn(
			`{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"\t","RefreshToken":"rt"}`)}, identityEncryptor{}},
		"whitespace-only refresh token": {fakeConnReader{conn: activeMicrosoftConn(
			`{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"\n"}`)}, identityEncryptor{}},
		// NOTE: the whitespace cases above are ALSO covered by
		// TestMicrosoft_WhitespaceCredentialsNeverReachTheAPI, which asserts no upstream
		// request is made. That distinction matters: without the trim these still produce an
		// error (the client calls Microsoft and fails), so erroring alone does not prove the
		// guard — only "no request was sent" does.
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
		t.Errorf("CustomerId header = %q, want the connection's customer_id 9999999", creds.customerHeader)
	}
	// The create must STAMP the creating account into the persisted blob: it is the sole
	// input to the provenance guard on the read/toggle paths. Asserting the VALUE (not just
	// the key's presence) is what makes this catch a stamp wired to the wrong field — the
	// account id must be the connection's 1234567, not the MCC customer_id 9999999.
	var blob struct {
		AccountID string `json:"accountId"`
	}
	if err := json.Unmarshal(camp.Result, &blob); err != nil {
		t.Fatalf("result blob must be valid JSON: %v", err)
	}
	if blob.AccountID != "1234567" {
		t.Errorf("result blob accountId = %q, want the creating account 1234567 — the provenance guard reads this field", blob.AccountID)
	}
	// And the guard must actually accept what the create just wrote: a stamp the reader
	// cannot parse would leave every new row silently unguarded.
	if got := microsoftCreationAccountID(camp); got != "1234567" {
		t.Errorf("microsoftCreationAccountID(created campaign) = %q, want 1234567", got)
	}
}

// TestMicrosoft_AmbiguousCreateRetainsClaim: when the campaign lookup is absent but the create
// POST returns a 5xx (ambiguous — the campaign MAY have been created upstream), the MS client
// returns a non-nil partial + error. The dispatcher must RETAIN the claim (return a non-nil
// campaign, NOT NoUpstreamCreate) and surface an UNCONFIRMED error, so the orchestrator does not
// release the claim and blind-retry into a possible duplicate.
func TestMicrosoft_AmbiguousCreateRetainsClaim(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/Campaigns/QueryByAccountId"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Campaigns":[]}`) // absent → create runs
		case strings.HasSuffix(p, "/Campaigns"):
			w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the create — may have committed
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer apiSrv.Close()
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, json.RawMessage(`{"microsoftConfig":{"budget":50}}`))
	if err == nil {
		t.Fatal("expected an error on an ambiguous 5xx campaign create")
	}
	if camp == nil {
		t.Fatal("an ambiguous create must return a NON-nil partial so the claim is RETAINED, got nil")
	}
	// It must NOT be NoUpstreamCreate — a released claim here would invite a duplicate campaign.
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Errorf("an ambiguous create must NOT be NoUpstreamCreate (claim must be retained): %v", err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("an ambiguous create must surface UNCONFIRMED, got: %v", err)
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

// TestMicrosoft_WhitespaceCredentialsNeverReachTheAPI proves the credential trim actually
// guards: a whitespace-only credential must be rejected LOCALLY, with no upstream request.
//
// This is the assertion that fails if the trim is reverted. Merely asserting "an error is
// returned" does not — without the trim the client happily calls Microsoft with the bad
// credential and errors anyway, so the test would pass for the wrong reason while the real
// defect (the failure being classified UNCONFIRMED and RETAINING the claim) went uncaught.
func TestMicrosoft_WhitespaceCredentialsNeverReachTheAPI(t *testing.T) {
	for name, creds := range map[string]string{
		"client id":       `{"ClientID":"   ","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`,
		"client secret":   `{"ClientID":"cid","ClientSecret":" ","DeveloperToken":"dev","RefreshToken":"rt"}`,
		"developer token": `{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"\t","RefreshToken":"rt"}`,
		"refresh token":   `{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"\n"}`,
	} {
		t.Run(name, func(t *testing.T) {
			// Mutex-guarded: the handler goroutine writes this and the test goroutine reads
			// it, and httptest.Server.Close only synchronizes at the deferred Close — which
			// runs AFTER the assertion below.
			var mu sync.Mutex
			reached := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				reached = true
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer srv.Close()

			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(creds)}, identityEncryptor{},
				microsoft.WithTokenURL(srv.URL), microsoft.WithBaseURL(srv.URL),
			)
			// A VALID config is essential: passing nil would fail at the microsoftConfig
			// budget check BEFORE credentials are consulted, and the test would pass without
			// exercising the guard at all.
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
				json.RawMessage(`{"microsoftConfig":{"budget":50}}`))
			if err == nil {
				t.Fatal("a whitespace-only credential must be rejected")
			}
			mu.Lock()
			sawRequest := reached
			mu.Unlock()
			if sawRequest {
				t.Error("no upstream request may be made — the credential is rejected locally")
			}
			if camp != nil {
				t.Errorf("a pre-create failure must return a nil campaign (claim released), got %+v", camp)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("must be NoUpstreamCreate so the claim is RELEASED, got %T: %v", err, err)
			}
		})
	}
}

// TestMicrosoft_ToggleStatus_CascadesToChildren verifies the dispatcher resolves creds and
// PUTs Status across the whole tree — the create path PAUSES all three, so toggling only the
// campaign would not serve.
func TestMicrosoft_ToggleStatus_CascadesToChildren(t *testing.T) {
	type call struct{ method, path string }
	var (
		mu    sync.Mutex
		calls []call
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "654", "987")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	// The handler runs on the server goroutine; take the same lock the writer used so the
	// assertions below observe every append (srv.Close is deferred, so it orders nothing here).
	mu.Lock()
	defer mu.Unlock()
	// PAUSE: campaign gate FIRST, then ad group, then ad.
	if len(calls) != 3 {
		t.Fatalf("issued %d calls, want 3 (campaign + ad group + ad): %+v", len(calls), calls)
	}
	wantOrder := []string{"Campaigns", "AdGroups", "Ads"}
	for i, want := range wantOrder {
		if calls[i].method != http.MethodPut {
			t.Errorf("call[%d] method = %s, want PUT", i, calls[i].method)
		}
		if !strings.HasSuffix(calls[i].path, want) {
			t.Errorf("call[%d] path = %q, want a %s update (order: gate first on pause)", i, calls[i].path, want)
		}
	}
}

// TestMicrosoft_ToggleStatus_ActivateOrdersChildrenFirst pins the reverse ordering: on
// ACTIVATE nothing may serve until the whole tree is ready, so the campaign gate flips LAST.
func TestMicrosoft_ToggleStatus_ActivateOrdersChildrenFirst(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "654", "987", "555")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Keywords are enabled BEFORE the campaign gate, alongside the other descendants: the gate
	// must flip last so the campaign never reports Active while something under it is Paused.
	if len(paths) != 4 || !strings.HasSuffix(paths[0], "AdGroups") || !strings.HasSuffix(paths[1], "Ads") ||
		!strings.HasSuffix(paths[2], "Keywords") || !strings.HasSuffix(paths[3], "Campaigns") {
		t.Errorf("activate order = %v, want [AdGroups Ads Keywords Campaigns] (descendants first, gate last)", paths)
	}
}

// TestMicrosoft_ToggleStatus_ActivateWithoutKeywordsIsNotProvisioned pins the OTHER
// fail-closed activate guard, the one that distinguishes a campaign that is fully built from
// one that can actually serve. A Search campaign whose ad group carries no keywords has
// nothing to match a query against, so enabling it would report success for a campaign that
// delivers nothing — the exact false claim ErrCampaignNotProvisioned exists to prevent.
//
// The fixture has a COMPLETE ad group + ad, so this cannot pass by accident on the
// missing-child guard above: keywords are the only thing absent. Refused locally, without
// calling Microsoft, because it is a fact about the persisted row.
func TestMicrosoft_ToggleStatus_ActivateWithoutKeywordsIsNotProvisioned(t *testing.T) {
	var (
		mu      sync.Mutex
		reached bool
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	// Ad group + ad both present; NO keyword ids.
	camp := microsoftToggleCampaign("321", "654", "987")
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected ACTIVATE to be refused for a campaign with no keyword targeting")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("error = %v, want ErrCampaignNotProvisioned (a 409 state error, not a platform failure)", err)
	}
	if !strings.Contains(err.Error(), "keyword") {
		t.Errorf("the refusal must name keywords as the missing piece, got: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reached {
		t.Error("the guard must refuse WITHOUT calling Microsoft")
	}
}

// TestMicrosoft_ToggleStatus_PauseNeedsNoKeywords is the counterpart: pausing a campaign that
// never got keywords must still work. The activate guard exists to prevent a false "this will
// serve" claim; refusing to PAUSE would strand a campaign an operator is trying to stop.
func TestMicrosoft_ToggleStatus_PauseNeedsNoKeywords(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "654", "987")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("PAUSE must not require keywords: %v", err)
	}
}

// TestMicrosoft_ToggleStatus_ActivateWithoutChildrenIsNotProvisioned pins the fail-closed
// guard: with a child id missing nothing could serve, so refuse with
// ErrCampaignNotProvisioned (a 409) WITHOUT calling Microsoft.
func TestMicrosoft_ToggleStatus_ActivateWithoutChildrenIsNotProvisioned(t *testing.T) {
	var (
		mu      sync.Mutex
		reached bool
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	for _, tc := range []struct{ name, adGroup, ad string }{
		{"no ad group", "", "987"},
		{"no ad", "654", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			camp := microsoftToggleCampaign("321", tc.adGroup, tc.ad)
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunActive)
			if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
				t.Fatalf("want ErrCampaignNotProvisioned, got %T: %v", err, err)
			}
			mu.Lock()
			sawRequest := reached
			mu.Unlock()
			if sawRequest {
				t.Error("no API call should be made — the refusal is a local state check")
			}
		})
	}
}

// TestMicrosoft_ToggleStatus_ChildIDsMatchPersistedShape pins microsoftChildIDs against the
// blob campaignFromMicrosoft actually writes (microsoft.CampaignResult's lowerCamel json
// tags). A renamed/nested field would silently yield "" and turn every ACTIVATE into a
// spurious not-provisioned 409.
func TestMicrosoft_ToggleStatus_ChildIDsMatchPersistedShape(t *testing.T) {
	marshaled, err := json.Marshal(&microsoft.CampaignResult{CampaignID: "321", AdGroupID: "654", AdID: "987"})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	ag, ad := microsoftChildIDs(&model.Campaign{Result: marshaled})
	if ag != "654" || ad != "987" {
		t.Fatalf("microsoftChildIDs over the real persisted blob = (%q, %q), want (654, 987); blob: %s", ag, ad, marshaled)
	}
	if ag, ad := microsoftChildIDs(nil); ag != "" || ad != "" {
		t.Errorf("a nil campaign must yield empty ids, got (%q, %q)", ag, ad)
	}
	if ag, ad := microsoftChildIDs(&model.Campaign{Result: json.RawMessage(`{`)}); ag != "" || ad != "" {
		t.Errorf("a malformed blob must yield empty ids, got (%q, %q)", ag, ad)
	}
}

// TestMicrosoft_ToggleStatus_RejectsUnsupportedStatus keeps the run-state vocabulary closed.
func TestMicrosoft_ToggleStatus_RejectsUnsupportedStatus(t *testing.T) {
	d := NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{})
	camp := microsoftToggleCampaign("321", "654", "987")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, "archived"); err == nil {
		t.Fatal("an unsupported run status must be rejected")
	}
}

// microsoftToggleCampaign builds a persisted campaign row for the toggle tests. keywordIDs is
// variadic so the many PAUSE cases keep their original three-argument shape: pause needs no
// keywords, and a row with none is exactly the pre-MS-4 state those tests were written
// against. ACTIVATE cases must pass at least one, because the dispatcher refuses to enable a
// campaign whose keyword targeting was never provisioned.
func microsoftToggleCampaign(campaignID, adGroupID, adID string, keywordIDs ...string) *model.Campaign {
	blob := `{"campaignId":"` + campaignID + `","adGroupId":"` + adGroupID + `","adId":"` + adID + `"`
	if len(keywordIDs) > 0 {
		quoted := make([]string, len(keywordIDs))
		for i, k := range keywordIDs {
			quoted[i] = `"` + k + `"`
		}
		blob += `,"keywordIds":[` + strings.Join(quoted, ",") + `]`
	}
	return &model.Campaign{
		Platform:           model.ProviderMicrosoftAds,
		PlatformCampaignID: campaignID,
		Result:             json.RawMessage(blob + `}`),
	}
}

// TestMicrosoft_ToggleStatus_PartialCascadeIsUnconfirmed pins the dispatcher's classification
// step: the client reports a partially-applied cascade as Unconfirmed, and the dispatcher must
// PROPAGATE that (as unconfirmedToggleError) rather than surface it as a definite failure.
// Without the wrap the service would report "not modified" for a change that DID partially land.
func TestMicrosoft_ToggleStatus_PartialCascadeIsUnconfirmed(t *testing.T) {
	var mu sync.Mutex
	var puts int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		puts++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// The campaign gate applies cleanly; the ad group is then REJECTED outright.
		if strings.HasSuffix(r.URL.Path, "AdGroups") {
			_, _ = io.WriteString(w, `{"PartialErrors":[{"Code":1234,"Message":"nope"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "654", "987")
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("a rejected child status update must not be reported as success")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a partial cascade (campaign paused, ad group rejected) must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestMicrosoft_ToggleStatus_5xxIsUnconfirmed pins the same propagation for a transport-level
// ambiguity: Microsoft may have applied the change before the 5xx, so the outcome is unknowable.
func TestMicrosoft_ToggleStatus_5xxIsUnconfirmed(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "654", "987")
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error on a 5xx toggle")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx toggle must be Unconfirmed() — the change may have applied, got %T: %v", err, err)
	}
}

// TestMicrosoft_ToggleStatus_PauseWithOrphanAdIsNotProvisioned pins the second local refusal:
// a persisted row with an ad id but NO ad-group id cannot be paused, because Microsoft
// addresses an ad by its parent (the Ads PUT is scoped by AdGroupId). Reporting success would
// leave the ad serving. It is a fact about the ROW, so it is ErrCampaignNotProvisioned (409)
// and must be caught before any credential resolution or upstream call.
func TestMicrosoft_ToggleStatus_PauseWithOrphanAdIsNotProvisioned(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := microsoftToggleCampaign("321", "", "987")
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("want ErrCampaignNotProvisioned for an ad with no ad group, got %T: %v", err, err)
	}
	mu.Lock()
	sawRequest := reached
	mu.Unlock()
	if sawRequest {
		t.Error("the refusal is a local row check — no token or API request may be made")
	}
}

// TestMicrosoft_ToggleStatus_ForeignAccountIs409AndNeverMutates pins the account-provenance
// guard on the TOGGLE path. It matters MORE here than on the read: Microsoft campaign ids are
// unique only WITHIN an ad account, so after UpdateMicrosoftAds re-points a project's
// connection, the stored id addressed against the new account can collide with an unrelated
// campaign and PAUSE OR ACTIVATE something this project does not own. The refusal must be a
// non-retryable ErrCampaignAccountMismatch (409) raised before Microsoft is contacted, and it
// must sit above BOTH branches — pause and activate.
func TestMicrosoft_ToggleStatus_ForeignAccountIs409AndNeverMutates(t *testing.T) {
	for _, status := range []string{model.CampaignRunPaused, model.CampaignRunActive} {
		t.Run("status="+status, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("no token may be fetched for a campaign owned by another ad account")
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("Microsoft must not be mutated for a campaign owned by another ad account")
			}))
			defer apiSrv.Close()

			// activeMicrosoftConn resolves to account 1234567; the row records 7654321.
			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
				microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderMicrosoftAds,
				PlatformCampaignID: "321",
				Result:             json.RawMessage(`{"accountId":"7654321","campaignId":"321","adGroupId":"654","adId":"987"}`),
			}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, status)
			if err == nil {
				t.Fatal("expected a mismatch error")
			}
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
			}
			// Assert the VALUES: the message must name the account the campaign was created
			// under and the one the connection resolves to today.
			if !strings.Contains(err.Error(), "7654321") || !strings.Contains(err.Error(), "1234567") {
				t.Errorf("error must name the created account (7654321) and the resolved one (1234567), got %v", err)
			}
		})
	}
}

// TestMicrosoft_ToggleStatus_MatchingOrUnknownAccountStillToggles is the guard's other half:
// a row recording the SAME account, and a legacy row recording none at all, must still toggle.
// Absence means "unknown, proceed" — turning it into a refusal would strand every campaign
// created before the account id was stamped into the result blob.
func TestMicrosoft_ToggleStatus_MatchingOrUnknownAccountStillToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching accountId", `{"accountId":"1234567","campaignId":"321","adGroupId":"654","adId":"987"}`},
		{"no provenance recorded", `{"campaignId":"321","adGroupId":"654","adId":"987"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var puts int
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					mu.Lock()
					puts++
					mu.Unlock()
				}
				_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
			}))
			defer apiSrv.Close()

			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
				microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderMicrosoftAds,
				PlatformCampaignID: "321",
				Result:             json.RawMessage(tc.result),
			}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused); err != nil {
				t.Fatalf("this row does not prove a mismatch and must still toggle: %v", err)
			}
			mu.Lock()
			got := puts
			mu.Unlock()
			if got == 0 {
				t.Error("the toggle must reach Microsoft, not stop at the provenance guard")
			}
		})
	}
}
