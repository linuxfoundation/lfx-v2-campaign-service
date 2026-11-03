// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	// The row id is the PERSISTED row's (the claim's), which the dispatcher's struct never has.
	if res[0].CampaignRowID == "" || !strings.HasPrefix(res[0].CampaignRowID, "claim-") {
		t.Fatalf("campaign_row_id = %q, want the claimed row's id", res[0].CampaignRowID)
	}
	// The persist carries the CLAIMED row's labels, so an upsert that takes its INSERT arm (the
	// claim was cleared mid-flight) recreates the row with the stage its name was composed from.
	if len(camps.upserted) != 1 || camps.upserted[0].FunnelStage == nil || *camps.upserted[0].FunnelStage != model.FunnelStageLower {
		t.Fatalf("upserted %+v, want the claimed row's lower stage on the persisted campaign", camps.upserted)
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

// A claim lost to a COMPLETED row (another request created slot 1 between this request's read and
// its claim) is a reuse: reused=true, the winner's row id and its stored labels, no dispatch.
func TestOrchestrator_AClaimLostToACompletedRowIsAReuse(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		camps.storeRow(&model.Campaign{
			ID: "winner", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
			Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: "pc-winner", Status: "created",
			FunnelStage: stagePtr(model.FunnelStageTop),
		})
	}
	disp := &labelRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	res := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(0)))

	if len(disp.labels) != 0 {
		t.Fatalf("dispatched %d times, want none", len(disp.labels))
	}
	if len(res) != 1 || !res[0].Reused || res[0].CampaignRowID != "winner" || res[0].FunnelStage != "top" {
		t.Fatalf("result = %+v, want the winner reused with its stored top stage", res)
	}
}

// An UNFINISHED latest campaign is never built on, whatever the expectation: the request claims
// that campaign's own slot version and is reported as skipped behind it — not as a reuse, since
// nothing complete came back.
func TestOrchestrator_NewVersionBehindAnUnfinishedCampaignIsSkippedNotReused(t *testing.T) {
	for _, expected := range []int{0, 1, 2} {
		jobs := newFakeJobRepo()
		camps := &fakeCampaignRepo{}
		camps.storeRow(&model.Campaign{
			ID: "inflight", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
			Variant: model.VariantDefault, SlotVersion: 1, Status: "pending",
		})
		disp := &slotRecordingDispatcher{}
		orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

		res := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(expected)))

		if got := disp.seen(); len(got) != 0 {
			t.Fatalf("expected %d: dispatched %v, want nothing beside an unfinished campaign", expected, got)
		}
		if len(res) != 1 || !res[0].Skipped || res[0].Reused {
			t.Fatalf("expected %d: result = %+v, want skipped and not reused", expected, res)
		}
	}
}

// The accepted residual, pinned so it is a decision rather than an accident: with every live
// version gone the expectation is NOT compared — there is no live campaign a repeat could
// duplicate — so a request that expected slot 1 still creates the next version past the deleted
// ones. The same holds for a replay arriving after the campaign it created was deleted.
func TestOrchestrator_NewVersionWithNoLiveCampaignIgnoresTheExpectation(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.deletedSlotVersions = map[string]int{slotKey("b1", model.ProviderGoogleAds, model.VariantDefault): 2}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	res := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(1)))

	if got := disp.seen(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("dispatched %v, want slot 3 created past the deleted versions", got)
	}
	if len(res) != 1 || res[0].Reused {
		t.Fatalf("result = %+v, want a creation, not a reuse", res)
	}
}

// partialDispatcher creates the campaign upstream and then fails a later step: a non-nil
// campaign WITH an error, the partial-result shape whose claim is retained.
type partialDispatcher struct{}

func (partialDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{Platform: p, PlatformCampaignID: "pc-partial", Status: "pending", CampaignName: "n"},
		errors.New("ad group create failed")
}

// A retained partial names the row it was recorded on (#453 review): with several campaigns on
// one platform, the result must say WHICH one needs reconciling.
func TestOrchestrator_ARetainedPartialNamesItsRow(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: partialDispatcher{}})
	opts := newVersionSaw(1)
	opts.Labels = model.CampaignLabels{FunnelStage: stagePtr(model.FunnelStageMid)}

	res := decodeResults(t, startAndWait(t, orch, jobs, opts))

	if len(res) != 1 || res[0].OK {
		t.Fatalf("result = %+v, want the partial reported as a failure", res)
	}
	if !strings.HasPrefix(res[0].CampaignRowID, "claim-") || res[0].SlotVersion != 2 || res[0].FunnelStage != "mid" {
		t.Fatalf("result = %+v, want the slot 2 claim row named, with its mid stage", res)
	}
}

// A RETRY that meets a retained partial (an earlier dispatch created something upstream, then
// failed) reports reconciliation and names the row (#453 review): its id, slot version and stored
// labels, which for an id-less partial are the only handle on it.
func TestOrchestrator_ARetryOfARetainedPartialNamesItsRow(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.storeRow(&model.Campaign{
		ID: "orphan", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, Status: "pending",
		Result: json.RawMessage(`{"campaignName":"n"}`), FunnelStage: stagePtr(model.FunnelStageLower),
	})
	disp := &labelRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	res := decodeResults(t, startAndWait(t, orch, jobs, StartOptions{}))

	if len(disp.labels) != 0 {
		t.Fatalf("a retry dispatched beside a retained partial %d times", len(disp.labels))
	}
	if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "reconciliation required") ||
		res[0].CampaignRowID != "orphan" || res[0].SlotVersion != 1 || res[0].FunnelStage != "lower" {
		t.Fatalf("result = %+v, want reconciliation required on the named orphan row with its lower stage", res)
	}
}

// #453 review: a replay (expecting slot 1) that reads an UNFINISHED slot 3 — a later request's
// in-flight claim — must not claim slot 3 itself. If that claim is released between the read and
// a claim here, the claim would insert slot 3 afresh and dispatch another paid campaign under a
// stale expectation. new_version therefore never claims an unfinished version: it is skipped.
func TestOrchestrator_NewVersionNeverReclaimsAnUnfinishedVersionReleasedAfterTheRead(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	key := slotKey("b1", model.ProviderGoogleAds, model.VariantDefault)
	camps.storeRow(&model.Campaign{
		ID: "row-3", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 3, Status: "pending",
	})
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		// The owner's pre-create failure releases its claim right after this request read it.
		delete(camps.existing, key)
		delete(camps.existing, legacySlotKey("b1", model.ProviderGoogleAds))
	}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	res := decodeResults(t, startAndWait(t, orch, jobs, newVersionSaw(1)))

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatched slot versions %v, want none: a stale replay re-claimed a released version", got)
	}
	if got := camps.claimSlotVersions; len(got) != 0 {
		t.Fatalf("claimed %v, want no claim", got)
	}
	if len(res) != 1 || !res[0].Skipped || res[0].CampaignRowID != "row-3" {
		t.Fatalf("result = %+v, want skipped behind row-3", res)
	}
}
