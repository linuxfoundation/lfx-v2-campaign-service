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

const goodGoogleAdsCreds = `{"ClientID":"cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`

func activeGoogleAdsConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderGoogleAds,
		AccountID:            "1234567890",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"login_customer_id": "9999999999"},
		Status:               model.StatusActive,
	}
}

// googleAdsServers wires a token endpoint + an API server whose budget/campaign
// credCapture records what the dispatcher's credential mapping actually put on the wire: the
// OAuth token-request form values and the API developer-token header. A test can assert these
// so a regression that drops/misroutes a credential is CAUGHT (otherwise the fake endpoints
// accept anything and the happy path stays green even with the mapping removed). Guarded by mu
// because the token and API requests run on separate httptest handler goroutines.
type credCapture struct {
	mu             sync.Mutex
	clientID       string
	clientSecret   string
	refreshToken   string
	developerToken string
	sawToken       bool
	sawAPI         bool
}

// :mutate handlers are supplied per-test, returning the base URLs as client options. The
// returned *credCapture records the OAuth token form + API developer-token header for assertion.
func googleAdsServers(t *testing.T, budgetH, campaignH http.HandlerFunc) ([]googleads.Option, *credCapture) {
	t.Helper()
	cap := &credCapture{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		cap.mu.Lock()
		cap.sawToken = true
		cap.clientID = r.PostFormValue("client_id")
		cap.clientSecret = r.PostFormValue("client_secret")
		cap.refreshToken = r.PostFormValue("refresh_token")
		cap.mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.mu.Lock()
		cap.sawAPI = true
		if dt := r.Header.Get("developer-token"); dt != "" {
			cap.developerToken = dt
		}
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			budgetH(w, r)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			campaignH(w, r)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	return []googleads.Option{googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL)}, cap
}

// ---- pre-create paths -----------------------------------------------------

func TestGoogleAds_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderGoogleAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, errEncryptor{}},
		{"incomplete credentials", fakeConnReader{conn: activeGoogleAdsConn(`{"ClientID":"cid"}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderGoogleAds, AccountID: "1", EncryptedCredentials: []byte(goodGoogleAdsCreds), Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing account id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderGoogleAds, EncryptedCredentials: []byte(goodGoogleAdsCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestGoogleAds_BadConfigIsPreCreate(t *testing.T) {
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// TestGoogleAds_ClientPreCreateRejectionReleasesClaim exercises the `result == nil` RELEASE
// branch of the dispatcher — the other pre-create cases fail during connection/config/brief
// handling BEFORE CreateCampaign is called, so they never reach it. Here the connection is
// active and the config is syntactically valid and passes the dispatcher's own checks, so the
// flow reaches the real GA client, which then rejects it BEFORE any upstream mutate because
// the budget rounds to 0 micros (campaign.go: "budget must be > 0"), returning (nil, err). The
// adapter must map that to a NoUpstreamCreate error so the orchestrator RELEASES the claim —
// otherwise a safe-to-retry job would stay claimed forever.
func TestGoogleAds_ClientPreCreateRejectionReleasesClaim(t *testing.T) {
	// A server that fails any request, proving the rejection happens BEFORE the client's
	// first upstream mutate (no request should reach here).
	opts, _ := googleAdsServers(t,
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("client must reject the zero-budget config before any upstream mutate")
			w.WriteHeader(http.StatusInternalServerError)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			t.Error("no campaign mutate should be reached on a pre-create rejection")
			w.WriteHeader(http.StatusInternalServerError)
		},
	)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	// Valid envelope + connection, but budget 0 → the client rejects it pre-create.
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":0}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if camp != nil {
		t.Errorf("a pre-create rejection must return a nil campaign, got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a client pre-create rejection must be NoUpstreamCreate (release the claim), got %T: %v", err, err)
	}
}

// ---- happy path through an httptest google ads API ------------------------

func TestGoogleAds_DispatchSuccessMapsResult(t *testing.T) {
	// Capture BOTH :mutate request bodies so we can assert the brief id (the
	// retry-safety key, mapped as NameSuffix) actually reaches BOTH the budget and
	// campaign names — otherwise dropping that mapping would leave the suite green while
	// distinct/retried briefs collide on names.
	var budgetName, campaignName string
	nameFromBody := func(t *testing.T, r *http.Request) string {
		t.Helper()
		var body struct {
			Operations []struct {
				Create struct {
					Name string `json:"name"`
				} `json:"create"`
			} `json:"operations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Operations) == 0 {
			t.Fatalf("decode mutate body: %v", err)
		}
		return body.Operations[0].Create.Name
	}
	// Also capture the customer path + MCC header from the first mutate to prove the
	// connection's AccountID and login_customer_id reach the outbound request — a
	// dropped/misrouted mapping would otherwise target the wrong account context.
	var gotPath, gotLoginCustomer string
	opts, creds := googleAdsServers(t,
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotLoginCustomer = r.Header.Get("login-customer-id")
			budgetName = nameFromBody(t, r)
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			campaignName = nameFromBody(t, r)
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		},
	)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)
	brief := testBrief() // ID "brief-1"
	camp, err := d.Dispatch(context.Background(), brief, model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "222" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("success status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	// The brief id must appear in BOTH outbound names (NameSuffix mapping).
	if !strings.Contains(budgetName, brief.ID) {
		t.Errorf("budget name %q must carry the brief id %q (retry-safety key)", budgetName, brief.ID)
	}
	if !strings.Contains(campaignName, brief.ID) {
		t.Errorf("campaign name %q must carry the brief id %q (retry-safety key)", campaignName, brief.ID)
	}
	// The connection's AccountID must scope the request path, and its optional
	// login_customer_id (MCC) must be sent as the login-customer-id header.
	if !strings.Contains(gotPath, "customers/1234567890/") {
		t.Errorf("request path %q must be scoped to the connection's customer id", gotPath)
	}
	if gotLoginCustomer != "9999999999" {
		t.Errorf("login-customer-id header = %q, want the connection's MCC id 9999999999", gotLoginCustomer)
	}
	// The dispatcher's credential mapping must actually reach the wire: the OAuth token
	// request carries the decoded client_id/client_secret/refresh_token, and the API
	// developer-token header carries the developer token. Asserting these means dropping any
	// of those mappings (or the developer-token header) FAILS this test instead of passing
	// silently against the accept-anything fakes. Values come from goodGoogleAdsCreds.
	creds.mu.Lock()
	defer creds.mu.Unlock()
	if !creds.sawToken || !creds.sawAPI {
		t.Fatalf("expected both a token exchange (%v) and an API call (%v)", creds.sawToken, creds.sawAPI)
	}
	if creds.clientID != "cid" || creds.clientSecret != "csec" || creds.refreshToken != "rt" {
		t.Errorf("OAuth token form = client_id %q / client_secret %q / refresh_token %q, want cid/csec/rt from the stored credential",
			creds.clientID, creds.clientSecret, creds.refreshToken)
	}
	if creds.developerToken != "dev" {
		t.Errorf("developer-token header = %q, want %q from the stored credential", creds.developerToken, "dev")
	}
	// The persisted row must carry the budget/type/config (via applyCampaignConfig), not just
	// id/name/status — a NULL budget/type/config_snapshot row would lose the configuration
	// (per @dealako's blocking review; mirrors the sibling adapters).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 50 {
		t.Errorf("BudgetAmount = %v, want 50 (persisted from googleAdsConfig.budget)", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetDaily {
		t.Errorf("BudgetType = %v, want %q (GA uses a daily budget)", camp.BudgetType, model.BudgetDaily)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot must capture the validated googleAdsConfig")
	}
}

func TestGoogleAds_TrimsWhitespaceCustomerID(t *testing.T) {
	// A whitespace-padded connection AccountID must be TRIMMED before it reaches the client:
	// the dispatcher's empty check already trims, so an untrimmed CustomerID would pass that
	// check and then fail the client's digits-only validation as a confusing pre-create error.
	// The outbound request path must be scoped to the trimmed customer id.
	var gotPath string
	opts, _ := googleAdsServers(t,
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		},
	)
	conn := activeGoogleAdsConn(goodGoogleAdsCreds)
	conn.AccountID = "  1234567890  " // padded — must be trimmed before use
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, opts...)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(`{"googleAdsConfig":{"budget":50}}`))
	if err != nil {
		t.Fatalf("Dispatch with a whitespace-padded customer id must succeed after trimming, got: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "222" {
		t.Fatalf("expected a created campaign, got %+v", camp)
	}
	if !strings.Contains(gotPath, "customers/1234567890/") || strings.Contains(gotPath, " ") {
		t.Errorf("request path %q must be scoped to the TRIMMED customer id (no whitespace)", gotPath)
	}
}

func TestGoogleAds_AmbiguousCreateRetainsClaim(t *testing.T) {
	// The budget is created, then the campaign :mutate returns a 5xx (ambiguous): the
	// GA client returns a non-nil partial (name-only, carrying the orphaned budget) with
	// an error. The adapter must RETAIN the claim (not NoUpstreamCreate) and still return
	// the campaign so the orphan is recorded.
	opts, _ := googleAdsServers(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the campaign create
		},
	)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil {
		t.Fatal("expected an error from an ambiguous create")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("an ambiguous create must NOT be NoUpstreamCreate — the claim must be retained")
	}
	if camp == nil {
		t.Fatal("an ambiguous create must return a non-nil campaign for orphan recording")
	}
	// The orphan is only PERSISTABLE because Result carries the reconcile-by-name payload:
	// the orchestrator records an id-less partial ONLY when Result is non-empty. Assert it is
	// populated and carries the orphaned budget's reconcile key, so dropping the marshal (or
	// the budget mapping) would fail here instead of silently losing reconciliation data.
	if len(camp.Result) == 0 {
		t.Fatal("ambiguous-partial Result must be non-empty (it is the sole reconcile-by-name carrier)")
	}
	if !strings.Contains(string(camp.Result), "campaignBudgetId") && !strings.Contains(string(camp.Result), "CampaignBudgetId") &&
		!strings.Contains(string(camp.Result), "111") {
		t.Errorf("Result must carry the orphaned budget's reconcile key (id/name), got: %s", camp.Result)
	}
}

// ---- status toggle --------------------------------------------------------

// TestGoogleAds_ToggleStatus_MutatesCampaignStatus verifies the dispatcher resolves creds and
// sends a campaigns:mutate UPDATE carrying status + updateMask. Exactly ONE call is expected:
// this fixture's campaign carries no adGroupId/adId in its Result blob, so the child-status
// cascade is skipped defensively (mirroring reddit's ToggleStatus) — there is nothing to pause.
func TestGoogleAds_ToggleStatus_MutatesCampaignStatus(t *testing.T) {
	// Guarded: the handler runs on the server's goroutine while the assertions below run on
	// the test goroutine, and the reads happen BEFORE the deferred Close() that would
	// otherwise supply the happens-before edge. Mirrors meta_test.go's mutex pattern.
	var mu sync.Mutex
	var gotBody string
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	mu.Lock()
	gotPaths, body := append([]string(nil), paths...), gotBody
	mu.Unlock()
	if len(gotPaths) != 1 {
		t.Fatalf("issued %d API calls, want exactly 1 (no cascade): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "campaigns:mutate") {
		t.Errorf("path = %q, want a campaigns:mutate", gotPaths[0])
	}
	for _, want := range []string{`"status":"PAUSED"`, `"updateMask":"status"`, `campaigns/777`} {
		if !strings.Contains(body, want) {
			t.Errorf("mutate body missing %s: %s", want, body)
		}
	}
	// An update must NOT carry a create payload.
	if strings.Contains(body, `"create"`) {
		t.Errorf("a status update must not send a create operation: %s", body)
	}
}

// TestGoogleAds_ToggleStatus_ActivateIsNotProvisioned pins the ACTIVATE refusal. The create
// path provisions only a campaign shell — no ad group, ad, or keywords — so flipping the
// campaign to ENABLED would report success while nothing can serve. That must be
// ErrCampaignNotProvisioned (a 409 state error) raised locally, without calling Google.
func TestGoogleAds_ToggleStatus_ActivateIsNotProvisioned(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("want ErrCampaignNotProvisioned, got %T: %v", err, err)
	}
	mu.Lock()
	sawCall := reached
	mu.Unlock()
	if sawCall {
		t.Error("no API call should be made — the refusal is a local state check")
	}
}

// TestGoogleAds_ToggleStatus_RejectsUnsupportedStatus keeps the run-state vocabulary closed.
func TestGoogleAds_ToggleStatus_RejectsUnsupportedStatus(t *testing.T) {
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{})
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, "archived"); err == nil {
		t.Fatal("an unsupported run status must be rejected")
	}
}

// TestGoogleAds_ToggleStatus_ClassifiesOutcome pins the ambiguity contract that drives the
// API's verify-before-retry response: a 5xx MAY have applied upstream, so it must report
// Unconfirmed; a definite 4xx did NOT apply and must stay definite. Without both halves the
// service either tells a caller "not modified" when Google did change the campaign, or
// nags "verify" after every clean rejection.
func TestGoogleAds_ToggleStatus_ClassifiesOutcome(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		wantUnconfirmed bool
	}{
		{"5xx may have applied -> unconfirmed", http.StatusBadGateway, true},
		{"definite 4xx did not apply -> definite", http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			}))
			defer apiSrv.Close()

			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused)
			if err == nil {
				t.Fatal("expected an error")
			}
			var unconf interface{ Unconfirmed() bool }
			got := errors.As(err, &unconf) && unconf.Unconfirmed()
			if got != tc.wantUnconfirmed {
				t.Errorf("Unconfirmed() = %v, want %v (err %T: %v)", got, tc.wantUnconfirmed, err, err)
			}
		})
	}
}

// TestGoogleAds_ToggleStatus_AlreadyCanceledContextSendsNothing pins the dispatcher-level
// behaviour: an already-done context must send nothing and must NOT be reported as an
// ambiguous upstream mutation, since nothing reached Google.
//
// NOTE on scope: this exercises the COLD-cache path only. resolveGoogleAdsClient builds a
// fresh googleads.Client per call, so a token cached by an earlier ToggleStatus is discarded
// and cannot be primed from here — an earlier version of this test claimed to prime it and
// silently proved nothing. The cached-token path, where the guard actually matters, is
// covered at the client level by
// TestUpdateCampaignStatus_AlreadyCanceledContextWithCachedToken.
func TestGoogleAds_ToggleStatus_AlreadyCanceledContextSendsNothing(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the call

	err := d.ToggleStatus(ctx, "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context")
	}
	mu.Lock()
	sawCall := reached
	mu.Unlock()
	if sawCall {
		t.Error("no mutate may be sent when the context is already done")
	}
	var unconf interface{ Unconfirmed() bool }
	if errors.As(err, &unconf) && unconf.Unconfirmed() {
		t.Errorf("nothing was sent, so the outcome must NOT be ambiguous: %v", err)
	}
}

// provisionedGoogleAdsCampaign returns a campaign whose persisted Result blob carries the
// ad group/ad ids the GA-3b create path stores, so ToggleStatus's cascade has children to flip.
func provisionedGoogleAdsCampaign() *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444"}`),
	}
}

// TestGoogleAds_ToggleStatus_PauseCascadesToChildren pins the PAUSE ordering contract: campaign
// first (stops delivery immediately), then the ad group/ad — mirroring the reddit adapter.
func TestGoogleAds_ToggleStatus_PauseCascadesToChildren(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroupAds/333~444"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroups/333"}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
		}
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := provisionedGoogleAdsCampaign()
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 3 {
		t.Fatalf("issued %d API calls, want 3 (campaign, ad group, ad): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "campaigns:mutate") {
		t.Errorf("first call = %q, want campaigns:mutate (parent before children on PAUSE)", gotPaths[0])
	}
	if !strings.HasSuffix(gotPaths[1], "adGroups:mutate") || !strings.HasSuffix(gotPaths[2], "adGroupAds:mutate") {
		t.Errorf("child calls = %v, want [adGroups:mutate, adGroupAds:mutate]", gotPaths[1:])
	}
}

// TestGoogleAds_ToggleStatus_ActivateCascadesChildrenFirst pins the ACTIVATE ordering contract:
// children first, campaign last, so the campaign never reports ENABLED before its ad group/ad do.
func TestGoogleAds_ToggleStatus_ActivateCascadesChildrenFirst(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroupAds/333~444"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroups/333"}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
		}
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := provisionedGoogleAdsCampaign()
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 3 {
		t.Fatalf("issued %d API calls, want 3 (ad group, ad, campaign): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "adGroups:mutate") || !strings.HasSuffix(gotPaths[1], "adGroupAds:mutate") {
		t.Errorf("child calls = %v, want [adGroups:mutate, adGroupAds:mutate] before the campaign", gotPaths[:2])
	}
	if !strings.HasSuffix(gotPaths[2], "campaigns:mutate") {
		t.Errorf("last call = %q, want campaigns:mutate (parent after children on ACTIVATE)", gotPaths[2])
	}
}

// TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure pins the partial-failure contract:
// on PAUSE, if the campaign mutate already applied and the child mutate then fails/is ambiguous,
// ToggleStatus must still surface that failure to the caller rather than swallowing it — the
// campaign is left paused but the child status is unknown/unchanged.
func TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "adGroups:mutate") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := provisionedGoogleAdsCampaign()
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error when the child ad-group mutate fails")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx child mutate must be reported UNCONFIRMED, got %T: %v", err, err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 2 {
		t.Fatalf("issued %d calls, want 2 (campaign, then the failing ad group mutate — no ad mutate attempted): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "campaigns:mutate") || !strings.HasSuffix(gotPaths[1], "adGroups:mutate") {
		t.Errorf("paths = %v, want [campaigns:mutate, adGroups:mutate]", gotPaths)
	}
}

// TestGoogleAds_ToggleStatus_ActivateCascadeStopsOnCampaignFailure pins the ACTIVATE
// partial-failure contract: children flip first and succeed, but the campaign mutate that
// follows fails/is ambiguous — that failure must still surface to the caller. The children
// are left active but the campaign's own status is unresolved.
func TestGoogleAds_ToggleStatus_ActivateCascadeStopsOnCampaignFailure(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "campaigns:mutate") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroupAds/333~444"}]}`)
		default:
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroups/333"}]}`)
		}
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := provisionedGoogleAdsCampaign()
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when the campaign mutate fails after the children succeed")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx campaign mutate must be reported UNCONFIRMED, got %T: %v", err, err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 3 {
		t.Fatalf("issued %d calls, want 3 (ad group, ad, then the failing campaign mutate): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "adGroups:mutate") || !strings.HasSuffix(gotPaths[1], "adGroupAds:mutate") || !strings.HasSuffix(gotPaths[2], "campaigns:mutate") {
		t.Errorf("paths = %v, want [adGroups:mutate, adGroupAds:mutate, campaigns:mutate]", gotPaths)
	}
}

// ---- googleAdsChildIDs ------------------------------------------------------

func TestGoogleAdsChildIDs(t *testing.T) {
	cases := []struct {
		name        string
		campaign    *model.Campaign
		wantAdGroup string
		wantAd      string
	}{
		{"nil campaign", nil, "", ""},
		{"empty result", &model.Campaign{}, "", ""},
		{"unparseable json", &model.Campaign{Result: json.RawMessage(`not json`)}, "", ""},
		{"valid ids present", &model.Campaign{Result: json.RawMessage(`{"adGroupId":"333","adId":"444"}`)}, "333", "444"},
		{"valid json without ids", &model.Campaign{Result: json.RawMessage(`{"resourceName":"customers/1/campaigns/777"}`)}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAdGroup, gotAd := googleAdsChildIDs(tc.campaign)
			if gotAdGroup != tc.wantAdGroup || gotAd != tc.wantAd {
				t.Errorf("googleAdsChildIDs(%v) = (%q, %q), want (%q, %q)", tc.campaign, gotAdGroup, gotAd, tc.wantAdGroup, tc.wantAd)
			}
		})
	}
}
