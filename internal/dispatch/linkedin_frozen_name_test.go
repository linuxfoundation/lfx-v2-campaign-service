// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// #587 retry safety, end to end through the dispatcher: the event's group exists and holds the
// campaign an earlier attempt created under a name the current rules would NOT compose for this
// row. With that name frozen on the dispatch context (the orchestrator reads it off the row), the
// dispatcher reuses the campaign; nothing is POSTed, so no second paid campaign exists.
func TestLinkedInDispatch_FrozenNameReusesTheEarlierCampaign(t *testing.T) {
	const recorded = "Events | KubeCon NA 2026 | LinkedIn | Traffic | Prospecting retargeting | Static | cncf | BoFU"
	w := &liWire{
		groups:    []fakeCampaign{{"100", "Events | KubeCon NA 2026 | cncf"}},
		campaigns: []fakeCampaign{{"200", recorded}},
	}
	ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), recorded)
	cfg := linkedInMultiCfg
	provider := model.ProviderLinkedInAds

	camp, err := w.dispatcher(t).Dispatch(ctx, testBrief(), provider, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	creates, _, _ := w.snapshot()
	if camp.PlatformCampaignID != "200" || len(creates) != 0 {
		t.Fatalf("campaign = %q, creates = %q; want the existing 200 reused and nothing created", camp.PlatformCampaignID, creates)
	}
	if camp.CampaignName != recorded {
		t.Errorf("CampaignName = %q, want the recorded %q", camp.CampaignName, recorded)
	}
}
