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

// Create 1 → create 2 → delete 2 → new_version must make slot 3, not slot 2 again. Deleting
// only unbinds the local row; the upstream campaign keeps the "-2" name, so a second slot 2
// would compose that name and a name-unique dispatcher would hand the deleted campaign back.
func TestOrchestrator_NewVersionSkipsADeletedCampaignsSlotVersion(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	camps.deletedSlotVersions = map[string]int{slotKey("b1", model.ProviderGoogleAds, model.VariantDefault): 2}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if j.Status != model.JobSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", j.Status, j.Result)
	}
	if got := disp.seen(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("dispatcher saw slot versions %v, want [3]: slot 2 belonged to a deleted campaign "+
			"whose upstream name a second slot 2 would reuse", got)
	}
	if len(camps.upserted) != 1 || camps.upserted[0].SlotVersion != 3 {
		t.Fatalf("persisted %+v, want one row at slot version 3", camps.upserted)
	}
}

// Create 1 → create 2 → delete both → new_version must make slot 3. With no live campaign left
// the read used to return nothing, the maximum was dropped with it, and the request restarted
// at slot 1, composing the first deleted campaign's upstream name again.
func TestOrchestrator_NewVersionAfterEveryVersionWasDeletedSkipsThemAll(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.deletedSlotVersions = map[string]int{slotKey("b1", model.ProviderGoogleAds, model.VariantDefault): 2}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if j.Status != model.JobSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", j.Status, j.Result)
	}
	if got := disp.seen(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("dispatcher saw slot versions %v, want [3]: slots 1 and 2 belonged to deleted "+
			"campaigns whose upstream names a new slot 1 or 2 would reuse", got)
	}
	if len(camps.upserted) != 1 || camps.upserted[0].SlotVersion != 3 {
		t.Fatalf("persisted %+v, want one row at slot version 3", camps.upserted)
	}
}

// A double-submitted new_version: request A has read the completed slot 1 and claimed slot 2,
// and request B's read sees slot 1 completed too. B must aim at the SAME slot 2, so the claim
// dedupes them. Reading the latest and the maximum separately let A's claim land between B's
// two reads: B saw slot 1 as latest but counted A's pending slot 2 in its maximum, chose slot 3,
// and both requests created a paid campaign.
func TestOrchestrator_ConcurrentNewVersionsTargetTheSameSlot(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		// Request A's claim, landing right after this request's read.
		camps.upserted = append(camps.upserted, &model.Campaign{
			ID: "row-a", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
			Variant: model.VariantDefault, SlotVersion: 2, Status: "pending",
		})
	}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := camps.claimSlotVersions; len(got) != 1 || got[0] != 2 {
		t.Fatalf("claimed slot versions %v, want [2]: a concurrent new_version must target the version "+
			"the other request is claiming, so the claim lets only one through", got)
	}
}

// The same double submit, but request A's campaign at slot 2 completes AND is deleted before this
// request's claim runs. The live-row index ignores the deleted row and would admit a second slot 2,
// recomposing the deleted campaign's upstream name. The claim refuses it, and this request is
// reported as the skipped half of the double submit: nothing is dispatched.
func TestOrchestrator_NewVersionDoesNotReclaimAVersionAConcurrentRequestUsedAndDeleted(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := completedSlot1()
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		camps.deletedSlotVersions = map[string]int{slotKey("b1", model.ProviderGoogleAds, model.VariantDefault): 2}
	}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher saw slot versions %v, want none: slot 2 belongs to a concurrent request's "+
			"deleted campaign, whose upstream name a second slot 2 would reuse", got)
	}
	if len(camps.upserted) != 0 {
		t.Fatalf("persisted %+v, want nothing", camps.upserted)
	}
	if !strings.Contains(string(j.Result), "already used this version") {
		t.Errorf("result = %s, want it reported as the skipped half of a double submit", j.Result)
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

// new_version on a slot never used targets slot 1. If a concurrent request creates slot 1 and it
// is deleted before this claim, the claim must not take slot 1 again: it is skipped.
func TestOrchestrator_NewVersionOnAnEmptySlotDoesNotReclaimAConcurrentlyDeletedSlot1(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		camps.deletedSlotVersions = map[string]int{slotKey("b1", model.ProviderGoogleAds, model.VariantDefault): 1}
	}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher saw slot versions %v, want none: slot 1 was used and deleted since the read", got)
	}
	if !strings.Contains(string(j.Result), "already used this version") {
		t.Errorf("result = %s, want it reported as skipped", j.Result)
	}
}

// new_version that finds an UNFINISHED latest campaign targets that campaign's own version. If the
// campaign completes and is deleted before this claim, the claim must not take its version again.
func TestOrchestrator_NewVersionOnAnUnfinishedSlotDoesNotReclaimItOnceDeleted(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	key := slotKey("b1", model.ProviderGoogleAds, model.VariantDefault)
	camps.storeRow(&model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		Variant: model.VariantDefault, SlotVersion: 1, Status: "pending",
	})
	camps.afterSlotRead = func() {
		camps.afterSlotRead = nil
		delete(camps.existing, key)
		delete(camps.existing, legacySlotKey("b1", model.ProviderGoogleAds))
		camps.deletedSlotVersions = map[string]int{key: 1}
	}
	disp := &slotRecordingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})

	j := startAndWait(t, orch, jobs, StartOptions{NewVersion: true})

	if got := disp.seen(); len(got) != 0 {
		t.Fatalf("dispatcher saw slot versions %v, want none: slot 1 was completed and deleted since the read", got)
	}
	if !strings.Contains(string(j.Result), "already used this version") {
		t.Errorf("result = %s, want it reported as skipped", j.Result)
	}
}
