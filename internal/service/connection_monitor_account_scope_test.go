// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The account monitors' scope=account read (#672): every campaign on the account, labeled with
// the project that recorded it, for global_marketing_ops only and only while the flag is on.
//
// These tests inject the checker themselves. The container's newConnectionService wiring of
// marketingOpsChecker is not covered here: without it the read fails closed (503), never open.

func TestAdmitAccountWideMonitor(t *testing.T) {
	scope := strPtr(monitorScopeAccount)
	for _, tc := range []struct {
		name    string
		flag    string
		scope   *string
		checker MarketingOpsChecker
		ctx     context.Context
		want    bool
		wantErr any
	}{
		{name: "no scope keeps the project read, whatever the flag", flag: "", scope: nil, ctx: context.Background()},
		{name: "flag off is 400", flag: "false", scope: scope, checker: &fakeMarketingOps{allow: true}, ctx: ctxWithActor(testActor), wantErr: &conn.BadRequestError{}},
		{name: "flag not exactly true is 400", flag: "TRUE", scope: scope, checker: &fakeMarketingOps{allow: true}, ctx: ctxWithActor(testActor), wantErr: &conn.BadRequestError{}},
		{name: "unknown scope is 400", flag: "true", scope: strPtr("project"), checker: &fakeMarketingOps{allow: true}, ctx: ctxWithActor(testActor), wantErr: &conn.BadRequestError{}},
		{name: "no checker is 503", flag: "true", scope: scope, ctx: ctxWithActor(testActor), wantErr: &conn.ConnServiceUnavailableError{}},
		{name: "no actor is 503", flag: "true", scope: scope, checker: &fakeMarketingOps{allow: true}, ctx: context.Background(), wantErr: &conn.ConnServiceUnavailableError{}},
		{name: "check failure is 503", flag: "true", scope: scope, checker: &fakeMarketingOps{err: errors.New("nats down")}, ctx: ctxWithActor(testActor), wantErr: &conn.ConnServiceUnavailableError{}},
		{name: "denial is 403", flag: "true", scope: scope, checker: &fakeMarketingOps{}, ctx: ctxWithActor(testActor), wantErr: &conn.ForbiddenError{}},
		{name: "grant admits", flag: "true", scope: scope, checker: &fakeMarketingOps{allow: true}, ctx: ctxWithActor(testActor), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, tc.flag)
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetMarketingOpsChecker(tc.checker)
			got, err := svc.admitAccountWideMonitor(tc.ctx, "tlf", tc.scope)
			if got != tc.want {
				t.Errorf("admitted = %v, want %v", got, tc.want)
			}
			switch want := tc.wantErr.(type) {
			case nil:
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			case *conn.BadRequestError:
				if !errors.As(err, &want) {
					t.Fatalf("err = %T %v, want 400", err, err)
				}
			case *conn.ConnServiceUnavailableError:
				if !errors.As(err, &want) {
					t.Fatalf("err = %T %v, want 503", err, err)
				}
			case *conn.ForbiddenError:
				if !errors.As(err, &want) {
					t.Fatalf("err = %T %v, want 403", err, err)
				}
			}
		})
	}
}

// The check is asked about the requested project and the authenticated user, nothing else.
func TestAdmitAccountWideMonitor_ChecksProjectAndActor(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	ops := &fakeMarketingOps{allow: true}
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetMarketingOpsChecker(ops)
	if _, err := svc.admitAccountWideMonitor(ctxWithActor(testActor), "cncf", strPtr(monitorScopeAccount)); err != nil {
		t.Fatal(err)
	}
	if len(ops.calls) != 1 || ops.calls[0] != "cncf|ada" {
		t.Errorf("checks = %v, want one for cncf|ada", ops.calls)
	}
}

// AC: scope=account returns every campaign on the account, not only the project's, with each
// row naming its recording project (absent for one created outside LFX), and totals over all.
func TestMonitorAccount_AccountWideReturnsEveryCampaignLabeled(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	cases := append(scopedLiveCases(), scopedMonitorCase{
		// LinkedIn reaches monitorAccount through defaultMonitorAccountID, and its system row
		// serves several foundations' accounts: the platform where this read matters most.
		name: "linkedin", provider: model.ProviderLinkedInAds, account: "512345678",
		live: []model.AccountCampaignMetrics{
			{PlatformCampaignID: "c1", Name: "own", Status: "ACTIVE", BudgetDay: 50, Spend: 5, Impressions: 100, Clicks: 2},
			{PlatformCampaignID: "c2", Name: "other foundation", Status: "ACTIVE"},
		},
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := scopeRepo([]string{"c1"}, nil)
			repo.recordedProjects = map[string]string{"c1": "p"}
			svc := liveScopeService(tc.provider,
				&scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}, repo)
			svc.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})

			got, err := accountWideCall(t, tc.provider, svc, tc.account)
			if err != nil {
				t.Fatalf("account-wide read: %v", err)
			}
			assertAccountWideLabels(t, got)
			if got.Totals.CampaignCount != 2 {
				t.Errorf("campaign_count = %d, want 2", got.Totals.CampaignCount)
			}
		})
	}
}

// Without scope the read stays project-scoped and unlabeled, flag on or not.
func TestMonitorAccount_NoScopeStaysProjectScoped(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	tc := scopedLiveCases()[1] // meta
	repo := scopeRepo([]string{"c1"}, nil)
	repo.recordedProjects = map[string]string{"c1": "p"}
	got, err := tc.call(liveScopeService(tc.provider,
		&scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}, repo), tc.account)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyOwnCampaign(t, got, tc.live[0].Spend)
	if got.Campaigns[0].RecordedProjectID != nil {
		t.Errorf("project-scoped row carries recorded_project_id %q, want absent", *got.Campaigns[0].RecordedProjectID)
	}
}

// A failed label lookup fails the read rather than calling every campaign "created outside LFX".
func TestMonitorAccount_AccountWideLabelFailureFails(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	tc := scopedLiveCases()[1] // meta
	repo := scopeRepo([]string{"c1"}, nil)
	repo.recordedErr = errors.New("db down")
	svc := liveScopeService(tc.provider,
		&scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live}}, repo)
	svc.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	if got, err := accountWideCall(t, tc.provider, svc, tc.account); err == nil {
		t.Fatalf("read = %v, want an error", campaignIDs(got))
	}
}

// The precheck still answers its 400 on an account-wide read, before any upstream call.
func TestMonitorAccount_AccountWideKeepsPrecheck(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	tc := scopedLiveCases()[1] // meta
	d := &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: tc.live},
		precheck: errors.New("malformed account")}
	svc := liveScopeService(tc.provider, d, scopeRepo([]string{"c1"}, nil))
	svc.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	if _, err := accountWideCall(t, tc.provider, svc, tc.account); err == nil {
		t.Fatal("want the precheck's error")
	}
	if d.reads != 0 {
		t.Errorf("upstream reads = %d, want 0", d.reads)
	}
}

// X's report-backed read: every campaign in the saved report, labeled.
func TestMonitorTwitter_AccountWide(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	reader, store := twitterScopeFixture()
	repo := scopeRepo([]string{"c1"}, nil)
	repo.recordedProjects = map[string]string{"c1": "p"}
	svc := twitterScopeService(reader, store, repo)
	svc.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	got, err := svc.MonitorTwitterAdsAccount(ctxWithActor(testActor), &conn.MonitorTwitterAdsAccountPayload{
		ProjectID: "p", AccountID: "18ce54d4x5t", Days: 7, Scope: strPtr(monitorScopeAccount)})
	if err != nil {
		t.Fatal(err)
	}
	assertAccountWideLabels(t, got)
}

// X's report-backed read fails on a failed label lookup too.
func TestMonitorTwitter_AccountWideLabelFailureFails(t *testing.T) {
	t.Setenv(constants.EnvAccountMonitorAccountScopeEnabled, "true")
	reader, store := twitterScopeFixture()
	repo := scopeRepo([]string{"c1"}, nil)
	repo.recordedErr = errors.New("db down")
	svc := twitterScopeService(reader, store, repo)
	svc.SetMarketingOpsChecker(&fakeMarketingOps{allow: true})
	if _, err := svc.MonitorTwitterAdsAccount(ctxWithActor(testActor), &conn.MonitorTwitterAdsAccountPayload{
		ProjectID: "p", AccountID: "18ce54d4x5t", Days: 7, Scope: strPtr(monitorScopeAccount)}); err == nil {
		t.Fatal("want an error")
	}
}

// assertAccountWideLabels checks an account-wide response of the c1 (recorded for p) / c2
// (created outside LFX) fixtures: both rows, c1 labeled p, c2 unlabeled.
func assertAccountWideLabels(t *testing.T, got *conn.AccountMonitor) {
	t.Helper()
	if ids := campaignIDs(got); len(ids) != 2 {
		t.Fatalf("campaigns = %v, want both c1 and c2", ids)
	}
	for _, c := range got.Campaigns {
		switch c.PlatformCampaignID {
		case "c1":
			if c.RecordedProjectID == nil || *c.RecordedProjectID != "p" {
				t.Errorf("c1 recorded_project_id = %v, want p", c.RecordedProjectID)
			}
		case "c2":
			if c.RecordedProjectID != nil {
				t.Errorf("c2 recorded_project_id = %q, want absent (created outside LFX)", *c.RecordedProjectID)
			}
		}
	}
}

func accountWideCall(t *testing.T, provider model.Provider, svc *ConnectionService, account string) (*conn.AccountMonitor, error) {
	t.Helper()
	ctx, scope := ctxWithActor(testActor), strPtr(monitorScopeAccount)
	switch provider {
	case model.ProviderGoogleAds:
		return svc.MonitorGoogleAdsAccount(ctx, &conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: strPtr(account), Days: 30, Scope: scope})
	case model.ProviderMetaAds:
		return svc.MonitorMetaAdsAccount(ctx, &conn.MonitorMetaAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 30, Scope: scope})
	case model.ProviderLinkedInAds:
		return svc.MonitorLinkedinAdsAccount(ctx, &conn.MonitorLinkedinAdsAccountPayload{ProjectID: "p", AccountID: strPtr(account), Days: 30, Scope: scope})
	case model.ProviderRedditAds:
		return svc.MonitorRedditAdsAccount(ctx, &conn.MonitorRedditAdsAccountPayload{ProjectID: "p", AccountID: account, Days: 30, Scope: scope})
	}
	t.Fatalf("no account-wide call for %s", provider)
	return nil, nil
}
