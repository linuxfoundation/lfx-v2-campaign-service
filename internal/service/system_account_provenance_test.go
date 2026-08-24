// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// provenanceDispatcher returns a campaign carrying a fixed RanOnSystemAccount, standing in
// for a real dispatcher that has just resolved its credentials and stamped what it learned.
type provenanceDispatcher struct{ ranOnSystem *bool }

func (d provenanceDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{
		PlatformCampaignID: "pc-" + string(p),
		Status:             "active",
		CampaignName:       "n",
		RanOnSystemAccount: d.ranOnSystem,
	}, nil
}

// TestOrchestrator_PersistsDispatcherProvenance pins the BOUNDARY half of LFXV2-3050.
//
// `fromSystem` is learned in internal/dispatch and the row is written in internal/service,
// and those two packages are siblings — neither imports the other. The value crosses on the
// *model.Campaign that PlatformDispatcher.Dispatch already returns, which means the
// orchestrator's contribution is entirely negative: it stamps ownership, variant, brief and
// status onto that campaign, and it must leave the provenance field ALONE.
//
// That is worth a test precisely because it is an absence. The orchestrator rewrites nearly
// every other field on this struct on the way to the upsert, so a future edit that
// normalises or zeroes the campaign — or that rebuilds it from parts, as the status-flatten
// logic nearby already does for Status — would silently drop the flag, and the row would
// record "unknown" for a campaign whose account was known all along. Nothing else in the
// suite would notice.
func TestOrchestrator_PersistsDispatcherProvenance(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name string
		flag *bool
	}{
		{"system account", &yes},
		{"project's own connection", &no},
		{"unknown", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := newFakeJobRepo()
			camps := &fakeCampaignRepo{}
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
				model.ProviderGoogleAds: provenanceDispatcher{ranOnSystem: tc.flag},
			})
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}

			id, err := orch.Start(context.Background(), brief, brief.Version,
				[]model.Provider{model.ProviderGoogleAds}, nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if j := waitForTerminal(t, jobs, id); j.Status != model.JobSucceeded {
				t.Fatalf("job status = %s, want succeeded", j.Status)
			}
			if len(camps.upserted) != 1 {
				t.Fatalf("upserted %d campaigns, want 1", len(camps.upserted))
			}

			got := camps.upserted[0].RanOnSystemAccount
			switch {
			case tc.flag == nil && got != nil:
				t.Errorf("the orchestrator invented provenance (%v) for a dispatcher that "+
					"reported none; nil must stay nil", *got)
			case tc.flag != nil && got == nil:
				t.Errorf("the orchestrator DROPPED the dispatcher's provenance (%v) on the way "+
					"to the upsert — the campaign persists as \"unknown\" even though the "+
					"account that served it was known at dispatch time", *tc.flag)
			case tc.flag != nil && got != nil && *got != *tc.flag:
				t.Errorf("the orchestrator rewrote provenance %v -> %v between the dispatcher "+
					"and the upsert", *tc.flag, *got)
			}
		})
	}
}
