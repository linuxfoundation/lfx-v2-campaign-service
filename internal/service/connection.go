// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service — connection service stub.
//
// This is the Phase 1 (LFXV2-2554) stub: it satisfies the generated connection
// service interface and its Auther so the contract compiles and serves, but
// every method returns "not implemented". The real business logic (validate,
// encrypt, persist, audit) and in-app JWT validation land in LFXV2-2556, and
// the remaining providers (linkedin-ads, meta-ads, …) are added alongside
// google-ads.
package service

import (
	"context"
	"errors"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"

	"goa.design/goa/v3/security"
)

// ConnectionService implements the generated connection service interface.
type ConnectionService struct{}

// Ensure ConnectionService satisfies the generated interfaces.
var (
	_ conn.Service = (*ConnectionService)(nil)
	_ conn.Auther  = (*ConnectionService)(nil)
)

// NewConnectionService constructs the connection service stub.
func NewConnectionService() *ConnectionService {
	return &ConnectionService{}
}

// errNotImplemented is the placeholder returned by every stub method until the
// persistence layer (LFXV2-2556) is wired in.
func errNotImplemented() error {
	return &conn.InternalServerError{Message: "connection endpoints are not implemented yet (LFXV2-2556)"}
}

// JWTAuth authorizes a request. The Heimdall gateway has already validated the
// JWT and enforced the campaign_manager relation; in-app validation of the
// token audience is wired in LFXV2-2556. For now a non-empty token is accepted.
func (s *ConnectionService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, errors.New("missing bearer token")
	}
	return ctx, nil
}

// CreateGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateGoogleAds(_ context.Context, _ *conn.CreateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetGoogleAds(_ context.Context, _ *conn.GetGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateGoogleAds(_ context.Context, _ *conn.UpdateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteGoogleAds(_ context.Context, _ *conn.DeleteGoogleAdsPayload) error {
	return errNotImplemented()
}

// TestGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestGoogleAds(_ context.Context, _ *conn.TestGoogleAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialGoogleAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialGoogleAds(_ context.Context, _ *conn.SetCredentialGoogleAdsPayload) error {
	return errNotImplemented()
}
