// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service contains the campaign service business logic, including the
// implementation of the generated Goa service interface.
package service

import (
	"context"
	"log/slog"
	"sync"
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

	// mu guards dep, which can be swapped in after construction: during a cold
	// start the container boots the service with a dep that reports NOT-ready (the
	// notReady{} checker — NOT nil, since a nil dep is treated as ready), then
	// injects the live pool via SetReadinessDep once migrations succeed. The swap
	// happens on a background goroutine while probe requests read dep concurrently,
	// so access is guarded.
	mu sync.RWMutex
	// dep is an optional backing dependency whose health is AND-ed into readiness.
	// A NIL dep means no readiness dependency, so /readyz is ready (this is the
	// no-database mode). To hold readiness DOWN until the DB is up, a non-nil
	// always-false checker (notReady{}) must be wired — not nil.
	dep ReadinessChecker
}

// CampaignService is exported for tests that need to toggle readiness directly.
type CampaignService = campaignService

// Ensure CampaignService satisfies the generated service interface.
var _ campaignsvc.Service = (*CampaignService)(nil)

// NewCampaignService constructs a CampaignService. A NIL dep means no readiness
// dependency and /readyz reports ready (the no-database mode). To keep /readyz
// NOT-ready during a cold start until the DB is up, the caller must pass a non-nil
// checker that returns false (e.g. notReady{}), NOT nil — Readyz skips a nil dep.
func NewCampaignService(dep ReadinessChecker) *CampaignService {
	return &CampaignService{ready: true, dep: dep}
}

// SetReadinessDep swaps in (or clears) the readiness dependency after
// construction. Used by the container to inject the database pool once it opens
// during a cold start, flipping /readyz from 503 to healthy. Safe for concurrent
// use with the Readyz/ServiceReady readers.
func (s *CampaignService) SetReadinessDep(dep ReadinessChecker) {
	s.mu.Lock()
	s.dep = dep
	s.mu.Unlock()
}

// readinessDep returns the current readiness dependency under the read lock.
func (s *CampaignService) readinessDep() ReadinessChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dep
}

// ServiceReady reports whether the service is able to accept inbound requests:
// the service is constructed and every wired dependency is healthy.
// Prefer Readyz for probe paths so the request context bounds the dependency
// check; this helper remains for tests and callers without a request context.
func (s *CampaignService) ServiceReady() bool {
	if !s.ready {
		return false
	}
	if dep := s.readinessDep(); dep != nil {
		ctx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		defer cancel()
		return dep.Ready(ctx)
	}
	return true
}

// Readyz checks if the service is able to take inbound requests, including a
// lightweight PostgreSQL connectivity check when a database dependency is wired.
func (s *CampaignService) Readyz(ctx context.Context) ([]byte, error) {
	if !s.ready {
		slog.DebugContext(ctx, "readyz: service not initialized")
		return nil, &campaignsvc.ServiceUnavailableError{
			Code:    "503",
			Message: "The service is unavailable.",
		}
	}

	if dep := s.readinessDep(); dep != nil {
		pingCtx, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
		defer cancel()
		if !dep.Ready(pingCtx) {
			slog.DebugContext(ctx, "readyz: database dependency not ready")
			return nil, &campaignsvc.ServiceUnavailableError{
				Code:    "503",
				Message: "The service is unavailable.",
			}
		}
	}

	return []byte("OK\n"), nil
}

// Livez checks if the service is alive.
//
// This always returns OK as long as the process can respond. It deliberately
// does not call the database: Kubernetes uses livez to restart hung processes,
// and a DB outage must not trigger restarts.
func (s *CampaignService) Livez(_ context.Context) ([]byte, error) {
	return []byte("OK\n"), nil
}
