// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
		{"microsoft ads", func(s *ConnectionService) error {
			_, err := s.MonitorMicrosoftAdsAccount(context.Background(),
				&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: model.SystemProjectID, AccountID: "1", Days: 30})
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

// TestMonitorAccount_UpstreamFailureNamesAccountMonitor pins a round-22 review fix: Google,
// LinkedIn and Meta's monitor handlers used to pass connection.go's plain
// googleAdsAccountDiscovery/linkedInAdsAccountDiscovery/metaAdsAccountDiscovery — the same
// descriptors the `/…/accounts` picker uses — into monitorAccount, leaving `operation` empty so
// classifyDiscoveryError's default arm reported an upstream monitor failure as "account
// discovery could not be completed", an operation this endpoint never performs. Reddit's own
// redditAdsAccountDiscovery already set operation: "account monitor"; this pins the other three
// dispatchers now doing the same via their own googleAdsMonitorDiscovery/
// linkedInAdsMonitorDiscovery/metaAdsMonitorDiscovery descriptors.
func TestMonitorAccount_UpstreamFailureNamesAccountMonitor(t *testing.T) {
	unclassified := errors.New("boom")
	tests := []struct {
		name     string
		provider model.Provider
		call     func(svc *ConnectionService) error
	}{
		{"google ads", model.ProviderGoogleAds, func(svc *ConnectionService) error {
			_, err := svc.MonitorGoogleAdsAccount(context.Background(),
				&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
			return err
		}},
		{"linkedin ads", model.ProviderLinkedInAds, func(svc *ConnectionService) error {
			_, err := svc.MonitorLinkedinAdsAccount(context.Background(),
				&conn.MonitorLinkedinAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
			return err
		}},
		{"meta ads", model.ProviderMetaAds, func(svc *ConnectionService) error {
			_, err := svc.MonitorMetaAdsAccount(context.Background(),
				&conn.MonitorMetaAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{
					tc.provider: &mockAccountMetricsReaderDispatcher{err: unclassified},
				},
			})

			err := tc.call(svc)
			svcErr, ok := err.(*conn.ConnServiceUnavailableError)
			if !ok {
				t.Fatalf("expected a 503 ConnServiceUnavailableError, got %T: %v", err, err)
			}
			if !strings.Contains(svcErr.Message, "account monitor") {
				t.Errorf("message = %q, want it to name \"account monitor\", not the default \"account discovery\"", svcErr.Message)
			}
		})
	}
}

// TestMonitorAccount_TotalsSumTheReturnedRows pins the contract every platform now shares: the
// totals are the sum of the campaigns array returned alongside them, so the aggregate and the
// list can never describe different populations.
func TestMonitorAccount_TotalsSumTheReturnedRows(t *testing.T) {
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
		t.Fatalf("expected non-nil totals")
	}
	if result.Totals.Spend != 50 || result.Totals.Impressions != 100 || result.Totals.Clicks != 5 || result.Totals.CampaignCount != 1 {
		t.Errorf("totals = %+v, want the row summed verbatim (spend=50, impressions=100, clicks=5, campaignCount=1)", result.Totals)
	}
}

// TestMonitorAccount_TotalsExcludeRowsTheRuleEngineDropped pins which rows the sum is over: the
// post-rule-engine ones, not the raw dispatcher read. EvaluateGoogleMonitor drops the operator's
// scratch campaigns, and a total that counted their spend would describe a population the caller
// never sees in the same response.
func TestMonitorAccount_TotalsExcludeRowsTheRuleEngineDropped(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{
				rows: []model.AccountCampaignMetrics{
					{PlatformCampaignID: "1", Name: "Live", Status: "enabled", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5},
					{PlatformCampaignID: "2", Name: "zz_old_draft", Status: "enabled", BudgetDay: 50, Spend: 900, Impressions: 900, Clicks: 90},
				},
			},
		},
	})

	result, err := svc.MonitorGoogleAdsAccount(context.Background(),
		&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount failed: %T: %v", err, err)
	}
	if len(result.Campaigns) != 1 {
		t.Fatalf("got %d campaigns, want 1 — the scratch campaign should be filtered", len(result.Campaigns))
	}
	if result.Totals.Spend != 50 || result.Totals.CampaignCount != 1 {
		t.Errorf("totals = %+v, want only the returned row (spend=50, campaignCount=1); the dropped "+
			"scratch campaign's spend must not appear in a total the caller cannot reconcile", result.Totals)
	}
}

// TestMonitorRedditAccount_TotalsSumTheReturnedRows is the regression test for
// linuxfoundation/lfx-self-serve#3022. Reddit's totals used to come from a SEPARATE account-wide
// report call, ported from reddit-ads.service.ts's fetchAccountMetrics. That call is unfiltered —
// it covers every campaign on the account, archived ones included — while the rows beside it are
// filtered to the statuses the monitor displays, so the two described different populations with
// nothing in the response saying so. Reddit now sums its rows like every other platform.
func TestMonitorRedditAccount_TotalsSumTheReturnedRows(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{
			model.ProviderRedditAds: &mockAccountMetricsReaderDispatcher{
				rows: []model.AccountCampaignMetrics{
					{PlatformCampaignID: "1", Name: "c", Status: "ACTIVE", Spend: 50, Impressions: 100, Clicks: 5},
					{PlatformCampaignID: "2", Name: "d", Status: "PAUSED", Spend: 25, Impressions: 40, Clicks: 2},
				},
			},
		},
	})

	result, err := svc.MonitorRedditAdsAccount(context.Background(),
		&conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: "a", Days: 30})
	if err != nil {
		t.Fatalf("MonitorRedditAdsAccount failed: %T: %v", err, err)
	}
	if result.Totals == nil {
		t.Fatalf("expected non-nil totals")
	}
	if result.Totals.Spend != 75 || result.Totals.Impressions != 140 || result.Totals.Clicks != 7 ||
		result.Totals.CampaignCount != len(result.Campaigns) {
		t.Errorf("totals = %+v, want the two returned rows summed (spend=75, impressions=140, clicks=7) "+
			"with campaignCount matching the %d campaigns returned", result.Totals, len(result.Campaigns))
	}
}

// TestMonitorAccount_TotalsConversionsAbsentWhenNoRowMeasuredThem pins the aggregate half of
// linuxfoundation/lfx-self-serve#3020. That fix stopped Reddit's rows from claiming a measured
// zero, so every Reddit row now carries a nil Conversions; if the totals summed those into a
// float64 the response would go on reporting "conversions": 0 for the account while reporting
// conversions as unmeasured on every campaign underneath it — the same false claim, one level
// up. Absent when nothing measured, a real sum when anything did.
func TestMonitorAccount_TotalsConversionsAbsentWhenNoRowMeasuredThem(t *testing.T) {
	none := monitorTotals([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Spend: 50, Impressions: 100, Clicks: 5},
		{PlatformCampaignID: "2", Spend: 25, Impressions: 40, Clicks: 2},
	})
	if none.Conversions != nil {
		t.Errorf("Conversions = %v, want nil when no row reported a conversion measurement", *none.Conversions)
	}

	zero := 0.0
	four := 4.0
	some := monitorTotals([]model.AccountCampaignMetrics{
		{PlatformCampaignID: "1", Conversions: &four},
		{PlatformCampaignID: "2"},
		{PlatformCampaignID: "3", Conversions: &zero},
	})
	if some.Conversions == nil {
		t.Fatalf("Conversions = nil, want a sum once any row reported a measurement")
	}
	if *some.Conversions != 4 {
		t.Errorf("Conversions = %v, want 4 — the measured rows summed, the unmeasured one skipped", *some.Conversions)
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

// microsoftMonitorService wires a ConnectionService whose orchestrator answers Microsoft's
// report-backed monitor from the given reader and store.
func microsoftMonitorService(reader *fakeReportReader, store domain.AccountReportRepository) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(reportOrch(reader, store))
	return svc
}

// The report-backed response says how old its metrics are and whether newer ones are building,
// and still evaluates and totals exactly the rows it returns.
func TestMonitorMicrosoftAdsAccount_ReportsMetricsFreshness(t *testing.T) {
	completed := time.Date(2026, 10, 5, 14, 30, 0, 0, time.UTC)
	reader := &fakeReportReader{campaigns: twoCampaigns()}
	store := &fakeReportStore{snap: &model.AccountReportSnapshot{Ready: &model.ReadyAccountReport{
		AsOf: completed,
		Rows: []model.AccountReportRow{
			{PlatformCampaignID: "1", Spend: 70, Impressions: 1000, Clicks: 30},
			{PlatformCampaignID: "2", Spend: 10, Impressions: 500, Clicks: 5},
		},
	}}}
	// The saved report is older than accountReportFreshFor relative to the wall clock, so the
	// read also submits; the fake accepts it and the response reports it as building.
	reader.submitID = "r-next"

	got, err := microsoftMonitorService(reader, store).MonitorMicrosoftAdsAccount(context.Background(),
		&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
	if err != nil {
		t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
	}
	if got.MetricsAsOf == nil || *got.MetricsAsOf != "2026-10-05T14:30:00Z" {
		t.Errorf("metrics_as_of = %v, want the report's completion time in RFC 3339", got.MetricsAsOf)
	}
	if got.MetricsPending == nil || !*got.MetricsPending {
		t.Errorf("metrics_pending = %v, want true", got.MetricsPending)
	}
	if got.Totals.Spend != 80 || got.Totals.CampaignCount != 2 {
		t.Errorf("totals = %+v, want the sum of the two returned rows", got.Totals)
	}
}

// With nothing finished yet, metrics_as_of is absent, metrics_pending is true, and no row is
// evaluated: unavailable metrics must not read as a campaign spending nothing.
func TestMonitorMicrosoftAdsAccount_FirstReadHasNoFindings(t *testing.T) {
	reader := &fakeReportReader{campaigns: twoCampaigns(), submitID: "r1"}
	got, err := microsoftMonitorService(reader, &fakeReportStore{}).MonitorMicrosoftAdsAccount(context.Background(),
		&conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
	if err != nil {
		t.Fatalf("MonitorMicrosoftAdsAccount: %v", err)
	}
	if got.MetricsAsOf != nil {
		t.Errorf("metrics_as_of = %v, want absent", *got.MetricsAsOf)
	}
	if got.MetricsPending == nil || !*got.MetricsPending {
		t.Errorf("metrics_pending = %v, want true", got.MetricsPending)
	}
	if len(got.ActionItems) != 0 {
		t.Errorf("action_items = %+v, want none while metrics are unavailable", got.ActionItems)
	}
	for _, c := range got.Campaigns {
		if !c.FetchFailed {
			t.Errorf("campaign %s fetch_failed = false, want true", c.PlatformCampaignID)
		}
	}
}

// The list's errors are classified like every other monitor's: a mismatched account is 400 and
// an unclassified failure is the 503 naming the account monitor.
func TestMonitorMicrosoftAdsAccount_ClassifiesListErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"account not managed", domain.ErrAccountNotManagedByConnection, "400"},
		{"no own connection", domain.ErrNotFound, "404"},
		{"disabled monitor", domain.ErrAccountMetricsUnsupported, "400"},
		{"upstream failure", errors.New("boom"), "503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := microsoftMonitorService(&fakeReportReader{listErr: tc.err}, &fakeReportStore{}).MonitorMicrosoftAdsAccount(
				context.Background(), &conn.MonitorMicrosoftAdsAccountPayload{ProjectID: "p", AccountID: "123", Days: 7})
			switch tc.want {
			case "400":
				if _, ok := err.(*conn.BadRequestError); !ok {
					t.Fatalf("got %T: %v, want 400", err, err)
				}
			case "404":
				if _, ok := err.(*conn.NotFoundError); !ok {
					t.Fatalf("got %T: %v, want 404", err, err)
				}
			case "503":
				su, ok := err.(*conn.ConnServiceUnavailableError)
				if !ok || !strings.Contains(su.Message, "account monitor") {
					t.Fatalf("got %T: %v, want a 503 naming the account monitor", err, err)
				}
			}
		})
	}
}
