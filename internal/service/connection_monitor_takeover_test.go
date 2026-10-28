// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The two precheck queries deliberately remain separate. This wrapper makes a takeover
// land between them; the final snapshot must not retain the originally live binding.
type takeoverScopeRepo struct {
	*fakeCampaignRepo
	afterLive   func()
	snapshotErr error
	snapshots   int
}

func (r *takeoverScopeRepo) ListProjectPlatformDeletedCampaignIDs(ctx context.Context, projectID string, platform model.Provider) ([]model.ProjectCampaignScope, error) {
	if r.afterLive != nil {
		r.afterLive()
	}
	return r.fakeCampaignRepo.ListProjectPlatformDeletedCampaignIDs(ctx, projectID, platform)
}

func (r *takeoverScopeRepo) ListProjectPlatformMonitorCampaignIDs(ctx context.Context, projectID string, platform model.Provider) ([]model.ProjectCampaignScope, error) {
	r.snapshots++
	if r.snapshotErr != nil {
		return nil, r.snapshotErr
	}
	return r.fakeCampaignRepo.ListProjectPlatformMonitorCampaignIDs(ctx, projectID, platform)
}

type takeoverLiveDispatcher struct {
	*scopedLiveDispatcher
	beforeRead func()
}

func (d *takeoverLiveDispatcher) ListAccountCampaignMetrics(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) ([]model.AccountCampaignMetrics, error) {
	if d.beforeRead != nil {
		d.beforeRead()
	}
	return d.scopedLiveDispatcher.ListAccountCampaignMetrics(ctx, projectID, platform, accountID, days)
}

type takeoverReportDispatcher struct {
	*scopedReportDispatcher
	beforeRead     func()
	beforeCurrency func()
}

func (d *takeoverReportDispatcher) ListAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AccountCampaignMetrics, error) {
	if d.beforeRead != nil {
		d.beforeRead()
	}
	return d.scopedReportDispatcher.ListAccountCampaigns(ctx, projectID, platform, accountID)
}

func (d *takeoverReportDispatcher) ReadAccountCurrency(context.Context, string, model.Provider, string) (string, error) {
	if d.beforeCurrency != nil {
		d.beforeCurrency()
	}
	return "USD", nil
}

func TestMonitorAccount_RevalidationWithoutScoperFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dispatchers map[model.Provider]PlatformDispatcher
	}{
		{name: "missing dispatcher"},
		{
			name: "dispatcher without scoper",
			dispatchers: map[model.Provider]PlatformDispatcher{
				model.ProviderGoogleAds: &mockAccountMetricsReaderDispatcher{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &takeoverScopeRepo{fakeCampaignRepo: scopeRepo([]string{"c1"}, nil)}
			o := NewOrchestrator(repo, newFakeJobRepo(), tc.dispatchers)
			ids, err := o.revalidateAccountMonitorScope(context.Background(), "p", model.ProviderGoogleAds, "1234567890", map[string]bool{"c1": true})
			if err == nil || ids != nil {
				t.Fatalf("revalidation without scoper = %v, %v; want no IDs and an error", ids, err)
			}
		})
	}
}

func TestMonitorAccount_RevalidatesOwnershipAfterUpstreamReads(t *testing.T) {
	for _, reportBacked := range []bool{false, true} {
		for _, stage := range []string{"between scope queries", "during campaign read", "deleted during campaign read", "during currency read", "snapshot failure", "changed provenance"} {
			if !reportBacked && stage == "during currency read" {
				continue
			}
			t.Run(fmt.Sprintf("report=%t/%s", reportBacked, stage), func(t *testing.T) {
				repo := &takeoverScopeRepo{fakeCampaignRepo: scopeRepo([]string{"c1", "c2"}, nil)}
				repo.deletedScopeIDs = []string{}
				if stage == "deleted during campaign read" {
					repo.scopeIDs = []string{"c1"}
					repo.deletedScopeIDs = []string{"c2"}
				}
				lookupErr := errors.New("ownership snapshot unavailable")
				takeover := func() {
					repo.mu.Lock()
					defer repo.mu.Unlock()
					// c2 now belongs to another project. c3 is newly acquired by this project,
					// but must not widen a read that never initially authorized it.
					repo.scopeIDs = []string{"c1", "c3"}
					repo.deletedScopeIDs = []string{}
				}
				if stage == "between scope queries" {
					repo.afterLive = takeover
				}
				if stage == "snapshot failure" {
					repo.snapshotErr = lookupErr
				}
				entered, release := make(chan struct{}), make(chan struct{})
				defer close(release)
				block := func() {
					close(entered)
					<-release
				}
				var beforeRead, beforeCurrency func()
				switch stage {
				case "during campaign read", "deleted during campaign read", "changed provenance":
					beforeRead = block
				case "during currency read":
					beforeCurrency = block
				}

				provider := model.ProviderGoogleAds
				rows := []model.AccountCampaignMetrics{
					{PlatformCampaignID: "c1", Name: "own", Status: "enabled", Spend: 7, BudgetDay: 10},
					{PlatformCampaignID: "c2", Name: "new owner's name", Status: "paused", Spend: 900, BudgetDay: 900},
					{PlatformCampaignID: "c3", Name: "new acquisition", Status: "paused", Spend: 800},
				}
				var dispatcher PlatformDispatcher = &takeoverLiveDispatcher{
					scopedLiveDispatcher: &scopedLiveDispatcher{mockAccountMetricsReaderDispatcher: mockAccountMetricsReaderDispatcher{rows: rows}},
					beforeRead:           beforeRead,
				}
				reader, store := twitterScopeFixture()
				if reportBacked {
					provider = model.ProviderTwitterAds
					reader.campaigns = rows
					dispatcher = &takeoverReportDispatcher{scopedReportDispatcher: reader, beforeRead: beforeRead, beforeCurrency: beforeCurrency}
				}
				o := NewOrchestrator(repo, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{provider: dispatcher})
				o.SetAccountReportStore(store)
				svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
				svc.SetOrchestrator(o)
				type outcome struct {
					monitor *conn.AccountMonitor
					err     error
				}
				result := make(chan outcome, 1)
				go func() {
					var m *conn.AccountMonitor
					var err error
					if reportBacked {
						m, err = monitorTwitter(svc, "a1")
					} else {
						m, err = svc.MonitorGoogleAdsAccount(context.Background(), &conn.MonitorGoogleAdsAccountPayload{ProjectID: "p", AccountID: "1234567890", Days: 7})
					}
					result <- outcome{m, err}
				}()
				if beforeRead != nil || beforeCurrency != nil {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("monitor never reached the blocked upstream read")
					}
					if stage == "changed provenance" {
						repo.mu.Lock()
						repo.scopeResults = scopeRepo(nil, map[string]string{"c2": "another-account"}).scopeResults
						repo.mu.Unlock()
					} else {
						takeover()
					}
					// Let the captured upstream result complete after the ownership change.
					release <- struct{}{}
				}
				var got outcome
				select {
				case got = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("monitor did not finish")
				}
				if repo.snapshots != 1 {
					t.Fatalf("final snapshots = %d, want exactly one", repo.snapshots)
				}
				if stage == "snapshot failure" {
					if got.err == nil || got.monitor != nil {
						t.Fatalf("snapshot failure returned data: %+v, %v", got.monitor, got.err)
					}
					return
				}
				if got.err != nil {
					t.Fatalf("monitor: %v", got.err)
				}
				if ids := campaignIDs(got.monitor); len(ids) != 1 || ids[0] != "c1" {
					t.Fatalf("campaigns = %v; want only c1, never the taken-over c2 or newly acquired c3", ids)
				}
				if got.monitor.Totals.CampaignCount != 1 || hasActionItemFor(got.monitor, "c2") || hasActionItemFor(got.monitor, "c3") {
					t.Fatalf("foreign campaign affected totals/actions: %+v", got.monitor)
				}
				if !reportBacked && got.monitor.Totals.Spend != 7 {
					t.Fatalf("spend = %v, want c1's 7 only", got.monitor.Totals.Spend)
				}
			})
		}
	}
}
