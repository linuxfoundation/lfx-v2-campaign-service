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
	// geo targets, an explicit facebook-only placement, and two variants (→ two
	// creatives + two ads). currencyOffset set → preflight skips FX derivation.
	cfg := json.RawMessage(`{"metaConfig":{
		"budget":2500,"lifetimeBudget":true,"startDate":"2099-01-01","endDate":"2099-02-01",
		"objective":"conversions","geoTargets":["US","GB"],"currencyOffset":100,
		"pixelId":"555000111","placements":{"facebook":true,"instagram":false},
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
	// facebook-only placement → publisher_platforms should include facebook, not instagram positions.
	if !strings.Contains(adsetBody, "facebook") {
		t.Errorf("adset targeting missing the facebook placement\nbody: %s", adsetBody)
	}
	// The connection's page id (987654321) rides on each creative's object_story_spec.
	creativeBody := find("/adcreatives")
	if !strings.Contains(creativeBody, "987654321") {
		t.Errorf("creative object_story_spec missing the connection page id 987654321\nbody: %s", creativeBody)
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
