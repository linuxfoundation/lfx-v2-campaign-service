// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// #587: a campaign name a slot has already recorded is frozen. The dispatcher is handed it on the
// context (model.WithDispatchCampaignName) so a name-based platform sends it back verbatim rather
// than recomposing one under naming rules that may have changed since the first attempt.

// nameRecordingDispatcher records the frozen name each dispatch saw on its context.
type nameRecordingDispatcher struct {
	mu    sync.Mutex
	names []string
}

func (d *nameRecordingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names = append(d.names, model.DispatchCampaignName(ctx))
	return &model.Campaign{Platform: p, PlatformCampaignID: "pc-1", Status: "created", CampaignName: "n"}, nil
}

func (d *nameRecordingDispatcher) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.names...)
}

// recordedClaimRepo hands back, on a WON claim, a row that already records a name in its Result
// blob — the shape a re-dispatch of a row that recorded a partial create presents.
type recordedClaimRepo struct {
	*fakeCampaignRepo
	result json.RawMessage
}

func (r *recordedClaimRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, p model.Provider, variant string, slotVersion int, jobID string, by *model.Actor, labels model.CampaignLabels) (bool, *model.Campaign, error) {
	claimed, row, err := r.fakeCampaignRepo.ClaimCampaignDispatch(ctx, projectID, briefID, p, variant, slotVersion, jobID, by, labels)
	if claimed && row != nil {
		cp := *row
		cp.Result = r.result
		row = &cp
	}
	return claimed, row, err
}

// clearedAfterReadRepo returns a retained partial row from the lookup and then removes it, as an
// operator clearing the stuck claim between this request's read and its claim would: the claim
// then WINS with a fresh row that records nothing.
type clearedAfterReadRepo struct {
	*fakeCampaignRepo
	row *model.Campaign
}

func (r *clearedAfterReadRepo) GetCampaignByPlatform(_ context.Context, _, _ string, _ model.Provider, _ string) (*model.Campaign, error) {
	return r.row, nil
}

const recordedV1Name = "Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | cncf | MoFU"

func TestOrchestrator_DispatchCarriesTheClaimedRowsRecordedName(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &recordedClaimRepo{fakeCampaignRepo: &fakeCampaignRepo{}, result: json.RawMessage(`{"campaignName":"` + recordedV1Name + `"}`)}
	disp := &nameRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{})

	if got := disp.seen(); len(got) != 1 || got[0] != recordedV1Name {
		t.Fatalf("frozen names = %q, want [%q]: the claimed row's recorded name must be sent back verbatim", got, recordedV1Name)
	}
}

func TestOrchestrator_DispatchCarriesTheNameOfARowClearedBeforeTheClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &clearedAfterReadRepo{fakeCampaignRepo: &fakeCampaignRepo{}, row: &model.Campaign{
		ID: "orphan", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, Status: "pending", CampaignName: recordedV1Name,
	}}
	disp := &nameRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{})

	if got := disp.seen(); len(got) != 1 || got[0] != recordedV1Name {
		t.Fatalf("frozen names = %q, want [%q]: the name the read row recorded must survive its clear", got, recordedV1Name)
	}
}

// A first attempt records nothing, so nothing is frozen and the dispatcher composes a name.
func TestOrchestrator_AFirstDispatchFreezesNoName(t *testing.T) {
	jobs := newFakeJobRepo()
	disp := &nameRecordingDispatcher{}
	orch := NewOrchestrator(&fakeCampaignRepo{}, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{})

	if got := disp.seen(); len(got) != 1 || got[0] != "" {
		t.Fatalf("frozen names = %q, want one empty name", got)
	}
}
