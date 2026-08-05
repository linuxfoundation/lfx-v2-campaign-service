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

// ---- ToggleStatus ------------------------------------------------------------

// msToggleCampaign builds a persisted *model.Campaign carrying the ad group/ad ids the
// create path would have stored in Result, matching the reddit/meta dispatch test fixtures.
func msToggleCampaign(campaignID, adGroupID, adID string) *model.Campaign {
	return &model.Campaign{
		PlatformCampaignID: campaignID,
		Result:             []byte(`{"adGroupId":"` + adGroupID + `","adId":"` + adID + `"}`),
	}
}

// TestMicrosoft_ToggleStatus_CascadesToTree verifies the dispatcher resolves creds and PUTs
// Status through the microsoft client — cascading to the campaign AND its child ad group + ad
// (all three are PAUSED at creation, so a partial toggle would not serve).
func TestMicrosoft_ToggleStatus_CascadesToTree(t *testing.T) {
	type call struct{ method, path, status string }
	var got []call
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Campaigns []struct {
				Status string `json:"Status"`
			} `json:"Campaigns"`
			AdGroups []struct {
				Status string `json:"Status"`
			} `json:"AdGroups"`
			Ads []struct {
				Status string `json:"Status"`
			} `json:"Ads"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var status string
		switch {
		case len(body.Campaigns) > 0:
			status = body.Campaigns[0].Status
		case len(body.AdGroups) > 0:
			status = body.AdGroups[0].Status
		case len(body.Ads) > 0:
			status = body.Ads[0].Status
		}
		got = append(got, call{r.Method, r.URL.Path, status})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := msToggleCampaign("111", "222", "333")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	want := []call{
		{http.MethodPut, "/CampaignManagement/v13/Campaigns", microsoft.StatusPaused},
		{http.MethodPut, "/CampaignManagement/v13/AdGroups", microsoft.StatusPaused},
		{http.MethodPut, "/CampaignManagement/v13/Ads", microsoft.StatusPaused},
	}
	if len(got) != len(want) {
		t.Fatalf("issued %d PUTs, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("PUT[%d] = %+v, want %+v", i, got[i], w)
		}
	}
	// An unsupported run state is rejected before any call.
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, "RUNNING"); err == nil {
		t.Error("expected an error for an unsupported run status")
	}
}

// TestMicrosoft_ToggleStatus_ActivateWithoutChildIDsRejected verifies that ACTIVATING a
// campaign with no known ad group/ad ids is refused before any PUT — activating only the
// campaign would leave the ad group/ad Paused and the tree unable to serve.
func TestMicrosoft_ToggleStatus_ActivateWithoutChildIDsRejected(t *testing.T) {
	var count int
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{PlatformCampaignID: "111"} // no child ids
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error activating a campaign with no ad group/ad ids")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("error = %v, want ErrCampaignNotProvisioned (a client/state error → 409, not 503)", err)
	}
	if count != 0 {
		t.Errorf("issued %d PUTs, want 0 (rejected before any PUT)", count)
	}
}

// TestMicrosoft_ToggleStatus_PartialCascadeIsUnconfirmed verifies that on a PAUSE — where the
// campaign gate is flipped FIRST — a subsequent child PUT failure is UNCONFIRMED (partially
// applied), not a clean failure.
func TestMicrosoft_ToggleStatus_PartialCascadeIsUnconfirmed(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/AdGroups") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msToggleCampaign("111", "222", "333"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error when a child PUT fails after the campaign PUT")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a partial cascade (campaign applied, child failed) must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestMicrosoft_ToggleStatus_ActivateChildFailureIsCleanNotServing verifies that on ACTIVATE
// (children-first, campaign gate LAST) a child PUT failure returns an error but the campaign
// gate is never opened.
func TestMicrosoft_ToggleStatus_ActivateChildFailureIsCleanNotServing(t *testing.T) {
	var campaignPatched bool
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/Campaigns") {
			campaignPatched = true
		}
		if strings.HasSuffix(r.URL.Path, "/AdGroups") {
			w.WriteHeader(http.StatusInternalServerError) // child fails before the gate flip
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"PartialErrors":[]}`)
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msToggleCampaign("111", "222", "333"), model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when a child PUT fails during activate")
	}
	if campaignPatched {
		t.Error("the campaign gate must NOT be opened when a child activate fails (nothing should serve)")
	}
}

// TestMicrosoft_ToggleStatus_5xxIsUnconfirmed verifies a 5xx on the PUT surfaces as an error
// whose Unconfirmed() is true (the change may have applied upstream).
func TestMicrosoft_ToggleStatus_5xxIsUnconfirmed(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the PUT
	}))
	defer apiSrv.Close()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at-123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()

	d := NewMicrosoftDispatcher(
		fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
		microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msToggleCampaign("111", "222", "333"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error on a 5xx toggle")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx toggle must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestMicrosoft_ToggleStatus_InactiveConnectionIsError verifies a non-active connection is
// rejected before any PUT.
func TestMicrosoft_ToggleStatus_InactiveConnectionIsError(t *testing.T) {
	d := NewMicrosoftDispatcher(fakeConnReader{conn: &model.Connection{
		Provider: model.ProviderMicrosoftAds, AccountID: "1234567",
		EncryptedCredentials: []byte(goodMicrosoftCreds), Status: model.StatusInactive,
	}}, identityEncryptor{})
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMicrosoftAds, msToggleCampaign("111", "222", "333"), model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error toggling a campaign whose connection is inactive")
	}
}
