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
	"time"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// TestMeta_PreflightCreate_PublishIdentity: the launch-readiness rules are a synchronous 400 at
// create time — an EU target without both DSA disclosures, a one-sided pair, a malformed Instagram
// id — and a config that satisfies them is left to Dispatch.
func TestMeta_PreflightCreate_PublishIdentity(t *testing.T) {
	runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
		{name: "US, nothing supplied", config: `{"metaConfig":{"geoTargets":["US"]}}`},
		{name: "no geo (defaults to US)", config: `{"metaConfig":{}}`},
		{name: "EU with both disclosures and an Instagram id", config: `{"metaConfig":{"geoTargets":["DE","US"],"dsaBeneficiary":"CNCF","dsaPayor":"The Linux Foundation","instagramUserId":"17841400000000000"}}`},
		{name: "EU without disclosures", config: `{"metaConfig":{"geoTargets":["US","fr"]}}`,
			want: "invalid campaign config: metaConfig: EU DSA disclosures are required: geoTargets include EU countries (FR)"},
		{name: "one-sided pair", config: `{"metaConfig":{"geoTargets":["US"],"dsaPayor":"LF"}}`,
			want: "dsaPayor was supplied without dsaBeneficiary"},
		{name: "malformed Instagram id", config: `{"metaConfig":{"instagramUserId":"lfprojects"}}`,
			want: "instagramUserId"},
		// A wrong-typed identity field is refused on its type, not read as absent (PR #414 review).
		{name: "boolean Instagram id", config: `{"metaConfig":{"geoTargets":["US"],"instagramUserId":true}}`,
			want: `invalid campaign config: metaConfig: "instagramUserId" must be a string`},
		{name: "numeric beneficiary on an EU target", config: `{"metaConfig":{"geoTargets":["DE"],"dsaBeneficiary":1,"dsaPayor":"LF"}}`,
			want: `"dsaBeneficiary" must be a string`},
		{name: "object payor, case-variant key", config: `{"metaConfig":{"DSAPayor":{},"dsaBeneficiary":"CNCF"}}`,
			want: `"DSAPayor" must be a string`},
		{name: "null disclosures read as absent", config: `{"metaConfig":{"geoTargets":["US"],"dsaBeneficiary":null,"dsaPayor":null,"instagramUserId":null}}`},
		// Best-effort decode: an unrelated wrong-typed field must not let an EU target fail open.
		{name: "EU without disclosures, broken budget", config: `{"metaConfig":{"budget":"lots","geoTargets":["IT"]}}`,
			want: "EU DSA disclosures are required"},
	})
}

// TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest: Dispatch applies the same rule for a path
// that skipped the pre-check — refused pre-create, claim released, and Meta is never called.
//
// The Meta client runs the same check as its own backstop, so "no Meta request" alone would
// still hold with the dispatcher's check deleted (#429). What only the dispatcher's check
// prevents is the work Dispatch does BEFORE the client exists: resolving a variant's uploaded
// image reads the asset store and reserves memory for its bytes. So one variant references an
// asset, and the refusal must come before that read.
func TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	assets := &fakeCreativeAssets{asset: &model.CreativeAsset{Bytes: []byte("IMAGE_BYTES"), MimeType: "image/png"}}
	d.SetCreativeAssetRepo(assets)
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["ES"],
		"variants":[{"primaryText":"Join us","headline":"KubeCon","imageAssetId":"6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate ErrCreateConfigInvalid refusal", camp, err)
	}
	if !strings.Contains(err.Error(), "EU DSA disclosures are required") {
		t.Fatalf("Dispatch = %v, want the EU DSA refusal", err)
	}
	if n := assets.callCount(); n != 0 {
		t.Fatalf("asset store read %d times before the refusal, want 0: the dispatcher must refuse before resolving images", n)
	}
}

// TestCreateCampaigns_MetaEUWithoutDSAIsSynchronous400: through the real create path, the refusal
// reaches the caller as a 400 with its actionable text, and no job is created.
func TestCreateCampaigns_MetaEUWithoutDSAIsSynchronous400(t *testing.T) {
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf", EventSlug: "kubecon", Status: model.BriefApproved,
		DeliveryType: model.DeliveryPaidMarketing, Version: 1}
	jobs := &e2eJobRepo{}
	orch := service.NewOrchestrator(nil, jobs, map[model.Provider]service.PlatformDispatcher{
		model.ProviderMetaAds: &MetaDispatcher{},
	})
	var campaigns struct{ domain.CampaignRepository }
	s := service.NewBriefService(e2eBriefRepo{brief: brief}, campaigns, jobs, orch)
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{"meta-ads"},
			Config: map[string]any{"metaConfig": map[string]any{"geoTargets": []any{"US", "DE"}}}},
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) || bad.Code != "400" || !strings.Contains(bad.Message, "EU DSA disclosures are required") {
		t.Fatalf("CreateCampaigns = %v, want a 400 naming the EU DSA requirement", err)
	}
	if n := jobs.count(); n != 0 {
		t.Fatalf("%d job creation attempts for a refused create, want 0", n)
	}
}

// TestMeta_DispatchEUCreateCarriesTrimmedDisclosures: an accepted EU create, through Dispatch, puts
// the caller's padded disclosures on the ad set trimmed, and the Instagram id on the creative —
// the hand-off checkMetaPublishIdentity's copy relies on the client to normalize.
func TestMeta_DispatchEUCreateCarriesTrimmedDisclosures(t *testing.T) {
	var (
		mu                      sync.Mutex
		adSetBody, creativeBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		decode := func() map[string]any {
			var m map[string]any
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &m); err != nil {
				// Form-encoded bodies are not expected on these paths; fail loudly if one appears.
				t.Errorf("decode %s body: %v", r.URL.Path, err)
			}
			return m
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			mu.Lock()
			adSetBody = decode()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			mu.Lock()
			creativeBody = decode()
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"currencyOffset":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":["DE"],"dsaBeneficiary":"  CNCF  ","dsaPayor":" The Linux Foundation ","instagramUserId":" 17841400000000000 ",
		"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if adSetBody["dsa_beneficiary"] != "CNCF" || adSetBody["dsa_payor"] != "The Linux Foundation" {
		t.Fatalf("ad set disclosures = %v / %v, want trimmed CNCF / The Linux Foundation", adSetBody["dsa_beneficiary"], adSetBody["dsa_payor"])
	}
	if creativeBody["instagram_user_id"] != "17841400000000000" {
		t.Fatalf("creative instagram_user_id = %v, want the trimmed IGSID", creativeBody["instagram_user_id"])
	}
}
