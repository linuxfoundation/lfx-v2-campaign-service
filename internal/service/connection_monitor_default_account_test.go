// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// #591: the Google Ads monitor's account_id is optional. Omitted, it is the customer the project's
// recorded campaigns were created under — the only way a project monitored on the LF system
// account, with no connection of its own to take the id from, can name it.

// defaultingLiveDispatcher is scopedLiveDispatcher with AccountMonitorDefaulter, using the same
// {"account": X} blob fakeMonitorScope reads, and recording the account each upstream read named.
type defaultingLiveDispatcher struct {
	scopedLiveDispatcher
	readAccounts []string
}

func (d *defaultingLiveDispatcher) ListAccountCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) ([]model.AccountCampaignMetrics, error) {
	d.mu.Lock()
	d.readAccounts = append(d.readAccounts, accountID)
	d.mu.Unlock()
	return d.scopedLiveDispatcher.ListAccountCampaignMetrics(ctx, projectID, platform, accountID, days)
}

// MonitorAccountForScope mirrors the Google adapter's contract (account_monitor_scope.go): the one
// recorded account, a provenance refusal when none records one, a mismatch when they disagree.
func (d *defaultingLiveDispatcher) MonitorAccountForScope(scope []model.ProjectCampaignScope) (string, error) {
	accounts := map[string]bool{}
	account := ""
	for _, s := range scope {
		var blob struct {
			Account string `json:"account"`
		}
		_ = json.Unmarshal(s.Result, &blob)
		if blob.Account != "" {
			accounts[blob.Account] = true
			account = blob.Account
		}
	}
	switch len(accounts) {
	case 0:
		return "", fmt.Errorf("fake: none recorded: %w", errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	case 1:
		return account, nil
	default:
		return "", fmt.Errorf("fake: %d accounts: %w", len(accounts), domain.ErrCampaignAccountMismatch)
	}
}

// MonitorScopePrecheck mirrors the Google adapter's on the one input that matters here: an empty
// customer id is malformed (googleads.ValidateCustomerID). Without it the handler's early return
// for "no account to name" would be absorbed by a precheck that accepts anything.
func (d *defaultingLiveDispatcher) MonitorScopePrecheck(accountID string) error {
	if accountID == "" {
		return fmt.Errorf("fake: empty account id: %w", domain.ErrAccountIDMalformed)
	}
	return nil
}

func newDefaultingDispatcher() *defaultingLiveDispatcher {
	return &defaultingLiveDispatcher{scopedLiveDispatcher: scopedLiveDispatcher{
		mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: []model.AccountCampaignMetrics{
			{PlatformCampaignID: "c1", Name: "own", Status: "enabled", BudgetDay: 50, Spend: 50, Impressions: 100, Clicks: 5},
			{PlatformCampaignID: "c2", Name: "other foundation", Status: "paused", BudgetDay: 20, Spend: 30},
		}},
	}}
}

func monitorGoogleWithoutAccount(svc *ConnectionService) (*conn.AccountMonitor, error) {
	return svc.MonitorGoogleAdsAccount(context.Background(),
		&conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", Days: 30})
}

// The customer the project's campaigns record is the one read, the response names it, and the
// read is still reduced to the project's own campaigns.
func TestMonitorGoogleAdsAccount_OmittedAccountReadsTheRecordedCustomer(t *testing.T) {
	d := newDefaultingDispatcher()
	svc := liveScopeService(model.ProviderGoogleAds, d, scopeRepo([]string{"c1"}, map[string]string{"c1": "8666746580"}))
	got, err := monitorGoogleWithoutAccount(svc)
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount without account_id: %v", err)
	}
	if len(d.readAccounts) != 1 || d.readAccounts[0] != "8666746580" {
		t.Errorf("upstream reads named %v, want one read of the recorded customer 8666746580", d.readAccounts)
	}
	if got.AccountID != "8666746580" {
		t.Errorf("account_id = %q, want the recorded customer 8666746580", got.AccountID)
	}
	assertOnlyOwnCampaign(t, got, 50)
}

// A project with no live campaigns takes the customer from its soft-deleted ones, which the
// monitor still shows because a local delete does not stop the upstream campaign.
func TestMonitorGoogleAdsAccount_OmittedAccountFallsBackToDeletedCampaigns(t *testing.T) {
	d := newDefaultingDispatcher()
	repo := scopeRepo([]string{}, map[string]string{"c1": "8666746580"})
	repo.deletedScopeIDs = []string{"c1"}
	got, err := monitorGoogleWithoutAccount(liveScopeService(model.ProviderGoogleAds, d, repo))
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount without account_id: %v", err)
	}
	if got.AccountID != "8666746580" || len(d.readAccounts) != 1 || d.readAccounts[0] != "8666746580" {
		t.Errorf("account_id = %q, upstream reads %v; want the deleted campaign's customer 8666746580, read once",
			got.AccountID, d.readAccounts)
	}
}

// Deleted campaigns are judged one at a time, as the monitor judges them: one recording no account
// is skipped rather than refused, and when they record more than one account the one recording
// the most is named (ties to the lowest id). A deleted row cannot be re-dispatched, so a 409 on it
// would have no remedy.
func TestMonitorGoogleAdsAccount_OmittedAccountDeletedOnlyNeverRefuses(t *testing.T) {
	cases := []struct {
		name     string
		deleted  []string
		accounts map[string]string
		want     string
	}{
		{"one recording none beside one recording a customer", []string{"c1", "c2"}, map[string]string{"c1": "8666746580"}, "8666746580"},
		{"the customer recorded most", []string{"c1", "c2", "c3"}, map[string]string{"c1": "2222222222", "c2": "1111111111", "c3": "2222222222"}, "2222222222"},
		{"a tie goes to the lowest id", []string{"c1", "c2"}, map[string]string{"c1": "2222222222", "c2": "1111111111"}, "1111111111"},
		{"none recording a customer", []string{"c1", "c2"}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDefaultingDispatcher()
			repo := scopeRepo([]string{}, tc.accounts)
			repo.deletedScopeIDs = tc.deleted
			got, err := monitorGoogleWithoutAccount(liveScopeService(model.ProviderGoogleAds, d, repo))
			if err != nil {
				t.Fatalf("MonitorGoogleAdsAccount without account_id: %v", err)
			}
			if got.AccountID != tc.want {
				t.Errorf("account_id = %q, want %q", got.AccountID, tc.want)
			}
			if wantReads := map[bool]int{true: 0, false: 1}[tc.want == ""]; len(d.readAccounts) != wantReads {
				t.Errorf("upstream reads %v, want %d", d.readAccounts, wantReads)
			}
		})
	}
}

// No campaigns recorded at all: an empty monitor with an empty account_id, and nothing is read.
func TestMonitorGoogleAdsAccount_OmittedAccountWithNoCampaignsIsEmpty(t *testing.T) {
	d := newDefaultingDispatcher()
	repo := scopeRepo([]string{}, nil)
	repo.deletedScopeIDs = []string{}
	got, err := monitorGoogleWithoutAccount(liveScopeService(model.ProviderGoogleAds, d, repo))
	if err != nil {
		t.Fatalf("MonitorGoogleAdsAccount without account_id: %v", err)
	}
	if got.AccountID != "" || got.Campaigns == nil || len(got.Campaigns) != 0 || got.ActionItems == nil || got.Totals == nil {
		t.Errorf("monitor = %+v, want an empty monitor (non-nil campaigns, action items and totals) with an empty account_id", got)
	}
	if len(d.readAccounts) != 0 {
		t.Errorf("upstream reads %v, want none for a project with no campaigns", d.readAccounts)
	}
}

// Campaigns under more than one customer, or recording none, are the 409 the caller would get
// naming a customer, and nothing is read.
func TestMonitorGoogleAdsAccount_OmittedAccountRefusesAnAmbiguousScope(t *testing.T) {
	cases := []struct {
		name     string
		accounts map[string]string
	}{
		{"two customers", map[string]string{"c1": "8666746580", "c2": "1111111111"}},
		{"none recorded", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDefaultingDispatcher()
			_, err := monitorGoogleWithoutAccount(liveScopeService(model.ProviderGoogleAds, d, scopeRepo([]string{"c1", "c2"}, tc.accounts)))
			var conflict *conn.AccountMonitorScopeConflictError
			if !errors.As(err, &conflict) || conflict.Reason != "campaign_account_mismatch" {
				t.Fatalf("err = %v, want the 409 campaign_account_mismatch", err)
			}
			if len(d.readAccounts) != 0 {
				t.Errorf("upstream reads %v, want none", d.readAccounts)
			}
		})
	}
}

// The handler's own refusals still come first, before the project's campaigns are read: the
// reserved system scope is the 404 every monitor answers it with, and an out-of-range days a 400.
func TestMonitorGoogleAdsAccount_OmittedAccountKeepsTheRequestRefusals(t *testing.T) {
	repo := scopeRepo([]string{"c1"}, map[string]string{"c1": "8666746580"})
	repo.scopeErr = errors.New("the scope must not be read")
	svc := liveScopeService(model.ProviderGoogleAds, newDefaultingDispatcher(), repo)

	_, err := svc.MonitorGoogleAdsAccount(context.Background(), &conn.MonitorGoogleAdsAccountPayload{ProjectID: model.SystemProjectID, Days: 30})
	var notFound *conn.NotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("system scope: err = %T %v, want the 404 rejectSystemScope answers", err, err)
	}
	_, err = svc.MonitorGoogleAdsAccount(context.Background(), &conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", Days: 0})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Errorf("days 0: err = %T %v, want a 400", err, err)
	}
}

// TestMonitorLinkedinAdsAccount_IsScopedAndDefaultsTheAccount (#581): the LinkedIn monitor now
// behaves as Google's does. Omitting account_id reads the account the project's campaigns record
// (how a project creating through the LF system row names a granted account), the response names
// it, the read is reduced to the project's own campaigns, a project with no recorded campaigns
// gets an empty monitor with no upstream read, and campaigns recorded under two accounts are the
// declared 409.
func TestMonitorLinkedinAdsAccount_IsScopedAndDefaultsTheAccount(t *testing.T) {
	monitor := func(svc *ConnectionService) (*conn.AccountMonitor, error) {
		return svc.MonitorLinkedinAdsAccount(context.Background(), &conn.MonitorLinkedinAdsAccountPayload{ProjectID: "p", Days: 30})
	}
	t.Run("omitted account reads the recorded one, scoped", func(t *testing.T) {
		d := newDefaultingDispatcher()
		got, err := monitor(liveScopeService(model.ProviderLinkedInAds, d, scopeRepo([]string{"c1"}, map[string]string{"c1": "500928401"})))
		if err != nil {
			t.Fatalf("MonitorLinkedinAdsAccount without account_id: %v", err)
		}
		if len(d.readAccounts) != 1 || d.readAccounts[0] != "500928401" || got.AccountID != "500928401" {
			t.Fatalf("reads %v, account_id %q; want one read of the recorded account 500928401", d.readAccounts, got.AccountID)
		}
		assertOnlyOwnCampaign(t, got, 50)
	})
	t.Run("no recorded campaigns is an empty monitor without a read", func(t *testing.T) {
		d := newDefaultingDispatcher()
		got, err := monitor(liveScopeService(model.ProviderLinkedInAds, d, scopeRepo([]string{}, nil)))
		if err != nil || got.AccountID != "" || len(got.Campaigns) != 0 || len(d.readAccounts) != 0 {
			t.Fatalf("got %+v, err %v, reads %v; want an empty monitor and no upstream read", got, err, d.readAccounts)
		}
	})
	t.Run("two recorded accounts is the scope 409", func(t *testing.T) {
		d := newDefaultingDispatcher()
		_, err := monitor(liveScopeService(model.ProviderLinkedInAds, d, scopeRepo([]string{"c1", "c2"}, map[string]string{"c1": "500928401", "c2": "538170226"})))
		var sce *conn.AccountMonitorScopeConflictError
		if !errors.As(err, &sce) || len(d.readAccounts) != 0 {
			t.Fatalf("err = %T %v, reads %v; want the declared 409 and no read", err, err, d.readAccounts)
		}
	})
}
