// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ctxWithActor puts an authenticated principal on the context the way JWTAuth does.
//
// The brief handlers read the actor with actorFromCtx and pass it to the repository, so
// this is the boundary the tests below drive. TestBriefActor_TokenToPersistedActor covers
// the step before it — that a bearer token actually produces this value.
func ctxWithActor(a *model.Actor) context.Context {
	return context.WithValue(context.Background(), actorCtxKey{}, a)
}

var testActor = &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}

// TestBriefActor_CreateStampsBothColumns asserts a created brief records its author.
//
// This is the load-bearing case for the whole feature: campaigns execute under SHARED,
// LF-owned system accounts, so every ad platform reports the same identity no matter who
// acted. If the actor is not captured here it is not recorded anywhere, and "who created
// this brief" becomes permanently unanswerable.
func TestBriefActor_CreateStampsBothColumns(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	created, err := s.CreateBrief(ctxWithActor(testActor), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	stored := repo.briefs[briefKey("cncf", created.ID)]
	if stored == nil {
		t.Fatal("brief was not stored")
	}
	if stored.CreatedBy == nil {
		t.Fatal("CreatedBy is nil: the handler never passed the context actor to the repository, " +
			"so the row commits with no record of who created it")
	}
	if stored.CreatedBy.Email != testActor.Email || stored.CreatedBy.Username != testActor.Username {
		t.Errorf("CreatedBy = %+v, want %+v", stored.CreatedBy, testActor)
	}
}

// TestBriefActor_MissingActorStillWrites pins the deliberate choice NOT to reject the write.
//
// A request with no bearer token, or one whose claims this service could not decode, yields a
// nil actor. Losing the attribution is bad; refusing the write because of it is worse — it
// would turn a token-decoding regression into a total outage of brief creation. NULL means
// "not recorded", never "nobody".
func TestBriefActor_MissingActorStillWrites(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	created, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief with no actor must still succeed, got: %v", err)
	}
	if stored := repo.briefs[briefKey("cncf", created.ID)]; stored.CreatedBy != nil {
		t.Errorf("CreatedBy = %+v, want nil for an unauthenticated context", stored.CreatedBy)
	}
}

// TestBriefActor_UpdateMovesOnlyUpdatedBy asserts an edit attributes itself to the editor
// without rewriting authorship. created_by is written once; if an edit moved it too, every
// brief would eventually claim its last editor wrote it.
func TestBriefActor_UpdateMovesOnlyUpdatedBy(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	author := &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}
	editor := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}

	created, err := s.CreateBrief(ctxWithActor(author), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	version := `"1"`
	if _, err = s.UpdateBrief(ctxWithActor(editor), &briefs.UpdateBriefPayload{
		ProjectID: "cncf",
		BriefID:   created.ID,
		IfMatch:   &version,
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025-emea"},
	}); err != nil {
		t.Fatalf("UpdateBrief: %v", err)
	}

	stored := repo.briefs[briefKey("cncf", created.ID)]
	if stored.UpdatedBy == nil || stored.UpdatedBy.Username != editor.Username {
		t.Errorf("UpdatedBy = %+v, want the editor %+v", stored.UpdatedBy, editor)
	}
	// The service builds a fresh model on update and leaves CreatedBy zero precisely because
	// the UPDATE statement does not touch created_by — the stored column keeps the author.
	// Asserting the handler does not carry the EDITOR into CreatedBy is the part that matters:
	// if it did, the edit would overwrite authorship on the way to the database.
	if stored.CreatedBy != nil && stored.CreatedBy.Username == editor.Username {
		t.Errorf("CreatedBy = %+v: the edit overwrote authorship with the editor", stored.CreatedBy)
	}
}

// TestBriefActor_DeleteAttributesTheArchive covers the write most worth attributing: archiving
// removes a brief from every list and cannot be undone through the API.
func TestBriefActor_DeleteAttributesTheArchive(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	created, err := s.CreateBrief(ctxWithActor(testActor), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	archiver := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}
	if err = s.DeleteBrief(ctxWithActor(archiver), &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); err != nil {
		t.Fatalf("DeleteBrief: %v", err)
	}

	stored := repo.briefs[briefKey("cncf", created.ID)]
	if stored.Status != model.BriefArchived {
		t.Fatalf("status = %q, want archived", stored.Status)
	}
	if stored.UpdatedBy == nil || stored.UpdatedBy.Username != archiver.Username {
		t.Errorf("UpdatedBy = %+v, want the archiver %+v", stored.UpdatedBy, archiver)
	}
}

// TestBriefActor_TokenToPersistedActor closes the loop from bearer token to stored row.
//
// The three tests above inject an actor directly, so all three would keep passing if JWTAuth
// stopped putting one on the context — every brief would silently persist with NULL
// attribution and nothing would fail. This drives the real auth entry point instead.
func TestBriefActor_TokenToPersistedActor(t *testing.T) {
	cs := newTestService(t, newFakeRepo())
	// payload {"email":"ada@lf.dev","preferred_username":"ada"} base64url-encoded, unpadded.
	const payload = "eyJlbWFpbCI6ImFkYUBsZi5kZXYiLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJhZGEifQ"
	ctx, err := cs.JWTAuth(context.Background(), "h."+payload+".s", nil)
	if err != nil {
		t.Fatalf("JWTAuth: %v", err)
	}

	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)
	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	stored := repo.briefs[briefKey("cncf", created.ID)]
	if stored.CreatedBy == nil || stored.CreatedBy.Email != "ada@lf.dev" || stored.CreatedBy.Username != "ada" {
		t.Fatalf("CreatedBy = %+v, want the principal from the bearer token", stored.CreatedBy)
	}
}
