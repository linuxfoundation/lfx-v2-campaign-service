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

func (r *fakeBriefRepo) Approve(_ context.Context, projectID, id string, _ *model.Actor, expectedVersion int64) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if b.Version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
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

// CreateCampaigns must reject a duplicate platform (400) rather than dispatching
// the same platform twice, which would create two paid upstream campaigns.
func TestBriefService_CreateCampaigns_RejectsDuplicatePlatforms(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefApproved,
	}
	s := newTestBriefService(repo)
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads", "google-ads"}},
	})
	if _, ok := err.(*briefs.BadRequestError); !ok {
		t.Fatalf("expected *briefs.BadRequestError for duplicate platforms, got %T (%v)", err, err)
	}
}

// Create/Get must round-trip the full brief content (event_details, copy,
// keywords, targeting), not drop it from the response.
func TestBriefService_ResponseIncludesBriefContent(t *testing.T) {
	repo := newFakeBriefRepo()
	s := newTestBriefService(repo)
	details := map[string]any{"venue": "Salt Lake City"}
	kw := []any{"kubernetes", "cloud native"}
	created, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief: &briefs.BriefInput{
			ProgramType:  "events",
			EventSlug:    "kubecon-2025",
			EventDetails: details,
			Keywords:     kw,
		},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}
	if created.EventDetails == nil {
		t.Error("create response dropped event_details")
	}
	got, err := s.GetBrief(context.Background(), &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: created.ID})
	if err != nil {
		t.Fatalf("GetBrief: %v", err)
	}
	if got.EventDetails == nil {
		t.Error("get response dropped event_details")
	}
	if got.Keywords == nil {
		t.Error("get response dropped keywords")
	}
}

// ApproveBrief requires an If-Match and is gated on version, so a brief that was
// replaced since the approver fetched it cannot be approved on stale content.
func TestBriefService_ApproveBrief_VersionGated(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefDraft, Version: 3,
	}
	s := newTestBriefService(repo)

	// Missing If-Match -> 428 PreconditionRequired.
	if _, err := s.ApproveBrief(context.Background(), &briefs.ApproveBriefPayload{ProjectID: "cncf", BriefID: "b1"}); err == nil {
		t.Fatal("expected an error when If-Match is missing")
	} else if _, ok := err.(*briefs.PreconditionRequiredError); !ok {
		t.Fatalf("missing If-Match: got %T (%v), want *PreconditionRequiredError", err, err)
	}

	// Stale version -> 412 PreconditionFailed.
	stale := "2"
	if _, err := s.ApproveBrief(context.Background(), &briefs.ApproveBriefPayload{ProjectID: "cncf", BriefID: "b1", IfMatch: &stale}); err == nil {
		t.Fatal("expected an error approving a stale version")
	} else if _, ok := err.(*briefs.PreconditionFailedError); !ok {
		t.Fatalf("stale version: got %T (%v), want *PreconditionFailedError", err, err)
	}

	// Current version -> approved.
	cur := "3"
	got, err := s.ApproveBrief(context.Background(), &briefs.ApproveBriefPayload{ProjectID: "cncf", BriefID: "b1", IfMatch: &cur})
	if err != nil {
		t.Fatalf("approve at current version: %v", err)
	}
	if got.Status != "approved" {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

// parseBriefIfMatch must accept a bare version, a quoted entity-tag, and a weak
// tag; and reject non-numeric input.
func TestParseBriefIfMatch_AcceptsQuotedETag(t *testing.T) {
	cases := map[string]int64{`3`: 3, `"3"`: 3, `W/"3"`: 3, ` "42" `: 42}
	for in, want := range cases {
		v, err := parseBriefIfMatch(&in)
		if err != nil {
			t.Errorf("parseBriefIfMatch(%q) error: %v", in, err)
			continue
		}
		if v != want {
			t.Errorf("parseBriefIfMatch(%q) = %d, want %d", in, v, want)
		}
	}
	bad := `abc`
	if _, err := parseBriefIfMatch(&bad); err == nil {
		t.Errorf("parseBriefIfMatch(%q) = nil error, want BadRequest", bad)
	}
	var nilp *string
	if _, err := parseBriefIfMatch(nilp); err == nil {
		t.Error("parseBriefIfMatch(nil) = nil error, want PreconditionRequired")
	}
}

// campaignEditRepo is a minimal CampaignRepository for UpdateCampaign tests.
type campaignEditRepo struct {
	got *model.Campaign // the campaign passed to ReplaceCampaign
	cur *model.Campaign // the stored campaign returned by GetCampaign
}

func (r *campaignEditRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	cp := *r.cur
	return &cp, nil
}
func (r *campaignEditRepo) GetCampaignByPlatform(context.Context, string, model.Provider) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}
func (r *campaignEditRepo) ClaimCampaignDispatch(context.Context, string, string, model.Provider, string) (bool, *model.Campaign, error) {
	return true, nil, nil
}
func (r *campaignEditRepo) UpsertCampaign(_ context.Context, c *model.Campaign) (*model.Campaign, error) {
	return c, nil
}
func (r *campaignEditRepo) ReplaceCampaign(_ context.Context, c *model.Campaign, _ int64) (*model.Campaign, error) {
	r.got = c
	return c, nil
}

// UpdateCampaign must NOT wipe the stored config when the caller omits config.
func TestBriefService_UpdateCampaign_PreservesConfigWhenOmitted(t *testing.T) {
	camps := &campaignEditRepo{cur: &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Version: 2,
		CampaignName: "old", Status: "active",
		ConfigSnapshot: []byte(`{"budget":100}`),
	}}
	s := &BriefService{briefs: &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}}, campaigns: camps, jobs: newFakeJobRepo(), orch: NewOrchestrator(camps, newFakeJobRepo(), nil)}
	v := "2"
	_, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "new", Status: "paused"}, // Config omitted
	})
	if err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if string(camps.got.ConfigSnapshot) != `{"budget":100}` {
		t.Errorf("config was overwritten: %s, want the stored {\"budget\":100}", camps.got.ConfigSnapshot)
	}
	if camps.got.CampaignName != "new" || camps.got.Status != "paused" {
		t.Errorf("name/status not applied: %q/%q", camps.got.CampaignName, camps.got.Status)
	}
}
