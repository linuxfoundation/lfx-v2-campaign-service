// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	audiencesvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// TestConnectionRoutesAreMounted locks in the invariant this PR establishes: the
// connection routes are actually reachable on the mux. The bug being fixed —
// generated routes that compile but are never mounted — is invisible to the
// service-layer tests (which call handlers directly), so without this test a
// future deletion of the connsvcsvr.Mount call would silently reintroduce the
// 404 regression. We assert a known connection route resolves to a real handler
// (any non-404 status, e.g. 401/503, proves it is mounted).
func TestConnectionRoutesAreMounted(t *testing.T) {
	endpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	// The no-DB connection and brief services are real Services whose routes
	// return a typed 503 rather than 404 — perfect for proving the route is
	// mounted.
	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))

	mux, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, audienceEndpoints)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	// One route per mounted server is enough to lock in the mount.
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"connection google-ads create", http.MethodPost, "/projects/proj-123/connection-google-ads"},
		{"brief create", http.MethodPost, "/projects/proj-123/briefs"},
		{"audience list", http.MethodGet, "/projects/proj-123/briefs/brief-1/audiences"},
		{"campaign health livez", http.MethodGet, "/livez"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s %s returned 404 — route is not mounted", tc.method, tc.path)
			}
		})
	}
}

// TestBuildMuxNilEndpointsFailsLoud verifies the fail-loud guard: a nil
// connEndpoints or briefEndpoints (a programmer-level mis-wiring) returns an
// error rather than silently building a mux with those routes unmounted.
func TestBuildMuxNilEndpointsFailsLoud(t *testing.T) {
	endpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))

	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, nil, briefEndpoints, audienceEndpoints); err == nil {
		t.Error("expected buildMux to fail loudly when connEndpoints is nil, got nil error")
	}
	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, nil, audienceEndpoints); err == nil {
		t.Error("expected buildMux to fail loudly when briefEndpoints is nil, got nil error")
	}
	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, nil); err == nil {
		t.Error("expected buildMux to fail loudly when audienceEndpoints is nil, got nil error")
	}
}
