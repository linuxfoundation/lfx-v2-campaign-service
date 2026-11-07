// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	exploresvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// An explicitly empty q on the audience list (#496) is refused by the REAL mux with the
// generated decoder's 400, naming q, exactly like a blank q; an omitted q is not a 400. Goa
// alone decodes `?q=` as absent, so without refuseEmptyQueryParam this returns the unfiltered
// list instead.
func TestAudiencesEmptyQIsRefusedByTheRealMux(t *testing.T) {
	mux, err := buildMux(context.Background(), &config.Config{},
		svc.NewEndpoints(service.NewCampaignService(nil)),
		connsvc.NewEndpoints(service.NewConnectionService(nil, nil)),
		briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil)),
		audiencesvc.NewEndpoints(service.NewAudienceService(nil)),
		exploresvc.NewEndpoints(service.NewAudienceExploreService(nil)),
		nil, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	do := func(rawQuery string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/projects/cncf/google-ads/audiences?"+rawQuery, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, raw := range []string{"type=audience&q=", "q=&type=audience", "type=audience&q", "type=audience&q=%20", "type=audience&q=&q=kube"} {
		rec := do(raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("?%s: status = %d, want 400; body %s", raw, rec.Code, rec.Body.String())
			continue
		}
		var resp struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !strings.Contains(resp.Message, "q") {
			t.Errorf("?%s: 400 body %s does not name q (err %v)", raw, rec.Body.String(), err)
		}
	}
	for _, raw := range []string{"type=audience", "type=audience&q=kube"} {
		if rec := do(raw); rec.Code == http.StatusBadRequest {
			t.Errorf("?%s: status 400, want the request past the decoder; body %s", raw, rec.Body.String())
		}
	}
}

// The Meta custom-audience picker (#550) refuses an explicitly empty q the same way: the real mux
// answers 400 naming q, and an omitted or non-blank q gets past the decoder.
func TestMetaCustomAudiencesEmptyQIsRefusedByTheRealMux(t *testing.T) {
	mux, err := buildMux(context.Background(), &config.Config{},
		svc.NewEndpoints(service.NewCampaignService(nil)),
		connsvc.NewEndpoints(service.NewConnectionService(nil, nil)),
		briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil)),
		audiencesvc.NewEndpoints(service.NewAudienceService(nil)),
		exploresvc.NewEndpoints(service.NewAudienceExploreService(nil)),
		nil, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}
	do := func(rawQuery string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/projects/cncf/meta-ads/custom-audiences?"+rawQuery, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, raw := range []string{"q=", "q", "q=%20", "q=&q=kube"} {
		rec := do(raw)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "q") {
			t.Errorf("?%s: status = %d, want a 400 naming q; body %s", raw, rec.Code, rec.Body.String())
		}
	}
	for _, raw := range []string{"", "q=kube"} {
		if rec := do(raw); rec.Code == http.StatusBadRequest {
			t.Errorf("?%s: status 400, want the request past the decoder; body %s", raw, rec.Body.String())
		}
	}
}
