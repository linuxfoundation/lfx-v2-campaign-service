// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
		// Verbatim from googleads.go: the value is NOT in the message. Copying the real text
		// is the point of these fixtures — the echo check below is only as strong as the
		// string it looks for, so a fixture that drifts from production silently tests
		// nothing. If this ever reverts to embedding the stored value, copy that shape back
		// here so the check has something real to catch.
		{"malformed manager id", "stored login_customer_id is invalid (must be digits only, no dashes or spaces)"},
		// This one comes from a different layer — credsSource.resolve, below the discovery
		// resolver — and reaches here unchanged. It is listed because the arm must key on
		// the sentinel alone, not on which function produced it.
		{"connection row with an empty credential blob", "google-ads connection for project p has no stored credentials"},
		// A blob that fails AUTHENTICATED decryption is deliberately absent: it is not a
		// connection problem at all. See TestListGoogleAdsAccounts_DecryptFailureIs500.
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
				t.Errorf("message echoes the wrapped cause %q; on this arm the cause is neither "+
					"returned nor logged — only a fixed reason token from unusableConnectionReason "+
					"is — because the decode case can quote decrypted credential bytes", tc.cause)
			}
		})
	}
}

// TestListGoogleAdsAccounts_DecryptFailureIs500 pins the arm that must NOT be a 400.
//
// A credential blob that fails authenticated decryption means a wrong or rotated
// application key, or tampering. That key is deployment-wide, so the same failure hits
// every project's connection in the same instant. 400 would tell each of their operators to
// go fix a row that is fine; 503 would promise that waiting helps. Both hide an outage
// behind a message about somebody's connection, which is why this arm sits ABOVE the
// ErrConnectionNotUsable arm and answers 500.
//
// The second case is the one that would regress silently: an error carrying BOTH sentinels
// must still take the 500 path. Reordering the switch is enough to break it, and no other
// test in this file would notice.
func TestListGoogleAdsAccounts_DecryptFailureIs500(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "authenticated decryption failed",
			err: fmt.Errorf("decrypt google-ads credentials: %w: cipher: message authentication failed",
				domain.ErrCredentialDecryptionFailed),
		},
		{
			name: "also tagged not-usable by a caller upstack",
			err: fmt.Errorf("%w: %w: cipher: message authentication failed",
				domain.ErrConnectionNotUsable, domain.ErrCredentialDecryptionFailed),
		},
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
				t.Fatalf("expected nil result, got %v", result)
			}
			internalErr, ok := err.(*conn.InternalServerError)
			if !ok {
				t.Fatalf("expected InternalServerError so the outage is visible as one, got %T: %v", err, err)
			}
			if internalErr.Code != "500" {
				t.Errorf("expected code 500, got %q", internalErr.Code)
			}
			if strings.Contains(internalErr.Message, "cipher") {
				t.Errorf("crypto detail leaked into the response message: %q", internalErr.Message)
			}
		})
	}
}

// TestListGoogleAdsAccounts_UnusableConnectionLogsAReasonNotTheCause pins the log line
// itself, which is the only place the finding lived: the response body was already
// sanitized while the same material still reached centralized logs.
//
// The cause here is the shape production actually produces — the dispatch layer wraps a
// reason sentinel alongside the status sentinel — and the assertions are on the emitted
// RECORD, not on the returned error. `reason` must be the classification token, and no part
// of the cause's text may appear anywhere in the line. The marker below stands in for what
// an encoding/json error quotes out of a decrypted blob.
func TestListGoogleAdsAccounts_UnusableConnectionLogsAReasonNotTheCause(t *testing.T) {
	const marker = "sUp3r-s3cr3t-value"

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cause := fmt.Errorf("%w: %w: decode google ads credentials: invalid character %q",
		domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, marker)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{err: cause},
		},
	})

	if _, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"}); err == nil {
		t.Fatal("expected an error for an unusable connection, got nil")
	}

	line := buf.String()
	if !strings.Contains(line, "reason=credentials_undecodable") {
		t.Errorf("log line does not carry the reason token, so the 400 is undiagnosable: %q", line)
	}
	if !strings.Contains(line, " project_id=p ") {
		t.Errorf("log line does not carry project metadata: %q", line)
	}
	if strings.Contains(line, marker) {
		t.Errorf("log line quotes credential-derived text: %q", line)
	}
	if strings.Contains(line, "decode google ads credentials") {
		t.Errorf("log line echoes the wrapped cause, which is the exposure this test exists to "+
			"prevent — the cause is a fixed message TODAY, but nothing stops a future wrap from "+
			"carrying plaintext into it: %q", line)
	}
}

// TestListGoogleAdsAccounts_AbsentCredentialsLogAReasonNotUnclassified pins the token for
// the one condition that is fully diagnosed before anything is attempted: the credential
// column is empty.
//
// It gets its own test because "unclassified" is not a neutral default in this vocabulary —
// it reads as "we do not know", and it was the token this state produced. An operator
// grepping a 400 for the most trivially fixable connection state found the answer that says
// nothing is known about it, which inverts the priority of the alert.
func TestListGoogleAdsAccounts_AbsentCredentialsLogAReasonNotUnclassified(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cause := fmt.Errorf("google-ads connection for project p has no stored credentials: %w: %w",
		domain.ErrConnectionNotUsable, domain.ErrCredentialsAbsent)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{err: cause},
		},
	})

	if _, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"}); err == nil {
		t.Fatal("expected an error for a connection with no credentials, got nil")
	}

	line := buf.String()
	if !strings.Contains(line, "reason=credentials_absent") {
		t.Errorf("log line does not carry reason=credentials_absent: %q — a known, fully "+
			"diagnosed state must not log as unclassified", line)
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
	// gotPlatform records the provider the handler passed down, so a test can distinguish
	// "the right dispatcher answered" from "some dispatcher answered".
	gotPlatform model.Provider
	// err, when set, is returned instead of accounts — used to exercise the handler's
	// error classification (missing connection vs. platform failure).
	err error
}

func (m *mockAccountListerDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockAccountListerDispatcher) ListAccounts(ctx context.Context, projectID string, platform model.Provider) ([]model.AccessibleAccount, error) {
	// Recorded so a caller can prove WHICH provider the handler asked for. Every status
	// assertion in this file passes with the wrong provider constant wired, because the
	// orchestrator would then reach a different dispatcher that answers just as well.
	m.gotPlatform = platform
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

func (m *mockConnectionRepo) UpdateWithCredential(ctx context.Context, c *model.Connection, ciphertext []byte, expectedVersion int64) (*model.Connection, error) {
	upd, err := m.Update(ctx, c, expectedVersion)
	if err != nil {
		return nil, err
	}
	upd.EncryptedCredentials = ciphertext
	return upd, nil
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

// TestListGoogleAdsAccounts_DecryptionFailureNamesTheRowThatFailed pins WHICH project id the
// operator log carries when the credentials came from the LF system fallback.
//
// The line asks whether one row or every connection is broken — that is the whole question
// separating a rotated application key from a single corrupted blob. Naming the caller's
// project answers it wrongly in both directions: whoever is paged inspects a row that project
// does not have, and N projects failing over one corrupt system row reads as N failing rows,
// which is exactly the deployment-wide conclusion the arm is written not to assert.
func TestListGoogleAdsAccounts_DecryptionFailureNamesTheRowThatFailed(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// The shape the fallback produces: origin marker, then the classification.
	cause := fmt.Errorf("%w: decrypt google_ads credentials: %w",
		domain.ErrSystemConnectionOrigin, domain.ErrCredentialDecryptionFailed)
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{err: cause},
		},
	})

	if _, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "cncf"}); err == nil {
		t.Fatal("expected an error for an undecryptable blob, got nil")
	}

	line := buf.String()
	// Leading space, as below: `project_id=` is a suffix of `requested_by_project_id=`, so
	// the bare substring cannot tell the two attributes apart.
	if !strings.Contains(line, " project_id="+model.SystemProjectID) {
		t.Errorf("log names the caller instead of the system row that failed: %q", line)
	}
	if !strings.Contains(line, "requested_by_project_id=cncf") {
		t.Errorf("log drops who was served, which is how the blast radius is counted: %q", line)
	}

	// A project's OWN row must still be named as itself, or every decryption failure points
	// at the system account and the single-row cause becomes uninvestigable.
	buf.Reset()
	svc = NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountListerDispatcher{
				err: fmt.Errorf("decrypt google_ads credentials: %w", domain.ErrCredentialDecryptionFailed),
			},
		},
	})
	if _, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "cncf"}); err == nil {
		t.Fatal("expected an error for an undecryptable blob, got nil")
	}
	// Matched with the leading space that slog puts before every attribute, because the
	// bare substring is also contained in `requested_by_project_id=cncf` — so without the
	// boundary this assertion would still pass if the primary project_id regressed to the
	// system scope, which is the exact regression it exists to catch.
	if line := buf.String(); !strings.Contains(line, " project_id=cncf") {
		t.Errorf("project's own row was not named as its own: %q", line)
	}
}

// ─── Meta Ads account discovery ───
//
// The Meta handler is three lines over the same listAccounts helper the Google Ads handler
// uses, so the status mapping is not re-tested here — it is the SAME code, and a second copy
// of those eleven tests would only assert that Go still calls the function it was given. What
// is genuinely per-provider is the two-field accountDiscovery descriptor, and that is exactly
// what the tests below pin, because nothing else in the suite can tell a mis-wired descriptor
// from a correct one: every status code is identical either way.

// TestListMetaAdsAccounts_QueriesTheMetaDispatcher pins the provider constant in
// metaAdsAccountDiscovery.
//
// The Google Ads dispatcher registered alongside is the whole point: it is an ordinary
// PlatformDispatcher with NO ListAccounts, so if the descriptor named ProviderGoogleAds the
// call would reach it and come back ErrAccountsUnsupported — a 400, which no status-code
// assertion in this file would flag as wrong for a Meta connection. Asserting the recorded
// platform closes that, and asserting the ids come back untouched closes the other half:
// act_-prefixed is the form the connection column stores, so a handler that stripped or
// re-added the prefix would hand the UI a value that fails on PUT.
func TestListMetaAdsAccounts_QueriesTheMetaDispatcher(t *testing.T) {
	lister := &mockAccountListerDispatcher{
		accounts: []model.AccessibleAccount{
			{ID: "act_123456789", Label: "LF Foundation Ads"},
			{ID: "act_987654321", Label: "CNCF Ads (disabled)"},
		},
	}
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderMetaAds:   lister,
			model.ProviderGoogleAds: &mockDispatcher{},
		},
	})

	result, err := svc.ListMetaAdsAccounts(context.Background(), &conn.ListMetaAdsAccountsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("ListMetaAdsAccounts failed: %v", err)
	}
	if lister.gotPlatform != model.ProviderMetaAds {
		t.Fatalf("dispatcher was asked for provider %q, want %q — the descriptor names the wrong provider",
			lister.gotPlatform, model.ProviderMetaAds)
	}
	wantIDs := []string{"act_123456789", "act_987654321"}
	if len(result.Accounts) != len(wantIDs) {
		t.Fatalf("expected %d accounts, got %d", len(wantIDs), len(result.Accounts))
	}
	for i, got := range result.Accounts {
		if got.ID != wantIDs[i] {
			t.Errorf("account %d: id = %q, want %q — the act_ prefix is the stored form and must survive verbatim",
				i, got.ID, wantIDs[i])
		}
	}
}

// TestListMetaAdsAccounts_MessagesNameMetaNotGoogleAds pins the caller-facing half of the
// descriptor.
//
// Both handlers reach the identical switch, so a Meta endpoint wired to
// googleAdsAccountDiscovery answers every request with the right STATUS and the wrong TEXT:
// a 404 saying no google ads connection exists on a project that has a Meta one, and a 400
// telling the operator to check `login_customer_id`, a field a Meta connection does not
// have. Neither is detectable from the status code, and the operator's next action is
// entirely determined by the text — so the text is the assertion.
func TestListMetaAdsAccounts_MessagesNameMetaNotGoogleAds(t *testing.T) {
	newSvc := func(dispatchErr error) *ConnectionService {
		svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
		svc.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{
				model.ProviderMetaAds: &mockAccountListerDispatcher{err: dispatchErr},
			},
		})
		return svc
	}

	t.Run("404 names meta", func(t *testing.T) {
		_, err := newSvc(domain.ErrNotFound).ListMetaAdsAccounts(context.Background(),
			&conn.ListMetaAdsAccountsPayload{ProjectID: "p"})
		notFound, ok := err.(*conn.NotFoundError)
		if !ok {
			t.Fatalf("expected NotFoundError, got %T: %v", err, err)
		}
		if !strings.Contains(notFound.Message, "meta ads") {
			t.Errorf("message = %q, want it to name meta ads", notFound.Message)
		}
		if strings.Contains(notFound.Message, "google") {
			t.Errorf("message = %q names google ads on the meta endpoint", notFound.Message)
		}
	})

	t.Run("400 names the fields a meta connection actually has", func(t *testing.T) {
		wrapped := fmt.Errorf("%w: %w: meta credentials need accessToken",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
		_, err := newSvc(wrapped).ListMetaAdsAccounts(context.Background(),
			&conn.ListMetaAdsAccountsPayload{ProjectID: "p"})
		badRequest, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Fatalf("expected BadRequestError, got %T: %v", err, err)
		}
		// access_token, the field name the caller sends to set-credential
		// (design/connection.go MetaAdsCredentials) — NOT the persisted blob's Go field
		// name AccessToken, which the operator has no way to address.
		if !strings.Contains(badRequest.Message, "access_token") {
			t.Errorf("remedy = %q, want it to name access_token — the API field a meta credential carries",
				badRequest.Message)
		}
		if strings.Contains(badRequest.Message, "login_customer_id") {
			t.Errorf("remedy = %q sends a meta operator looking for a google ads field", badRequest.Message)
		}
		// Same discipline as the Google arm: the cause is credential-derived and neither
		// returned nor logged.
		if strings.Contains(badRequest.Message, "accessToken\"") || strings.Contains(badRequest.Message, "need accessToken") {
			t.Errorf("message %q echoes the wrapped cause", badRequest.Message)
		}
	})
}

// TestListAccounts_RejectsTheReservedSystemScope covers every endpoint that reaches a platform
// through a dispatcher, in one table.
//
// The two discovery endpoints share `listAccounts`, so the guard there is written once. The
// HubSpot email search is NOT one of them — it does not enumerate accounts, so it calls
// `rejectSystemScope` itself — and a DUPLICATED security guard is exactly the kind that gets
// added without a test and later removed without one failing. It is in this table rather than
// beside the email tests so the next endpoint that copies the guard is added here too.
//
// The reserved scope is unaddressable by design: a GET on it would decrypt the LF system
// credential and enumerate the Linux Foundation's own ad accounts for whoever asked. The
// rejection has to happen before resolveBackendWithOrch, so a service with NO orchestrator
// wired still rejects rather than answering 503 — which is what the assertion below relies
// on to prove the guard runs first.
func TestListAccounts_RejectsTheReservedSystemScope(t *testing.T) {
	cases := []struct {
		name string
		call func(*ConnectionService) error
	}{
		{"google ads", func(s *ConnectionService) error {
			_, err := s.ListGoogleAdsAccounts(context.Background(),
				&conn.ListGoogleAdsAccountsPayload{ProjectID: model.SystemProjectID})
			return err
		}},
		{"meta ads", func(s *ConnectionService) error {
			_, err := s.ListMetaAdsAccounts(context.Background(),
				&conn.ListMetaAdsAccountsPayload{ProjectID: model.SystemProjectID})
			return err
		}},
		// Not account discovery, same reserved scope. A GET here would decrypt the LF system
		// credential and list the Linux Foundation's own marketing emails — subjects and all —
		// for whoever asked.
		{"hubspot emails", func(s *ConnectionService) error {
			_, err := s.ListHubspotEmails(context.Background(),
				&conn.ListHubspotEmailsPayload{ProjectID: model.SystemProjectID})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately no orchestrator: if the guard moved below resolveBackendWithOrch
			// this would be a 503 and the reserved scope would be reachable the moment the
			// service finished warming up.
			err := tc.call(NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{}))
			if _, ok := err.(*conn.ConnServiceUnavailableError); ok {
				t.Fatalf("got 503, want the system-scope rejection — the guard is running after "+
					"the backend check, so the reserved scope is reachable on a warm service: %v", err)
			}
			if err == nil {
				t.Fatal("the reserved system scope was accepted")
			}
		})
	}
}

// TestListLinkedinAndMicrosoftAccounts_MessagesNameTheirOwnProvider extends the descriptor
// assertion above to the two handlers LFXV2-3064 added.
//
// Same failure it guards against, and it is invisible to a status-code test: every handler
// reaches the identical switch, so one wired to another provider's `accountDiscovery` answers
// with the right STATUS and the wrong TEXT — a 404 naming google ads on a LinkedIn project, or a
// 400 telling a Microsoft operator to check `access_token`, which a Microsoft credential does not
// carry. The operator's next action is determined entirely by that text.
//
// The remedy strings are asserted on the FIELD NAMES the caller sends to set-credential, not the
// persisted blob's Go field names, which an operator has no way to address.
func TestListLinkedinAndMicrosoftAccounts_MessagesNameTheirOwnProvider(t *testing.T) {
	newSvc := func(p model.Provider, dispatchErr error) *ConnectionService {
		svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
		svc.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{
				p: &mockAccountListerDispatcher{err: dispatchErr},
			},
		})
		return svc
	}

	t.Run("linkedin 404 names linkedin, not google", func(t *testing.T) {
		_, err := newSvc(model.ProviderLinkedInAds, domain.ErrNotFound).ListLinkedinAdsAccounts(
			context.Background(), &conn.ListLinkedinAdsAccountsPayload{ProjectID: "p"})
		notFound, ok := err.(*conn.NotFoundError)
		if !ok {
			t.Fatalf("expected NotFoundError, got %T: %v", err, err)
		}
		if !strings.Contains(notFound.Message, "linkedin ads") {
			t.Errorf("message = %q, want it to name linkedin ads", notFound.Message)
		}
		if strings.Contains(notFound.Message, "google") || strings.Contains(notFound.Message, "meta") {
			t.Errorf("message = %q names another provider on the linkedin endpoint", notFound.Message)
		}
	})

	t.Run("linkedin 400 names the field a linkedin credential carries", func(t *testing.T) {
		wrapped := fmt.Errorf("%w: %w: linkedin credentials need accessToken",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
		_, err := newSvc(model.ProviderLinkedInAds, wrapped).ListLinkedinAdsAccounts(
			context.Background(), &conn.ListLinkedinAdsAccountsPayload{ProjectID: "p"})
		badRequest, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Fatalf("expected BadRequestError, got %T: %v", err, err)
		}
		if !strings.Contains(badRequest.Message, "access_token") {
			t.Errorf("remedy = %q, want it to name access_token", badRequest.Message)
		}
		if strings.Contains(badRequest.Message, "login_customer_id") {
			t.Errorf("remedy = %q names a google ads field on the linkedin endpoint", badRequest.Message)
		}
	})

	t.Run("microsoft 404 names microsoft, not google", func(t *testing.T) {
		_, err := newSvc(model.ProviderMicrosoftAds, domain.ErrNotFound).ListMicrosoftAdsAccounts(
			context.Background(), &conn.ListMicrosoftAdsAccountsPayload{ProjectID: "p"})
		notFound, ok := err.(*conn.NotFoundError)
		if !ok {
			t.Fatalf("expected NotFoundError, got %T: %v", err, err)
		}
		if !strings.Contains(notFound.Message, "microsoft ads") {
			t.Errorf("message = %q, want it to name microsoft ads", notFound.Message)
		}
		if strings.Contains(notFound.Message, "google") || strings.Contains(notFound.Message, "meta") {
			t.Errorf("message = %q names another provider on the microsoft endpoint", notFound.Message)
		}
	})

	// Microsoft's remedy is the one that differs most: four fields, none of which any other
	// provider's remedy names. Wired to Meta's descriptor it would tell the operator to check
	// access_token — a field a Microsoft credential does not have.
	t.Run("microsoft 400 names all four fields a microsoft credential carries", func(t *testing.T) {
		wrapped := fmt.Errorf("%w: %w: microsoft credentials incomplete",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
		_, err := newSvc(model.ProviderMicrosoftAds, wrapped).ListMicrosoftAdsAccounts(
			context.Background(), &conn.ListMicrosoftAdsAccountsPayload{ProjectID: "p"})
		badRequest, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Fatalf("expected BadRequestError, got %T: %v", err, err)
		}
		for _, field := range []string{"client_id", "client_secret", "developer_token", "refresh_token"} {
			if !strings.Contains(badRequest.Message, field) {
				t.Errorf("remedy = %q, want it to name %s", badRequest.Message, field)
			}
		}
	})
}
