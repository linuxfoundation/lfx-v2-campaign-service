// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service — connection service stub.
//
// This is the Phase 1 (LFXV2-2554) stub: it satisfies the generated connection
// service interface and its Auther so the contract compiles and serves, but
// every method returns "not implemented". The real business logic (validate,
// encrypt, persist, audit) and in-app JWT validation land in LFXV2-2556.
//
// The 42 methods below (7 providers x 6 endpoints) are intentionally
// mechanical; they exist so the generated interface is satisfied end to end.
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

// CreateLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateLinkedinAds(_ context.Context, _ *conn.CreateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetLinkedinAds(_ context.Context, _ *conn.GetLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateLinkedinAds(_ context.Context, _ *conn.UpdateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteLinkedinAds(_ context.Context, _ *conn.DeleteLinkedinAdsPayload) error {
	return errNotImplemented()
}

// TestLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestLinkedinAds(_ context.Context, _ *conn.TestLinkedinAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialLinkedinAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialLinkedinAds(_ context.Context, _ *conn.SetCredentialLinkedinAdsPayload) error {
	return errNotImplemented()
}

// CreateMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateMetaAds(_ context.Context, _ *conn.CreateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetMetaAds(_ context.Context, _ *conn.GetMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateMetaAds(_ context.Context, _ *conn.UpdateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteMetaAds(_ context.Context, _ *conn.DeleteMetaAdsPayload) error {
	return errNotImplemented()
}

// TestMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestMetaAds(_ context.Context, _ *conn.TestMetaAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialMetaAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialMetaAds(_ context.Context, _ *conn.SetCredentialMetaAdsPayload) error {
	return errNotImplemented()
}

// CreateRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateRedditAds(_ context.Context, _ *conn.CreateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetRedditAds(_ context.Context, _ *conn.GetRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateRedditAds(_ context.Context, _ *conn.UpdateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteRedditAds(_ context.Context, _ *conn.DeleteRedditAdsPayload) error {
	return errNotImplemented()
}

// TestRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestRedditAds(_ context.Context, _ *conn.TestRedditAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialRedditAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialRedditAds(_ context.Context, _ *conn.SetCredentialRedditAdsPayload) error {
	return errNotImplemented()
}

// CreateTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateTwitterAds(_ context.Context, _ *conn.CreateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetTwitterAds(_ context.Context, _ *conn.GetTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateTwitterAds(_ context.Context, _ *conn.UpdateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteTwitterAds(_ context.Context, _ *conn.DeleteTwitterAdsPayload) error {
	return errNotImplemented()
}

// TestTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestTwitterAds(_ context.Context, _ *conn.TestTwitterAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialTwitterAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialTwitterAds(_ context.Context, _ *conn.SetCredentialTwitterAdsPayload) error {
	return errNotImplemented()
}

// CreateMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateMicrosoftAds(_ context.Context, _ *conn.CreateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	return nil, errNotImplemented()
}

// GetMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetMicrosoftAds(_ context.Context, _ *conn.GetMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	return nil, errNotImplemented()
}

// UpdateMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateMicrosoftAds(_ context.Context, _ *conn.UpdateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	return nil, errNotImplemented()
}

// DeleteMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteMicrosoftAds(_ context.Context, _ *conn.DeleteMicrosoftAdsPayload) error {
	return errNotImplemented()
}

// TestMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestMicrosoftAds(_ context.Context, _ *conn.TestMicrosoftAdsPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialMicrosoftAds is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialMicrosoftAds(_ context.Context, _ *conn.SetCredentialMicrosoftAdsPayload) error {
	return errNotImplemented()
}

// CreateHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) CreateHubspot(_ context.Context, _ *conn.CreateHubspotPayload) (*conn.HubspotConnection, error) {
	return nil, errNotImplemented()
}

// GetHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) GetHubspot(_ context.Context, _ *conn.GetHubspotPayload) (*conn.HubspotConnection, error) {
	return nil, errNotImplemented()
}

// UpdateHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) UpdateHubspot(_ context.Context, _ *conn.UpdateHubspotPayload) (*conn.HubspotConnection, error) {
	return nil, errNotImplemented()
}

// DeleteHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) DeleteHubspot(_ context.Context, _ *conn.DeleteHubspotPayload) error {
	return errNotImplemented()
}

// TestHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) TestHubspot(_ context.Context, _ *conn.TestHubspotPayload) (*conn.ConnectionTestResult, error) {
	return nil, errNotImplemented()
}

// SetCredentialHubspot is not implemented yet (LFXV2-2556).
func (s *ConnectionService) SetCredentialHubspot(_ context.Context, _ *conn.SetCredentialHubspotPayload) error {
	return errNotImplemented()
}
