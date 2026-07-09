// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
)

func TestConnectionService_StubMethodsReturnNotImplemented(t *testing.T) {
	s := NewConnectionService()
	ctx := context.Background()

	// Google Ads (full CRUD + test + set-credential).
	if _, err := s.CreateGoogleAds(ctx, &conn.CreateGoogleAdsPayload{}); err == nil {
		t.Error("CreateGoogleAds: expected not-implemented error, got nil")
	}
	if _, err := s.GetGoogleAds(ctx, &conn.GetGoogleAdsPayload{}); err == nil {
		t.Error("GetGoogleAds: expected not-implemented error, got nil")
	}
	if _, err := s.UpdateGoogleAds(ctx, &conn.UpdateGoogleAdsPayload{}); err == nil {
		t.Error("UpdateGoogleAds: expected not-implemented error, got nil")
	}
	if err := s.DeleteGoogleAds(ctx, &conn.DeleteGoogleAdsPayload{}); err == nil {
		t.Error("DeleteGoogleAds: expected not-implemented error, got nil")
	}
	if _, err := s.TestGoogleAds(ctx, &conn.TestGoogleAdsPayload{}); err == nil {
		t.Error("TestGoogleAds: expected not-implemented error, got nil")
	}
	if err := s.SetCredentialGoogleAds(ctx, &conn.SetCredentialGoogleAdsPayload{}); err == nil {
		t.Error("SetCredentialGoogleAds: expected not-implemented error, got nil")
	}

	// Spot-check the other six providers' create methods to confirm each is
	// wired to the stub. (Interface satisfaction for all 42 methods is enforced
	// at compile time by the `var _ conn.Service` assertion.)
	if _, err := s.CreateLinkedinAds(ctx, &conn.CreateLinkedinAdsPayload{}); err == nil {
		t.Error("CreateLinkedinAds: expected not-implemented error, got nil")
	}
	if _, err := s.CreateMetaAds(ctx, &conn.CreateMetaAdsPayload{}); err == nil {
		t.Error("CreateMetaAds: expected not-implemented error, got nil")
	}
	if _, err := s.CreateRedditAds(ctx, &conn.CreateRedditAdsPayload{}); err == nil {
		t.Error("CreateRedditAds: expected not-implemented error, got nil")
	}
	if _, err := s.CreateTwitterAds(ctx, &conn.CreateTwitterAdsPayload{}); err == nil {
		t.Error("CreateTwitterAds: expected not-implemented error, got nil")
	}
	if _, err := s.CreateMicrosoftAds(ctx, &conn.CreateMicrosoftAdsPayload{}); err == nil {
		t.Error("CreateMicrosoftAds: expected not-implemented error, got nil")
	}
	if _, err := s.CreateHubspot(ctx, &conn.CreateHubspotPayload{}); err == nil {
		t.Error("CreateHubspot: expected not-implemented error, got nil")
	}
}

func TestConnectionService_JWTAuth(t *testing.T) {
	s := NewConnectionService()
	ctx := context.Background()

	if _, err := s.JWTAuth(ctx, "", nil); err == nil {
		t.Error("JWTAuth: expected error for empty token, got nil")
	}
	if _, err := s.JWTAuth(ctx, "a-token", nil); err != nil {
		t.Errorf("JWTAuth: expected nil error for non-empty token, got %v", err)
	}
}
