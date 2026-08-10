// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
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
	// Raw :mutate request bodies for the two cascade stages. Captured for every test
	// (the handlers below are canned), so a test that cares can assert the dispatcher's
	// field mappings actually reach the wire rather than only that the call happened.
	adGroupBody   []byte
	adGroupAdBody []byte
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
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.adGroupBody = body
			cap.mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			body, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.adGroupAdBody = body
			cap.mu.Unlock()
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

// TestGoogleAds_AdCopyMappingsReachTheWire pins the four field mappings GA-3b adds to
// CampaignInput — RegistrationURL, Headlines, Descriptions, and EventSlug. The client-level
// tests construct CampaignInput directly, so they cannot catch a dispatcher that drops or
// swaps one of these; and the cascade's fake handlers accept anything, so without body
// assertions a dropped mapping leaves `go test ./...` green while every ad ships with
// generated fallback copy and a bare registration URL.
//
// The values below are deliberately distinctive so a swap between Headlines and Descriptions
// fails rather than coincidentally matching.
func TestGoogleAds_AdCopyMappingsReachTheWire(t *testing.T) {
	opts, cap := googleAdsServers(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		},
	)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,` +
		`"headlines":["ZebraHeadlineOne","ZebraHeadlineTwo","ZebraHeadlineThree"],` +
		`"descriptions":["QuokkaDescriptionOne","QuokkaDescriptionTwo"]}}`)
	brief := testBrief() // EventSlug "kubecon-na-2026", registrationUrl https://events.example/kc
	if _, err := d.Dispatch(context.Background(), brief, model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	adGroupAdBody := string(cap.adGroupAdBody)
	adGroupBody := string(cap.adGroupBody)
	cap.mu.Unlock()

	if adGroupBody == "" {
		t.Fatal("no adGroups:mutate body captured — the cascade did not run")
	}
	if adGroupAdBody == "" {
		t.Fatal("no adGroupAds:mutate body captured — the ad was never created")
	}

	// Headlines and Descriptions must land in their OWN asset lists. Decoding rather than
	// substring-matching the whole body is what makes a swap detectable.
	var payload struct {
		Operations []struct {
			Create struct {
				Ad struct {
					ResponsiveSearchAd struct {
						Headlines []struct {
							Text string `json:"text"`
						} `json:"headlines"`
						Descriptions []struct {
							Text string `json:"text"`
						} `json:"descriptions"`
					} `json:"responsiveSearchAd"`
					FinalUrls []string `json:"finalUrls"`
				} `json:"ad"`
			} `json:"create"`
		} `json:"operations"`
	}
	// Decode the string copied out under cap.mu above, NOT cap.adGroupAdBody — reaching
	// back into the live field re-reads handler-written state from the test goroutine
	// without the lock, which is the race the copy was made to avoid.
	if err := json.Unmarshal([]byte(adGroupAdBody), &payload); err != nil || len(payload.Operations) == 0 {
		t.Fatalf("decode adGroupAds:mutate body: %v (body %s)", err, adGroupAdBody)
	}
	rsa := payload.Operations[0].Create.Ad.ResponsiveSearchAd

	texts := func(assets []struct {
		Text string `json:"text"`
	}) string {
		var b strings.Builder
		for _, a := range assets {
			b.WriteString(a.Text)
			b.WriteString("|")
		}
		return b.String()
	}
	gotHeadlines, gotDescriptions := texts(rsa.Headlines), texts(rsa.Descriptions)

	for _, want := range []string{"ZebraHeadlineOne", "ZebraHeadlineTwo", "ZebraHeadlineThree"} {
		if !strings.Contains(gotHeadlines, want) {
			t.Errorf("configured headline %q did not reach the ad payload; headlines were %q", want, gotHeadlines)
		}
		if strings.Contains(gotDescriptions, want) {
			t.Errorf("headline %q landed in the DESCRIPTIONS list — the two mappings are swapped", want)
		}
	}
	for _, want := range []string{"QuokkaDescriptionOne", "QuokkaDescriptionTwo"} {
		if !strings.Contains(gotDescriptions, want) {
			t.Errorf("configured description %q did not reach the ad payload; descriptions were %q", want, gotDescriptions)
		}
		if strings.Contains(gotHeadlines, want) {
			t.Errorf("description %q landed in the HEADLINES list — the two mappings are swapped", want)
		}
	}

	// RegistrationURL is the base of the ad's final URL, and EventSlug is woven into its
	// tracking parameters — dropping either would send traffic to an untagged or wrong page.
	if len(payload.Operations[0].Create.Ad.FinalUrls) == 0 {
		t.Fatalf("ad has no finalUrls; body %s", adGroupAdBody)
	}
	finalURL := payload.Operations[0].Create.Ad.FinalUrls[0]
	if !strings.Contains(finalURL, "events.example/kc") {
		t.Errorf("final URL %q must be built from the brief's registrationUrl", finalURL)
	}
	if !strings.Contains(finalURL, brief.EventSlug) {
		t.Errorf("final URL %q must carry the brief's event slug %q", finalURL, brief.EventSlug)
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

// TestGoogleAds_ToggleStatus_ActivateIsNotProvisioned pins the ACTIVATE refusal. GA-3b creates
// the ad group and ad, but no targeting criteria (keywords/audiences), so activating would
// report success while nothing can deliver. That must be ErrCampaignNotProvisioned (a 409 state
// error) raised locally, without calling Google — not wired until GA-4 targeting is added.
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

// googleAdsCampaignWithChildrenNoTargeting returns a campaign whose persisted Result blob
// carries the ad group/ad ids the GA-3b create path stores, so ToggleStatus's PAUSE cascade
// has children to flip — but NO keywordCriteriaIds. It is deliberately not fully
// provisioned: this is the shape that exercises every PAUSE path (which does not care about
// targeting) and, on ACTIVATE, the provisioning guard's second condition. Use the inline
// blob with keywordCriteriaIds (see ActivateSucceedsChildrenFirst) for the activatable shape.
func googleAdsCampaignWithChildrenNoTargeting() *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444"}`),
	}
}

// TestGoogleAds_ToggleStatus_ActivateRefusedWhenChildIDsMissing pins the multi-entity cascade
// rule for the case Bugbot flagged: keyword targeting alone is not enough to activate. A
// campaign whose ad group/ad create hit a duplicate-name orphan or unconfirmed outcome (see
// createAdGroupAndAd) has keyword criteria recorded but no ad group/ad id to cascade to.
// ACTIVATE must refuse locally with ErrCampaignNotProvisioned rather than reaching
// UpdateAdGroupAndAdStatus with an empty id, which would surface as a plain client error.
func TestGoogleAds_ToggleStatus_ActivateRefusedWhenChildIDsMissing(t *testing.T) {
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
	)
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		// Keyword targeting is provisioned, but the ad group/ad ids are absent.
		Result: json.RawMessage(`{"keywordCriteriaIds":["111"]}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected ACTIVATE to be refused when ad group/ad ids are missing")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("expected ErrCampaignNotProvisioned, got %T: %v", err, err)
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
	camp := googleAdsCampaignWithChildrenNoTargeting()
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

// TestGoogleAds_ToggleStatus_ActivateWithoutKeywordCriteriaIsNotProvisioned pins the second
// condition of the activation guard, the one this slice adds: ad group + ad ids alone are not
// enough. A campaign created before targeting was provisioned has children to cascade to but
// no keyword criteria, so enabling it would report success for a campaign that cannot serve.
// The refusal is local — no dispatcher client is configured here, so any API call would fail
// with a connection error rather than ErrCampaignNotProvisioned.
func TestGoogleAds_ToggleStatus_ActivateWithoutKeywordCriteriaIsNotProvisioned(t *testing.T) {
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
	)
	camp := googleAdsCampaignWithChildrenNoTargeting()
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected ACTIVATE to be refused: keyword criteria are not provisioned")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("expected ErrCampaignNotProvisioned, got %v", err)
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
	camp := googleAdsCampaignWithChildrenNoTargeting()
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

// TestGoogleAds_ToggleStatus_PauseCascadeChildDefiniteFailureIsUnconfirmed pins the definite-4xx
// counterpart of TestGoogleAds_ToggleStatus_PauseCascadeStopsOnChildFailure (which uses a 5xx):
// once the campaign pause has already succeeded, ANY child mutate failure — including a definite
// 4xx — is a genuine partial cascade (the parent already changed, the child's outcome is
// unknown) and must be reported Unconfirmed, not passed through as a clean error.
func TestGoogleAds_ToggleStatus_PauseCascadeChildDefiniteFailureIsUnconfirmed(t *testing.T) {
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
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid ad group status"}}`)
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
	camp := googleAdsCampaignWithChildrenNoTargeting()
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error when the child ad-group mutate fails")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a definite 4xx child mutate after the campaign already paused must be reported UNCONFIRMED, got %T: %v", err, err)
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

// TestGoogleAds_ToggleStatus_ActivateGuardRunsBeforeAnyMutate pins the ORDERING of the
// activation guard: an unprovisioned campaign is refused before the cascade starts, so no
// child is left enabled by a run that then refuses. The dispatcher here has no client
// options at all, so if the guard were checked after the first mutate the error would be a
// connection failure instead of ErrCampaignNotProvisioned.
func TestGoogleAds_ToggleStatus_ActivateGuardRunsBeforeAnyMutate(t *testing.T) {
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
	)
	camp := googleAdsCampaignWithChildrenNoTargeting()
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected ACTIVATE to be refused before any mutate is sent")
	}
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Errorf("expected ErrCampaignNotProvisioned, got %v", err)
	}
}

// TestGoogleAds_ToggleStatus_ActivateChildDefiniteFailureIsNotUnconfirmed verifies that on
// ACTIVATE (children-first), a definite child failure (4xx) is NOT wrapped as unconfirmed.
// The child mutate happens before the campaign gate is opened; a definite error means the
// change was not applied upstream, so the failure is clean (not a partial cascade).
func TestGoogleAds_ToggleStatus_ActivateChildDefiniteFailureIsNotUnconfirmed(t *testing.T) {
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
		// Ad group/ad mutate fails with definite 4xx (validation error)
		if strings.HasSuffix(r.URL.Path, "adGroups:mutate") || strings.HasSuffix(r.URL.Path, "adGroupAds:mutate") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid targeting"}}`)
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
	// Must have targeting criteria provisioned for ACTIVATE to be allowed
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444","keywordCriteriaIds":["999"]}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when the child ad-group mutate fails")
	}
	// Verify the error is NOT wrapped as unconfirmed
	var unconf interface{ Unconfirmed() bool }
	if errors.As(err, &unconf) && unconf.Unconfirmed() {
		t.Errorf("a definite 4xx child failure must NOT be reported UNCONFIRMED, got: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	// Should only have the child mutate, no campaign mutate (campaign gate never opened)
	if len(gotPaths) != 1 {
		t.Fatalf("issued %d calls, want 1 (ad group mutate only — no campaign mutate after definite failure): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "adGroups:mutate") {
		t.Errorf("path = %v, want adGroups:mutate", gotPaths[0])
	}
}

// TestGoogleAds_ToggleStatus_ActivateSecondChildDefiniteFailureIsUnconfirmed verifies that a
// definite second-child failure (adGroupAds:mutate 4xx) on ACTIVATE is a partial cascade: the
// first child (ad group) has already been changed, so the outcome of the second child is
// definitively unknown — must be wrapped as unconfirmed.
func TestGoogleAds_ToggleStatus_ActivateSecondChildDefiniteFailureIsUnconfirmed(t *testing.T) {
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
		// Only the second child (adGroupAds:mutate) fails; ad group succeeds
		if strings.HasSuffix(r.URL.Path, "adGroupAds:mutate") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid ad status"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/adGroups/333"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	// Must have targeting criteria provisioned for ACTIVATE to be allowed
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444","keywordCriteriaIds":["999"]}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when the second child (ad) mutate fails")
	}
	// Verify the error IS wrapped as unconfirmed (partial cascade)
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a definite 4xx second-child failure must be reported UNCONFIRMED, got: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	// Should have ad-group mutate and ad mutate, but no campaign mutate (stopped by ad failure)
	if len(gotPaths) != 2 {
		t.Fatalf("issued %d calls, want 2 (ad group, then ad — no campaign mutate after ad failure): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[0], "adGroups:mutate") || !strings.HasSuffix(gotPaths[1], "adGroupAds:mutate") {
		t.Errorf("paths = %v, want [adGroups:mutate, adGroupAds:mutate]", gotPaths)
	}
}

// TestGoogleAds_ToggleStatus_ActivateCampaignDefiniteFailureIsUnconfirmed pins the reverse
// case: on ACTIVATE, once the children have already succeeded, ANY campaign mutate failure —
// including a definite 4xx — is a genuine partial cascade (the children already changed, the
// campaign's outcome is unknown) and must be reported Unconfirmed, the same as the PAUSE
// path's child-after-campaign wrap.
func TestGoogleAds_ToggleStatus_ActivateCampaignDefiniteFailureIsUnconfirmed(t *testing.T) {
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
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid campaign status"}}`)
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
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444","keywordCriteriaIds":["999"]}`),
	}
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error when the campaign mutate fails")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a definite 4xx campaign failure after the children already succeeded must be reported UNCONFIRMED, got %T: %v", err, err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	// adGroups + adGroupAds mutate (children) then the failing campaigns mutate.
	if len(gotPaths) != 3 {
		t.Fatalf("issued %d calls, want 3 (adGroups, adGroupAds, then the failing campaign mutate): %v", len(gotPaths), gotPaths)
	}
	if !strings.HasSuffix(gotPaths[2], "campaigns:mutate") {
		t.Errorf("path = %v, want campaigns:mutate", gotPaths[2])
	}
}

func TestGoogleAds_DispatchWiresKeywordsAndAudienceSegments(t *testing.T) {
	// Verify that the dispatcher wires keywords and audience segments from the config
	// through to the client, and that both reach the criteria endpoint. A typo or dropped
	// mapping in either field would leave this test green while the feature was broken.
	var mu sync.Mutex
	var sawCriteria bool
	var criteriaOps []map[string]any
	opts, _ := googleAdsServers(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		},
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "campaignBudgets:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		case strings.HasSuffix(r.URL.Path, "campaigns:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroups:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroups/333"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupAds:mutate"):
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/adGroupAds/333~444"}]}`)
		case strings.HasSuffix(r.URL.Path, "adGroupCriteria:mutate"):
			var body struct {
				Operations []map[string]any `json:"operations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			sawCriteria = true
			criteriaOps = body.Operations
			mu.Unlock()
			_, _ = io.WriteString(w, `{"results":[`+
				`{"resourceName":"customers/1234567890/adGroupCriteria/333~1"},`+
				`{"resourceName":"customers/1234567890/adGroupCriteria/333~2"},`+
				`{"resourceName":"customers/1234567890/adGroupCriteria/333~3"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(apiSrv.Close)
	opts = append(opts, googleads.WithBaseURL(apiSrv.URL))

	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{
		"googleAdsConfig":{
			"budget":50,
			"keywords":[
				{"text":"kubernetes","matchType":"EXACT"},
				{"text":"go programming","matchType":"PHRASE"}
			],
			"audienceSegments":["customers/123/userLists/456"]
		}
	}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("expected a campaign result")
	}
	mu.Lock()
	if !sawCriteria {
		mu.Unlock()
		t.Error("adGroupCriteria:mutate must be called when keywords/audience segments are supplied")
		return
	}
	if len(criteriaOps) != 3 {
		mu.Unlock()
		t.Fatalf("criteria operations count = %d, want 3 (2 keywords + 1 audience segment)", len(criteriaOps))
	}
	// Verify the keyword operations have the correct shape
	kw1 := criteriaOps[0]["create"].(map[string]any)
	if kw, ok := kw1["keyword"].(map[string]any); !ok || kw["text"] != "kubernetes" || kw["matchType"] != "EXACT" {
		mu.Unlock()
		t.Errorf("keyword[0] = %v, want {text: kubernetes, matchType: EXACT}", kw1["keyword"])
		return
	}
	kw2 := criteriaOps[1]["create"].(map[string]any)
	if kw, ok := kw2["keyword"].(map[string]any); !ok || kw["text"] != "go programming" || kw["matchType"] != "PHRASE" {
		mu.Unlock()
		t.Errorf("keyword[1] = %v, want {text: go programming, matchType: PHRASE}", kw2["keyword"])
		return
	}
	// Verify the audience segment operation has the correct shape
	aud := criteriaOps[2]["create"].(map[string]any)
	if ul, ok := aud["userList"].(map[string]any); !ok || ul["userList"] != "customers/123/userLists/456" {
		mu.Unlock()
		t.Errorf("audience segment = %v, want {userList: customers/123/userLists/456}", aud["userList"])
		return
	}
	mu.Unlock()
}

// ---- ReadMetrics --------------------------------------------------------

func TestGoogleAds_ReadMetrics_HappyPath(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"777"},"metrics":{"impressions":"1000","clicks":"40","costMicros":"25000000"}}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	// Window echoes the platform-agnostic request value, NOT the GAQL literal the query
	// carried — the dialect must not escape the platform package (the DURING assertion
	// below pins that it still reaches the wire).
	if m.CampaignID != "777" || m.Window != model.MetricsWindowLast30Days || m.Impressions != 1000 || m.Clicks != 40 || m.CostMicros != 25_000_000 {
		t.Errorf("got %+v", m)
	}
	if want := 0.04; m.Ctr != want {
		t.Errorf("Ctr = %v, want %v", m.Ctr, want)
	}
	mu.Lock()
	body := gotBody
	mu.Unlock()
	if !strings.Contains(body, "campaign.id = 777") || !strings.Contains(body, "DURING LAST_30_DAYS") {
		t.Errorf("query body = %s", body)
	}
}

// TestGoogleAds_ReadMetrics_ConnectionUnresolvedPropagates pins that a broken/inactive
// connection surfaces as a plain error (NOT wrapped with notCreated, unlike Dispatch) — a
// metrics read has no create-claim semantics to protect.
func TestGoogleAds_ReadMetrics_ConnectionUnresolvedPropagates(t *testing.T) {
	d := NewGoogleAdsDispatcher(fakeConnReader{err: errors.New("no connection")}, identityEncryptor{})
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	if _, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, model.MetricsWindowLast30Days); err == nil {
		t.Fatal("expected an error when the connection cannot be resolved")
	}
}

// TestGoogleAds_ReadMetrics_UnusableConnectionIsClassifiedNotRetryable pins the READ path's
// half of the classification the ListAccounts table pins for discovery. The two reach the
// validator through different resolvers, and only the discovery one used to tag the result:
// an inactive or incomplete connection came back off resolveGoogleAdsClient bare, missed
// the handler's ErrConnectionNotUsable arm (internal/service/brief.go), and answered 503 —
// "the platform did not respond" about a platform never contacted, telling the caller to
// retry a request no amount of waiting can satisfy. The status toggle resolves through the
// same function and inherits the same guarantee.
func TestGoogleAds_ReadMetrics_UnusableConnectionIsClassifiedNotRetryable(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("reached Google Ads with an unusable connection")
	}))
	defer apiSrv.Close()

	cases := []struct {
		name   string
		mutate func(*model.Connection)
		want   error
	}{
		{"inactive", func(c *model.Connection) { c.Status = model.StatusInactive }, domain.ErrConnectionInactive},
		{"undecodable blob", func(c *model.Connection) { c.EncryptedCredentials = []byte(`{`) }, domain.ErrCredentialsUndecodable},
		{"incomplete credentials", func(c *model.Connection) {
			c.EncryptedCredentials = []byte(`{"ClientID":"a","ClientSecret":"b","DeveloperToken":"c"}`)
		}, domain.ErrCredentialsIncomplete},
		{"no account selected", func(c *model.Connection) { c.AccountID = "" }, domain.ErrAccountNotSelected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeGoogleAdsConn(goodGoogleAdsCreds)
			tc.mutate(conn)
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, model.MetricsWindowLast30Days)
			if err == nil {
				t.Fatal("expected a connection error, got nil")
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): without it "+
					"the handler falls to its default arm and answers 503 for a condition only an "+
					"edit to the connection can clear", err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want errors.Is(err, %v): the reason sentinel is what the "+
					"handler logs, and it must survive alongside the status marker", err, tc.want)
			}
		})
	}
}

// TestGoogleAds_ReadMetrics_UnmappableWindowIs400AndNeverContactsPlatform pins the
// translation boundary: a window outside the closed model vocabulary is caller input, so it
// must surface as domain.ErrMetricsWindowUnsupported (400) and the platform must never be
// contacted for it. The connection reader fails the test if it is ever consulted, which
// proves the guard runs BEFORE credential resolution rather than after a wasted round trip.
func TestGoogleAds_ReadMetrics_UnmappableWindowIs400AndNeverContactsPlatform(t *testing.T) {
	d := NewGoogleAdsDispatcher(
		fakeConnReader{err: errors.New("connection reader must not be consulted for an unmappable window")},
		identityEncryptor{},
	)
	camp := &model.Campaign{Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	// "LAST_30_DAYS" is Google's own GAQL literal — valid downstream, but NOT a member of
	// the platform-agnostic vocabulary, so it must be rejected at this boundary.
	for _, w := range []model.MetricsWindow{"LAST_30_DAYS", "last_90_days", ""} {
		_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, w)
		if err == nil {
			t.Fatalf("window %q: expected an error", w)
		}
		if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
			t.Errorf("window %q: error must wrap ErrMetricsWindowUnsupported (400), got %v", w, err)
		}
		if !errors.Is(err, googleads.ErrUnsupportedWindow) {
			t.Errorf("window %q: error must also keep the client's cause reachable, got %v", w, err)
		}
	}
}

// TestGoogleAds_ReadMetrics_ForeignAccountIs409AndNeverQueries pins the account-identity
// guard. The campaign row records the customer it was created under; the connection the
// project resolves TODAY points somewhere else (UpdateGoogleAds can re-point it). Google
// would answer that query with an empty result set — indistinguishable from a campaign with
// genuinely zero activity — so the read must fail with domain.ErrCampaignAccountMismatch
// (409) instead, and must not reach the API at all.
func TestGoogleAds_ReadMetrics_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("the Google Ads API must not be queried for a campaign owned by another customer")
	}))
	defer apiSrv.Close()

	// The connection resolves to customer 1234567890 (activeGoogleAdsConn); both rows below
	// record 5555555555 — one via the modern customerId field, one via a legacy row that
	// only has the ocid in its console URL.
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"customerId field", `{"customerId":"5555555555","campaignId":"777"}`},
		{"legacy ocid fallback", `{"campaignId":"777","googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=5555555555"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderGoogleAds,
				PlatformCampaignID: "777",
				Result:             json.RawMessage(tc.result),
			}
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, model.MetricsWindowLast30Days)
			if err == nil {
				t.Fatal("expected a mismatch error")
			}
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %v", err)
			}
		})
	}
}

// TestGoogleAds_ReadMetrics_UnknownOrMatchingAccountStillReads is the other half of the
// guard: it must not become a wall. A row with no recoverable customer id (every row created
// before the field existed and before the console URL carried an ocid) cannot PROVE a
// mismatch, and a row recording the same customer the connection resolves to is not one — so
// both still read.
func TestGoogleAds_ReadMetrics_UnknownOrMatchingAccountStillReads(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"campaign":{"id":"777"},"metrics":{"impressions":"10","clicks":"1","costMicros":"5"}}]}`)
	}))
	defer apiSrv.Close()

	for _, tc := range []struct {
		name   string
		result json.RawMessage
	}{
		{"no result blob", nil},
		{"blob without a customer id", json.RawMessage(`{"campaignId":"777"}`)},
		{"unparseable blob", json.RawMessage(`not json`)},
		{"matching customer id", json.RawMessage(`{"customerId":"1234567890"}`)},
		{"matching legacy ocid", json.RawMessage(`{"googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=1234567890"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderGoogleAds,
				PlatformCampaignID: "777",
				Result:             tc.result,
			}
			m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds, camp, model.MetricsWindowLast30Days)
			if err != nil {
				t.Fatalf("ReadMetrics: %v", err)
			}
			if m.Impressions != 10 {
				t.Errorf("got %+v", m)
			}
		})
	}
}

// ---- googleAdsCreationCustomerID --------------------------------------------

func TestGoogleAdsCreationCustomerID(t *testing.T) {
	cases := []struct {
		name     string
		campaign *model.Campaign
		want     string
	}{
		{"nil campaign", nil, ""},
		{"empty result", &model.Campaign{}, ""},
		{"unparseable json", &model.Campaign{Result: json.RawMessage(`not json`)}, ""},
		{"explicit field", &model.Campaign{Result: json.RawMessage(`{"customerId":"111"}`)}, "111"},
		{
			"legacy ocid fallback",
			&model.Campaign{Result: json.RawMessage(`{"googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=222"}`)},
			"222",
		},
		{
			// The explicit field wins: the URL is a derived rendering, the field is the record.
			"field beats ocid",
			&model.Campaign{Result: json.RawMessage(`{"customerId":"111","googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=222"}`)},
			"111",
		},
		{"url without ocid", &model.Campaign{Result: json.RawMessage(`{"googleAdsUrl":"https://ads.google.com/aw/campaigns"}`)}, ""},
		{"unparseable url", &model.Campaign{Result: json.RawMessage(`{"googleAdsUrl":"://nope"}`)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := googleAdsCreationCustomerID(tc.campaign); got != tc.want {
				t.Errorf("googleAdsCreationCustomerID = %q, want %q", got, tc.want)
			}
		})
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

// TestGoogleAds_ToggleStatus_ForeignAccountIs409AndNeverMutates is the mutation-side twin of
// TestGoogleAds_ReadMetrics_ForeignAccountIs409AndNeverQueries, and the sharper of the two: a
// read against the wrong customer returns another account's numbers, but a MUTATE against the
// wrong customer pauses or enables another account's live campaign. Both PAUSE and ACTIVATE
// must be refused before any Google call, with a non-retryable ErrCampaignAccountMismatch.
func TestGoogleAds_ToggleStatus_ForeignAccountIs409AndNeverMutates(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the Google Ads API must not be mutated for a campaign owned by another customer (got %s %s)", r.Method, r.URL.Path)
	}))
	defer apiSrv.Close()

	// The connection resolves to customer 1234567890 (activeGoogleAdsConn); both rows below
	// record 5555555555 — one via the modern customerId field, one via a legacy row that only
	// has the ocid in its console URL. Both carry fully-provisioned children and keyword
	// criteria so the ACTIVATE pre-guards pass and the account guard is what fires.
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"customerId field", `{"customerId":"5555555555","campaignId":"777","adGroupId":"888","adId":"999","keywordCriteriaIds":["kw-1"]}`},
		{"legacy ocid fallback", `{"campaignId":"777","adGroupId":"888","adId":"999","keywordCriteriaIds":["kw-1"],"googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=5555555555"}`},
	} {
		for _, status := range []string{"paused", "active"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				d := NewGoogleAdsDispatcher(
					fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
					googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
				)
				camp := &model.Campaign{
					Platform:           model.ProviderGoogleAds,
					PlatformCampaignID: "777",
					Result:             json.RawMessage(tc.result),
				}
				err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, status)
				if err == nil {
					t.Fatal("expected a mismatch error")
				}
				if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
					t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %v", err)
				}
				// A refusal that never reached Google changed nothing upstream, so it must NOT
				// be reported as an unconfirmed outcome — that would send the caller
				// reconciling a mutation that provably did not happen.
				var unconfirmed interface{ Unconfirmed() bool }
				if errors.As(err, &unconfirmed) && unconfirmed.Unconfirmed() {
					t.Errorf("a pre-flight refusal must not be classified as unconfirmed, got %v", err)
				}
			})
		}
	}
}

// TestGoogleAds_ToggleStatus_UnknownOrMatchingAccountStillToggles is the other half of the
// toggle guard: a row with no recoverable customer id (created before the field and before
// the console URL carried an ocid) cannot PROVE a mismatch, and a row recording the same
// customer the connection resolves to is not one — both must still toggle.
func TestGoogleAds_ToggleStatus_UnknownOrMatchingAccountStillToggles(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	for _, tc := range []struct {
		name   string
		result string
	}{
		{"no customer id recorded", `{"campaignId":"777","adGroupId":"888","adId":"999","keywordCriteriaIds":["kw-1"]}`},
		{"same customer id", `{"customerId":"1234567890","campaignId":"777","adGroupId":"888","adId":"999","keywordCriteriaIds":["kw-1"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderGoogleAds,
				PlatformCampaignID: "777",
				Result:             json.RawMessage(tc.result),
			}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, "paused"); err != nil {
				t.Fatalf("toggle must proceed when no mismatch is provable, got %v", err)
			}
		})
	}
}

// TestGoogleAds_ToggleStatus_ActivateSucceedsChildrenFirst pins the GA-4 happy path that the
// refusal tests above cannot cover: with targeting criteria provisioned, ACTIVATE issues all
// three mutates, children FIRST (ad group, then ad, then campaign), and returns nil. The
// ordering is the safety property — the campaign gate is opened last so no traffic is served
// against an ad group or ad that is still paused.
func TestGoogleAds_ToggleStatus_ActivateSucceedsChildrenFirst(t *testing.T) {
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
		_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/123/campaigns/777"}]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	camp := &model.Campaign{
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		Result:             json.RawMessage(`{"adGroupId":"333","adId":"444","keywordCriteriaIds":["999"]}`),
	}
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ACTIVATE with provisioned targeting must succeed, got: %v", err)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 3 {
		t.Fatalf("issued %d calls, want 3 (ad group, ad, campaign): %v", len(gotPaths), gotPaths)
	}
	wantSuffixes := []string{"adGroups:mutate", "adGroupAds:mutate", "campaigns:mutate"}
	for i, want := range wantSuffixes {
		if !strings.HasSuffix(gotPaths[i], want) {
			t.Errorf("call %d = %v, want %v (children must be enabled before the campaign gate opens)", i, gotPaths[i], want)
		}
	}
}

// TestGoogleAds_ListAccounts_EmptyUpstreamIsEmptySliceNotNil pins the empty-discovery
// case end to end through the real dispatcher and the real client. A credential that
// legitimately reaches zero ad accounts must produce an EMPTY slice, not nil: the two
// have to stay distinguishable at the service boundary, or the caller that lands next
// reads "no answer" and reports the platform as down for a correct, ordinary answer.
// Building the slice with `var accounts []model.AccessibleAccount` would do exactly that.
func TestGoogleAds_ListAccounts_EmptyUpstreamIsEmptySliceNotNil(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotPath string
	var sawFlatList bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		gotMethod, gotPath = r.Method, r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/customers:listAccessibleCustomers") {
			sawFlatList = true
		}
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"resourceNames":[]}`)
	}))
	defer apiSrv.Close()

	// MANAGER MODE. activeGoogleAdsConn carries a login_customer_id, so the answer comes
	// from expanding the hierarchy and the flat list is never requested — a call whose
	// success is not needed must not be able to cause a failure. Asserting the flat
	// request here would pin the opposite of what the client is supposed to do.
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if accounts == nil {
		t.Fatal("ListAccounts returned nil for an empty upstream result; it must return an empty slice")
	}
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(accounts))
	}
	mu.Lock()
	flat := sawFlatList
	mu.Unlock()
	if flat {
		t.Error("manager mode issued customers:listAccessibleCustomers; its result is discarded, " +
			"so the only thing that request can contribute is a way for discovery to fail")
	}

	// DIRECT MODE. Without a login_customer_id the flat list IS the answer, and its REST
	// binding is GET on an account-agnostic path — no customers/{id} segment, unlike every
	// other Google Ads call this client makes.
	direct := activeGoogleAdsConn(goodGoogleAdsCreds)
	direct.ProviderConfig = nil
	dd := NewGoogleAdsDispatcher(
		fakeConnReader{conn: direct}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	accounts, err = dd.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListAccounts (direct mode): %v", err)
	}
	if accounts == nil {
		t.Fatal("ListAccounts returned nil for an empty upstream result; it must return an empty slice")
	}
	mu.Lock()
	method, path := gotMethod, gotPath
	mu.Unlock()
	if method != http.MethodGet {
		t.Errorf("method = %q, want GET", method)
	}
	if !strings.HasSuffix(path, "/customers:listAccessibleCustomers") {
		t.Errorf("path = %q, want the account-agnostic customers:listAccessibleCustomers", path)
	}
}

// TestGoogleAds_ListAccounts_WorksBeforeAnAccountIsChosen pins the fix for the
// chicken-and-egg in the discovery path. validateGoogleAdsConnection demands a non-empty
// account id, and ListAccounts used to route through it — so a caller had to have already
// pasted a customer id before they could ask which customer ids exist. A freshly
// authorized connection has credentials and NO account id; that is the exact state
// discovery is for, and it must not be rejected as a connection error.
func TestGoogleAds_ListAccounts_WorksBeforeAnAccountIsChosen(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resourceNames":["customers/1234567890"]}`)
	}))
	defer apiSrv.Close()

	conn := activeGoogleAdsConn(goodGoogleAdsCreds)
	conn.AccountID = ""       // not chosen yet — this is what discovery resolves
	conn.ProviderConfig = nil // and no manager either, so no hierarchy to expand

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("discovery must work with no account id chosen yet, got: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "1234567890" {
		t.Errorf("accounts = %+v, want the one discovered customer id", accounts)
	}
}

// TestGoogleAds_ListAccounts_StillRejectsUnusableConnections pins the other half: dropping
// the account-id requirement must not drop the rest of the connection contract. An
// inactive connection, one whose credential blob is missing OAuth fields, or one carrying a
// dashed manager id is a connection problem and should read as one — not as an opaque
// failure from Google.
//
// Each case must also satisfy errors.Is(err, domain.ErrConnectionNotUsable). That is not
// decoration: the service layer that answers this endpoint has exactly one way to tell a
// "the connection needs editing" failure (400) from "Google did not answer" (503), and it is
// this sentinel. Without it every case here lands in the default arm and becomes a 503 — a
// promise that retrying might help, made about three conditions that cannot change until a
// human edits the connection. The substring assertions alone would not catch that: the
// message stays identical whether or not the wrap is there.
//
// The dashed login_customer_id case is the one that motivated the check at the dispatch
// boundary. The client validates it too, but it does so inside the same call that talks to
// Google, so by the time it fails the error is indistinguishable here from an upstream one.
func TestGoogleAds_ListAccounts_StillRejectsUnusableConnections(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("reached Google Ads with an unusable connection")
	}))
	defer apiSrv.Close()

	cases := []struct {
		name    string
		mutate  func(*model.Connection)
		enc     domain.Encryptor // nil means identityEncryptor
		wantSub string
	}{
		{
			name:    "inactive connection",
			mutate:  func(c *model.Connection) { c.Status = model.StatusInactive },
			wantSub: "not active",
		},
		{
			// Caught inside creds.resolve, before any adapter sees the blob. It is the
			// same class as the cases below — the row needs editing, not retrying — so it
			// must carry the same sentinel even though a different function produces it.
			name:    "connection row with an empty credential blob",
			mutate:  func(c *model.Connection) { c.EncryptedCredentials = nil },
			wantSub: "no stored credentials",
		},
		{
			// Also inside creds.resolve, and note how narrow it is: a blob the encryptor
			// could not even ATTEMPT to authenticate. That proves the row is bad and
			// implicates nothing else, so it belongs in this table. A blob that failed
			// AUTHENTICATION does not belong here at all — see
			// TestGoogleAds_ListAccounts_DecryptFailureIsNotAConnectionProblem.
			name:    "structurally malformed credential blob",
			enc:     malformedEncryptor{},
			wantSub: "decrypt",
		},
		{
			name: "credentials missing the refresh token",
			mutate: func(c *model.Connection) {
				c.EncryptedCredentials = []byte(`{"clientId":"a","clientSecret":"b","developerToken":"c"}`)
			},
			wantSub: "incomplete",
		},
		{
			name: "manager id stored with dashes",
			mutate: func(c *model.Connection) {
				c.ProviderConfig = map[string]string{"login_customer_id": "999-999-9999"}
			},
			wantSub: "login_customer_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeGoogleAdsConn(goodGoogleAdsCreds)
			conn.AccountID = ""
			if tc.mutate != nil {
				tc.mutate(conn)
			}
			var enc domain.Encryptor = identityEncryptor{}
			if tc.enc != nil {
				enc = tc.enc
			}
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: conn}, enc,
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			_, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
			if err == nil {
				t.Fatal("expected a connection error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantSub)
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): without "+
					"the sentinel the service layer cannot separate this from an upstream failure "+
					"and answers 503, telling the caller to retry a request that cannot succeed "+
					"until the connection is edited", err)
			}
			if errors.Is(err, domain.ErrNotFound) {
				t.Errorf("error = %v, must NOT satisfy ErrNotFound: the connection exists, it is "+
					"just unusable — a 404 would tell the caller to create a second one", err)
			}
		})
	}
}

// TestGoogleAds_ListAccounts_UnusableReasonsAreClassifiedWithoutPlaintext pins the two
// properties the discovery handler's log line depends on.
//
// FIRST: each stored-connection defect carries its own sentinel alongside
// ErrConnectionNotUsable, so the reason survives as something `errors.Is` can read. The
// status is still decided by the one sentinel; these only say WHICH defect it was.
//
// SECOND, and the reason the first matters: one of these conditions is detected by decoding
// the DECRYPTED credential blob, and encoding/json QUOTES ITS INPUT — a *json.SyntaxError
// names the offending character, a *json.UnmarshalTypeError names the field being read. That
// error is dropped at the point of detection rather than wrapped, so the undecodable case
// asserts its message by EQUALITY against the fixed text. A substring hunt for a planted
// secret would be vacuous here: the blob most likely to leak yields only ONE character of
// input, so "the marker is absent" can pass against a chain that still carries the json
// error. Equality cannot: any re-wrap changes the string. The three blobs cover the two
// error types plus a truncation.
func TestGoogleAds_ListAccounts_UnusableReasonsAreClassifiedWithoutPlaintext(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("reached Google Ads with an unusable connection")
	}))
	defer apiSrv.Close()

	const secretMarker = "sUp3r-s3cr3t-refresh-token"

	cases := []struct {
		name   string
		mutate func(*model.Connection)
		want   error
	}{
		{
			name:   "inactive",
			mutate: func(c *model.Connection) { c.Status = model.StatusInactive },
			want:   domain.ErrConnectionInactive,
		},
		{
			// The empty-credential column. It is the ONE case here that never reaches
			// the decrypt/decode path at all — creds.resolve short-circuits on length —
			// which is exactly why it was the case that regressed to
			// reason=unclassified: it is wrapped in a different branch from every other
			// row in this table, so a fix applied to the classification switch does not
			// reach it.
			name:   "absent credentials",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = nil },
			want:   domain.ErrCredentialsAbsent,
		},
		{
			// A *json.SyntaxError case: trailing garbage after a valid value makes
			// encoding/json report "invalid character '@' after top-level value", quoting
			// a byte of the decrypted blob.
			name:   "undecodable blob, syntax error",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = []byte(`{"RefreshToken":"tok"}@`) },
			want:   domain.ErrCredentialsUndecodable,
		},
		{
			// A *json.UnmarshalTypeError case: the error names the struct field it was
			// reading, i.e. which credential is malformed.
			name:   "undecodable blob, type error",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = []byte(`{"RefreshToken":123}`) },
			want:   domain.ErrCredentialsUndecodable,
		},
		{
			name:   "undecodable blob, truncated",
			mutate: func(c *model.Connection) { c.EncryptedCredentials = []byte(`{"RefreshToken":"` + secretMarker) },
			want:   domain.ErrCredentialsUndecodable,
		},
		{
			name: "incomplete credentials",
			mutate: func(c *model.Connection) {
				c.EncryptedCredentials = []byte(`{"ClientID":"a","ClientSecret":"b","DeveloperToken":"c"}`)
			},
			want: domain.ErrCredentialsIncomplete,
		},
		{
			name: "invalid provider config",
			mutate: func(c *model.Connection) {
				c.ProviderConfig = map[string]string{"login_customer_id": "999-999-9999"}
			},
			want: domain.ErrProviderConfigInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeGoogleAdsConn(goodGoogleAdsCreds)
			conn.AccountID = ""
			tc.mutate(conn)
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				googleads.WithBaseURL(apiSrv.URL),
			)
			_, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
			if err == nil {
				t.Fatal("expected a connection error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want errors.Is(err, %v): the handler logs a reason token "+
					"derived from these sentinels, and an unclassified error logs nothing that "+
					"an operator can act on", err, tc.want)
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): the "+
					"reason sentinel names the defect, it does not replace the status marker", err)
			}
			if tc.want == domain.ErrCredentialsUndecodable {
				const want = "the stored connection is not usable as configured: " +
					"the stored credential blob could not be decoded: " +
					"google ads credentials for project proj are not valid JSON"
				if err.Error() != want {
					t.Errorf("error = %q, want exactly %q: the decode error is derived from the "+
						"decrypted blob and must not appear anywhere in this chain", err, want)
				}
			}
		})
	}
}

// TestGoogleAds_ListAccounts_DecryptFailureIsNotAConnectionProblem pins the other side of
// the table above: a decrypt failure is not one condition, and the two halves ask different
// people to act.
//
// A blob too short to hold a nonce was never authenticated, so it proves the stored ROW is
// bad — that half stays in the table above and answers 400. A GCM AUTHENTICATION failure
// proves something else entirely: the ciphertext is well formed and the key did not open it,
// which means a wrong or rotated APPLICATION key, or tampering
// (internal/infrastructure/crypto/aesgcm.go). The application key is deployment-wide, so
// that failure hits EVERY project's connection in the same instant. Wrapped as
// ErrConnectionNotUsable it would answer 400 to all of them — every operator told to go fix
// a connection that is fine, and the one signal that says the deployment is broken erased.
//
// The first case runs the REAL AESGCM rather than a fake, because the classification is a
// two-package contract: crypto must wrap the domain sentinel and creds.resolve must read it.
// A fake asserting its own error back would prove neither half. The second case is an
// encryptor whose error carries no classification at all; it must take the same path, since
// an implementation that proves nothing about the row cannot be read as accusing it.
func TestGoogleAds_ListAccounts_DecryptFailureIsNotAConnectionProblem(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("reached Google Ads with credentials that never decrypted")
	}))
	defer apiSrv.Close()

	sealingKey := bytes.Repeat([]byte{0xA5}, crypto.KeySize)
	sealer, err := crypto.NewAESGCM(sealingKey)
	if err != nil {
		t.Fatalf("build sealing encryptor: %v", err)
	}
	sealed, err := sealer.Encrypt([]byte(goodGoogleAdsCreds))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	// A DIFFERENT 32-byte key — the rotated-deployment-key scenario, exactly.
	rotated, err := crypto.NewAESGCM(bytes.Repeat([]byte{0x5A}, crypto.KeySize))
	if err != nil {
		t.Fatalf("build rotated encryptor: %v", err)
	}

	cases := []struct {
		name string
		enc  domain.Encryptor
		blob []byte
	}{
		{name: "GCM authentication failure under a rotated key", enc: rotated, blob: sealed},
		{name: "decrypt error with no classification", enc: errEncryptor{}, blob: []byte("anything")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeGoogleAdsConn(goodGoogleAdsCreds)
			conn.AccountID = ""
			conn.EncryptedCredentials = tc.blob
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: conn}, tc.enc,
				googleads.WithBaseURL(apiSrv.URL),
			)
			_, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
			if err == nil {
				t.Fatal("expected a decrypt error, got nil")
			}
			if !errors.Is(err, domain.ErrCredentialDecryptionFailed) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrCredentialDecryptionFailed): "+
					"this is the only marker that carries the infrastructure/security "+
					"classification to the service layer", err)
			}
			if errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error = %v, must NOT satisfy ErrConnectionNotUsable: that maps to 400 "+
					"and blames the project's connection row, but a wrong application key fails "+
					"every project at once and needs ops, not the operator", err)
			}
			if errors.Is(err, domain.ErrCredentialsMalformed) {
				t.Errorf("error = %v, must NOT satisfy ErrCredentialsMalformed: nothing here "+
					"showed the stored blob to be structurally bad", err)
			}
		})
	}
}

// TestGoogleAds_ListAccounts_MissingConnectionKeepsErrNotFound pins the ONE error
// distinction this staged adapter owes the service layer that lands next. Discovery is a
// read, and its two failure modes ask the operator to do opposite things: "this project has
// no Google Ads connection" is a setup state the operator fixes by creating one (404), while
// "the call to Google failed" is an outage worth retrying (503). Both arrive here as a
// non-nil error, so the only thing separating them is whether domain.ErrNotFound survives
// the credential resolution — resolve() wraps it with %w for precisely this reason, and a
// future refactor that flattens the wrap to a plain string error would silently collapse
// the 404 into a 503 with every existing test still green.
//
// The test also asserts Google is never contacted: a missing connection has no credential
// to send, so reaching upstream at all would mean the guard ran too late.
func TestGoogleAds_ListAccounts_MissingConnectionKeepsErrNotFound(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("contacted Google Ads for a project with no stored connection")
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{},
		googleads.WithBaseURL(apiSrv.URL),
	)
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
	if err == nil {
		t.Fatal("expected an error for a project with no connection, got nil")
	}
	if accounts != nil {
		t.Errorf("accounts = %+v, want nil on error", accounts)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want it to still satisfy errors.Is(err, domain.ErrNotFound) "+
			"so the caller can answer 404 rather than 503", err)
	}
}

// TestGoogleAds_ListAccounts_TranslatesIDAndLabel pins the actual translation this adapter
// performs. AccessibleAccount is a two-field projection, and each field comes from a
// different upstream shape: the ID is the trailing segment of a `customers/{id}` resource
// name, and the Label is the descriptiveName that only exists on rows returned by the
// manager-hierarchy expansion. The existing empty and no-account-chosen tests exercise the
// paths but never a populated Label, so a change that dropped DescriptiveName on the way
// through the adapter — the field an operator actually reads when picking an account —
// would leave every discovery test passing.
func TestGoogleAds_ListAccounts_TranslatesIDAndLabel(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The hierarchy expansion is where a display name is available at all; the flat
		// listAccessibleCustomers response carries resource names and nothing else.
		if strings.HasSuffix(r.URL.Path, "googleAds:search") {
			_, _ = io.WriteString(w, `{"results":[{"customerClient":{"id":"2222222222","descriptiveName":"CNCF Ad Account"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"resourceNames":["customers/2222222222"]}`)
	}))
	defer apiSrv.Close()

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	accounts, err := d.ListAccounts(context.Background(), "proj", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want exactly 1", accounts)
	}
	// Bare numeric id, NOT the customers/ resource name: the id is what a caller pastes
	// back as the connection's AccountID.
	if accounts[0].ID != "2222222222" {
		t.Errorf("ID = %q, want the bare numeric customer id", accounts[0].ID)
	}
	if accounts[0].Label != "CNCF Ad Account" {
		t.Errorf("Label = %q, want the descriptiveName from the expanded row", accounts[0].Label)
	}
}

// TestValidateGoogleAdsCredentials_WhitespaceOnlyIsIncomplete pins the trim. A credential
// pasted into a form arrives with surrounding whitespace routinely, and a field that is
// ONLY whitespace is the empty case wearing a disguise: `!= ""` accepts it, and the value
// then reaches Google, which rejects it as an opaque upstream failure rather than the local
// "incomplete credentials" error that names the field to fix.
//
// The second half matters as much as the first: trimming must happen IN PLACE, so the
// strings checked for emptiness are the same strings handed to NewClient. Trimming only
// inside the check would pass the guard and still send the padded value.
func TestValidateGoogleAdsCredentials_WhitespaceOnlyIsIncomplete(t *testing.T) {
	for _, field := range []string{"ClientID", "ClientSecret", "DeveloperToken", "RefreshToken"} {
		t.Run(field+" whitespace only", func(t *testing.T) {
			creds := googleAdsCreds{ClientID: "cid", ClientSecret: "csec", DeveloperToken: "dev", RefreshToken: "rt"}
			switch field {
			case "ClientID":
				creds.ClientID = "   "
			case "ClientSecret":
				creds.ClientSecret = "\t\n"
			case "DeveloperToken":
				creds.DeveloperToken = " "
			case "RefreshToken":
				creds.RefreshToken = "\n  "
			}
			raw, err := json.Marshal(creds)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			res := &resolved{plaintext: raw, status: model.StatusActive}
			if _, err := validateGoogleAdsCredentials("proj", res); err == nil {
				t.Fatalf("a whitespace-only %s passed as present; want an incomplete-credentials error", field)
			} else if !strings.Contains(err.Error(), "incomplete") {
				t.Errorf("error = %v, want the local incomplete-credentials error", err)
			}
		})
	}

	t.Run("padded values are returned trimmed", func(t *testing.T) {
		raw, err := json.Marshal(googleAdsCreds{
			ClientID: " cid ", ClientSecret: "\tcsec\n", DeveloperToken: " dev", RefreshToken: "rt ",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		got, err := validateGoogleAdsCredentials("proj", &resolved{plaintext: raw, status: model.StatusActive})
		if err != nil {
			t.Fatalf("validateGoogleAdsCredentials: %v", err)
		}
		want := googleAdsCreds{ClientID: "cid", ClientSecret: "csec", DeveloperToken: "dev", RefreshToken: "rt"}
		if got != want {
			t.Errorf("creds = %+v, want %+v (the trimmed values must be what reaches NewClient)", got, want)
		}
	})
}

// TestValidateGoogleAdsConnection_NoAccountSelected pins the sentinel pair on the empty
// account-id guard. Until credentials-first bootstrap existed, an account-less connection
// was an unintended state rather than an impossible one — Required("account_id") checked
// only that the JSON key was present, so `"account_id": ""` was accepted and stored — and
// the guard returned a bare error carrying no sentinel at all.
//
// That is now the ordinary state of a freshly created connection, and a bare error would
// fall to each handler's default arm and answer 503 "the platform did not respond" for a
// project that has simply not finished setting up. Both sentinels are load-bearing and
// serve different consumers: ErrConnectionNotUsable is what every handler switches on to
// pick a non-retryable status, and ErrAccountNotSelected is what unusableConnectionReason
// turns into the log's fixed reason token. Asserting only one would let the other be
// dropped silently.
func TestValidateGoogleAdsConnection_NoAccountSelected(t *testing.T) {
	raw := []byte(goodGoogleAdsCreds)
	// Whitespace, not "", so this also pins that the check runs on the TRIMMED value —
	// a connection holding " " has no more selected an account than one holding "".
	for _, accountID := range []string{"", "   "} {
		_, gotID, err := validateGoogleAdsConnection("proj", &resolved{
			plaintext: raw, status: model.StatusActive, accountID: accountID,
		})
		if err == nil {
			t.Fatalf("accountID %q: want an error, got nil", accountID)
		}
		if !errors.Is(err, domain.ErrConnectionNotUsable) {
			t.Errorf("accountID %q: want ErrConnectionNotUsable in the chain, got %v", accountID, err)
		}
		if !errors.Is(err, domain.ErrAccountNotSelected) {
			t.Errorf("accountID %q: want ErrAccountNotSelected in the chain, got %v", accountID, err)
		}
		if gotID != "" {
			t.Errorf("accountID %q: returned id = %q, want empty on the error path", accountID, gotID)
		}
	}
}

// TestGoogleAds_ToggleStatus_MalformedManagerIDIsClassified is the regression test for
// LFXV2-3052.
//
// The stored-login_customer_id check used to sit inline in the discovery resolver, so only
// the discovery endpoint ran it. The toggle path read the SAME column and handed it to the
// SAME client, but uninspected: the value failed later inside client.validateLoginCustomerID,
// during the call that talks to Google, and arrived here indistinguishable from an upstream
// failure. The service layer's default arm then answered 503 — "the platform did not
// respond", with an implied remedy of retrying — about a stored value that no amount of
// retrying can repair.
//
// So the assertion that matters is the SENTINEL, not the message. The substring check alone
// would have passed against the buggy version too, because the client's own validator
// produces near-identical text; what it could not produce is the wrap that tells the handler
// to answer 409. Both statuses are covered because ACTIVATE and PAUSE take different
// pre-guard routes to the same resolver, and a check reachable from only one of them is the
// same class of bug all over again.
//
// The API server fails the test if it is touched at all: a connection this broken must be
// refused before anything is sent, or a refusal becomes an unconfirmed mutation.
func TestGoogleAds_ToggleStatus_MalformedManagerIDIsClassified(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("reached Google Ads with an invalid stored login_customer_id (got %s %s)", r.Method, r.URL.Path)
	}))
	defer apiSrv.Close()

	for _, status := range []string{"paused", "active"} {
		t.Run(status, func(t *testing.T) {
			conn := activeGoogleAdsConn(goodGoogleAdsCreds)
			conn.ProviderConfig = map[string]string{"login_customer_id": "999-999-9999"}
			d := NewGoogleAdsDispatcher(
				fakeConnReader{conn: conn}, identityEncryptor{},
				googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderGoogleAds,
				PlatformCampaignID: "777",
				Result:             json.RawMessage(`{"campaignId":"777","adGroupId":"888","adId":"999","keywordCriteriaIds":["kw-1"]}`),
			}
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderGoogleAds, camp, status)
			if err == nil {
				t.Fatal("expected a connection error, got nil")
			}
			if !errors.Is(err, domain.ErrProviderConfigInvalid) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrProviderConfigInvalid): this "+
					"sentinel is what names the specific defect in the handler's fixed reason "+
					"vocabulary, so an unclassified error logs nothing an operator can act on", err)
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): without "+
					"the sentinel this falls to the handler's default arm and answers 503, "+
					"promising that a retry might fix a stored value only a human can fix", err)
			}
			// A refusal that never reached Google changed nothing upstream, so it must not be
			// reported as unconfirmed — that sends the caller reconciling a mutation that
			// provably did not happen.
			var unconfirmed interface{ Unconfirmed() bool }
			if errors.As(err, &unconfirmed) && unconfirmed.Unconfirmed() {
				t.Errorf("a pre-flight refusal must not be classified as unconfirmed, got %v", err)
			}
		})
	}
}

// TestGoogleAds_Dispatch_MalformedManagerIDIsClassified is the toggle test's counterpart on
// the path that spends money, and it exists because its absence is exactly why the create
// path was missed when the shape check was hoisted into validatedLoginCustomerID: two of the
// three readers of the stored login_customer_id had a test, and the third did not.
//
// What it asserts is NOT a status code, and the difference from the toggle test matters.
// Create is queued work: dispatchOne collapses every dispatcher error into the same
// "platform campaign creation failed" job result, so there is no 503 here to replace. An
// unclassified failure costs claim semantics instead — and the client's own validator embeds
// the raw manager id in its message with %q, which then lands in the dispatch-failure log
// line the orchestrator writes on both the released- and retained-claim arms. A manager id is
// account-identifying configuration; the rest of this path keeps error text to a fixed
// sentinel vocabulary with no payload precisely so it can be logged.
//
// The two assertions unique to create are the last ones. NoUpstreamCreate must hold: nothing
// was sent, so the orchestrator RELEASES the claim. The old path would have wrapped a
// pre-send CreateCampaign failure (result==nil) with notCreated anyway, so claim semantics
// were already correct; what this precheck adds is log hygiene (preventing the raw manager id
// from being embedded in the error) and no unnecessary upstream API call. And the campaign
// result must be nil, since a non-nil partial is this adapter's signal for "may exist upstream".
func TestGoogleAds_Dispatch_MalformedManagerIDIsClassified(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("reached Google Ads with an invalid stored login_customer_id (got %s %s)", r.Method, r.URL.Path)
	}))
	defer apiSrv.Close()

	conn := activeGoogleAdsConn(goodGoogleAdsCreds)
	conn.ProviderConfig = map[string]string{"login_customer_id": "999-999-9999"}
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds,
		json.RawMessage(`{"googleAdsConfig":{"budget":50}}`))
	if err == nil {
		t.Fatalf("expected a connection error, got nil (campaign %+v)", camp)
	}
	if camp != nil {
		t.Errorf("campaign = %+v, want nil: a non-nil result means \"may exist upstream\", and "+
			"nothing was sent", camp)
	}
	if !errors.Is(err, domain.ErrProviderConfigInvalid) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrProviderConfigInvalid): this sentinel "+
			"names the specific defect in the handler's fixed reason vocabulary", err)
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("error = %v, want errors.Is(err, domain.ErrConnectionNotUsable): without the "+
			"sentinel this is reported as an upstream failure, promising that a retry might fix "+
			"a stored value only a human can fix", err)
	}
	if strings.Contains(err.Error(), "999-999-9999") {
		t.Errorf("error %q echoes the raw manager id; this text reaches the orchestrator's "+
			"dispatch-failure log line, and an account identifier does not belong there", err)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("error = %v, want NoUpstreamCreate(): nothing was sent, so the orchestrator must "+
			"release the claim — retaining it would wedge the (brief, platform) slot against the "+
			"re-dispatch that fixing the stored row exists to enable", err)
	}
}
