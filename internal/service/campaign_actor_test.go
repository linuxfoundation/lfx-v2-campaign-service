// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestCampaignActor_DispatchAttributesToTheRequestingActor is the binding test for the whole
// change, and the one that distinguishes campaigns from briefs.
//
// A brief write happens on the request goroutine, so reading the actor at the write is
// enough. Campaign creation does NOT: Start returns immediately and the dispatch runs on
// o.rootCtx in a goroutine that outlives the request. Every context reachable from inside
// dispatchPlatform therefore carries no actor at all — an `actorFromCtx` there would return
// nil for every campaign ever created, silently, with no failing test and no log line.
//
// The context passed to Start below carries the actor; nothing downstream does. So the only
// way this test can pass is if Start captured the value while it still had the request
// context and threaded it down.
func TestCampaignActor_DispatchAttributesToTheRequestingActor(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}

	id, err := orch.Start(ctxWithActor(testActor), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, jobs, id)

	// The claim INSERT is the row's only INSERT — the upsert that follows takes the
	// conflict arm, which deliberately never writes created_by. If the actor did not
	// reach here it is not recorded at all.
	if len(camps.claimActors) != 1 {
		t.Fatalf("ClaimCampaignDispatch called %d times, want 1", len(camps.claimActors))
	}
	got := camps.claimActors[0]
	if got == nil {
		t.Fatal("ClaimCampaignDispatch received a nil actor: the dispatch goroutine runs on the " +
			"root context, so an actor not captured in Start is gone — every campaign row would " +
			"commit with created_by NULL and no error anywhere")
	}
	if got.Email != testActor.Email || got.Username != testActor.Username {
		t.Errorf("claim actor = %+v, want %+v", got, testActor)
	}

	if len(camps.upserted) != 1 {
		t.Fatalf("upserted %d campaigns, want 1", len(camps.upserted))
	}
	up := camps.upserted[0]
	// UpdatedBy on the same write: the upsert's conflict arm is what a re-dispatch takes,
	// and updated_by is the column that arm is allowed to move. Without it a re-dispatch
	// would record no mover at all.
	if up.UpdatedBy == nil || up.UpdatedBy.Email != testActor.Email {
		t.Errorf("upserted UpdatedBy = %+v, want %+v", up.UpdatedBy, testActor)
	}
	if up.CreatedBy == nil || up.CreatedBy.Email != testActor.Email {
		t.Errorf("upserted CreatedBy = %+v, want %+v — the INSERT arm is reachable on a retry "+
			"whose claim row was rolled back, and it is the only arm that can set the column",
			up.CreatedBy, testActor)
	}
}

// TestCampaignActor_SystemDispatchRecordsNoActor pins the legitimate nil: a dispatch with no
// authenticated principal (the recovery sweeper re-dispatching with no originating request)
// must still succeed and must record NULL rather than inventing an actor. NULL means "not
// recorded", never "nobody" — the distinction matters when reading an audit trail.
func TestCampaignActor_SystemDispatchRecordsNoActor(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}

	id, err := orch.Start(context.Background(), brief, brief.Version,
		[]model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Fatalf("status = %s, want succeeded: an unattributed dispatch is ordinary, not an error", j.Status)
	}
	if len(camps.claimActors) != 1 || camps.claimActors[0] != nil {
		t.Errorf("claim actors = %+v, want exactly one nil", camps.claimActors)
	}
}

// TestCampaignActor_UpdateStampsUpdatedByOnly asserts an edit records the editor and does not
// touch created_by. The two columns answer different questions — who authorized the spend, and
// who last changed it — and collapsing them loses the first, which is the one that cannot be
// reconstructed from anywhere else.
func TestCampaignActor_UpdateStampsUpdatedByOnly(t *testing.T) {
	author := &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}
	editor := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}

	camps := &campaignEditRepo{cur: &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Version: 5,
		CampaignName: "old", Status: model.CampaignStatusCreated, CreatedBy: author,
	}}
	s := &BriefService{
		briefs:    &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}},
		campaigns: camps,
		jobs:      newFakeJobRepo(),
		orch:      NewOrchestrator(camps, newFakeJobRepo(), nil),
	}
	v := "5"

	if _, err := s.UpdateCampaign(ctxWithActor(editor), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "new", Status: model.CampaignStatusCreated},
	}); err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}

	if camps.got == nil {
		t.Fatal("ReplaceCampaign was never called")
	}
	if camps.got.UpdatedBy == nil || camps.got.UpdatedBy.Email != editor.Email {
		t.Errorf("UpdatedBy = %+v, want %+v", camps.got.UpdatedBy, editor)
	}
	if camps.got.CreatedBy == nil || camps.got.CreatedBy.Email != author.Email {
		t.Errorf("CreatedBy = %+v, want the original author %+v — an edit must not rewrite "+
			"who launched the campaign", camps.got.CreatedBy, author)
	}
}

// TestCampaignActor_ToggleStampsUpdatedByOnly asserts a status toggle records the toggler and
// does not touch created_by. A toggle is a mutation but it records a different actor than the
// creator — the person who paused/resumed the campaign, not the person who authorized the spend.
// The two must be kept separate to properly attribute who did what.
//
// Uses the same fake infrastructure as the toggle tests in brief_test.go, but asserts the
// actor attribution specifically.
func TestCampaignActor_ToggleStampsUpdatedByOnly(t *testing.T) {
	author := &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}
	toggler := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}

	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: model.CampaignStatusCreated, Version: 2,
		CreatedBy: author,
	}
	// Use an okDispatcher that supports StatusToggler (stubToggler does).
	tog := &stubToggler{}
	repo := &toggleCampaignRepo{got: camp}
	s := &BriefService{
		briefs:    &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}},
		campaigns: repo,
		jobs:      newFakeJobRepo(),
		orch:      NewOrchestrator(repo, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: tog}),
	}
	v := "2"

	res, err := s.ToggleCampaignStatus(ctxWithActor(toggler), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Status: model.CampaignRunPaused,
	})
	if err != nil {
		t.Fatalf("ToggleCampaignStatus: %v", err)
	}

	if repo.replaced == nil {
		t.Fatal("ReplaceCampaign was never called")
	}
	if repo.replaced.UpdatedBy == nil || repo.replaced.UpdatedBy.Email != toggler.Email {
		t.Errorf("UpdatedBy = %+v, want %+v — the toggle must record who paused/resumed the campaign",
			repo.replaced.UpdatedBy, toggler)
	}
	if repo.replaced.CreatedBy == nil || repo.replaced.CreatedBy.Email != author.Email {
		t.Errorf("CreatedBy = %+v, want the original author %+v — a toggle must not rewrite "+
			"who authorized the spend", repo.replaced.CreatedBy, author)
	}
	if res.Status != model.CampaignRunPaused {
		t.Errorf("result status = %q, want paused", res.Status)
	}
}

// TestCampaignActor_SystemToggleRecordsNoActor pins the legitimate nil: a system-initiated
// toggle with no authenticated principal must still succeed and must record NULL rather than
// inventing an actor. This case is less common than system-initiated dispatch (which is the
// recovery sweeper), but a scheduled toggle or an automated remediation might do this in future.
func TestCampaignActor_SystemToggleRecordsNoActor(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: model.CampaignStatusCreated, Version: 1,
		CreatedBy: &model.Actor{Email: "creator@lf.dev"},
	}
	tog := &stubToggler{}
	repo := &toggleCampaignRepo{got: camp}
	s := &BriefService{
		briefs:    &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}},
		campaigns: repo,
		jobs:      newFakeJobRepo(),
		orch:      NewOrchestrator(repo, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: tog}),
	}
	v := "1"

	// System-initiated toggle: no actor in context (like a scheduled task or recovery action).
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Status: model.CampaignRunActive,
	})
	if err != nil {
		t.Fatalf("ToggleCampaignStatus: %v", err)
	}

	if repo.replaced == nil {
		t.Fatal("ReplaceCampaign was never called")
	}
	if repo.replaced.UpdatedBy != nil {
		t.Errorf("UpdatedBy = %+v, want nil — a system toggle has no authenticated actor", repo.replaced.UpdatedBy)
	}
	// CreatedBy must be preserved unchanged.
	if repo.replaced.CreatedBy == nil || repo.replaced.CreatedBy.Email != "creator@lf.dev" {
		t.Errorf("CreatedBy = %+v, want the original creator — a toggle must not touch it",
			repo.replaced.CreatedBy)
	}
}
