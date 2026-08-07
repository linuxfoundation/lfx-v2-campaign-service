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

// TestListGoogleAdsAccounts_LabelsAreDistinctPerAccount pins the pointer conversion in
// ListGoogleAdsAccounts. The loop takes the address of a per-iteration copy (`label :=
// acct.Label`), which Go's escape analysis moves to the heap — one allocation per
// iteration. Taking `&acct.Label` instead would be the classic loop-variable-aliasing bug:
// before Go 1.22 every result would share one pointer and report the LAST label. This test
// fails loudly in that case, so the conversion cannot silently regress.
func TestListGoogleAdsAccounts_LabelsAreDistinctPerAccount(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	orch := &Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{
				accounts: []model.AccessibleAccount{
					{ID: "customers/1111111111", Label: "Alpha"},
					{ID: "customers/2222222222", Label: "Beta"},
					{ID: "customers/3333333333", Label: ""},
				},
			},
		},
	}
	svc.SetOrchestrator(orch)

	result, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("ListGoogleAdsAccounts failed: %v", err)
	}
	if len(result.Accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(result.Accounts))
	}

	wantIDs := []string{"customers/1111111111", "customers/2222222222", "customers/3333333333"}
	wantLabels := []string{"Alpha", "Beta", ""}
	for i, got := range result.Accounts {
		if got.ID != wantIDs[i] {
			t.Errorf("account %d: expected ID %q, got %q", i, wantIDs[i], got.ID)
		}
		if got.Label == nil {
			t.Fatalf("account %d: Label pointer is nil; every account must carry a non-nil label pointer", i)
		}
		if *got.Label != wantLabels[i] {
			t.Errorf("account %d: expected label %q, got %q", i, wantLabels[i], *got.Label)
		}
	}

	// Each account must own its label storage. If the loop aliased a single variable,
	// these pointers would be equal and mutating one would corrupt the others.
	for i := range result.Accounts {
		for j := i + 1; j < len(result.Accounts); j++ {
			if result.Accounts[i].Label == result.Accounts[j].Label {
				t.Errorf("accounts %d and %d share the same Label pointer; labels must not alias", i, j)
			}
		}
	}
}

// Mock types for testing

// mockAccountListerDispatcher is a dispatcher that DOES implement AccountLister, so
// ReadAccounts reaches the conversion loop instead of short-circuiting on
// ErrAccountsUnsupported like mockDispatcher does.
type mockAccountListerDispatcher struct {
	accounts []model.AccessibleAccount
}

func (m *mockAccountListerDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockAccountListerDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	return m.accounts, nil
}

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
