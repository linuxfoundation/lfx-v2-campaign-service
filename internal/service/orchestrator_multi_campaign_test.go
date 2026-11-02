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

// cs-1 of the multi-campaign plan: a deliberate second campaign on one platform is a
// compare-and-set on the slot version the caller saw, and every campaign carries the funnel
// stage and tactic it was CREATED with.

func decodeResults(t *testing.T, j *model.CampaignJob) []platformResult {
	t.Helper()
	var got []platformResult
	if err := json.Unmarshal(j.Result, &got); err != nil {
		t.Fatalf("decode job result %s: %v", j.Result, err)
	}
	return got
}

// AC 9: a new_version request whose expected slot version is not the latest live campaign is a
// stale repeat — it returns the latest campaign with reused=true and claims nothing.
func TestOrchestrator_NewVersionWithAStaleExpectationReusesTheLatest(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts StartOptions
	}{
		{"expected behind the latest", newVersionSaw(0)},
		{"expected ahead of the latest", newVersionSaw(5)},
		{"no expectation at all", StartOptions{NewVersion: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := newFakeJobRepo()
			camps := completedSlot1()
			disp := &slotRecordingDispatcher{}
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

			j := startAndWait(t, orch, jobs, tc.opts)

			if j.Status != model.JobSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", j.Status, j.Result)
			}
			if got := disp.seen(); len(got) != 0 {
				t.Fatalf("dispatcher saw slot versions %v, want none: a stale new_version must not create", got)
			}
			if got := camps.claimSlotVersions; len(got) != 0 {
				t.Fatalf("claimed slot versions %v, want none", got)
			}
			res := decodeResults(t, j)
			if len(res) != 1 || !res[0].OK || !res[0].Reused || res[0].CampaignRowID != "row-1" ||
				res[0].SlotVersion != 1 || res[0].CampaignID != "pc-first" {
				t.Fatalf("result = %+v, want the latest campaign (row-1, slot 1) reported as reused", res)
			}
		})
	}
}

// AC 10 (cs part): the SEQUENCES a user produces — a double click after the first job finished, a
// reload, a second tab, a late replay — all repeat the request with the expectation they were
// built from. Only the first creates; every repeat is a reuse of what it created. A request built
// from the NEW latest then creates the next one.
func TestOrchestrator_RepeatedNewVersionRequestsCreateExactlyOne(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	first := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(1)))
	if len(first) != 1 || first[0].Reused || first[0].SlotVersion != 2 {
		t.Fatalf("first request = %+v, want slot 2 created (not reused)", first)
	}
	for _, repeat := range []string{"double click", "reload", "second tab", "late replay"} {
		res := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(1)))
		if len(res) != 1 || !res[0].Reused || res[0].SlotVersion != 2 {
			t.Fatalf("%s = %+v, want slot 2 returned as reused", repeat, res)
		}
	}
	if got := disp.seen(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("dispatcher saw slot versions %v, want exactly [2]: the repeats must not create", got)
	}
	next := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(2)))
	if len(next) != 1 || next[0].Reused || next[0].SlotVersion != 3 {
		t.Fatalf("a request built from slot 2 = %+v, want slot 3 created", next)
	}
}

// labelRecordingDispatcher records the labels each dispatch saw on its context.
type labelRecordingDispatcher struct {
	mu     sync.Mutex
	labels []model.CampaignLabels
}

func (d *labelRecordingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.labels = append(d.labels, model.DispatchLabels(ctx))
	return &model.Campaign{Platform: p, PlatformCampaignID: "pc-new", Status: "created", CampaignName: "n"}, nil
}

func stagePtr(s model.FunnelStage) *model.FunnelStage { return &s }
func tacticPtr(t model.Tactic) *model.Tactic          { return &t }

// A created campaign carries the request's labels on its row, its dispatch and its result.
func TestOrchestrator_LabelsReachTheClaimTheDispatchAndTheResult(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	disp := &labelRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	opts := newVersionSaw(1)
	opts.Labels = model.CampaignLabels{FunnelStage: stagePtr(model.FunnelStageLower), Tactic: tacticPtr(model.TacticRetargeting)}

	res := decodeResults(t, startAndWait(t, orch, jobs, opts))

	if len(disp.labels) != 1 || disp.labels[0].FunnelStage == nil || *disp.labels[0].FunnelStage != model.FunnelStageLower {
		t.Fatalf("dispatch labels = %+v, want the lower stage", disp.labels)
	}
	if len(res) != 1 || res[0].FunnelStage != "lower" || res[0].Tactic != "retargeting" || res[0].Variant != model.VariantDefault {
		t.Fatalf("result = %+v, want funnel_stage lower, tactic retargeting, variant default", res)
	}
}

// A retry that carries a DIFFERENT stage returns the campaign with the stage it was created
// with: the request's labels never relabel an existing campaign.
func TestOrchestrator_ARetryWithADifferentStageKeepsTheStoredStage(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.storeRow(&model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: "pc-first", Status: "created",
		FunnelStage: stagePtr(model.FunnelStageTop), Tactic: tacticPtr(model.TacticProspecting),
	})
	disp := &labelRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	res := decodeResults(t, startAndWait(t, orch, jobs, StartOptions{
		Labels: model.CampaignLabels{FunnelStage: stagePtr(model.FunnelStageLower), Tactic: tacticPtr(model.TacticKeyword)},
	}))

	if len(disp.labels) != 0 {
		t.Fatalf("a retry of a completed campaign dispatched %d times", len(disp.labels))
	}
	if len(res) != 1 || !res[0].Reused || res[0].FunnelStage != "top" || res[0].Tactic != "prospecting" {
		t.Fatalf("result = %+v, want the stored top/prospecting campaign reported as reused", res)
	}
}

// relabellingClaimRepo hands back a claimed row whose stored labels differ from the request's —
// the case that proves the orchestrator names a campaign from the ROW, not the request.
type relabellingClaimRepo struct {
	*fakeCampaignRepo
	stored model.CampaignLabels
}

func (r *relabellingClaimRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, p model.Provider, variant string, slotVersion int, jobID string, by *model.Actor, _ model.CampaignLabels) (bool, *model.Campaign, error) {
	return r.fakeCampaignRepo.ClaimCampaignDispatch(ctx, projectID, briefID, p, variant, slotVersion, jobID, by, r.stored)
}

// The dispatcher composes names from the CLAIMED ROW's labels (model.WithDispatchLabels), never
// from the request: here the row says mid, the request says lower, and the dispatch must see mid.
func TestOrchestrator_DispatchLabelsComeFromTheClaimedRow(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &relabellingClaimRepo{fakeCampaignRepo: &fakeCampaignRepo{}, stored: model.CampaignLabels{FunnelStage: stagePtr(model.FunnelStageMid)}}
	disp := &labelRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{Labels: model.CampaignLabels{FunnelStage: stagePtr(model.FunnelStageLower)}})

	if len(disp.labels) != 1 || disp.labels[0].FunnelStage == nil || *disp.labels[0].FunnelStage != model.FunnelStageMid {
		t.Fatalf("dispatch labels = %+v, want the claimed row's mid stage, not the request's lower", disp.labels)
	}
}
