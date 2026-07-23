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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

const goodLinkedInCreds = `{"AccessToken":"tok"}`

func activeLinkedInConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderLinkedInAds,
		AccountID:            "123456789",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"org_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestLinkedIn_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, errEncryptor{}},
		{"empty access token", fakeConnReader{conn: activeLinkedInConn(`{"AccessToken":""}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, AccountID: "1", EncryptedCredentials: []byte(goodLinkedInCreds), ProviderConfig: map[string]string{"org_id": "o"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing org id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderLinkedInAds, AccountID: "1", EncryptedCredentials: []byte(goodLinkedInCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewLinkedInDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestLinkedIn_BadConfigIsPreCreate(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// An empty variant set is rejected UP FRONT (pre-create, claim released) — no upstream
// call is made. Per Rashad's #37 review.
func TestLinkedIn_EmptyVariantsIsPreCreate(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	// A well-formed config with NO variants.
	cfg := json.RawMessage(`{"linkedInConfig":{"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01","variants":[]}}`)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("empty variants must be a pre-create (claim-releasing) error, got %T: %v", err, err)
	}
	if err != nil && !strings.Contains(err.Error(), "at least one creative variant") {
		t.Errorf("error should name the empty-variants cause, got: %v", err)
	}
}

// campaignFromLinkedIn maps a creative shortfall (fewer creatives than requested) to
// created_degraded, a group-only orphan to group_created, and a full result to created.
// Per Rashad's #37 review (degraded-state detection).
func TestLinkedIn_CampaignFromLinkedInStatus(t *testing.T) {
	ctx := context.Background()
	// Full success: 3 creatives created for 3 requested → clean created.
	if c := campaignFromLinkedIn(ctx, &linkedin.CampaignResult{CampaignID: "c1", CreativeCount: 3}, 3, linkedinConfig{}); c.Status != campaignStatusCreated {
		t.Errorf("3/3 creatives should be %q, got %q", campaignStatusCreated, c.Status)
	}
	// Creative shortfall: campaign created but only 2 of 3 creatives → degraded.
	if c := campaignFromLinkedIn(ctx, &linkedin.CampaignResult{CampaignID: "c1", CreativeCount: 2}, 3, linkedinConfig{}); c.Status != campaignStatusCreatedDegraded {
		t.Errorf("2/3 creatives should be %q, got %q", campaignStatusCreatedDegraded, c.Status)
	}
	// Group-only orphan: empty CampaignID + group id → group_created (not degraded).
	if c := campaignFromLinkedIn(ctx, &linkedin.CampaignResult{CampaignID: "", CampaignGroupID: "g1"}, 3, linkedinConfig{}); c.Status != campaignStatusGroupCreated {
		t.Errorf("group-only orphan should be %q, got %q", campaignStatusGroupCreated, c.Status)
	}
	// Group-AMBIGUOUS partial: BOTH ids empty (the group create itself was
	// unconfirmed) → unconfirmed, NOT a false "created" (dealako #37).
	if c := campaignFromLinkedIn(ctx, &linkedin.CampaignResult{CampaignID: "", CampaignGroupID: ""}, 3, linkedinConfig{}); c.Status != campaignStatusUnconfirmed {
		t.Errorf("both-ids-empty ambiguous partial should be %q, got %q", campaignStatusUnconfirmed, c.Status)
	}
	// The Result blob must be populated on the happy path.
	if c := campaignFromLinkedIn(ctx, &linkedin.CampaignResult{CampaignID: "c1", CreativeCount: 3}, 3, linkedinConfig{}); len(c.Result) == 0 {
		t.Error("Result blob should be marshaled on a normal result")
	}
}

// ---- happy path through an httptest linkedin API --------------------------

func TestLinkedIn_DispatchSuccessMapsResult(t *testing.T) {
	// Minimal LinkedIn REST API: search GETs return empty (force create), then
	// group/campaign/post/creative creates return ids. Mirrors the client's own
	// happy-path harness, just enough to yield a CampaignID for the adapter to map.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "200" {
		t.Fatalf("adapter must map the upstream campaign id (numeric), got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	// Persistence-contract columns populated from the config (not left NULL).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 100 {
		t.Errorf("BudgetAmount = %v, want 100", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetDaily {
		t.Errorf("BudgetType = %v, want daily (no lifetimeBudget in config)", camp.BudgetType)
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2099-01-01" {
		t.Errorf("StartDate = %v, want 2099-01-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2099-02-01" {
		t.Errorf("EndDate = %v, want 2099-02-01", camp.EndDate)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot should capture the validated linkedin config")
	}
}

func TestLinkedIn_ForeignAccountIDRejected(t *testing.T) {
	// A caller adAccountId that doesn't match the connection's account must be
	// rejected up front (pre-create) — appending it to the allowlist would defeat the
	// client's cross-tenant fail-closed check.
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{})
	// A variant IS supplied so the flow gets PAST the empty-variants pre-create check
	// and actually reaches the cross-account guard — otherwise this test would pass on
	// the wrong error and a broken account guard would go unnoticed.
	cfg := json.RawMessage(`{"linkedInConfig":{"adAccountId":"999999999","targetingProfile":"x","targetingProfiles":[{"id":"x","label":"X"}],"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]}}`)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a foreign adAccountId must be rejected pre-create, got %T: %v", err, err)
	}
	// The rejection must be the ACCOUNT-mismatch guard, not the empty-variants check —
	// assert the cause so this test genuinely exercises the cross-account rejection.
	if err != nil && strings.Contains(err.Error(), "creative variant") {
		t.Errorf("test must reach the account guard, but failed on empty-variants instead: %v", err)
	}
}

// A whitespace-PADDED adAccountId that TRIMS to the connection's account must be
// ACCEPTED (the guard trims once and uses the trimmed value both to compare and to
// build the client input) — the complement of the reject path, per dealako's #37 note.
func TestLinkedIn_PaddedMatchingAccountIDAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	// The connection's account is "123456789"; supply it whitespace-padded.
	cfg := json.RawMessage(`{"linkedInConfig":{
		"adAccountId":"  123456789  ",
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("a padded adAccountId that trims to the connection account must be accepted, got: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "200" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
}

func TestLinkedIn_AmbiguousCreateRetainsClaim(t *testing.T) {
	// A 5xx on the campaign-group create is ambiguous → the linkedin client returns a
	// non-nil partial (empty CampaignID). The adapter must retain the claim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on a create POST
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
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
	// Both the group and campaign creates 5xx'd, so BOTH ids are empty — the object
	// must be `unconfirmed`, never a false `created` (dealako #37).
	if camp.PlatformCampaignID != "" {
		t.Errorf("an ambiguous group create yields no campaign id, got %q", camp.PlatformCampaignID)
	}
	if camp.Status != campaignStatusUnconfirmed {
		t.Errorf("a both-ids-empty ambiguous create must be %q, got %q", campaignStatusUnconfirmed, camp.Status)
	}
}

func TestLinkedIn_ConfigHSTokenTakesPrecedence(t *testing.T) {
	// hsToken is a documented TOP-LEVEL config field (sibling to linkedInConfig, per
	// api-catalog). A request supplying config.hsToken must be honored — it drives
	// utm_campaign for HubSpot attribution on the dark post's destination URL — and take
	// precedence over any token in the brief blobs, mirroring the reddit adapter.
	var gotPostBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			body, _ := io.ReadAll(r.Body)
			gotPostBody = string(body) // the dark post carries the utm destination URL
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	// hsToken is a TOP-LEVEL envelope field (sibling to linkedInConfig, per api-catalog).
	cfg := json.RawMessage(`{"hsToken":"HS-FROM-CONFIG","linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(gotPostBody, "HS-FROM-CONFIG") {
		t.Errorf("config.hsToken must drive utm_campaign on the dark post; body did not carry it: %q", gotPostBody)
	}
}

func TestLinkedIn_GroupCreatedButCampaignFails_RecordsGroupOrphan(t *testing.T) {
	// The campaign GROUP is created, then the campaign create 5xx's. The client
	// returns a non-nil result with CampaignGroupID set + empty CampaignID. The
	// adapter must retain the claim AND capture the group orphan — recorded via the
	// group_created status plus the CampaignGroupID in Result (PlatformCampaignID stays
	// empty) — so it's reconcilable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[]}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:500"}`) // group created
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.WriteHeader(http.StatusBadGateway) // campaign create fails (group already exists)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(clock),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err == nil {
		t.Fatal("expected an error")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("a group-created failure must retain the claim, not release it")
	}
	// PlatformCampaignID MUST stay empty (no campaign was created) so the orchestrator's
	// idempotency doesn't short-circuit a retry as success; the group orphan is captured
	// via the group_created status + the CampaignGroupID in Result.
	if camp == nil {
		t.Fatalf("expected a non-nil campaign for orphan recording, got nil")
	}
	if camp.PlatformCampaignID != "" {
		t.Errorf("PlatformCampaignID must be empty for a group-only orphan (else idempotency false-succeeds), got %q", camp.PlatformCampaignID)
	}
	if camp.Status != campaignStatusGroupCreated {
		t.Errorf("a group-only orphan must have the group_created status, got %q", camp.Status)
	}
	if len(camp.Result) == 0 || !strings.Contains(string(camp.Result), "500") {
		t.Errorf("the group id must be preserved in Result for reconciliation, got %s", camp.Result)
	}
}

// TestPartialOrphanStatusValues locks the STRING VALUES of the partial-orphan
// statuses. The orchestrator's service-layer partialOrphanStatuses map
// (internal/service/orchestrator.go) hardcodes these same literals to decide which
// degraded statuses to PRESERVE on a retained orphan row (rather than flatten to
// "pending") and to exclude from completed-campaign reuse. The service test package
// deliberately does not import dispatch (to avoid coupling), so nothing there fails if
// these constants are renamed — this test in the OWNING package is the drift guard: if
// a value changes, update the service map in lockstep. (Addresses dealako's PR #37 nit.)
func TestPartialOrphanStatusValues(t *testing.T) {
	if campaignStatusGroupCreated != "group_created" {
		t.Errorf("campaignStatusGroupCreated = %q; the service partialOrphanStatuses map expects %q — update both in lockstep", campaignStatusGroupCreated, "group_created")
	}
	if campaignStatusUnconfirmed != "unconfirmed" {
		t.Errorf("campaignStatusUnconfirmed = %q; the service partialOrphanStatuses map expects %q — update both in lockstep", campaignStatusUnconfirmed, "unconfirmed")
	}
}

// linkedinToggleHandler routes the cascade's requests: the campaign PARTIAL_UPDATE, the
// creatives FINDER (returns the given creative URNs), and each creative PARTIAL_UPDATE. It
// records every request on a buffered channel (race-safe) and returns 200 for updates.
func linkedinToggleHandler(t *testing.T, gotCh chan<- struct{ method, path, restli, status string }, creativeURNs ...string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		restli := r.Header.Get("X-Restli-Method")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			gotCh <- struct{ method, path, restli, status string }{r.Method, r.URL.Path, restli, ""}
			w.Header().Set("Content-Type", "application/json")
			els := ""
			for i, u := range creativeURNs {
				if i > 0 {
					els += ","
				}
				els += `{"id":"` + u + `"}`
			}
			_, _ = io.WriteString(w, `{"elements":[`+els+`],"metadata":{}}`)
			return
		}
		var body struct {
			Patch struct {
				Set struct {
					Status         string `json:"status"`
					IntendedStatus string `json:"intendedStatus"`
				} `json:"$set"`
			} `json:"patch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := body.Patch.Set.Status
		if status == "" {
			status = body.Patch.Set.IntendedStatus
		}
		w.WriteHeader(http.StatusOK)
		gotCh <- struct{ method, path, restli, status string }{r.Method, r.URL.Path, restli, status}
	}
}

// TestLinkedIn_ToggleStatus_CascadesToCreatives verifies the dispatcher issues the campaign
// PARTIAL_UPDATE, discovers the creatives via the FINDER, and PARTIAL_UPDATEs each creative's
// intendedStatus (creatives are DRAFT at creation, so activating only the campaign would not
// serve).
func TestLinkedIn_ToggleStatus_CascadesToCreatives(t *testing.T) {
	type req = struct{ method, path, restli, status string }
	gotCh := make(chan req, 8)
	srv := httptest.NewServer(linkedinToggleHandler(t, gotCh, "urn:li:sponsoredCreative:900", "urn:li:sponsoredCreative:901"))
	defer srv.Close()
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderLinkedInAds, &model.Campaign{PlatformCampaignID: "555"}, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	close(gotCh)
	var campaignUpdated, finderCalled int
	creativeUpdates := 0
	for r := range gotCh {
		switch {
		case r.method == http.MethodGet:
			finderCalled++
		case strings.Contains(r.path, "/adCampaigns/555"):
			campaignUpdated++
			if r.restli != "PARTIAL_UPDATE" || r.status != "ACTIVE" {
				t.Errorf("campaign update = restli %q status %q, want PARTIAL_UPDATE ACTIVE", r.restli, r.status)
			}
		case strings.Contains(r.path, "/creatives/"):
			creativeUpdates++
			if r.restli != "PARTIAL_UPDATE" || r.status != "ACTIVE" {
				t.Errorf("creative update = restli %q status %q, want PARTIAL_UPDATE ACTIVE", r.restli, r.status)
			}
		}
	}
	if campaignUpdated != 1 {
		t.Errorf("campaign updated %d times, want 1", campaignUpdated)
	}
	if finderCalled != 1 {
		t.Errorf("creatives finder called %d times, want 1", finderCalled)
	}
	if creativeUpdates != 2 {
		t.Errorf("creative updates = %d, want 2 (one per discovered creative)", creativeUpdates)
	}
}

// TestLinkedIn_ToggleStatus_UnsupportedStatusRejected verifies an unsupported run state is
// rejected before any call (no creative discovery needed).
func TestLinkedIn_ToggleStatus_UnsupportedStatusRejected(t *testing.T) {
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderLinkedInAds, &model.Campaign{PlatformCampaignID: "555"}, "RUNNING"); err == nil {
		t.Error("expected an error for an unsupported run status")
	}
}

func TestLinkedIn_ToggleStatus_5xxIsUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // the campaign PARTIAL_UPDATE 5xxes first
	}))
	defer srv.Close()
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderLinkedInAds, &model.Campaign{PlatformCampaignID: "555"}, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error on a 5xx toggle")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx toggle must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestLinkedIn_ToggleStatus_NoOrgIDNeeded proves a status update works with a connection
// that has an access token + account id but NO org_id (Dispatch requires org_id; a toggle
// must not) — locking in that contract against a future refactor.
func TestLinkedIn_ToggleStatus_NoOrgIDNeeded(t *testing.T) {
	conn := &model.Connection{
		Provider:             model.ProviderLinkedInAds,
		AccountID:            "123456789",
		EncryptedCredentials: []byte(goodLinkedInCreds), // {"AccessToken":"tok"} — no org_id in ProviderConfig
		Status:               model.StatusActive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/creatives") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`) // no creatives to cascade
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := NewLinkedInDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderLinkedInAds, &model.Campaign{PlatformCampaignID: "555"}, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus must work without an org_id: %v", err)
	}
}
