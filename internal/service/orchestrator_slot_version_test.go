// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The bug these tests exist for: an operator asked for a second campaign on a brief that
// already had one on the same platform, and the service treated the repeat as a retry and
// returned the first campaign as a success. new_version is the caller's way to say "another
// one", and the slot version is how the service keeps that apart from a retry.

// slotRecordingDispatcher creates a campaign and records the slot version each call saw on
// its context, so a test can assert what a name-unique dispatcher would have composed.
type slotRecordingDispatcher struct {
	mu    sync.Mutex
	slots []int
}

func (d *slotRecordingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := model.DispatchSlotVersion(ctx)
	d.slots = append(d.slots, n)
	return &model.Campaign{
		Platform:           p,
		PlatformCampaignID: "pc-" + string(p) + "-" + strings.Repeat("v", n),
		Status:             "created",
		CampaignName:       "n",
	}, nil
}

func (d *slotRecordingDispatcher) seen() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int(nil), d.slots...)
}

// completedSlot1 is a brief that already holds a finished google-ads Search campaign.
func completedSlot1() *fakeCampaignRepo {
	camps := &fakeCampaignRepo{}
	camps.storeRow(&model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: "pc-first", Status: "created",
	})
	return camps
}

func startAndWait(t *testing.T, orch *Orchestrator, jobs *fakeJobRepo, opts StartOptions) *model.CampaignJob {
	t.Helper()
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.StartWithOptions(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil, opts)
	if err != nil {
		t.Fatalf("StartWithOptions: %v", err)
	}
	return waitForTerminal(t, jobs, id)
}

func TestOrchestrator_NewVersionCreatesASecondCampaignOnACompletedSlot(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if j.Status != model.JobSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", j.Status, j.Result)
	}
	if got := disp.seen(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("dispatcher saw slot versions %v, want [2]: the second campaign must be created, "+
			"and created as slot 2 so a name-unique platform gets a name the first does not hold", got)
	}
	if len(camps.upserted) != 1 || camps.upserted[0].SlotVersion != 2 {
		t.Fatalf("persisted %+v, want one row at slot version 2", camps.upserted)
	}
	if strings.Contains(string(j.Result), "pc-first") {
		t.Errorf("result = %s, must report the NEW campaign, not hand back the first", j.Result)
	}
}

// Without new_version a repeat create is still a retry. This is the behaviour every existing
// caller relies on, and the reason new_version is opt-in rather than inferred.
func TestOrchestrator_WithoutNewVersionARepeatCreateStillReuses(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{})

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher called with slot versions %v, want no call: a retry must reuse", got)
	}
	if !strings.Contains(string(j.Result), "pc-first") {
		t.Errorf("result = %s, want the existing campaign pc-first", j.Result)
	}
}

// On an empty slot new_version has nothing to be "another" of, so it creates the first.
func TestOrchestrator_NewVersionOnAnEmptySlotCreatesTheFirst(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := disp.seen(); len(got) != 1 || got[0] != model.FirstSlotVersion {
		t.Fatalf("dispatcher saw slot versions %v, want [1]", got)
	}
}

// An UNFINISHED latest campaign is not built on. Starting a second campaign beside one that is
// still in flight or orphaned upstream would spend twice while the first is unresolved, so
// new_version claims the latest slot version and gets the answer a retry gets.
func TestOrchestrator_NewVersionDoesNotBuildOnAnUnfinishedCampaign(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.storeRow(&model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, PlatformCampaignID: "pc-orphan", Status: "pending",
	})
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher called with slot versions %v, want no call beside an unfinished campaign", got)
	}
	if got := camps.claimSlotVersions; len(got) != 1 || got[0] != 1 {
		t.Errorf("claimed slot versions %v, want [1]: the unfinished campaign's own slot", got)
	}
	if !strings.Contains(string(j.Result), "reconciliation required") {
		t.Errorf("result = %s, want the reconciliation-required failure a retry gets", j.Result)
	}
}

// While 000022's one-campaign-per-slot index is still in place (the release that ships
// 000037), the second campaign cannot be claimed. That must surface as an explicit refusal —
// never as a reuse of the first campaign, which is the bug — and nothing may be dispatched.
func TestOrchestrator_NewVersionIsRefusedDuringTheExpandPhase(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	camps.legacySlotIndex = true
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher called with slot versions %v, want no call", got)
	}
	if !strings.Contains(string(j.Result), "not available yet") {
		t.Errorf("result = %s, want the not-available-yet refusal", j.Result)
	}
	if strings.Contains(string(j.Result), "pc-first") {
		t.Errorf("result = %s, must not report the first campaign as the second", j.Result)
	}
}

// Releasing a slot-2 claim after a pre-create failure must free slot 2 only. The first
// campaign stays live and is the latest again, so the next plain retry reuses it. (An
// AMBIGUOUS failure retains the slot-2 claim instead, exactly as it would for slot 1.)
func TestOrchestrator_ReleasingASlot2ClaimLeavesSlot1Live(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: preCreateErrDispatcher{}})

	startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	latest, err := camps.GetCampaignByPlatform(context.Background(), "cncf", "b1", model.ProviderGoogleAds, model.VariantDefault)
	if err != nil {
		t.Fatalf("GetCampaignByPlatform after the release: %v", err)
	}
	if latest.PlatformCampaignID != "pc-first" {
		t.Errorf("latest campaign = %+v, want the first campaign back as the latest", latest)
	}
}
