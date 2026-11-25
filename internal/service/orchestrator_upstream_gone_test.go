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
}

func (d *presenceDispatcher) UpstreamGone(_ context.Context, _ string, c *model.Campaign) (bool, error) {
	d.checkMu.Lock()
	defer d.checkMu.Unlock()
	d.checked = append(d.checked, c.PlatformCampaignID)
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
