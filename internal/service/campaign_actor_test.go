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

// TestCampaignActor_SystemDispatchRecordsNoActor pins the legitimate nil. attributedActor
// returns nil — after logging a warning — whenever the request context carries no
// authenticated principal, and that nil is threaded all the way to the claim INSERT. Such a
// dispatch must still succeed and must record NULL rather than inventing an actor: NULL means
// "not recorded", never "nobody", and the distinction matters when reading an audit trail.
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

// TestCampaignActor_DeleteStampsTheDeletingActor pins the attribution on the one mutation
// where "who did this" is asked after the fact.
//
// A soft delete keeps the row precisely because it may still point at a campaign spending
// real money upstream, and the question then asked of that row is who retired it. Without
// this wiring updated_by is left naming whoever last EDITED the campaign, which is worse
// than NULL: it reads as knowledge and it is wrong.
func TestCampaignActor_DeleteStampsTheDeletingActor(t *testing.T) {
	deleter := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}
	s, camps := newDeleteService(nil)
	im := "3"

	if err := s.DeleteCampaign(ctxWithActor(deleter), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
	}); err != nil {
		t.Fatalf("DeleteCampaign: %v", err)
	}
	if !camps.called {
		t.Fatal("repo DeleteCampaign was not called")
	}
	if camps.gotActor == nil {
		t.Fatal("DeleteCampaign received a nil actor: the deletion would commit with updated_by " +
			"still naming the last editor, attributing the delete to someone who did not perform it")
	}
	if camps.gotActor.Email != deleter.Email || camps.gotActor.Username != deleter.Username {
		t.Errorf("delete actor = %+v, want %+v", camps.gotActor, deleter)
	}
}

// TestCampaignActor_SystemDeleteRecordsNoActor is the negative half. It is deliberately NOT
// binding on the attribution code — with the wiring removed the actor is nil here anyway —
// and guards the opposite regression: a future change that substitutes a placeholder actor
// (the campaign's creator, a literal "system") onto an unauthenticated delete, which would
// make the audit trail name a principal that never acted. The repo's COALESCE is what turns
// this nil into "leave the previous value alone" rather than "clear it".
func TestCampaignActor_SystemDeleteRecordsNoActor(t *testing.T) {
	s, camps := newDeleteService(nil)
	im := "3"

	if err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
	}); err != nil {
		t.Fatalf("DeleteCampaign: %v", err)
	}
	if camps.gotActor != nil {
		t.Errorf("delete actor = %+v, want nil — an unauthenticated delete records nothing "+
			"rather than inventing a principal", camps.gotActor)
	}
}

// TestCampaignActor_ToggleAttributesToTheRequestingActor closes the last unattributed
// spend-affecting write. Pausing or resuming a live campaign changes whether money is
// being spent, and ToggleCampaignStatus is the ONLY path that records who did it.
//
// The gap this closes is a test-coverage gap, not a code one: the stamp at brief.go's
// `existing.UpdatedBy = attributedActor(ctx, "toggle campaign status")` is real and
// reachable, but every other test in the toggle suite calls with context.Background(),
// where the actor is nil either way. Delete the assignment and all of them stay green
// while attribution for pause/resume is silently lost.
//
// The ordering is the second thing under test. The stamp reads the LIVE ctx; the write
// runs on a cancel-detached derivative built a line later. context.WithoutCancel keeps
// values, so both orders work today — which is exactly why a future change to how that
// detached context is built could reverse it without any test objecting.
func TestCampaignActor_ToggleAttributesToTheRequestingActor(t *testing.T) {
	toggler := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 3,
		// A DIFFERENT actor already on the row: the assertion below must show the toggler
		// replacing the previous editor, not merely that some actor is present.
		UpdatedBy: testActor,
	}
	s, camps := newToggleService(camp, &stubToggler{})
	im := "3"

	if _, err := s.ToggleCampaignStatus(ctxWithActor(toggler), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	}); err != nil {
		t.Fatalf("ToggleCampaignStatus: %v", err)
	}

	if camps.replaced == nil {
		t.Fatal("ReplaceCampaign was never called; nothing was persisted to attribute")
	}
	if camps.replaced.UpdatedBy == nil {
		t.Fatal("toggled campaign UpdatedBy is nil: whoever paused or resumed a spending " +
			"campaign is recorded nowhere, and no other write on this path records it either")
	}
	if camps.replaced.UpdatedBy.Email != toggler.Email || camps.replaced.UpdatedBy.Username != toggler.Username {
		t.Errorf("toggle actor = %+v, want %+v — the row still names the PREVIOUS editor, "+
			"attributing the pause/resume to someone who did not perform it",
			camps.replaced.UpdatedBy, toggler)
	}
}

// TestCampaignActor_UnauthenticatedToggleLeavesTheRecordedActorAlone is the negative half,
// and it pins a genuine choice rather than an accident. A toggle with no principal must
// stamp nil — NOT the campaign's creator and not a synthetic "system" actor, either of
// which would name someone who did not perform the pause. Nil is what the repository's
// `updated_by=COALESCE(...)` then turns into "leave the previous value in place", so the
// column keeps naming the last KNOWN actor rather than being cleared.
func TestCampaignActor_UnauthenticatedToggleLeavesTheRecordedActorAlone(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 3,
		UpdatedBy: testActor,
	}
	s, camps := newToggleService(camp, &stubToggler{})
	im := "3"

	if _, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	}); err != nil {
		t.Fatalf("ToggleCampaignStatus: %v", err)
	}
	if camps.replaced == nil {
		t.Fatal("ReplaceCampaign was never called")
	}
	if camps.replaced.UpdatedBy != nil {
		t.Errorf("toggle actor = %+v, want nil — an unauthenticated toggle must record "+
			"nothing rather than inherit or invent a principal", camps.replaced.UpdatedBy)
	}
}
