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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
)

const goodMetaCreds = `{"AccessToken":"tok"}`

func activeMetaConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_777",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"page_id": "987654321"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestMeta_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, errEncryptor{}},
		{"empty access token", fakeConnReader{conn: activeMetaConn(`{"AccessToken":""}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), ProviderConfig: map[string]string{"page_id": "p"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing page id", fakeConnReader{conn: &model.Connection{Provider: model.ProviderMetaAds, AccountID: "act_1", EncryptedCredentials: []byte(goodMetaCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewMetaDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestMeta_BadConfigIsPreCreate(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// TestMeta_ClientPreCreateRejectionReleasesClaim exercises the `result == nil`
// RELEASE branch (meta.go: "failed before any upstream create" -> notCreated), which
// the other pre-create tests don't reach — they fail during envelope decode or before
// the client is called. Here the connection is active and the config is syntactically
// valid and passes the dispatcher's own checks, so the flow reaches the real Meta
// client; the client then rejects it BEFORE its first upstream create because it
// carries no ad variants (client.go: "at least one ad variant is required"), returning
// (nil, err). The adapter must map that to a NoUpstreamCreate error so the orchestrator
// RELEASES the claim (nothing was created upstream) — the release half of the
// client-result contract.
func TestMeta_ClientPreCreateRejectionReleasesClaim(t *testing.T) {
	// A server that fails any request, proving the rejection happens BEFORE the client
	// issues its first create (no request should reach here).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("client must reject the variant-less config before any upstream HTTP call")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	// Valid budget/dates/objective but NO variants — passes envelope decode + the
	// dispatcher's checks, reaches the client, and is rejected pre-create.
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if camp != nil {
		t.Errorf("a pre-create rejection must return a nil campaign, got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a client pre-create rejection must be NoUpstreamCreate (release the claim), got %T: %v", err, err)
	}
}

// ---- happy path through an httptest meta API ------------------------------

func TestMeta_DispatchSuccessMapsResult(t *testing.T) {
	// Capture every mutating request path + body so we can assert the full mapping
	// contract, not just the returned id. A mapping regression (a dropped pixel,
	// placements, lifetime budget, account/page id, or geo target) must fail this test
	// rather than quietly create a materially different paid campaign.
	type captured struct {
		path string
		body string
	}
	var (
		mu                   sync.Mutex
		reqs                 []captured
		creativeCount, adCnt int32
	)
	record := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, captured{path: r.URL.Path, body: string(b)})
		mu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			record(r)
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			record(r)
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			record(r)
			n := atomic.AddInt32(&creativeCount, 1)
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(int(n))+`"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			record(r)
			n := atomic.AddInt32(&adCnt, 1)
			_, _ = io.WriteString(w, `{"id":"ad_`+strconv.Itoa(int(n))+`"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	// NON-DEFAULT values for every mapped field: a lifetime budget, an explicit
	// objective ("conversions" → OUTCOME_SALES + a numeric pixel promoted object), two
	// geo targets, an explicit facebook-ONLY placement (InstagramFeed:false, overriding
	// the client default that enables both feeds), and two variants (→ two creatives +
	// two ads). currencyOffset set → preflight skips FX derivation.
	//
	// NOTE the placement keys: metaConfig.Placements is a meta.Placement, which has NO
	// json tags, so the JSON keys are the Go field names (FacebookFeed/InstagramFeed) —
	// lowercase "facebook"/"instagram" would be silently ignored and the client would
	// apply its both-feeds default. We assert below that instagram is actually absent.
	cfg := json.RawMessage(`{"metaConfig":{
		"budget":2500,"lifetimeBudget":true,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"conversions","geoTargets":["US","GB"],"currencyOffset":100,
		"pixelId":"555000111","placements":{"FacebookFeed":true,"InstagramFeed":false},
		"variants":[
			{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"Cloud native event"},
			{"headline":"Register now","primaryText":"Early bird pricing","description":"Save your seat"}
		]
	}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "camp_123" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("success status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	// Persistence-contract columns populated from the config (config uses a LIFETIME
	// budget of 2500, so BudgetType must be lifetime — not left NULL or daily).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 2500 {
		t.Errorf("BudgetAmount = %v, want 2500", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetLifetime {
		t.Errorf("BudgetType = %v, want lifetime (lifetimeBudget:true)", camp.BudgetType)
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2099-01-01" {
		t.Errorf("StartDate = %v, want 2099-01-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2099-02-01" {
		t.Errorf("EndDate = %v, want 2099-02-01", camp.EndDate)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot should capture the validated meta config")
	}

	// Per-variant fan-out: two variants → two creatives + two ads.
	if got := atomic.LoadInt32(&creativeCount); got != 2 {
		t.Errorf("adcreatives created = %d, want 2 (one per variant)", got)
	}
	if got := atomic.LoadInt32(&adCnt); got != 2 {
		t.Errorf("ads created = %d, want 2 (one per variant)", got)
	}

	// Assert the mapped fields landed in the outbound request bodies. We match on the
	// account/page ids and each config field so a dropped mapping fails loudly.
	mu.Lock()
	defer mu.Unlock()
	find := func(suffix string) string {
		for _, rq := range reqs {
			if strings.HasSuffix(rq.path, suffix) {
				return rq.body
			}
		}
		return ""
	}
	// Every mutating request must target the connection's ad account (act_777) in its path.
	for _, rq := range reqs {
		if !strings.Contains(rq.path, "act_777") {
			t.Errorf("request path %q does not target the connection account act_777", rq.path)
		}
	}
	// Campaign body carries the mapped objective ("conversions" → OUTCOME_SALES).
	campBody := find("/campaigns")
	if !strings.Contains(campBody, "OUTCOME_SALES") {
		t.Errorf("campaign body missing objective OUTCOME_SALES (from \"conversions\")\nbody: %s", campBody)
	}
	// Ad-set body carries budget (lifetime, in minor units = 2500*100), geo countries,
	// and the pixel promoted object (conversions objective).
	adsetBody := find("/adsets")
	if !strings.Contains(adsetBody, `"lifetime_budget"`) {
		t.Errorf("adset body should use lifetime_budget (lifetimeBudget:true)\nbody: %s", adsetBody)
	}
	if strings.Contains(adsetBody, `"daily_budget"`) {
		t.Errorf("adset body must NOT use daily_budget when lifetimeBudget:true\nbody: %s", adsetBody)
	}
	if !strings.Contains(adsetBody, "250000") { // 2500 * currencyOffset(100)
		t.Errorf("adset body missing minor-unit budget 250000 (2500 * offset 100)\nbody: %s", adsetBody)
	}
	for _, want := range []string{"US", "GB"} { // geo targets
		if !strings.Contains(adsetBody, want) {
			t.Errorf("adset body missing geo target %q\nbody: %s", want, adsetBody)
		}
	}
	if !strings.Contains(adsetBody, "555000111") { // numeric pixel on promoted_object
		t.Errorf("adset body missing the pixel id 555000111\nbody: %s", adsetBody)
	}
	// facebook-only placement (InstagramFeed:false) → targeting must include facebook
	// and must NOT include instagram. Asserting the ABSENCE is what proves the override
	// was honored (a silently-ignored placement key would leave the default both-feeds
	// on and instagram would appear here).
	if !strings.Contains(adsetBody, "facebook") {
		t.Errorf("adset targeting missing the facebook placement\nbody: %s", adsetBody)
	}
	if strings.Contains(adsetBody, "instagram") {
		t.Errorf("adset targeting must NOT include instagram (InstagramFeed:false)\nbody: %s", adsetBody)
	}
	// The connection's page id (987654321) rides on each creative's object_story_spec.
	creativeBody := find("/adcreatives")
	if !strings.Contains(creativeBody, "987654321") {
		t.Errorf("creative object_story_spec missing the connection page id 987654321\nbody: %s", creativeBody)
	}
}

func TestMeta_DegradedSuccessSetsCreatedDegraded(t *testing.T) {
	// Two variants requested, but the SECOND ad POST fails (Meta rejects it). Meta
	// treats per-variant ad failures as non-fatal, so CreateCampaign returns
	// (result, nil) with AdCount=1 < 2 — a DEGRADED success. The adapter must persist
	// created_degraded, not a clean created (which would let idempotency block a
	// re-dispatch while the missing ad is only visible inside the result blob).
	var adCnt int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			// First ad succeeds; the second is rejected → AdCount ends at 1 of 2.
			if atomic.AddInt32(&adCnt, 1) == 1 {
				_, _ = io.WriteString(w, `{"id":"ad_1"}`)
			} else {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
			}
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[
		{"headline":"A","primaryText":"first","description":"d1"},
		{"headline":"B","primaryText":"second","description":"d2"}
	]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("a degraded success (campaign created, one ad failed) must NOT error: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "camp_123" {
		t.Fatalf("the created campaign must still be mapped, got %+v", camp)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("status = %q, want %q (fewer ads created than requested is a degraded success)", camp.Status, campaignStatusCreatedDegraded)
	}
}

func TestMeta_ConfigHSTokenTakesPrecedence(t *testing.T) {
	// config.hsToken is a documented top-level field and must drive utm_campaign,
	// taking precedence over any brief token — not be silently ignored.
	var creativeBody string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"camp_123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"adset_456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			creativeBody = string(b) // the creative carries the utm link
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	cfg := json.RawMessage(`{"hsToken":"HS-FROM-CONFIG","metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"headline":"A","primaryText":"first","description":"d1"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	got := creativeBody
	mu.Unlock()
	if !strings.Contains(got, "HS-FROM-CONFIG") {
		t.Errorf("config.hsToken must drive utm_campaign; creative link did not carry it: %q", got)
	}
}

func TestMeta_AmbiguousCreateRetainsClaim(t *testing.T) {
	// A 5xx on the campaign POST is ambiguous → the meta client returns a non-nil
	// name-only partial (empty CampaignID). The adapter must retain the claim.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "account_status") {
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the campaign POST
	}))
	defer srv.Close()
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(clock),
	)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"headline":"KubeCon 2099","primaryText":"Join us — it's great","description":"x"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
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
	// The name-only reconciliation contract: no upstream id was confirmed, but the
	// deterministic campaign name + result blob must survive so the orphan can be
	// reconciled on retry. A regression that dropped these — or wrongly populated an
	// upstream id — must fail here.
	if camp.PlatformCampaignID != "" {
		t.Errorf("an ambiguous create must NOT report an upstream campaign id, got %q", camp.PlatformCampaignID)
	}
	if camp.CampaignName == "" {
		t.Error("an ambiguous create must retain the deterministic campaign name for reconciliation")
	}
	if len(camp.Result) == 0 {
		t.Error("an ambiguous create must retain the result blob for reconciliation")
	}
}

// TestMeta_ToggleStatus_PostsStatus verifies the dispatcher resolves creds and POSTs the
// status to the campaign node via the meta client.
func TestMeta_ToggleStatus_PostsStatus(t *testing.T) {
	// Capture the request over a channel so the handler-goroutine writes happen-before the
	// test-goroutine reads — race-safe under `go test -race`.
	type req struct{ method, path, status string }
	gotCh := make(chan req, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status, _ := body["status"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
		gotCh <- req{r.Method, r.URL.Path, status}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	got := <-gotCh
	if got.method != http.MethodPost || got.path != "/23847290" {
		t.Errorf("request = %s %s, want POST /23847290", got.method, got.path)
	}
	if got.status != "PAUSED" {
		t.Errorf("status = %q, want PAUSED", got.status)
	}
	// An unsupported run state is rejected before any call.
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, "RUNNING"); err == nil {
		t.Error("expected an error for an unsupported run status")
	}
}

// TestMeta_ToggleStatus_5xxIsUnconfirmed verifies a 5xx surfaces as Unconfirmed().
func TestMeta_ToggleStatus_5xxIsUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, model.CampaignRunActive)
	if err == nil {
		t.Fatal("expected an error on a 5xx toggle")
	}
	var unconf interface{ Unconfirmed() bool }
	if !errors.As(err, &unconf) || !unconf.Unconfirmed() {
		t.Errorf("a 5xx toggle must be Unconfirmed(), got %T: %v", err, err)
	}
}

// TestMeta_ToggleStatus_NoPageIDNeeded proves a status update works with a connection that
// has an access token + account id but NO page_id (Dispatch requires page_id; a toggle must
// not) — locking in that contract against a future refactor.
func TestMeta_ToggleStatus_NoPageIDNeeded(t *testing.T) {
	conn := &model.Connection{
		Provider:             model.ProviderMetaAds,
		AccountID:            "act_1",
		EncryptedCredentials: []byte(goodMetaCreds), // {"AccessToken":"tok"} — no page_id in ProviderConfig
		Status:               model.StatusActive,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderMetaAds, &model.Campaign{PlatformCampaignID: "23847290"}, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus must work without a page_id: %v", err)
	}
}
