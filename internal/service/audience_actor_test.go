// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"testing"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// actorJSON decodes an actor column so assertions name the person rather than compare
// raw JSON bytes, whose key order is an implementation detail of encoding/json.
func actorJSON(t *testing.T, raw json.RawMessage) *model.Actor {
	t.Helper()
	if raw == nil {
		return nil
	}
	var a model.Actor
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("unmarshal actor %s: %v", raw, err)
	}
	return &a
}

// seedAudience creates one audience attributed to creator and returns the service, the
// repo and the created view — so each test below starts from a row that already carries
// a created_by, which is the state that makes the update stamp non-trivial.
func seedAudience(t *testing.T, creator *model.Actor) (*AudienceService, *fakeAudienceRepo, *audiences.Audience) {
	t.Helper()
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, err := s.CreateAudience(ctxWithActor(creator), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})
	if err != nil {
		t.Fatalf("CreateAudience: %v", err)
	}
	return s, repo, created
}

// TestAudienceActor_UpdateStampsTheEditorNotTheCreator is the load-bearing case. The
// handler patches a row it LOADED from the database, so that row arrives already carrying
// the previous actor; writing it back untouched would silently re-assert the creator as
// the author of somebody else's edit. An audit trail that names the wrong person is worse
// than one that names nobody, because it reads as evidence.
func TestAudienceActor_UpdateStampsTheEditorNotTheCreator(t *testing.T) {
	ada := &model.Actor{Username: "ada", Email: "ada@lf.dev", Name: "Ada Lovelace"}
	grace := &model.Actor{Username: "grace", Email: "grace@lf.dev", Name: "Grace Hopper"}

	s, repo, created := seedAudience(t, ada)
	if _, err := s.UpdateAudience(ctxWithActor(grace), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{InclusionSummary: strptr("past attendees, EMEA")},
	}); err != nil {
		t.Fatalf("UpdateAudience: %v", err)
	}

	stored := repo.items[created.ID]
	gotCreator := actorJSON(t, stored.CreatedBy)
	if gotCreator == nil || gotCreator.Username != ada.Username {
		t.Errorf("created_by = %+v, want it to still name the creator %q", gotCreator, ada.Username)
	}
	gotEditor := actorJSON(t, stored.UpdatedBy)
	if gotEditor == nil {
		t.Fatal("updated_by is nil after an authenticated edit; the edit records no actor at all")
	}
	if gotEditor.Username != grace.Username {
		t.Errorf("updated_by = %q, want the EDITOR %q — the row re-asserted a stale actor",
			gotEditor.Username, grace.Username)
	}

	// A SECOND edit, by a third person. This is what a fill-only-if-empty stamp would get
	// wrong: updated_by is no longer nil, so "set it when missing" leaves Grace's name on
	// Alan's edit. The column is last-writer, not first-writer.
	alan := &model.Actor{Username: "alan", Email: "alan@lf.dev", Name: "Alan Turing"}
	if _, err := s.UpdateAudience(ctxWithActor(alan), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("2"),
		Audience: &audiences.AudienceUpdateInput{InclusionSummary: strptr("past attendees, APAC")},
	}); err != nil {
		t.Fatalf("second UpdateAudience: %v", err)
	}
	if got := actorJSON(t, repo.items[created.ID].UpdatedBy); got == nil || got.Username != alan.Username {
		t.Errorf("updated_by = %+v after a second edit, want the LATEST editor %q", got, alan.Username)
	}
}

// TestAudienceActor_CreateStampsBothColumns pins the insert half, matching the brief
// statements: leaving updated_by NULL until the first edit makes "who touched this last"
// unanswerable without also reading created_by.
func TestAudienceActor_CreateStampsBothColumns(t *testing.T) {
	ada := &model.Actor{Username: "ada", Email: "ada@lf.dev"}
	_, repo, created := seedAudience(t, ada)

	stored := repo.items[created.ID]
	if a := actorJSON(t, stored.CreatedBy); a == nil || a.Username != ada.Username {
		t.Errorf("created_by = %+v, want the creator %q", a, ada.Username)
	}
	if a := actorJSON(t, stored.UpdatedBy); a == nil || a.Username != ada.Username {
		t.Errorf("updated_by = %+v on a fresh row, want the creator %q — until somebody edits "+
			"it, the person who created it IS who touched it last", a, ada.Username)
	}
}

// TestAudienceActor_UnattributedUpdateRecordsNoActor covers a PATCH that arrives with no
// principal in its context. NULL means "not recorded", never "nobody" — substituting a
// placeholder, or carrying the creator forward, would invent attribution for a write no
// identified human made.
//
// It was named ..._SystemUpdateRecordsNoActor and its comment said it covered the BUILD
// path, which it does not: the build never reaches this handler. See
// TestAudienceActor_BuildCarriesTheInitiatorForward for that path, and note the two
// answers differ — which is why one test could not have covered both.
func TestAudienceActor_UnattributedUpdateRecordsNoActor(t *testing.T) {
	ada := &model.Actor{Username: "ada", Email: "ada@lf.dev"}
	s, repo, created := seedAudience(t, ada)

	if _, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{InclusionSummary: strptr("built by the job")},
	}); err != nil {
		t.Fatalf("UpdateAudience: %v", err)
	}

	stored := repo.items[created.ID]
	if stored.UpdatedBy != nil {
		t.Errorf("updated_by = %s on an unauthenticated write. NULL means \"not recorded\"; "+
			"carrying the creator forward would attribute a system write to a human.", stored.UpdatedBy)
	}
	if a := actorJSON(t, stored.CreatedBy); a == nil || a.Username != ada.Username {
		t.Errorf("created_by = %+v, want the system update to leave the creator alone", a)
	}
}

// TestAudienceActor_BuildCarriesTheInitiatorForward pins the build path, which answers the
// attribution question DIFFERENTLY from the actorless PATCH above — and does so by a route
// that never touches the update handler.
//
// `BuildAudience` inserts the row from the request context (stamping the initiator into both
// actor columns) and then writes progress with `repo.UpdateAudience(ctx, created, ...)`,
// passing the model it already holds. Nothing clears `UpdatedBy` on the way, so the person
// who asked for the build stays recorded as its last writer. That is the right answer: the
// build is machinery acting on one human's instruction, and blanking the column would lose
// the only identity anyone could be asked about. It is also the OPPOSITE of what the handler
// does for an unattributed PATCH, where there is no such human.
//
// The comment on the test above used to claim this case, which is how a difference this
// sharp went unpinned — a rename alone would have left the coverage gap it described.
func TestAudienceActor_BuildCarriesTheInitiatorForward(t *testing.T) {
	ada := &model.Actor{Username: "ada", Email: "ada@lf.dev", Name: "Ada Lovelace"}
	b := &fakeBuilder{editions: []string{"KubeCon Korea 2025"}}
	s, arepo, _ := newBuildService(t, b, `{"eventName":"KubeCon Korea 2026","country":"South Korea","location":"Korea","year":"2026"}`)

	res, err := s.BuildAudience(ctxWithActor(ada), &audiences.BuildAudiencePayload{
		ProjectID: "cncf", BriefID: "brief-1",
	})
	if err != nil {
		t.Fatalf("BuildAudience: %v", err)
	}

	stored := arepo.items[res.ID]
	if a := actorJSON(t, stored.UpdatedBy); a == nil || a.Username != ada.Username {
		t.Errorf("updated_by = %+v after a build initiated by %s; the build's progress write "+
			"must not drop the initiator — she is the only identity attached to this row's "+
			"machinery", a, ada.Username)
	}
	if a := actorJSON(t, stored.CreatedBy); a == nil || a.Username != ada.Username {
		t.Errorf("created_by = %+v, want the build's own initiator", a)
	}
}
