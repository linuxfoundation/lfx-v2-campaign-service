// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
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
	return a, nil
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
	if res.Etag == nil || *res.Etag != "1" || res.Version != 1 {
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

	// Missing If-Match → 428.
	_, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID,
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built")},
	})
	var preReq *audiences.PreconditionRequiredError
	if !errors.As(err, &preReq) {
		t.Errorf("missing If-Match must be 428, got %T: %v", err, err)
	}

	// Wrong version → 412.
	_, err = s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("99"),
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built")},
	})
	var preFail *audiences.PreconditionFailedError
	if !errors.As(err, &preFail) {
		t.Errorf("stale If-Match must be 412, got %T: %v", err, err)
	}

	// Correct version → success, version bumps.
	updated, err := s.UpdateAudience(context.Background(), &audiences.UpdateAudiencePayload{
		ProjectID: "cncf", BriefID: "b1", AudienceID: created.ID, IfMatch: strptr("1"),
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built"), PlatformMasterListID: strptr("12345")},
	})
	if err != nil {
		t.Fatalf("UpdateAudience: %v", err)
	}
	if updated.Version != 2 || updated.Status != "built" || updated.PlatformMasterListID == nil || *updated.PlatformMasterListID != "12345" {
		t.Errorf("update did not apply: %+v", updated)
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
		Audience: &audiences.AudienceInput{Platform: "hubspot", Status: strptr("built")},
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
