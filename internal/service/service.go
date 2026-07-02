// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service contains the campaign service business logic, including the
// implementation of the generated Goa service interface.
package service

import (
	"context"

	campaignsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
)

// CampaignService implements the generated campaign service interface.
//
// The service currently has no external runtime dependencies wired in. The
// readiness predicate is structured so that future dependency checks (e.g.,
// messaging, data stores) can be combined into ServiceReady without changing
// the health endpoint contract.
type CampaignService struct {
	// ready reports whether the service can accept inbound requests. It is a
	// field (rather than a hardcoded return) so readiness can be exercised in
	// tests and so future dependency checks can be AND-ed in here.
	ready bool
}

// Ensure CampaignService satisfies the generated service interface.
var _ campaignsvc.Service = (*CampaignService)(nil)

// NewCampaignService constructs a CampaignService that is ready to serve.
func NewCampaignService() *CampaignService {
	return &CampaignService{ready: true}
}

// ServiceReady reports whether the service is able to accept inbound requests.
//
// With no external dependencies wired, this returns true once the service is
// constructed. Future dependency checks should be combined here with logical
// AND (e.g., return s.ready && s.natsConn.IsConnected()).
func (s *CampaignService) ServiceReady() bool {
	return s.ready
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
