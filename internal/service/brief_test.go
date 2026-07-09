// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeBriefRepo is a minimal in-memory BriefRepository for handler tests.
type fakeBriefRepo struct {
	briefs map[string]*model.CampaignBrief // key: projectID|id
}

func newFakeBriefRepo() *fakeBriefRepo {
	return &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}}
}

func briefKey(projectID, id string) string { return projectID + "|" + id }

func (r *fakeBriefRepo) GetBrief(_ context.Context, projectID, id string) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return b, nil
}

func (r *fakeBriefRepo) CreateBrief(_ context.Context, b *model.CampaignBrief) (*model.CampaignBrief, error) {
	b.ID = "b-new"
	b.Version = 1
	r.briefs[briefKey(b.ProjectID, b.ID)] = b
	return b, nil
}

func (r *fakeBriefRepo) ReplaceBrief(_ context.Context, b *model.CampaignBrief, _ int64) (*model.CampaignBrief, error) {
	r.briefs[briefKey(b.ProjectID, b.ID)] = b
	return b, nil
}

func (r *fakeBriefRepo) Approve(_ context.Context, projectID, id string, _ *model.Actor) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	b.Status = model.BriefApproved
	return b, nil
}

func (r *fakeBriefRepo) ArchiveBrief(_ context.Context, projectID, id string) error {
	if _, ok := r.briefs[briefKey(projectID, id)]; !ok {
		return domain.ErrNotFound
	}
	return nil
}

// A BriefService built with nil repos (DATABASE_URL unset) must return the typed
// 503 ServiceUnavailable for every route rather than panicking on a nil repo, so
// runtime matches the published OpenAPI contract (mirrors the connection service).
func TestBriefService_NilRepo_ReturnsServiceUnavailable(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	ctx := context.Background()

	if _, err := s.GetBrief(ctx, &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: "b1"}); !isBriefUnavailable(err) {
		t.Errorf("GetBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if _, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{ProjectID: "cncf", Brief: &briefs.BriefInput{}}); !isBriefUnavailable(err) {
		t.Errorf("CreateBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if _, err := s.GetJob(ctx, &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"}); !isBriefUnavailable(err) {
		t.Errorf("GetJob: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if err := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{ProjectID: "cncf", BriefID: "b1"}); !isBriefUnavailable(err) {
		t.Errorf("DeleteBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
}

// A missing bearer token is a client-side problem and must map to 400, not 500
// (a 500 misrepresents it as a server fault and can trigger ops alerting).
func TestBriefService_JWTAuth_EmptyTokenIsBadRequest(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	_, err := s.JWTAuth(context.Background(), "", nil)
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("expected *briefs.BadRequestError for empty token, got %T (%v)", err, err)
	}
}

func isBriefUnavailable(err error) bool {
	_, ok := err.(*briefs.ConnServiceUnavailableError)
	return ok
}

func newTestBriefService(repo *fakeBriefRepo) *BriefService {
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, nil)
	return NewBriefService(repo, camps, jobs, orch)
}

func TestBriefService_CreateAndGet_HappyPath(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)
	created, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon-2025"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}
	got, err := s.GetBrief(context.Background(), &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: created.ID})
	if err != nil {
		t.Fatalf("GetBrief: %v", err)
	}
	if got.EventSlug != "kubecon-2025" {
		t.Errorf("event_slug = %q, want kubecon-2025", got.EventSlug)
	}
}

// CreateCampaigns must reject a brief that has not been approved (400), the
// approval-gate invariant from the architecture (a brief must be approved
// before campaigns can be created from it).
func TestBriefService_CreateCampaigns_RejectsUnapprovedBrief(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefDraft,
	}
	s := newTestBriefService(repo)
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads"}},
	})
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("expected *briefs.BadRequestError for unapproved brief, got %T (%v)", err, err)
	}
}

// CreateCampaigns must reject an empty platform set (400) rather than creating a
// no-op job that instantly aggregates to succeeded.
func TestBriefService_CreateCampaigns_RejectsEmptyPlatforms(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefApproved,
	}
	s := newTestBriefService(repo)
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{}},
	})
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("expected *briefs.BadRequestError for empty platforms, got %T (%v)", err, err)
	}
}
