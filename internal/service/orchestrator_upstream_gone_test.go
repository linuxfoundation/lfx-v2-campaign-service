// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// A HubSpot slot takes no new version, so a completed email row is reused for every later create
// of its brief. These tests pin what happens when that row's draft has been deleted in HubSpot:
// the row is retired and the brief stages a fresh draft -- and that any doubt keeps the reuse,
// because a duplicate draft is what the reuse exists to prevent. Observed live before the fix: a
// draft staged a month earlier and since deleted came back from every Stage as "reused".

// presenceDispatcher is a dispatcher that can also answer UpstreamPresenceChecker.
type presenceDispatcher struct {
	countingDispatcher
	gone bool
	err  error

	checkMu sync.Mutex
	checked []string
	// onCheck, when set, runs during the upstream check: it stands in for a CONCURRENT create
	// that saw the same deleted draft and got to the row first.
	onCheck func()
}

func (d *presenceDispatcher) UpstreamGone(_ context.Context, _ string, c *model.Campaign) (bool, error) {
	d.checkMu.Lock()
	defer d.checkMu.Unlock()
	d.checked = append(d.checked, c.PlatformCampaignID)
	if d.onCheck != nil {
		d.onCheck()
	}
	return d.gone, d.err
}

func (d *presenceDispatcher) checks() []string {
	d.checkMu.Lock()
	defer d.checkMu.Unlock()
	return append([]string(nil), d.checked...)
}

func (d *presenceDispatcher) dispatches() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// stagedRow is a completed campaign on brief b1, as a stage a while ago left it.
func stagedRow(p model.Provider) *model.Campaign {
	return &model.Campaign{
		ID: "row-1", ProjectID: "cncf", BriefID: "b1", Platform: p,
		PlatformCampaignID: "225163932254", Status: "created", SlotVersion: model.FirstSlotVersion, Version: 3,
		Result: []byte(`{"hubspotUrl":"https://app.hubspot.com/email/8112310/edit/225163932254/settings","portalId":"8112310"}`),
	}
}

// runCreate starts one create of brief b1 on the row's platform and waits for the job to finish.
// retirable seeds the row where DeleteCampaign finds it; without it the retirement fails.
func runCreate(t *testing.T, row *model.Campaign, disp PlatformDispatcher, retirable bool) string {
	t.Helper()
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{"b1|" + string(row.Platform): row}}
	if retirable {
		camps.byID = map[string]*model.Campaign{row.ID: row}
	}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{row.Platform: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{row.Platform}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Fatalf("status = %s, want succeeded (result %s)", j.Status, j.Result)
	}
	return string(j.Result)
}

func TestOrchestrator_AHubSpotDraftDeletedUpstreamIsStagedAgain(t *testing.T) {
	row := stagedRow(model.ProviderHubSpot)
	disp := &presenceDispatcher{gone: true}

	result := runCreate(t, row, disp, true)

	if got := disp.dispatches(); got != 1 {
		t.Fatalf("dispatched %d times, want 1: a draft deleted in HubSpot must be cloned again", got)
	}
	if row.Status != model.CampaignStatusDeleted {
		t.Errorf("stale row status = %q, want %q: it must be retired, keeping its id for the audit trail", row.Status, model.CampaignStatusDeleted)
	}
	if strings.Contains(result, `"reused":true`) {
		t.Errorf("result = %s, want a fresh campaign, not a reuse", result)
	}
	if !strings.Contains(result, `"campaign_id":"pc-hubspot"`) || strings.Contains(result, "225163932254") {
		t.Errorf("result = %s, want the new draft's id and none of the deleted one's", result)
	}
}

func TestOrchestrator_AHubSpotDraftStillUpstreamIsReused(t *testing.T) {
	row := stagedRow(model.ProviderHubSpot)
	disp := &presenceDispatcher{gone: false}

	result := runCreate(t, row, disp, true)

	if got := disp.dispatches(); got != 0 {
		t.Fatalf("dispatched %d times, want 0: a draft that still exists is reused, never cloned twice", got)
	}
	if got := disp.checks(); len(got) != 1 || got[0] != "225163932254" {
		t.Errorf("checked %v, want exactly the reused draft's id", got)
	}
	if row.Status != "created" {
		t.Errorf("row status = %q, want it untouched", row.Status)
	}
	if !strings.Contains(result, `"reused":true`) || !strings.Contains(result, `"campaign_id":"225163932254"`) {
		t.Errorf("result = %s, want the existing draft reused", result)
	}
}

// Any doubt reuses the row: the check failing, and the retirement failing after a definite "gone".
func TestOrchestrator_DoubtAboutAnUpstreamDraftKeepsTheReuse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		disp      *presenceDispatcher
		retirable bool
	}{
		{name: "the platform could not be read", disp: &presenceDispatcher{err: errors.New("hubspot: 503 synthetic")}, retirable: true},
		{name: "the row could not be retired", disp: &presenceDispatcher{gone: true}, retirable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := stagedRow(model.ProviderHubSpot)

			result := runCreate(t, row, tc.disp, tc.retirable)

			if got := tc.disp.dispatches(); got != 0 {
				t.Fatalf("dispatched %d times, want 0", got)
			}
			if row.Status != "created" {
				t.Errorf("row status = %q, want it untouched", row.Status)
			}
			if !strings.Contains(result, `"reused":true`) {
				t.Errorf("result = %s, want the existing campaign reused", result)
			}
		})
	}
}

// A platform with slot versions has new_version for a deliberate second campaign, so its reuse is
// never second-guessed, even by a dispatcher that could check.
func TestOrchestrator_ASlotVersionedPlatformIsNeverCheckedUpstream(t *testing.T) {
	row := stagedRow(model.ProviderGoogleAds)
	disp := &presenceDispatcher{gone: true}

	result := runCreate(t, row, disp, true)

	if got := disp.checks(); len(got) != 0 {
		t.Errorf("checked %v, want no upstream read for a slot-versioned platform", got)
	}
	if got := disp.dispatches(); got != 0 {
		t.Fatalf("dispatched %d times, want 0", got)
	}
	if !strings.Contains(result, `"reused":true`) {
		t.Errorf("result = %s, want the existing campaign reused", result)
	}
}

// Losing the retirement race is not doubt. Both requests have confirmed the draft is gone, so the
// loser must never report the deleted draft's id as a successful reuse: it answers from what the
// slot holds once the winner has acted.
func TestOrchestrator_LosingTheRetirementRaceNeverReusesTheDeletedDraft(t *testing.T) {
	const slot = "b1|" + string(model.ProviderHubSpot)
	for _, tc := range []struct {
		name string
		// winner mutates the repository the way the concurrent request left it.
		winner         func(camps *fakeCampaignRepo, row *model.Campaign)
		wantDispatches int
		wantInResult   []string
	}{
		{
			name: "the winner retired the row and the slot is free",
			winner: func(camps *fakeCampaignRepo, row *model.Campaign) {
				row.Status = model.CampaignStatusDeleted
				delete(camps.existing, slot)
			},
			wantDispatches: 1,
			wantInResult:   []string{`"campaign_id":"pc-hubspot"`},
		},
		{
			name: "the winner already staged the replacement",
			winner: func(camps *fakeCampaignRepo, row *model.Campaign) {
				row.Status = model.CampaignStatusDeleted
				camps.existing[slot] = &model.Campaign{
					ID: "row-2", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderHubSpot,
					PlatformCampaignID: "228610027856", Status: "created", SlotVersion: model.FirstSlotVersion, Version: 1,
				}
			},
			wantDispatches: 0,
			wantInResult:   []string{`"reused":true`, `"campaign_id":"228610027856"`},
		},
		{
			name: "the winner is still staging the replacement",
			winner: func(camps *fakeCampaignRepo, row *model.Campaign) {
				row.Status = model.CampaignStatusDeleted
				camps.existing[slot] = &model.Campaign{
					ID: "row-2", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderHubSpot,
					Status: model.CampaignStatusPending, SlotVersion: model.FirstSlotVersion, Version: 1,
				}
			},
			wantDispatches: 0,
			wantInResult:   []string{`"skipped":true`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := stagedRow(model.ProviderHubSpot)
			jobs := newFakeJobRepo()
			camps := &fakeCampaignRepo{
				existing: map[string]*model.Campaign{slot: row},
				byID:     map[string]*model.Campaign{row.ID: row},
			}
			disp := &presenceDispatcher{gone: true}
			disp.onCheck = func() {
				camps.mu.Lock()
				defer camps.mu.Unlock()
				tc.winner(camps, row)
			}
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: disp})
			brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
			id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderHubSpot}, nil)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			result := string(waitForTerminal(t, jobs, id).Result)

			if got := disp.dispatches(); got != tc.wantDispatches {
				t.Errorf("dispatched %d times, want %d (result %s)", got, tc.wantDispatches, result)
			}
			if strings.Contains(result, "225163932254") {
				t.Errorf("result = %s, must not carry the id of the draft this request confirmed deleted", result)
			}
			for _, want := range tc.wantInResult {
				if !strings.Contains(result, want) {
					t.Errorf("result = %s, want it to contain %s", result, want)
				}
			}
		})
	}
}
