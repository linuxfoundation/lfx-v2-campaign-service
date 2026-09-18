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

// TestValidateMonitorDays pins the 7..90 inclusive range validateMonitorDays enforces
// independently of the design layer's own Minimum/Maximum (see its doc comment).
func TestValidateMonitorDays(t *testing.T) {
	tests := []struct {
		days    int
		wantErr bool
	}{
		{6, true},
		{7, false},
		{90, false},
		{91, true},
	}
	for _, tc := range tests {
		err := validateMonitorDays(tc.days)
		if tc.wantErr && err == nil {
			t.Errorf("days=%d: expected an error, got nil", tc.days)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("days=%d: expected no error, got %v", tc.days, err)
		}
		if tc.wantErr {
			badReq, ok := err.(*conn.BadRequestError)
			if !ok || badReq.Code != "400" {
				t.Errorf("days=%d: expected 400 BadRequest, got %T: %v", tc.days, err, err)
			}
		}
	}
}

// TestMonitorAccount_RejectsTheReservedSystemScope mirrors
// TestListAccounts_RejectsTheReservedSystemScope's exact shape: each of the four
// Monitor*AdsAccount handlers is called with the reserved system-project scope and NO
// orchestrator wired at all, so a *conn.ConnServiceUnavailableError (the 503
// resolveBackendWithOrch would return) proves rejectSystemScope did NOT run first, while any
// other error type proves it did.
func TestMonitorAccount_RejectsTheReservedSystemScope(t *testing.T) {
	cases := []struct {
		name string
		call func(*ConnectionService) error
	}{
		{"google ads", func(s *ConnectionService) error {
			_, err := s.MonitorGoogleAdsAccount(context.Background(),
				&conn.MonitorGoogleAdsAccountPayload{ProjectID: model.SystemProjectID, AccountID: "a", Days: 30})
			return err
		}},
		{"linkedin ads", func(s *ConnectionService) error {
			_, err := s.MonitorLinkedinAdsAccount(context.Background(),
				&conn.MonitorLinkedinAdsAccountPayload{ProjectID: model.SystemProjectID, AccountID: "a", Days: 30})
			return err
		}},
		{"meta ads", func(s *ConnectionService) error {
			_, err := s.MonitorMetaAdsAccount(context.Background(),
				&conn.MonitorMetaAdsAccountPayload{ProjectID: model.SystemProjectID, AccountID: "a", Days: 30})
			return err
		}},
		{"reddit ads", func(s *ConnectionService) error {
			_, err := s.MonitorRedditAdsAccount(context.Background(),
				&conn.MonitorRedditAdsAccountPayload{ProjectID: model.SystemProjectID, AccountID: "a", Days: 30})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{}) // no SetOrchestrator call
			err := tc.call(svc)
			if err == nil {
				t.Fatalf("expected an error for the reserved system scope, got nil")
			}
			if _, ok := err.(*conn.ConnServiceUnavailableError); ok {
				t.Fatalf("got a 503 ConnServiceUnavailableError, meaning resolveBackendWithOrch ran "+
					"before rejectSystemScope: %v", err)
			}
			notFound, ok := err.(*conn.NotFoundError)
			if !ok || notFound.Code != "404" {
				t.Fatalf("expected a 404 NotFoundError from rejectSystemScope, got %T: %v", err, err)
			}
		})
	}
}

// mockAccountMetricsReaderDispatcher implements AccountMetricsReader (and Dispatch, to satisfy
// PlatformDispatcher) so a test can drive monitorAccount's ReadAccountCampaignMetrics call
// down a chosen path without wiring a real ad-platform client.
type mockAccountMetricsReaderDispatcher struct {
	rows []model.AccountCampaignMetrics
	err  error
}

func (m *mockAccountMetricsReaderDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockAccountMetricsReaderDispatcher) ListAccountCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) ([]model.AccountCampaignMetrics, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.rows, nil
}

// TestMonitorAccount_ClassifiesDiscoveryError pins classifyDiscoveryError's mapping as
// exercised through monitorAccount's ReadAccountCampaignMetrics call: an
// AccountMetricsReader that returns domain.ErrNotFound must surface as 404, and one wrapping
// domain.ErrAccountsUnsupported must surface as 400 — the same two arms
// TestListGoogleAdsAccounts_Unsupported and this repo's other discovery-error tests already
// pin for the sibling list endpoints.
func TestMonitorAccount_ClassifiesDiscoveryError(t *testing.T) {
	tests := []struct {
		name       string
		readerErr  error
		wantStatus string
	}{
		{"no connection configured maps to 404", domain.ErrNotFound, "404"},
		{"accounts unsupported maps to 400", domain.ErrAccountsUnsupported, "400"},
		// A distinct sentinel from ErrAccountsUnsupported (list-accounts' "not supported"),
		// deliberately not folded into it — classifyDiscoveryError must recognize both, or
		// this one falls through to the default 503 arm, which promises a retry that can
		// never succeed for a platform with no monitor dispatcher wired.
		{"account metrics unsupported maps to 400", domain.ErrAccountMetricsUnsupported, "400"},
		// Every dispatcher also validates account_id shape itself, as defense-in-depth for a
		// non-HTTP caller that bypasses the design attribute's Goa Pattern (see
		// docs/knowledge/architecture/account-monitor-endpoints.md); this classifier is what
		// gives that dispatcher-level rejection a clean 400 instead of falling through to the
		// default 503 arm a plain unsentineled dispatcher error would land on.
		{"malformed account id maps to 400", domain.ErrAccountIDMalformed, "400"},
		// Same rationale as the account_id row above, for days: every dispatcher re-checks the
		// design attribute's 7..90 Minimum/Maximum itself, as defense-in-depth for a non-HTTP
		// caller that bypasses Goa; this classifier is what gives that rejection a clean 400
		// instead of the default 503 arm.
		{"invalid days maps to 400", domain.ErrMonitorDaysInvalid, "400"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{
					model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{err: tc.readerErr},
				},
			})

			result, err := svc.MonitorGoogleAdsAccount(context.Background(),
				&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
			if result != nil {
				t.Fatalf("expected nil result on error, got %v", result)
			}
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			switch tc.wantStatus {
			case "404":
				if nf, ok := err.(*conn.NotFoundError); !ok || nf.Code != "404" {
					t.Fatalf("expected 404 NotFoundError, got %T: %v", err, err)
				}
			case "400":
				if br, ok := err.(*conn.BadRequestError); !ok || br.Code != "400" {
					t.Fatalf("expected 400 BadRequest, got %T: %v", err, err)
				}
			}
		})
	}
}

// TestMonitorAccount_FallsBackToRowSumTotals pins the AccountTotalsReader-absent path: a
// dispatcher implementing ONLY AccountMetricsReader (not AccountTotalsReader, which
// Orchestrator.ReadAccountTotals reports via its `ok=false` return, not an error) must reach
// monitorTotalsFallback rather than fail, matching every platform but Reddit.
func TestMonitorAccount_FallsBackToRowSumTotals(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{
				rows: []model.AccountCampaignMetrics{
					{PlatformCampaignID: "1", Name: "c", Status: "enabled", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5},
				},
			},
		},
	})

	result, err := svc.MonitorGoogleAdsAccount(context.Background(),
		&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount failed: %T: %v", err, err)
	}
	if result.Totals == nil {
		t.Fatalf("expected non-nil totals from the row-sum fallback")
	}
	if result.Totals.Spend != 50 || result.Totals.Impressions != 100 || result.Totals.Clicks != 5 || result.Totals.CampaignCount != 1 {
		t.Errorf("totals = %+v, want the row summed verbatim (spend=50, impressions=100, clicks=5, campaignCount=1)", result.Totals)
	}
	if result.Totals.DerivedFromRows {
		t.Errorf("DerivedFromRows = true, want false: for every platform but Reddit this row sum " +
			"IS the contractual totals figure, not a stand-in for one that failed")
	}
}

// mockAccountTotalsReaderDispatcher additionally implements AccountTotalsReader, so a test can
// drive monitorAccount's ReadAccountTotals call — the Reddit-shaped path — independently of the
// metrics-read call above.
type mockAccountTotalsReaderDispatcher struct {
	mockAccountMetricsReaderDispatcher
	totalsErr error
	// totalsNilResult drives the Orchestrator.ReadAccountTotals contract-violation path: a
	// dispatcher returning (nil, nil), the same broken-adapter shape ReadAccountCampaignMetrics
	// and every sibling reader guard against.
	totalsNilResult bool
}

func (m *mockAccountTotalsReaderDispatcher) ReadAccountTotals(ctx context.Context, projectID string, platform model.Provider, accountID string, days, campaignCount int) (*model.AccountMonitorTotals, error) {
	if m.totalsErr != nil {
		return nil, m.totalsErr
	}
	if m.totalsNilResult {
		return nil, nil
	}
	return &model.AccountMonitorTotals{Spend: 999, Impressions: 999, Clicks: 999, CampaignCount: 999}, nil
}

// TestMonitorAccount_TotalsReaderErrorFallsBackToRowSum pins the fix for a real defect: a
// dispatcher implementing AccountTotalsReader (Reddit's shape) whose separate account-wide
// totals call FAILS must still return the per-campaign rows and action items already fetched
// successfully, falling back to summing them — the same fallback the !ok (unsupported) arm
// uses — rather than aborting the whole endpoint with an error. The per-campaign data is the
// response's primary content; the account-wide totals are a secondary, derivable figure.
func TestMonitorAccount_TotalsReaderErrorFallsBackToRowSum(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderRedditAds: &mockAccountTotalsReaderDispatcher{
				mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{
					rows: []model.AccountCampaignMetrics{
						{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE", Spend: 50, Impressions: 100, Clicks: 5},
					},
				},
				totalsErr: domain.ErrConnectionNotUsable,
			},
		},
	})

	result, err := svc.MonitorRedditAdsAccount(context.Background(),
		&conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorRedditAdsAccount failed: %T: %v — a failed account-totals call must fall "+
			"back to the row sum, not abort the endpoint", err, err)
	}
	if result.Totals == nil {
		t.Fatalf("expected non-nil totals from the row-sum fallback")
	}
	if result.Totals.Spend != 50 || result.Totals.Impressions != 100 || result.Totals.Clicks != 5 || result.Totals.CampaignCount != 1 {
		t.Errorf("totals = %+v, want the row summed verbatim (spend=50, impressions=100, clicks=5, "+
			"campaignCount=1), not the platform's own (999) totals since that call failed", result.Totals)
	}
	if !result.Totals.DerivedFromRows {
		t.Errorf("DerivedFromRows = false, want true: Reddit's own account-wide totals call failed, " +
			"so this row sum stands in for a platform-native figure that was expected but unavailable")
	}
}

// TestMonitorAccount_ContractViolationFallsBackToRowSum pins the fix for general reviewer's
// finding #3: when ReadAccountTotals' own dispatcher adapter is broken — it returns (nil, nil)
// instead of a real result or error — that must not be logged the same way as an ordinary
// upstream failure (ErrConnectionNotUsable, a timeout, a 500). It is still non-fatal to the
// endpoint (the campaign rows and action items already succeeded), so it still falls back to
// the row sum, but errAccountTotalsContractViolation must be identifiable via errors.Is so the
// caller can log it at ERROR rather than folding it into the routine WARN-level path.
func TestMonitorAccount_ContractViolationFallsBackToRowSum(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderRedditAds: &mockAccountTotalsReaderDispatcher{
				mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{
					rows: []model.AccountCampaignMetrics{
						{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE", Spend: 50, Impressions: 100, Clicks: 5},
					},
				},
				totalsNilResult: true,
			},
		},
	})

	result, err := svc.MonitorRedditAdsAccount(context.Background(),
		&conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorRedditAdsAccount failed: %T: %v — a broken AccountTotalsReader adapter must "+
			"still fall back to the row sum, not abort the endpoint", err, err)
	}
	if result.Totals == nil {
		t.Fatalf("expected non-nil totals from the row-sum fallback")
	}
	if result.Totals.Spend != 50 || result.Totals.Impressions != 100 || result.Totals.Clicks != 5 || result.Totals.CampaignCount != 1 {
		t.Errorf("totals = %+v, want the row summed verbatim (spend=50, impressions=100, clicks=5, campaignCount=1)", result.Totals)
	}
	if !result.Totals.DerivedFromRows {
		t.Errorf("DerivedFromRows = false, want true: Reddit's own totals reader is what returned the " +
			"contract violation, so this row sum stands in for a platform-native figure that was expected")
	}
}

// TestToConnAccountMonitorCampaign_CampaignURL pins the round-21-review fix (PR #215 comment
// #4): a Google row's CampaignURL must come through as a non-nil pointer on the response, and
// every other platform's empty CampaignURL must stay nil rather than becoming an empty-string
// pointer — the same optional-field convention CampaignID/CampaignName already use.
func TestToConnAccountMonitorCampaign_CampaignURL(t *testing.T) {
	withURL := toConnAccountMonitorCampaign(model.AccountMonitorRow{
		Metrics: model.AccountCampaignMetrics{PlatformCampaignID: "24183781329",
			CampaignURL: "https://ads.google.com/aw/campaigns?campaignId=24183781329"},
	})
	if withURL.CampaignURL == nil || *withURL.CampaignURL != "https://ads.google.com/aw/campaigns?campaignId=24183781329" {
		t.Errorf("CampaignURL = %v, want a non-nil pointer to the Google Ads URL", withURL.CampaignURL)
	}

	withoutURL := toConnAccountMonitorCampaign(model.AccountMonitorRow{
		Metrics: model.AccountCampaignMetrics{PlatformCampaignID: "reddit-1"},
	})
	if withoutURL.CampaignURL != nil {
		t.Errorf("CampaignURL = %v, want nil for a platform row with no campaign_url", *withoutURL.CampaignURL)
	}
}
