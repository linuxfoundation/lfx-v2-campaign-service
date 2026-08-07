// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestListGoogleAdsAccounts_Unavailable tests that account listing returns 503 during
// cold start, covering BOTH unwired dependencies separately.
//
// The two matter independently and the distinction is easy to lose: resolveBackendWithOrch
// checks repo first and orchestrator second, and both return 503, so a service built with
// NewConnectionService(nil, nil) returns at the repo check and the orchestrator branch is
// never executed at all. That is why each case wires everything except the one dependency
// under test, and why the messages are asserted — the status code alone cannot tell the two
// branches apart.
func TestListGoogleAdsAccounts_Unavailable(t *testing.T) {
	tests := []struct {
		name    string
		svc     func() *ConnectionService
		wantMsg string
	}{
		{
			name:    "storage not wired",
			svc:     func() *ConnectionService { return NewConnectionService(nil, &mockEncryptor{}) },
			wantMsg: "connection storage is unavailable",
		},
		{
			name: "orchestrator not wired",
			svc: func() *ConnectionService {
				// Live repo, SetOrchestrator deliberately not called: this is the
				// cold-start window where storage is up but dispatchers are not.
				return NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			},
			wantMsg: "account discovery service is unavailable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := &conn.ListGoogleAdsAccountsPayload{ProjectID: "test-project"}
			result, err := tc.svc().ListGoogleAdsAccounts(context.Background(), payload)

			if result != nil {
				t.Fatalf("expected nil result on unavailable, got %v", result)
			}
			unavailable, ok := err.(*conn.ConnServiceUnavailableError)
			if !ok {
				t.Fatalf("expected ServiceUnavailable error, got %T: %v", err, err)
			}
			if unavailable.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q — the wrong guard fired, so this case is not covering what it names", unavailable.Message, tc.wantMsg)
			}
		})
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

// TestListGoogleAdsAccounts_ZeroAccountsIsOKNotUnavailable pins the empty case. A
// credential that legitimately reaches no ad accounts is a valid 200 with an empty list —
// NOT a platform failure. Orchestrator.ReadAccounts converts a nil result into an error
// (it cannot tell "no accounts" from "the lister forgot to return anything"), so a
// dispatcher that builds its slice with `var accounts []T` reports 503 for a correct
// answer. This test fails the moment the conversion loop stops pre-allocating.
func TestListGoogleAdsAccounts_ZeroAccountsIsOKNotUnavailable(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	orch := &Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{
				accounts: []model.AccessibleAccount{},
			},
		},
	}
	svc.SetOrchestrator(orch)

	result, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("zero accessible accounts must succeed, got error: %v", err)
	}
	if len(result.Accounts) != 0 {
		t.Fatalf("expected 0 accounts, got %d", len(result.Accounts))
	}
	// len() alone is satisfied by nil, which is exactly the regression worth catching:
	// the dispatcher deliberately builds its slice with make(..., 0, n) so an empty
	// result is `[]`, and a `var connAccounts []*conn.AccessibleAccount` in the
	// conversion loop undoes that one layer up — every client then has to special-case
	// a null it was promised it would never see.
	if result.Accounts == nil {
		t.Fatal("Accounts is nil; an empty result must serialize as [], not null")
	}
	encoded, err := json.Marshal(result.Accounts)
	if err != nil {
		t.Fatalf("marshal accounts: %v", err)
	}
	if string(encoded) != "[]" {
		t.Errorf("empty accounts serialized as %s, want []", encoded)
	}
}

// TestListGoogleAdsAccounts_NoConnectionIs404 pins the missing-connection mapping. The
// project simply has no stored Google Ads connection — a client-side state error the
// caller fixes by creating one, not a platform outage. Reporting 503 would tell the UI to
// retry something that can never succeed. This works only because credsSource.resolve
// WRAPS domain.ErrNotFound instead of flattening it into an opaque message.
func TestListGoogleAdsAccounts_NoConnectionIs404(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	orch := &Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{
				err: domain.ErrNotFound,
			},
		},
	}
	svc.SetOrchestrator(orch)

	result, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
	if result != nil {
		t.Fatalf("expected nil result when no connection exists, got %v", result)
	}
	notFound, ok := err.(*conn.NotFoundError)
	if !ok {
		t.Fatalf("expected NotFound error, got %T: %v", err, err)
	}
	if notFound.Code != "404" {
		t.Errorf("expected code 404, got %q", notFound.Code)
	}
}

// TestListGoogleAdsAccounts_UnusableConnectionIs400 pins the arm that keeps the 503 below
// honest. The connection EXISTS — so it is not the 404 case — but it cannot be used as it
// stands, and no amount of retrying changes that until a human edits it. Without this arm
// every such failure lands in `default` and the caller is told to retry forever.
//
// The three sub-cases are the three shapes the dispatcher wraps: an inactive connection, an
// incomplete credential blob, and a malformed stored config value. They are separate here
// because they arrive from separate call sites in resolveGoogleAdsDiscoveryClient, and a
// refactor that drops the wrap from any ONE of them would still leave the other two green.
//
// The response must NOT echo the cause: one of these wraps a json.Unmarshal failure over the
// DECRYPTED credential blob, and an unmarshal error can quote its input, which would put
// credential bytes in an HTTP response body.
func TestListGoogleAdsAccounts_UnusableConnectionIs400(t *testing.T) {
	cases := []struct {
		name  string
		cause string
	}{
		{"inactive connection", "google ads connection for project p is inactive, not active"},
		{"incomplete credentials", "google ads credentials are incomplete (need clientId, clientSecret, developerToken, refreshToken)"},
		{"malformed manager id", `stored login_customer_id "999-999-9999" must be digits only (no dashes or spaces)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("%w: %s", domain.ErrConnectionNotUsable, tc.cause)
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{
					model.ProviderGoogleAds: &mockAccountListerDispatcher{err: wrapped},
				},
			})

			result, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
			if result != nil {
				t.Fatalf("expected nil result for an unusable connection, got %v", result)
			}
			badRequest, ok := err.(*conn.BadRequestError)
			if !ok {
				t.Fatalf("expected BadRequestError so the caller stops retrying, got %T: %v", err, err)
			}
			if badRequest.Code != "400" {
				t.Errorf("expected code 400, got %q", badRequest.Code)
			}
			if strings.Contains(badRequest.Message, tc.cause) {
				t.Errorf("message echoes the wrapped cause %q; the cause is logged, not returned, "+
					"because the decode case can quote decrypted credential bytes", tc.cause)
			}
		})
	}
}

// TestListGoogleAdsAccounts_ProviderFailureIs503 pins the `default` branch — the only
// one of the three that tells the caller to RETRY. The existing 503 test covers the
// dependency-unavailable case (no orchestrator wired at all), which never reaches this
// switch; this one covers a wired dispatcher whose upstream call failed, which is the
// case that actually happens in production.
//
// The two subtests exist to pin the boundary rather than the happy path. An error that
// merely READS like a missing connection must NOT be mapped to 404: the 404 branch is
// gated on `errors.Is(aerr, domain.ErrNotFound)`, and if it ever loosened into string
// matching, a transient Google Ads failure whose message happened to say "not found"
// would be reported as permanent client-side state and the UI would stop retrying a
// call that would have succeeded.
func TestListGoogleAdsAccounts_ProviderFailureIs503(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"plain upstream failure", errors.New("google ads: 500 internal error")},
		// Reads like the 404 case, is not the 404 case: the sentinel is absent.
		{"message mentions not found but wraps no sentinel", errors.New("customer 123 not found upstream")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{
					model.ProviderGoogleAds: &mockAccountListerDispatcher{err: tc.err},
				},
			})

			result, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
			if result != nil {
				t.Fatalf("expected nil result on provider failure, got %v", result)
			}
			unavailable, ok := err.(*conn.ConnServiceUnavailableError)
			if !ok {
				t.Fatalf("expected ConnServiceUnavailableError, got %T: %v", err, err)
			}
			if unavailable.Code != "503" {
				t.Errorf("expected code 503, got %q", unavailable.Code)
			}
			// The upstream text is logged, not returned. A provider message can carry
			// customer ids and account state the caller has no relation to.
			if strings.Contains(unavailable.Message, tc.err.Error()) {
				t.Errorf("upstream error text leaked into the response message: %q", unavailable.Message)
			}
		})
	}
}

// Mock types for testing

// mockAccountListerDispatcher is a dispatcher that DOES implement AccountLister, so
// ReadAccounts reaches the conversion loop instead of short-circuiting on
// ErrAccountsUnsupported like mockDispatcher does.
type mockAccountListerDispatcher struct {
	accounts []model.AccessibleAccount
	// err, when set, is returned instead of accounts — used to exercise the handler's
	// error classification (missing connection vs. platform failure).
	err error
}

func (m *mockAccountListerDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockAccountListerDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	if m.err != nil {
		return nil, m.err
	}
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
