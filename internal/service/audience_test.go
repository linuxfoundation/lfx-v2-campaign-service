// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeAudienceRepo is a minimal in-memory AudienceRepository for handler tests.
type fakeAudienceRepo struct {
	items   map[string]*model.CampaignAudience
	seq     int
	createE error
	getE    error
	// updateE, when set, makes every UpdateAudience fail. It models the compound failure the
	// partial-build path has to survive: the build broke AND recording what it left behind
	// broke, so the row cannot carry the created list ids and the caller's error is the only
	// remaining channel for them.
	updateE error
	// briefs is the SAME brief store the service reads. The real
	// CreateAudienceForApprovedBrief locks the parent brief and gates on it, so a fake
	// holding its own private notion of approval could not express the thing these tests
	// exist for: a ReplaceBrief landing partway through a build.
	briefs *fakeBriefRepo
	// leaseHeld makes CreateAudienceForApprovedBrief report ErrAudienceBuildInFlight, which
	// is what the real repo returns when the partial unique index from migration 000018
	// rejects the insert because a concurrent build for this (brief, platform) already
	// holds the lease.
	leaseHeld bool
	// afterClaim, when set, fires once immediately after a SUCCESSFUL claim. It models the
	// only window the post-claim brief re-read can lose: the claim committed while the brief
	// was approved, and a withdrawal commits before the service reads it back. onGet cannot
	// express this — it fires after the read and returns the pre-mutation snapshot, so the
	// service would still see an approved brief.
	afterClaim func()
}

func newFakeAudienceRepo() *fakeAudienceRepo {
	return &fakeAudienceRepo{items: map[string]*model.CampaignAudience{}}
}

// CreateAudience stores a COPY and returns a SECOND copy, for the same reason GetAudience
// does. Handing the caller the stored pointer makes every later in-place mutation of the
// returned row visible in the store without any repository call, and that silently converts
// the lease tests into tautologies: releaseUnstartedClaim sets row.Status = FAILED before it
// calls UpdateAudience, so a shared pointer makes `rows()[0].Status == AudienceFailed` true
// whether or not the release is ever persisted. PostgreSQL cannot do that; neither may this.
func (r *fakeAudienceRepo) CreateAudience(_ context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	if r.createE != nil {
		return nil, r.createE
	}
	r.seq++
	a.ID = "aud-" + string(rune('a'+r.seq))
	a.Version = 1
	stored := *a
	r.items[a.ID] = &stored
	out := stored
	return &out, nil
}

// CreateAudienceForApprovedBrief models the real repo's two gates in the real repo's order:
// the parent brief is read under the claim and must be APPROVED, and the version it is at is
// reported back; then the lease. Reading the shared brief store rather than a private flag is
// what lets a test move the brief mid-build and see the same answer production would give.
func (r *fakeAudienceRepo) CreateAudienceForApprovedBrief(ctx context.Context, a *model.CampaignAudience) (*model.CampaignAudience, int64, error) {
	var version int64
	if r.briefs != nil {
		brief, ok := r.briefs.snapshot(a.ProjectID, a.BriefID)
		if !ok || brief.Status != model.BriefApproved {
			return nil, 0, domain.ErrStaleApproval
		}
		version = brief.Version
	}
	// Checked after the approval because the real repo checks the approval FIRST — the lease
	// is only consulted once the insert is attempted.
	if r.leaseHeld {
		return nil, 0, domain.ErrAudienceBuildInFlight
	}
	// Model the partial unique index itself, not just the flag: an existing BUILDING row for
	// the same (brief, platform) rejects the insert. The flag alone cannot express WHEN the
	// lease is taken, so a fake that only honoured it would pass whether the service claimed
	// before or after its slow pre-build work — which is the ordering under test.
	for _, existing := range r.items {
		if existing.BriefID == a.BriefID && existing.Platform == a.Platform &&
			existing.Status == model.AudienceBuilding {
			return nil, 0, domain.ErrAudienceBuildInFlight
		}
	}
	out, cerr := r.CreateAudience(ctx, a)
	if cerr != nil {
		return nil, 0, cerr
	}
	if r.afterClaim != nil {
		hook := r.afterClaim
		r.afterClaim = nil // one-shot
		hook()
	}
	return out, version, nil
}

func (r *fakeAudienceRepo) GetAudience(_ context.Context, _, _, id string) (*model.CampaignAudience, error) {
	if r.getE != nil {
		return nil, r.getE
	}
	a, ok := r.items[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	// Return a COPY, like PostgreSQL — otherwise the service's load-then-merge would
	// mutate the stored row in place, hiding a bug where update forgets to persist.
	cp := *a
	return &cp, nil
}

func (r *fakeAudienceRepo) ListAudiences(_ context.Context, _, _ string) ([]*model.CampaignAudience, error) {
	out := make([]*model.CampaignAudience, 0, len(r.items))
	for _, a := range r.items {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

func (r *fakeAudienceRepo) UpdateAudience(_ context.Context, a *model.CampaignAudience, expectedVersion int64) (*model.CampaignAudience, error) {
	if r.updateE != nil {
		return nil, r.updateE
	}
	cur, ok := r.items[a.ID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if cur.Version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
	}
	a.Version = cur.Version + 1
	stored := *a
	r.items[a.ID] = &stored
	out := stored
	return &out, nil
}

func strptr(s string) *string { return &s }

func TestAudienceService_NilRepo_ReturnsServiceUnavailable(t *testing.T) {
	s := NewAudienceService(nil)
	_, err := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})
	var un *audiences.ConnServiceUnavailableError
	if !errors.As(err, &un) {
		t.Errorf("nil repo must return the typed 503, got %T: %v", err, err)
	}
}

func TestAudienceService_CreateMapsInputAndDefaultsStatus(t *testing.T) {
	s := NewAudienceService(newFakeAudienceRepo())
	res, err := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{
			Platform:           "hubspot",
			InclusionSummary:   strptr("attended KubeCon NA 2025"),
			SuppressionListIds: []string{"90", "91"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAudience: %v", err)
	}
	if res.ID == "" || res.ProjectID != "cncf" || res.BriefID != "b1" || res.Platform != "hubspot" {
		t.Errorf("unexpected result: %+v", res)
	}
	// An omitted status defaults to "building".
	if res.Status != string(model.AudienceBuilding) {
		t.Errorf("status = %q, want building", res.Status)
	}
	if res.Etag == nil || *res.Etag != `"1"` || res.Version != 1 {
		t.Errorf("version/etag not set: %+v", res)
	}
	if len(res.SuppressionListIds) != 2 {
		t.Errorf("suppression ids = %v", res.SuppressionListIds)
	}
}

func TestAudienceService_Create_NoActorPersistsNullCreatedBy(t *testing.T) {
	// With no authenticated actor in the context, created_by must be SQL NULL (nil
	// json.RawMessage), NOT the JSONB literal `null` — a typed-nil *model.Actor boxed in
	// `any` slips past marshalAny's == nil guard, so marshalActor guards the pointer.
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, err := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})
	if err != nil {
		t.Fatalf("CreateAudience: %v", err)
	}
	stored := repo.items[created.ID]
	if stored.CreatedBy != nil {
		t.Errorf("created_by must be nil (SQL NULL) with no actor, got JSONB %q", string(stored.CreatedBy))
	}
}

func TestAudienceService_Create_PreservesExplicitStatus(t *testing.T) {
	// An explicit status on create must be preserved, not downgraded to the default.
	// (A built status needs a master-list id or Validate() rejects it, so use that.)
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, err := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built"), PlatformMasterListID: strptr("m-1")},
	})
	if err != nil {
		t.Fatalf("CreateAudience: %v", err)
	}
	if created.Status != string(model.AudienceBuilt) {
		t.Errorf("explicit status downgraded: got %q, want built", created.Status)
	}
}

func TestAudienceService_Update_RequiresAndChecksIfMatch(t *testing.T) {
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})

	// Missing If-Match → 428. (Use a consistent built patch — id present — so the
	// If-Match precondition is what's exercised, not content validation.)
	_, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID,
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built"), PlatformMasterListID: strptr("12345")},
	})
	var preReq *audiences.PreconditionRequiredError
	if !errors.As(err, &preReq) {
		t.Errorf("missing If-Match must be 428, got %T: %v", err, err)
	}

	// Wrong version → 412.
	_, err = s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("99"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built"), PlatformMasterListID: strptr("12345")},
	})
	var preFail *audiences.PreconditionFailedError
	if !errors.As(err, &preFail) {
		t.Errorf("stale If-Match must be 412, got %T: %v", err, err)
	}

	// Correct version → success, version bumps.
	updated, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built"), PlatformMasterListID: strptr("12345")},
	})
	if err != nil {
		t.Fatalf("UpdateAudience: %v", err)
	}
	if updated.Version != 2 || updated.Status != "built" || updated.PlatformMasterListID == nil || *updated.PlatformMasterListID != "12345" {
		t.Errorf("update did not apply: %+v", updated)
	}
}

func TestAudienceService_Update_EmptyPatchRejected(t *testing.T) {
	// An all-omitted patch is a no-op that would still bump version/updated_at and
	// invalidate other clients' ETags — it must be rejected as a 400, not applied.
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})
	_, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{}, // no field set
	})
	var bad *audiences.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("an empty patch must be a 400 BadRequest, got %T: %v", err, err)
	}
	// The version must NOT have been bumped (the no-op write was refused).
	got, _ := s.GetAudience(context.Background(), &audiences.GetAudiencePayload{ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID})
	if got.Version != 1 {
		t.Errorf("a rejected empty patch must not bump the version, got %d", got.Version)
	}
}

func TestAudienceService_Create_BuiltWithoutMasterListRejected(t *testing.T) {
	// status=built means the platform master list exists — creating one with no
	// platform_master_list_id is an inconsistent state and must be a 400.
	s := NewAudienceService(newFakeAudienceRepo())
	_, err := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built")}, // no master list id
	})
	var bad *audiences.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("a built audience with no master-list id must be a 400, got %T: %v", err, err)
	}
}

func TestAudienceService_Update_BuiltInvariantEnforcedAfterMerge(t *testing.T) {
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	// Start from a building audience with no master list id.
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", Audience: &audiences.AudienceInput{Platform: "hubspot"},
	})

	// (a) Patch ONLY status→built on a row with no master-list id → 400 (the merged
	// row would claim a list that doesn't exist).
	_, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built")},
	})
	var bad *audiences.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("status-only built patch on an id-less row must be 400, got %T: %v", err, err)
	}

	// Now legitimately build it (status + id together).
	built, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built"), PlatformMasterListID: strptr("master-1")},
	})
	if err != nil {
		t.Fatalf("building with id must succeed: %v", err)
	}

	// (b) Clearing the master-list id on an already-built row → 400.
	_, err = s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr(strconv.FormatInt(built.Version, 10)),
		Audience: &audiences.AudienceUpdateInput{PlatformMasterListID: strptr("")}, // explicit clear
	})
	if !errors.As(err, &bad) {
		t.Fatalf("clearing the master-list id on a built row must be 400, got %T: %v", err, err)
	}
}

func TestAudienceService_Update_StaleIfMatchIs412NotContent400(t *testing.T) {
	// A patch that is VALID against the version the client fetched, but content-invalid
	// once merged onto a NEWER stored version, must return 412 (stale ETag → refetch),
	// not 400 — the If-Match version is checked against the loaded row before the
	// merge/content validation.
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built"), PlatformMasterListID: strptr("m-1")},
	})
	// v1 = built with id "m-1". A concurrent writer clears the id + reverts to building,
	// producing v2 = building, no id (simulated by mutating the stored row directly).
	stored := repo.items[created.ID]
	stored.PlatformMasterListID = ""
	stored.Status = model.AudienceBuilding
	stored.Version = 2

	// A stale `If-Match: "1"` patch that sets status=built (no id supplied): this WOULD
	// be valid against v1 (which still had the id), but merged onto v2 it has no id.
	// It must be a 412 (stale version), NOT a 400 (content) — the client just needs to
	// refetch v2 and reconsider.
	_, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built")},
	})
	var preFail *audiences.PreconditionFailedError
	if !errors.As(err, &preFail) {
		t.Fatalf("a stale If-Match that's only invalid after merge must be 412, got %T: %v", err, err)
	}
}

// parseAudienceIfMatch is a separate copy of the strong-validator parser (mirroring
// parseBriefIfMatch); the service emits QUOTED ETags, so exercise the full response-to-
// If-Match round trip — bare version, strong quoted entity-tag, surrounding whitespace,
// weak tags, an unbalanced quote, non-numeric input, and missing input — asserting the
// typed error kind (428 vs 400) each case must produce.
func TestParseAudienceIfMatch_StrongValidator(t *testing.T) {
	// Accepted: bare, quoted, and whitespace-padded quoted → the numeric version.
	for in, want := range map[string]int64{`3`: 3, `"3"`: 3, ` "42" `: 42} {
		v, err := parseAudienceIfMatch(&in)
		if err != nil {
			t.Errorf("parseAudienceIfMatch(%q) unexpected error: %v", in, err)
			continue
		}
		if v != want {
			t.Errorf("parseAudienceIfMatch(%q) = %d, want %d", in, v, want)
		}
	}

	// Weak tags and an unbalanced/malformed value are a 400 BadRequest.
	for _, bad := range []string{`W/"3"`, `w/"3"`, `"3`, `3"`, `abc`, `""`} {
		in := bad
		_, err := parseAudienceIfMatch(&in)
		var badReq *audiences.BadRequestError
		if !errors.As(err, &badReq) {
			t.Errorf("parseAudienceIfMatch(%q) = %T, want *BadRequestError", bad, err)
		}
	}

	// Missing input (nil or empty) is a 428 PreconditionRequired.
	for _, name := range []string{"nil", "empty"} {
		var p *string
		if name == "empty" {
			empty := ""
			p = &empty
		}
		_, err := parseAudienceIfMatch(p)
		var preReq *audiences.PreconditionRequiredError
		if !errors.As(err, &preReq) {
			t.Errorf("parseAudienceIfMatch(%s) = %T, want *PreconditionRequiredError", name, err)
		}
	}
}

func TestAudienceService_Get_NotFoundMaps404(t *testing.T) {
	s := NewAudienceService(newFakeAudienceRepo())
	_, err := s.GetAudience(context.Background(), &audiences.GetAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: "missing",
	})
	var nf *audiences.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("a missing audience must map to 404, got %T: %v", err, err)
	}
}

func TestAudienceService_SetBackend_LateBinding(t *testing.T) {
	s := NewAudienceService(nil) // starts unavailable
	if _, err := s.GetAudience(context.Background(), &audiences.GetAudiencePayload{ProjectID: "cncf", BriefID: "b1", AudienceID: "x"}); err == nil {
		t.Fatal("expected 503 before SetBackend")
	}
	s.SetBackend(newFakeAudienceRepo())
	// Now a not-found (404), not a 503 — proves the repo is bound.
	_, err := s.GetAudience(context.Background(), &audiences.GetAudiencePayload{ProjectID: "cncf", BriefID: "b1", AudienceID: "x"})
	var nf *audiences.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("after SetBackend a missing id must be 404 (repo bound), got %T: %v", err, err)
	}
}

func TestAudienceService_Update_MergesOmittedFields(t *testing.T) {
	// An update that only sets status must NOT wipe the previously-set master list id
	// / suppressions / summary — those are preserved by the load-then-merge.
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{
			Platform:             "hubspot",
			PlatformMasterListID: strptr("master-777"),
			SuppressionListIds:   []string{"90"},
			InclusionSummary:     strptr("attended KubeCon"),
		},
	})
	// Update ONLY the status.
	updated, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{Status: strptr("built")},
	})
	if err != nil {
		t.Fatalf("UpdateAudience: %v", err)
	}
	if updated.Status != "built" {
		t.Errorf("status not applied: %+v", updated)
	}
	if updated.PlatformMasterListID == nil || *updated.PlatformMasterListID != "master-777" {
		t.Errorf("master list id was wiped by a status-only update: %+v", updated)
	}
	if updated.InclusionSummary == nil || *updated.InclusionSummary != "attended KubeCon" {
		t.Errorf("inclusion summary was wiped: %+v", updated)
	}
	if len(updated.SuppressionListIds) != 1 {
		t.Errorf("suppression ids were wiped: %v", updated.SuppressionListIds)
	}
}

func TestAudienceService_Update_SuppressionListOps(t *testing.T) {
	repo := newFakeAudienceRepo()
	s := NewAudienceService(repo)
	created, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1",
		Audience: &audiences.AudienceInput{Platform: "hubspot", SuppressionListIds: []string{"90", "91"}},
	})

	// Replace: a non-empty list replaces the set.
	replaced, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{SuppressionListIds: []string{"92"}},
	})
	if err != nil {
		t.Fatalf("replace suppressions: %v", err)
	}
	if len(replaced.SuppressionListIds) != 1 || replaced.SuppressionListIds[0] != "92" {
		t.Errorf("suppressions not replaced: %v", replaced.SuppressionListIds)
	}

	// Clear via the explicit flag (an empty array can't round-trip through the client's
	// omitempty tag, which is why the boolean exists) → empties the set.
	clearTrue := true
	cleared, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr(strconv.FormatInt(replaced.Version, 10)),
		Audience: &audiences.AudienceUpdateInput{ClearSuppressionLists: &clearTrue},
	})
	if err != nil {
		t.Fatalf("clear suppressions: %v", err)
	}
	if len(cleared.SuppressionListIds) != 0 {
		t.Errorf("suppressions not cleared: %v", cleared.SuppressionListIds)
	}

	// clear_suppression_lists=true takes precedence over a supplied list.
	created2, _ := s.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "cncf", BriefID: "b2",
		Audience: &audiences.AudienceInput{Platform: "hubspot", SuppressionListIds: []string{"5"}},
	})
	both, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b2", AudienceID: created2.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceUpdateInput{SuppressionListIds: []string{"6", "7"}, ClearSuppressionLists: &clearTrue},
	})
	if err != nil {
		t.Fatalf("clear+supply: %v", err)
	}
	if len(both.SuppressionListIds) != 0 {
		t.Errorf("clear flag must win over a supplied list, got: %v", both.SuppressionListIds)
	}
}

// TestMapAudienceErr_ConflictReasonsAreDistinctAndStable pins the machine-readable half of
// the three 409s.
//
// All three carry code "409". Before `reason` existed the only thing telling them apart was
// the message, and a caller cannot act on those: "wait for the build that holds the lease"
// and "refresh and rebuild" are opposite instructions, and the prose that expresses them is
// rewritten whenever an operator finds it unclear. This asserts the slugs, so a reworded
// message is free and a renamed slug is a failing test.
//
// It also asserts the three are pairwise distinct — a copy-paste that gave two branches the
// same slug would re-merge exactly the cases the attribute exists to separate, and every
// individual assertion would still pass.
func TestMapAudienceErr_ConflictReasonsAreDistinctAndStable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"stale approval", domain.ErrStaleApproval, "stale_approval"},
		{"lease held", domain.ErrAudienceBuildInFlight, "audience_build_in_flight"},
		{"generic conflict", domain.ErrConflict, "already_exists"},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var conflict *audiences.ConflictError
			if !errors.As(mapAudienceErr(tc.err), &conflict) {
				t.Fatalf("mapAudienceErr(%v) is not a ConflictError", tc.err)
			}
			if conflict.Reason == nil {
				t.Fatalf("no reason on the %s conflict: the caller is back to matching on "+
					"message prose to tell it from the other two 409s", tc.name)
			}
			if *conflict.Reason != tc.want {
				t.Errorf("reason = %q, want %q — this slug is the part clients are promised, "+
					"so renaming it breaks them silently", *conflict.Reason, tc.want)
			}
			if prev, dup := seen[*conflict.Reason]; dup {
				t.Errorf("%s reuses the reason %q already returned for %s, which merges two "+
					"conflicts that call for opposite client behaviour",
					tc.name, *conflict.Reason, prev)
			}
			seen[*conflict.Reason] = tc.name
		})
	}
}
