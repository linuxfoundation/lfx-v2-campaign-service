// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestListGoogleAdsAccounts_Unavailable tests that account listing returns 503 when orchestrator is not wired.
func TestListGoogleAdsAccounts_Unavailable(t *testing.T) {
	svc := NewConnectionService(nil, nil)
	// Do not call SetOrchestrator - leave it nil to simulate startup mode

	payload := &conn.ListGoogleAdsAccountsPayload{ProjectID: "test-project"}
	result, err := svc.ListGoogleAdsAccounts(context.Background(), payload)

	if result != nil {
		t.Fatalf("expected nil result on unavailable, got %v", result)
	}
	if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
		t.Fatalf("expected ServiceUnavailable error, got %T: %v", err, err)
	}
}

// TestListGoogleAdsAccounts_Unsupported tests that account listing returns 400 when orchestrator has no AccountLister.
func TestListGoogleAdsAccounts_Unsupported(t *testing.T) {
	// Create a mock dispatcher without AccountLister capability
	mockRepo := &mockConnectionRepo{}
	mockEnc := &mockEncryptor{}
	svc := NewConnectionService(mockRepo, mockEnc)

	// Create a minimal orchestrator with a dispatcher that doesn't implement AccountLister
	mockDispatcher := &mockDispatcher{} // no AccountLister methods
	orch := &Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: mockDispatcher,
		},
	}
	svc.SetOrchestrator(orch)

	payload := &conn.ListGoogleAdsAccountsPayload{ProjectID: "test-project"}
	result, err := svc.ListGoogleAdsAccounts(context.Background(), payload)

	if result != nil {
		t.Fatalf("expected nil result on unsupported, got %v", result)
	}
	if badReq, ok := err.(*conn.BadRequestError); !ok || badReq.Code != "400" {
		t.Fatalf("expected 400 BadRequest error, got %T: %v", err, err)
	}
}

// Mock types for testing

type mockConnectionRepo struct{}

func (m *mockConnectionRepo) Create(ctx context.Context, c *model.Connection) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}

func (m *mockConnectionRepo) Get(ctx context.Context, projectID string, provider model.Provider) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}

func (m *mockConnectionRepo) Update(ctx context.Context, c *model.Connection, expectedVersion int64) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}

func (m *mockConnectionRepo) SetCredential(ctx context.Context, projectID string, provider model.Provider, ciphertext []byte, by *model.Actor) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}

func (m *mockConnectionRepo) Delete(ctx context.Context, projectID string, provider model.Provider, actor *model.Actor) error {
	return domain.ErrNotFound
}

type mockEncryptor struct{}

func (m *mockEncryptor) Encrypt(plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

func (m *mockEncryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

type mockDispatcher struct{}

func (m *mockDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}
