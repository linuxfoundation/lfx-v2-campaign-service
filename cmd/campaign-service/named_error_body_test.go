// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	audiencesvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_audiences/server"
	briefsvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	connsvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	svcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_svc/server"
	exploresvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audience_builder"
	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"

	goahttp "goa.design/goa/v3/http"
	goa "goa.design/goa/v3/pkg"
)

// failingEndpoint is an endpoint that returns err without running a service method, so a
// test can put any named error a design declares in front of the generated encoder.
func failingEndpoint(err error) goa.Endpoint {
	return func(context.Context, any) (any, error) { return nil, err }
}

// TestNamedErrorBodies_AreTheGeneratedBodies drives named service errors — the ones a design
// declares with their own code/message (and, for conflicts, reason) fields — through the REAL
// mux and asserts the response is byte for byte what the generated encoder writes with a nil
// formatter: the generated New<Method><Error>ResponseBody, encoded by goahttp.ResponseEncoder.
// The generated Encode<Method>Error functions hand a named error to the server's formatter
// INSTEAD of building that body whenever the formatter is non-nil, and the named error types'
// Error() returns "" — so a formatter on the generated servers turns every one of these into a
// generic {name,id,message,...} body with an empty message and no reason. This fails whenever
// buildMux builds a generated server with a non-nil formatter.
func TestNamedErrorBodies_AreTheGeneratedBodies(t *testing.T) {
	reason := "campaign_already_adopted"
	adoptConflict := &briefsvc.ConflictError{Code: "409", Message: "campaign 123 is already adopted by another brief", Reason: &reason}
	monitorConflict := &connsvc.AccountMonitorConflictError{Code: "409", Message: "too many active campaigns for one report", Reason: "account_too_many_active_campaigns"}
	badRequest := &connsvc.BadRequestError{Code: "400", Message: "platform_campaign_id must be the numeric Microsoft Ads campaign id"}
	connUnavailable := &connsvc.ConnServiceUnavailableError{Code: "503", Message: "connection store is not configured"}
	notFound := &audiencesvc.NotFoundError{Code: "404", Message: "audience not found"}
	readyUnavailable := &svc.ServiceUnavailableError{Code: "503", Message: "database is not reachable"}

	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	connEndpoints.ResolveMicrosoftAdsCampaign = failingEndpoint(badRequest)
	connEndpoints.MonitorTwitterAdsAccount = failingEndpoint(monitorConflict)
	connEndpoints.GetMicrosoftAds = failingEndpoint(connUnavailable)
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	briefEndpoints.AdoptCampaign = failingEndpoint(adoptConflict)
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))
	audienceEndpoints.GetAudience = failingEndpoint(notFound)
	campaignEndpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	campaignEndpoints.Readyz = failingEndpoint(readyUnavailable)

	mux, err := buildMux(context.Background(), &config.Config{},
		campaignEndpoints, connEndpoints, briefEndpoints, audienceEndpoints,
		exploresvc.NewEndpoints(service.NewAudienceExploreService(nil)),
		nil, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	const (
		briefID    = "6f1c1c1e-3c55-4a8e-9a43-2f1d0f8b7a10"
		audienceID = "0b7e5f0e-8a7d-4a51-9a3e-5c2f3b1d9e21"
	)
	cases := []struct {
		name, method, target, body string
		wantStatus                 int
		wantGoaError               string
		wantBody                   any
	}{
		{"connections bad request", http.MethodGet, "/projects/cncf/microsoft-ads/campaign-ref?platform_campaign_id=123", "",
			http.StatusBadRequest, "BadRequest", connsvcsvr.NewResolveMicrosoftAdsCampaignBadRequestResponseBody(badRequest)},
		{"connections account-monitor conflict reason", http.MethodGet, "/projects/cncf/connection-twitter-ads/account-monitor?account_id=abc123&days=30", "",
			http.StatusConflict, "Conflict", connsvcsvr.NewMonitorTwitterAdsAccountConflictResponseBody(monitorConflict)},
		{"briefs adopt conflict reason", http.MethodPost, "/projects/cncf/briefs/" + briefID + "/campaigns/adopt", `{"platform":"microsoft_ads","platform_campaign_id":"123"}`,
			http.StatusConflict, "Conflict", briefsvcsvr.NewAdoptCampaignConflictResponseBody(adoptConflict)},
		{"audiences not found", http.MethodGet, "/projects/cncf/briefs/" + briefID + "/audiences/" + audienceID, "",
			http.StatusNotFound, "NotFound", audiencesvcsvr.NewGetAudienceNotFoundResponseBody(notFound)},
		{"campaign svc service unavailable", http.MethodGet, "/readyz", "",
			http.StatusServiceUnavailable, "ServiceUnavailable", svcsvr.NewReadyzServiceUnavailableResponseBody(readyUnavailable)},
		{"connections service unavailable", http.MethodGet, "/projects/cncf/connection-microsoft-ads", "",
			http.StatusServiceUnavailable, "ServiceUnavailable", connsvcsvr.NewGetMicrosoftAdsServiceUnavailableResponseBody(connUnavailable)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer test-token")
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("goa-error"); got != tc.wantGoaError {
				t.Errorf("goa-error header = %q, want %q", got, tc.wantGoaError)
			}
			want := httptest.NewRecorder()
			if err := goahttp.ResponseEncoder(context.Background(), want).Encode(tc.wantBody); err != nil {
				t.Fatalf("encode expected body: %v", err)
			}
			if rec.Body.String() != want.Body.String() {
				t.Errorf("body is not the generated named-error body\n got: %s\nwant: %s", rec.Body.String(), want.Body.String())
			}
		})
	}
}
