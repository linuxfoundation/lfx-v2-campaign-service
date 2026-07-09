// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service contains the campaign service business logic, including the
// implementation of the generated Goa service interface.
package service

import (
	"context"
	"time"

	campaignsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
)

// readinessProbeTimeout bounds the dependency health check so a slow or
// unreachable backing dependency makes readiness fail fast rather than blocking
// the /readyz handler.
const readinessProbeTimeout = 2 * time.Second

// ReadinessChecker reports whether a backing dependency (e.g. the database
// pool) can serve requests.
type ReadinessChecker interface {
	Ready(ctx context.Context) bool
}

type campaignService struct {
	// ready reports whether the service can accept inbound requests. It is a
	// field (rather than a hardcoded return) so readiness can be exercised in
	// tests.
	ready bool
	// dep is an optional backing dependency whose health is AND-ed into
	// readiness (nil when no database is wired).
	dep ReadinessChecker
}

// CampaignService is exported for tests that need to toggle readiness directly.
type CampaignService = campaignService

// Ensure CampaignService satisfies the generated service interface.
var _ campaignsvc.Service = (*CampaignService)(nil)

// NewCampaignService constructs a CampaignService. dep may be nil (no database
// wired); when non-nil, its health is required for readiness.
func NewCampaignService(dep ReadinessChecker) *CampaignService {
	return &CampaignService{ready: true, dep: dep}
}

// ServiceReady reports whether the service is able to accept inbound requests:
// the service is constructed and every wired dependency is healthy.
func (s *CampaignService) ServiceReady() bool {
	if !s.ready {
		return false
	}
	if s.dep != nil {
		ctx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		defer cancel()
		return s.dep.Ready(ctx)
	}
	return true
}

// Readyz checks if the service is able to take inbound requests.
func (s *CampaignService) Readyz(_ context.Context) ([]byte, error) {
	if !s.ServiceReady() {
		return nil, &campaignsvc.ServiceUnavailableError{
			Code:    "503",
			Message: "The service is unavailable.",
		}
	}
	return []byte("OK\n"), nil
}

// Livez checks if the service is alive.
func (s *CampaignService) Livez(_ context.Context) ([]byte, error) {
	// This always returns OK as long as the service is still running. As this
	// endpoint is used as a Kubernetes liveness check, the service must
	// self-detect non-recoverable errors and self-terminate (the process exits
	// non-zero on a fatal startup error in main).
	return []byte("OK\n"), nil
}
