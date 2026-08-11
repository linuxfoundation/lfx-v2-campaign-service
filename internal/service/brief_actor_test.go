// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
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
	// BOTH columns are stamped on insert: createBriefQuery binds $11 to created_by and
	// updated_by alike. Leaving updated_by NULL until the first edit would make "who
	// touched this last" unanswerable for every brief nobody has edited yet.
	if stored.UpdatedBy == nil || stored.UpdatedBy.Username != testActor.Username {
		t.Errorf("UpdatedBy = %+v, want the author %+v on a freshly created brief",
			stored.UpdatedBy, testActor)
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

// capturingHandler records the records slog emits, so a test can assert on the WARNING
// rather than only on the write that accompanies it.
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// TestBriefActor_MissingActorWarns pins the warning itself, not just the write beside it.
//
// A lost actor fails NOTHING: the row commits with NULL attribution and every response is a
// normal 2xx. This log line is the only signal an operator gets that a gateway change, a
// claim rename, or a regression in the verifier's claims-to-actor mapping has silently
// stopped attribution — and its
// RATE is what alerting keys on, so the operation name has to be on the record too. Without
// this test the line could be deleted or renamed and TestBriefActor_MissingActorStillWrites
// would stay green, because it only checks that the write succeeded.
func TestBriefActor_MissingActorWarns(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := newTestBriefService(newFakeBriefRepo())
	if _, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	}); err != nil {
		t.Fatalf("CreateBrief with no actor must still succeed, got: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Level != slog.LevelWarn || !strings.Contains(r.Message, "no authenticated actor") {
			continue
		}
		var op string
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "operation" {
				op = a.Value.String()
				return false
			}
			return true
		})
		if op == "" {
			t.Fatalf("the missing-actor warning carries no %q attribute; without it an operator "+
				"sees that attribution broke but not on which write path", "operation")
		}
		return
	}
	t.Fatalf("no WARN record mentioning a missing authenticated actor was emitted; an "+
		"unattributed write must not be silent. Records seen: %v", h.recs)
}

// TestBriefActor_MissingActorWarnsEvenWhenTheWriteFails pins the deliberate choice that the
// warning counts ATTEMPTS, not commits.
//
// Whether a request carried an actor is settled at the gateway, upstream of anything the
// repository does, so a write that fails on a version conflict or a database error is
// evidence about the auth path in exactly the same way a successful one is. Moving the
// warning after a successful commit would look more precise and would go silent during a
// deploy that broke auth AND writes together — precisely when the signal is wanted.
//
// This test is what stops that "tightening" from being made later without noticing: without
// it, moving the log below the repository call leaves TestBriefActor_MissingActorWarns green,
// because that test's write succeeds.
func TestBriefActor_MissingActorWarnsEvenWhenTheWriteFails(t *testing.T) {
	h := &capturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	repo := newFakeBriefRepo()
	repo.createErr = errors.New("write conflict: brief was modified concurrently")
	s := newTestBriefService(repo)
	if _, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	}); err == nil {
		t.Fatal("expected the repository failure to surface; the test needs a FAILED write")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Level == slog.LevelWarn && strings.Contains(r.Message, "no authenticated actor") {
			return
		}
	}
	t.Fatalf("a write that failed downstream emitted no missing-actor warning. The warning "+
		"counts attempts on purpose: an absent actor is a gateway fact, not a repository "+
		"outcome, and suppressing it on failure blinds the alert during exactly the "+
		"deploy that breaks both. Records seen: %v", h.recs)
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
	// The stored row must still name the AUTHOR. Asserting only "not the editor" would be
	// satisfied by a repository that dropped created_by entirely — the fake models the real
	// UPDATE, which never touches the column, so the author has to survive the edit intact.
	if stored.CreatedBy == nil {
		t.Fatal("CreatedBy is nil after an edit: the update erased authorship, which the " +
			"UPDATE statement's omission of created_by is supposed to make impossible")
	}
	if stored.CreatedBy.Username != author.Username {
		t.Errorf("CreatedBy = %+v, want the original author %+v", stored.CreatedBy, author)
	}
}

// TestBriefActor_ApproveMovesApprovedByAndUpdatedBy covers the approval path end to end.
//
// Nothing else reaches it: the other service tests approve on context.Background(), and the
// SQL-text test only proves approveBriefQuery binds updated_by to a placeholder — it cannot
// tell whether the handler passes the context actor or nil into that placeholder. Approving is
// the sign-off that lets a brief be dispatched to paid platforms, so "who signed off" and "who
// touched this last" both have to name the approver, and the two columns are stamped from the
// SAME bind parameter ($1).
func TestBriefActor_ApproveMovesApprovedByAndUpdatedBy(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	author := &model.Actor{Name: "Ada Lovelace", Email: "ada@lf.dev", Username: "ada"}
	approver := &model.Actor{Name: "Grace Hopper", Email: "grace@lf.dev", Username: "grace"}

	created, err := s.CreateBrief(ctxWithActor(author), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	version := `"1"`
	if _, err = s.ApproveBrief(ctxWithActor(approver), &briefs.ApproveBriefPayload{
		ProjectID: "cncf", BriefID: created.ID, IfMatch: &version,
	}); err != nil {
		t.Fatalf("ApproveBrief: %v", err)
	}

	stored := repo.briefs[briefKey("cncf", created.ID)]
	if stored.Status != model.BriefApproved {
		t.Fatalf("status = %q, want approved", stored.Status)
	}
	if stored.ApprovedBy == nil || stored.ApprovedBy.Username != approver.Username {
		t.Errorf("ApprovedBy = %+v, want the approver %+v", stored.ApprovedBy, approver)
	}
	if stored.UpdatedBy == nil || stored.UpdatedBy.Username != approver.Username {
		t.Errorf("UpdatedBy = %+v, want the approver %+v — approving is a write, so it moves "+
			"\"who touched this last\" as well as \"who signed off\"", stored.UpdatedBy, approver)
	}
	// Approving does not rewrite authorship: approveBriefQuery never names created_by.
	if stored.CreatedBy == nil || stored.CreatedBy.Username != author.Username {
		t.Errorf("CreatedBy = %+v, want the original author %+v", stored.CreatedBy, author)
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
//
// It must be BriefService.JWTAuth, not the connection service's: Goa wires each service to its
// own security handler, so a regression that dropped the actor from the brief endpoints alone
// would be invisible to a test that authenticated through a different service.
func TestBriefActor_TokenToPersistedActor(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)

	// The token is opaque here on purpose: since JWTAuth began VERIFYING rather than
	// decoding, "what a valid token looks like" belongs to internal/infrastructure/auth.
	// What this test still owns is everything downstream of the verifier's answer.
	s.SetTokenVerifier(verifierFor("ada-token", &model.Actor{Username: "ada", Email: "ada@lf.dev"}))
	ctx, err := s.JWTAuth(context.Background(), "ada-token", nil)
	if err != nil {
		t.Fatalf("JWTAuth: %v", err)
	}

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
