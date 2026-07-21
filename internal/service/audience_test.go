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
}

func newFakeAudienceRepo() *fakeAudienceRepo {
	return &fakeAudienceRepo{items: map[string]*model.CampaignAudience{}}
}

func (r *fakeAudienceRepo) CreateAudience(_ context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	if r.createE != nil {
		return nil, r.createE
	}
	r.seq++
	a.ID = "aud-" + string(rune('a'+r.seq))
	a.Version = 1
	r.items[a.ID] = a
	return a, nil
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
		out = append(out, a)
	}
	return out, nil
}

func (r *fakeAudienceRepo) UpdateAudience(_ context.Context, a *model.CampaignAudience, expectedVersion int64) (*model.CampaignAudience, error) {
	cur, ok := r.items[a.ID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if cur.Version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
	}
	a.Version = cur.Version + 1
	r.items[a.ID] = a
	return a, nil
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
