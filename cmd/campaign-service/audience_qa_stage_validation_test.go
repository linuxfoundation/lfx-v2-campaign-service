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

// TestRunAudienceQaStageIsValidatedAtTheEdge drives run-audience-qa's optional `stage` through
// the REAL mux (#244). A stage outside the brief stage vocabulary is a 400 from the generated
// decoder, so a misspelt "Post Event" can never quietly run check 4 as a registration push. A
// valid stage and an absent one pass the decoder and reach the service, which here has no
// explorer and answers 503.
func TestRunAudienceQaStageIsValidatedAtTheEdge(t *testing.T) {
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

	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/projects/cncf/audience-builder/qa/run", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	for name, stage := range map[string]string{
		"unhyphenated": "Post Event",
		"lower case":   "post-event",
		"unknown":      "Thank You",
	} {
		t.Run("refused/"+name, func(t *testing.T) {
			rec := post(t, `{"list_ref":"123","event_name":"KubeCon 2026","stage":"`+stage+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for stage %q; body %s", rec.Code, stage, rec.Body.String())
			}
			var resp struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("400 body is not the JSON error shape: %v; body %s", err, rec.Body.String())
			}
			if !strings.Contains(resp.Message, "stage") {
				t.Errorf("400 message %q does not name the rejected field", resp.Message)
			}
		})
	}

	for name, body := range map[string]string{
		"post-event":        `{"list_ref":"123","event_name":"KubeCon 2026","stage":"Post-Event"}`,
		"registration push": `{"list_ref":"123","event_name":"KubeCon 2026","stage":"Registration Push"}`,
		"absent":            `{"list_ref":"123","event_name":"KubeCon 2026"}`,
	} {
		t.Run("accepted/"+name, func(t *testing.T) {
			if rec := post(t, body); rec.Code == http.StatusBadRequest {
				t.Fatalf("a valid or absent stage was refused by the decoder: %s", rec.Body.String())
			}
		})
	}
}
