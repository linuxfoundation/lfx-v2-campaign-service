// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
)

// The brief repository's write path has, until now, been asserted only over SQL SOURCE
// TEXT — brief_repo_test.go regexes the four statement constants. That technique is
// exactly the one this package's doc comment says cannot tell you whether PostgreSQL
// accepts the statement, whether the Go argument list still lines up with the
// placeholders, or whether the version gate actually gates anything.
//
// It is a live gap with teeth here specifically because every brief write is a GUARDED
// UPDATE that reports "no row matched" by returning pgx.ErrNoRows, which the repo then
// re-interprets through classifyNoRowTx into either ErrNotFound or ErrPreconditionFailed.
// Nothing about that re-interpretation is visible in the statement text: an off-by-one in
// the placeholder numbering, a dropped `AND version=$n`, or a classifier that answered
// the wrong sentinel would all leave the source regexes green.
//
// These tests drive the real repo methods against the real table.

// newBriefRepo builds a BriefRepo over the live pool, matching how every other live test
// in this package adapts dbtest.Pool's *pgxpool.Pool to the repo's *postgres.Pool.
func newBriefRepo(pool *pgxpool.Pool) *postgres.BriefRepo {
	return postgres.NewBriefRepo(&postgres.Pool{Pool: pool})
}

// draftBrief returns an in-memory brief ready to hand to CreateBrief. The JSON columns are
// populated rather than left empty: nullJSON maps an empty json.RawMessage to SQL NULL, so
// a brief with no JSON would exercise the NULL arm of every column and never prove that a
// real document round-trips through jsonb.
func draftBrief(projectID, slug string) *model.CampaignBrief {
	return &model.CampaignBrief{
		ProjectID:    projectID,
		ProgramType:  model.ProgramType("events"),
		EventSlug:    slug,
		URL:          "https://events.example.test/" + slug,
		Platforms:    json.RawMessage(`["google_ads"]`),
		EventDetails: json.RawMessage(`{"city":"Seoul"}`),
		Copy:         json.RawMessage(`{"headline":"original"}`),
		Keywords:     json.RawMessage(`["kubernetes"]`),
		Targeting:    json.RawMessage(`{"geo":"KR"}`),
		CreatedBy:    &model.Actor{Name: "Ada Lovelace", Email: "ada@example.test"},
	}
}

// TestLiveCreateGetBriefRoundTripsEveryColumn covers the create/get half of the scope
// bullet. It asserts the values COME BACK, not merely that the calls returned nil: a
// CreateBrief whose RETURNING clause lost a column, or a scanBrief whose destination
// order drifted from briefCols, returns a populated-looking struct with the fields
// silently transposed. Comparing field by field against what went in is what catches that.
func TestLiveCreateGetBriefRoundTripsEveryColumn(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	slug := dbtest.UniqueID(t, "slug")

	created, err := repo.CreateBrief(ctx, draftBrief(project, slug), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// A freshly created brief is a DRAFT at version 1. Both are database defaults rather
	// than values the insert supplies, so this is the only place they get checked.
	if created.Status != model.BriefDraft {
		t.Errorf("new brief status = %q, want %q", created.Status, model.BriefDraft)
	}
	if created.Version != 1 {
		t.Errorf("new brief version = %d, want 1", created.Version)
	}
	if created.ID == "" {
		t.Error("CreateBrief returned an empty id; the RETURNING clause is not yielding the generated key")
	}

	// created_by AND updated_by are both stamped on insert ($11 twice in createBriefQuery).
	// If that collapsed to one column, updated_by would come back nil and "who touched this
	// last" would be unanswerable on a brand-new brief.
	if created.CreatedBy == nil || created.CreatedBy.Name != "Ada Lovelace" {
		t.Errorf("created_by = %+v, want the inserting actor", created.CreatedBy)
	}
	if created.UpdatedBy == nil || created.UpdatedBy.Name != "Ada Lovelace" {
		t.Errorf("updated_by = %+v, want the inserting actor stamped on insert", created.UpdatedBy)
	}

	// Read it back through the real GetBrief and compare against the CREATE's return
	// value. The two are produced by different statements sharing one scan function, so a
	// disagreement between them is a real defect rather than a tautology.
	got, err := repo.GetBrief(ctx, project, created.ID)
	if err != nil {
		t.Fatalf("GetBrief after create: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetBrief id = %q, want %q", got.ID, created.ID)
	}
	if got.ProjectID != project {
		t.Errorf("GetBrief project_id = %q, want %q", got.ProjectID, project)
	}
	if got.EventSlug != slug {
		t.Errorf("GetBrief event_slug = %q, want %q", got.EventSlug, slug)
	}
	if got.URL != "https://events.example.test/"+slug {
		t.Errorf("GetBrief url = %q, want the inserted url", got.URL)
	}
	if got.ProgramType != model.ProgramType("events") {
		t.Errorf("GetBrief program_type = %q, want events", got.ProgramType)
	}
	// jsonb normalises whitespace but preserves content, so compare semantically.
	assertJSONEqual(t, "copy", got.Copy, `{"headline":"original"}`)
	assertJSONEqual(t, "keywords", got.Keywords, `["kubernetes"]`)
	assertJSONEqual(t, "targeting", got.Targeting, `{"geo":"KR"}`)
	assertJSONEqual(t, "platforms", got.Platforms, `["google_ads"]`)
	assertJSONEqual(t, "event_details", got.EventDetails, `{"city":"Seoul"}`)
}

// TestLiveCreateBriefConflictsOnTheLiveSlug pins the partial unique index through the real
// method. The mapping under test is isUniqueViolation -> domain.ErrConflict: without it the
// caller sees a raw SQLSTATE 23505 wrapped in %w and answers 500 to what is a legitimate
// 409. Only a live insert can raise that SQLSTATE at all.
func TestLiveCreateBriefConflictsOnTheLiveSlug(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	slug := dbtest.UniqueID(t, "slug")

	first, err := repo.CreateBrief(ctx, draftBrief(project, slug), nil)
	if err != nil {
		t.Fatalf("first CreateBrief: %v", err)
	}

	if _, err := repo.CreateBrief(ctx, draftBrief(project, slug), nil); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second CreateBrief on the same (project, slug) = %v, want domain.ErrConflict", err)
	}

	// The index is PARTIAL — `WHERE status <> 'archived'`. Archiving the holder must free
	// the slug for a fresh brief, or an event whose brief was archived could never be
	// re-briefed. This is the half of the index a plain conflict test never reaches.
	if _, err := repo.ArchiveBrief(ctx, project, first.ID, &model.Actor{Name: "archiver"}, nil); err != nil {
		t.Fatalf("ArchiveBrief to free the slug: %v", err)
	}
	if _, err := repo.CreateBrief(ctx, draftBrief(project, slug), nil); err != nil {
		t.Errorf("CreateBrief after the holder was archived = %v, want success: the unique index is not partial on status", err)
	}
}

// TestLiveReplaceBriefTellsMissingApartFromStale is the version-gating test the ticket
// names, and it is the brief-repo analogue of TestConnectionUpdateTellsMissingApartFromStale.
//
// The distinction is the whole point. Both cases match zero rows and both surface as
// pgx.ErrNoRows out of the UPDATE, so the repo has to go back to the table through the SAME
// transaction (classifyNoRowTx) to decide which happened. Collapsing them would tell a
// client holding a stale ETag that their brief was DELETED — "refresh and retry" versus
// "it's gone" is the difference between a recoverable 412 and a dead end.
func TestLiveReplaceBriefTellsMissingApartFromStale(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	created, err := repo.CreateBrief(ctx, draftBrief(project, dbtest.UniqueID(t, "slug")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// A replace at the CURRENT version succeeds and bumps the version by exactly one.
	edit := draftBrief(project, created.EventSlug)
	edit.ID = created.ID
	edit.Copy = json.RawMessage(`{"headline":"revised"}`)
	edit.UpdatedBy = &model.Actor{Name: "Grace Hopper"}

	replaced, err := repo.ReplaceBrief(ctx, edit, created.Version, nil)
	if err != nil {
		t.Fatalf("ReplaceBrief at the current version: %v", err)
	}
	if replaced.Version != created.Version+1 {
		t.Errorf("version after replace = %d, want %d: the write is not bumping the gate", replaced.Version, created.Version+1)
	}
	assertJSONEqual(t, "copy", replaced.Copy, `{"headline":"revised"}`)
	if replaced.UpdatedBy == nil || replaced.UpdatedBy.Name != "Grace Hopper" {
		t.Errorf("updated_by after replace = %+v, want the editing actor", replaced.UpdatedBy)
	}

	// STALE: replaying the same expectedVersion must now fail the gate. Without the
	// `AND version=$12` predicate this second call would succeed and silently overwrite
	// the newer content — the lost-update the gate exists to prevent.
	if _, err := repo.ReplaceBrief(ctx, edit, created.Version, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("ReplaceBrief replayed at the stale version = %v, want domain.ErrPreconditionFailed", err)
	}

	// MISSING: a well-formed but absent id. The version is deliberately the one that
	// WOULD match a live row, so the only thing distinguishing this from the stale case
	// above is the row's absence — which is precisely what classifyNoRowTx must detect.
	absent := draftBrief(project, dbtest.UniqueID(t, "slug"))
	absent.ID = "00000000-0000-4000-8000-00000000dead"
	if _, err := repo.ReplaceBrief(ctx, absent, replaced.Version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ReplaceBrief on an absent id = %v, want domain.ErrNotFound", err)
	}
}

// TestLiveReplaceBriefWithdrawsApproval pins the security-relevant half of
// replaceBriefQuery: `status='draft', approved_by=NULL, approved_at=NULL`.
//
// If editing a brief left status='approved', changed ad copy and targeting would inherit
// the previous sign-off and could be dispatched as spend without re-review. That clause is
// invisible to a version-gate test — a replace that bumps the version correctly while
// retaining approval passes every assertion in the test above.
func TestLiveReplaceBriefWithdrawsApproval(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	created, err := repo.CreateBrief(ctx, draftBrief(project, dbtest.UniqueID(t, "slug")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	approved, err := repo.Approve(ctx, project, created.ID, &model.Actor{Name: "Approver"}, created.Version, nil)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.Status != model.BriefApproved {
		t.Fatalf("status after Approve = %q, want %q", approved.Status, model.BriefApproved)
	}

	edit := draftBrief(project, created.EventSlug)
	edit.ID = created.ID
	edit.Copy = json.RawMessage(`{"headline":"edited after approval"}`)
	edit.UpdatedBy = &model.Actor{Name: "Editor"}

	replaced, err := repo.ReplaceBrief(ctx, edit, approved.Version, nil)
	if err != nil {
		t.Fatalf("ReplaceBrief on an approved brief: %v", err)
	}
	if replaced.Status != model.BriefDraft {
		t.Errorf("status after replacing an APPROVED brief = %q, want %q: edited content is retaining its sign-off",
			replaced.Status, model.BriefDraft)
	}
	if replaced.ApprovedBy != nil {
		t.Errorf("approved_by after replace = %+v, want nil: the stale approver is still recorded", replaced.ApprovedBy)
	}
	if replaced.ApprovedAt != nil {
		t.Errorf("approved_at after replace = %v, want nil", replaced.ApprovedAt)
	}
}

// TestLiveApproveBriefGatesOnVersion covers the approve half of "approve/archive" plus its
// own version gate. Approve is gated for a distinct reason from replace: it stops a brief
// that was EDITED since the approver read it from being signed off on content they never
// saw. Same sentinel split, so the same missing-vs-stale evidence is required.
func TestLiveApproveBriefGatesOnVersion(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	created, err := repo.CreateBrief(ctx, draftBrief(project, dbtest.UniqueID(t, "slug")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// A stale approve must be refused BEFORE any successful one, so the refusal cannot be
	// explained by the brief already being approved.
	staleVersion := created.Version + 7
	if _, err := repo.Approve(ctx, project, created.ID, &model.Actor{Name: "Approver"}, staleVersion, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Fatalf("Approve at a stale version = %v, want domain.ErrPreconditionFailed", err)
	}

	// An absent id at a plausible version is ErrNotFound, not a precondition failure.
	if _, err := repo.Approve(ctx, project, "00000000-0000-4000-8000-00000000beef", &model.Actor{Name: "Approver"}, created.Version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Approve on an absent id = %v, want domain.ErrNotFound", err)
	}

	// Cross-project: the row exists, but not for this tenant. Tenant scoping must read as
	// "not found" rather than leaking that the id is real somewhere else.
	otherProject := dbtest.UniqueID(t, "other")
	if _, err := repo.Approve(ctx, otherProject, created.ID, &model.Actor{Name: "Approver"}, created.Version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Approve from another project = %v, want domain.ErrNotFound", err)
	}

	// The happy path stamps BOTH approved_by and updated_by from the one actor argument,
	// and bumps the version so a second approve at the same version cannot replay.
	approved, err := repo.Approve(ctx, project, created.ID, &model.Actor{Name: "Approver", Email: "approver@example.test"}, created.Version, nil)
	if err != nil {
		t.Fatalf("Approve at the current version: %v", err)
	}
	if approved.Status != model.BriefApproved {
		t.Errorf("status = %q, want %q", approved.Status, model.BriefApproved)
	}
	if approved.Version != created.Version+1 {
		t.Errorf("version after approve = %d, want %d", approved.Version, created.Version+1)
	}
	if approved.ApprovedBy == nil || approved.ApprovedBy.Email != "approver@example.test" {
		t.Errorf("approved_by = %+v, want the approving actor", approved.ApprovedBy)
	}
	if approved.UpdatedBy == nil || approved.UpdatedBy.Email != "approver@example.test" {
		t.Errorf("updated_by = %+v, want the approving actor: approving is a write and must be attributed", approved.UpdatedBy)
	}
	if approved.ApprovedAt == nil {
		t.Error("approved_at is nil after Approve; the timestamp is not being stamped")
	}

	// Replaying the now-stale version must fail rather than re-approve.
	if _, err := repo.Approve(ctx, project, created.ID, &model.Actor{Name: "Approver"}, created.Version, nil); !errors.Is(err, domain.ErrPreconditionFailed) {
		t.Errorf("replayed Approve at the consumed version = %v, want domain.ErrPreconditionFailed", err)
	}
}

// TestLiveArchiveBriefIsTerminalAndHidesTheRow covers archive. Archiving is deliberately
// NOT version-gated (archiveBriefQuery has no version predicate), so the behaviour worth
// pinning is different: it must be idempotent-safe — a second archive reports ErrNotFound
// rather than succeeding twice — and the archived row must disappear from GetBrief, since
// the read carries `status <> 'archived'`.
func TestLiveArchiveBriefIsTerminalAndHidesTheRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	created, err := repo.CreateBrief(ctx, draftBrief(project, dbtest.UniqueID(t, "slug")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	archived, err := repo.ArchiveBrief(ctx, project, created.ID, &model.Actor{Name: "Archiver"}, nil)
	if err != nil {
		t.Fatalf("ArchiveBrief: %v", err)
	}
	if archived.Status != model.BriefArchived {
		t.Errorf("status = %q, want %q", archived.Status, model.BriefArchived)
	}
	// Archiving is the write most worth attributing: it removes the brief from every list
	// and cannot be undone through the API.
	if archived.UpdatedBy == nil || archived.UpdatedBy.Name != "Archiver" {
		t.Errorf("updated_by after archive = %+v, want the archiving actor", archived.UpdatedBy)
	}
	if archived.Version != created.Version+1 {
		t.Errorf("version after archive = %d, want %d", archived.Version, created.Version+1)
	}

	// The archived row is invisible to the tenant-scoped read.
	if _, err := repo.GetBrief(ctx, project, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetBrief on an archived brief = %v, want domain.ErrNotFound", err)
	}

	// Archiving twice must not silently succeed: the `status <> 'archived'` guard makes the
	// second call match no row, which ArchiveBrief reports as ErrNotFound.
	if _, err := repo.ArchiveBrief(ctx, project, created.ID, &model.Actor{Name: "Archiver"}, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second ArchiveBrief = %v, want domain.ErrNotFound", err)
	}

	// An archived brief is also frozen against further edits: replaceBriefQuery and
	// approveBriefQuery both carry `status <> 'archived'`. classifyNoRowTx applies the same
	// predicate, so it reports the archived row as MISSING rather than stale — "refresh and
	// retry" would be the wrong instruction for a brief that is gone.
	edit := draftBrief(project, created.EventSlug)
	edit.ID = created.ID
	edit.UpdatedBy = &model.Actor{Name: "Editor"}
	if _, err := repo.ReplaceBrief(ctx, edit, archived.Version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("ReplaceBrief on an archived brief = %v, want domain.ErrNotFound", err)
	}
	if _, err := repo.Approve(ctx, project, created.ID, &model.Actor{Name: "Approver"}, archived.Version, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Approve on an archived brief = %v, want domain.ErrNotFound", err)
	}
}

// TestLiveGetBriefIsTenantScoped pins the project predicate on the read. A job or brief UUID
// alone must never expose another tenant's row; the id is a bearer capability otherwise.
func TestLiveGetBriefIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Pool(t)
	repo := newBriefRepo(pool)

	project := dbtest.UniqueID(t, "project")
	created, err := repo.CreateBrief(ctx, draftBrief(project, dbtest.UniqueID(t, "slug")), nil)
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// Same id, different project: must be indistinguishable from absent.
	other := dbtest.UniqueID(t, "other")
	if _, err := repo.GetBrief(ctx, other, created.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetBrief with a foreign project_id = %v, want domain.ErrNotFound", err)
	}
	// Sanity: the row really is readable by its owner, so the assertion above is about
	// tenancy and not about the row having failed to insert.
	if _, err := repo.GetBrief(ctx, project, created.ID); err != nil {
		t.Fatalf("GetBrief by the owning project: %v", err)
	}
}

// assertJSONEqual compares a jsonb column against expected JSON semantically. jsonb
// reorders object keys and strips insignificant whitespace, so a byte comparison against
// the literal that was inserted fails for reasons that have nothing to do with the code
// under test.
func assertJSONEqual(t *testing.T, column string, got json.RawMessage, want string) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Errorf("%s: stored value is not valid JSON (%q): %v", column, string(got), err)
		return
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("%s: test's own expected JSON is invalid: %v", column, err)
	}
	gotNorm, err := json.Marshal(gotVal)
	if err != nil {
		t.Fatalf("%s: re-marshal stored value: %v", column, err)
	}
	wantNorm, err := json.Marshal(wantVal)
	if err != nil {
		t.Fatalf("%s: re-marshal expected value: %v", column, err)
	}
	if string(gotNorm) != string(wantNorm) {
		t.Errorf("%s = %s, want %s", column, gotNorm, wantNorm)
	}
}
