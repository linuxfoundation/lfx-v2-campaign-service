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
		// Best-effort decode: an unrelated wrong-typed field must not let an EU target fail open.
		{name: "EU without disclosures, broken budget", config: `{"metaConfig":{"budget":"lots","geoTargets":["IT"]}}`,
			want: "EU DSA disclosures are required"},
	})
}

// TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest: Dispatch applies the same rule for a path
// that skipped the pre-check — refused pre-create, claim released, and Meta is never called.
func TestMeta_DispatchRefusesEUWithoutDSABeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["ES"],
		"variants":[{"primaryText":"Join us","headline":"KubeCon"}]}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate ErrCreateConfigInvalid refusal", camp, err)
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
