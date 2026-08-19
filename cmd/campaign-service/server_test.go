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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/metrics"
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

	mux, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, audienceEndpoints, nil)
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

	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, nil, briefEndpoints, audienceEndpoints, nil); err == nil {
		t.Error("expected buildMux to fail loudly when connEndpoints is nil, got nil error")
	}
	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, nil, audienceEndpoints, nil); err == nil {
		t.Error("expected buildMux to fail loudly when briefEndpoints is nil, got nil error")
	}
	if _, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, nil, nil); err == nil {
		t.Error("expected buildMux to fail loudly when audienceEndpoints is nil, got nil error")
	}
}

// TestMetricsRouteIsMountedAndUnauthenticated pins the /metrics contract:
// mounted when a registry is supplied, served without any credential, and
// carrying the Prometheus text format. The unauthenticated part is deliberate
// and matches /livez and /readyz — the scraper has no bearer token — so a future
// change that puts the metrics handler behind the auth middleware must fail here.
func TestMetricsRouteIsMountedAndUnauthenticated(t *testing.T) {
	endpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))

	reg, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}

	mux, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, audienceEndpoints, reg)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	rec := httptest.NewRecorder()
	// No Authorization header: the scrape must succeed anyway.
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code == http.StatusNotFound {
		t.Fatal("GET /metrics returned 404 — the route is not mounted")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics without credentials returned %d, want 200 (the endpoint must be unauthenticated)", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text exposition format", ct)
	}
}

// TestMetricsRouteAbsentWithoutRegistry pins the other half: with no registry the
// route is NOT mounted, rather than mounted and serving an empty body. An empty
// 200 would tell a scraper the target is healthy and reporting nothing.
func TestMetricsRouteAbsentWithoutRegistry(t *testing.T) {
	endpoints := svc.NewEndpoints(service.NewCampaignService(nil))
	connEndpoints := connsvc.NewEndpoints(service.NewConnectionService(nil, nil))
	briefEndpoints := briefsvc.NewEndpoints(service.NewBriefService(nil, nil, nil, nil))
	audienceEndpoints := audiencesvc.NewEndpoints(service.NewAudienceService(nil))

	mux, err := buildMux(context.Background(), &config.Config{}, endpoints, connEndpoints, briefEndpoints, audienceEndpoints, nil)
	if err != nil {
		t.Fatalf("buildMux: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /metrics with no registry returned %d, want 404", rec.Code)
	}
}
