// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// scopedInDispatcher is a scoped shared-account monitor reader that can also read only the
// scope (ScopedAccountMonitorReader). Both reads return every row, so the test also proves the
// orchestrator still filters afterwards.
type scopedInDispatcher struct {
	scopedLiveDispatcher
	fullReads int
	inReads   int
	gotIDs    map[string]bool
}

func (d *scopedInDispatcher) ReadAccountMonitor(context.Context, string, model.Provider, string, int) (*model.AccountMonitorRead, error) {
	d.fullReads++
	return &model.AccountMonitorRead{Rows: append([]model.AccountCampaignMetrics{}, d.rows...)}, nil
}

func (d *scopedInDispatcher) ReadAccountMonitorIn(_ context.Context, _ string, _ model.Provider, _ string, _ int, ids map[string]bool) (*model.AccountMonitorRead, error) {
	d.inReads++
	d.gotIDs = ids
	return &model.AccountMonitorRead{Rows: append([]model.AccountCampaignMetrics{}, d.rows...)}, nil
}

// TestReadAccountCampaignMetrics_PassesTheScopeToTheRead pins #659: a scoped read hands the
// project's campaign ids to a dispatcher that can use them, so the platform is asked only about
// those campaigns, and the rows are still filtered after the read.
func TestReadAccountCampaignMetrics_PassesTheScopeToTheRead(t *testing.T) {
	d := &scopedInDispatcher{}
	d.rows = []model.AccountCampaignMetrics{{PlatformCampaignID: "c1"}, {PlatformCampaignID: "c2"}}
	o := NewOrchestrator(scopeRepo([]string{"c1"}, nil), newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d})

	read, err := o.ReadAccountCampaignMetrics(context.Background(), "p", model.ProviderRedditAds, "a2_owner", 30)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if d.inReads != 1 || d.fullReads != 0 {
		t.Fatalf("scoped reads = %d, full reads = %d; want 1 and 0", d.inReads, d.fullReads)
	}
	if len(d.gotIDs) != 1 || !d.gotIDs["c1"] {
		t.Errorf("scope passed = %v, want {c1}", d.gotIDs)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "c1" {
		t.Errorf("rows = %+v, want only c1 (still filtered after the read)", read.Rows)
	}
}
