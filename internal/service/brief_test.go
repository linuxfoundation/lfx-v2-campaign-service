// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	briefsclient "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/client"
	briefsserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	goahttp "goa.design/goa/v3/http"
)

// fakeBriefRepo is a minimal in-memory BriefRepository for handler tests.
type fakeBriefRepo struct {
	briefs map[string]*model.CampaignBrief // key: projectID|id
	// onGet, when set, fires once after the next GetBrief read, modelling a
	// concurrent brief mutation that commits in the approve→dispatch window.
	onGet func()
	// lastIndexPayload is the outbox payload built during the last ArchiveBrief.
	lastIndexPayload []byte
	// indexPayloads records EVERY co-committed message, so a test can assert that a write was
	// indexed at all rather than only inspecting the most recent one.
	indexPayloads [][]byte
}

func newFakeBriefRepo() *fakeBriefRepo {
	return &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}}
}

func briefKey(projectID, id string) string { return projectID + "|" + id }

// FindBriefByEventSlug scans for a non-archived brief matching the slug, mirroring the
// repo's partial-unique-index semantics (archived rows free the slug).
func (r *fakeBriefRepo) FindBriefByEventSlug(_ context.Context, projectID, eventSlug string) (*model.CampaignBrief, error) {
	for _, b := range r.briefs {
		if b.ProjectID == projectID && b.EventSlug == eventSlug && b.Status != model.BriefArchived {
			cp := *b
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (r *fakeBriefRepo) GetBrief(_ context.Context, projectID, id string) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	// Return a snapshot copy so a caller holding the result observes the state as
	// of the read, even if the stored brief is subsequently mutated (used to model
	// a concurrent replace committing in the approve→dispatch window).
	cp := *b
	// onGet, if set, fires after the read to simulate a concurrent mutation
	// committing between this read and a later guarded write.
	if r.onGet != nil {
		hook := r.onGet
		r.onGet = nil // one-shot
		hook()
	}
	return &cp, nil
}

func (r *fakeBriefRepo) CreateBrief(_ context.Context, b *model.CampaignBrief, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	b.ID = "b-new"
	b.Version = 1
	r.briefs[briefKey(b.ProjectID, b.ID)] = b
	return b, r.enqueue(b, indexPayload)
}

func (r *fakeBriefRepo) ReplaceBrief(_ context.Context, b *model.CampaignBrief, _ int64, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	r.briefs[briefKey(b.ProjectID, b.ID)] = b
	return b, r.enqueue(b, indexPayload)
}

func (r *fakeBriefRepo) Approve(_ context.Context, projectID, id string, _ *model.Actor, expectedVersion int64, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	if b.Version != expectedVersion {
		return nil, domain.ErrPreconditionFailed
	}
	b.Status = model.BriefApproved
	return b, r.enqueue(b, indexPayload)
}

// ArchiveBrief mirrors the real repo's single-statement semantics: it applies the archive AND
// returns the committed row. Modelling the mutation (status + version bump) matters — a fake
// that only reported success would let a caller publishing a stale snapshot pass.
func (r *fakeBriefRepo) ArchiveBrief(_ context.Context, projectID, id string, indexPayload domain.IndexPayloadFunc) (*model.CampaignBrief, error) {
	b, ok := r.briefs[briefKey(projectID, id)]
	if !ok || b.Status == model.BriefArchived {
		// The real query guards on status <> 'archived', so a second archive is ErrNotFound.
		return nil, domain.ErrNotFound
	}
	b.Status = model.BriefArchived
	b.Version++
	return b, r.enqueue(b, indexPayload)
}

// enqueue mirrors the real repo's co-commit: the payload builder runs INSIDE the "transaction",
// after the row is updated, and a build error fails the write. Recording every payload is what
// lets tests assert that a mutation is indexed at all — the whole point of routing writes
// through the outbox is that no write publishes outside it.
func (r *fakeBriefRepo) enqueue(b *model.CampaignBrief, indexPayload domain.IndexPayloadFunc) error {
	if indexPayload == nil {
		return nil
	}
	payload, err := indexPayload(b)
	if err != nil {
		return err
	}
	r.lastIndexPayload = payload
	r.indexPayloads = append(r.indexPayloads, payload)
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
	// FindBrief must 503 like every other route during a cold start. Without this, a refactor
	// touching only the lookup could drop its ready() call and start returning 404 instead —
	// telling the caller "no brief exists" when the truth is "the database is not up yet",
	// which for this endpoint means silently regenerating a brief that already exists.
	if _, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "kubecon-eu-2026"}); !isBriefUnavailable(err) {
		t.Errorf("FindBrief: expected *briefs.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
}

// TestBriefService_SetBackend_LateBinding verifies the container can inject the
// repos + orchestrator after construction (the DB cold-start retry path): a service
// booted with nil repos returns 503, and once SetBackend injects live collaborators
// the same call succeeds — without rebuilding the service (its routes are already
// mounted). This is the fix for "briefs stay broken after DB retry".
func TestBriefService_SetBackend_LateBinding(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)
	ctx := context.Background()

	// Before the pool is ready: brief + job routes return 503.
	if _, err := s.GetBrief(ctx, &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: "b1"}); !isBriefUnavailable(err) {
		t.Fatalf("expected 503 before backend is set, got %T (%v)", err, err)
	}
	if _, err := s.GetJob(ctx, &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"}); !isBriefUnavailable(err) {
		t.Fatalf("GetJob: expected 503 before backend is set, got %T (%v)", err, err)
	}
	if _, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "kubecon-eu-2026"}); !isBriefUnavailable(err) {
		t.Fatalf("FindBrief: expected 503 before backend is set, got %T (%v)", err, err)
	}

	// Inject live collaborators (as the background DB-init goroutine does).
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, nil)
	s.SetBackend(repo, camps, jobs, orch)

	// After the swap: the repo is consulted; a missing brief is NotFound, NOT 503 —
	// proving the backend went live without a pod restart.
	if _, err := s.GetBrief(ctx, &briefs.GetBriefPayload{ProjectID: "cncf", BriefID: "missing"}); isBriefUnavailable(err) {
		t.Fatalf("expected the live repo to be consulted after SetBackend, still got 503")
	}
	if _, err := s.GetJob(ctx, &briefs.GetJobPayload{ProjectID: "cncf", JobID: "missing"}); isBriefUnavailable(err) {
		t.Fatalf("GetJob: expected the live repo after SetBackend, still got 503")
	}
	// A slug with no saved brief is the ordinary first-generation case: NotFound, not 503.
	if _, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "brand-new-event"}); isBriefUnavailable(err) {
		t.Fatalf("FindBrief: expected the live repo after SetBackend, still got 503")
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
// CreateBrief and CreateCampaigns must reject a project_id that is a UUID (or any
// non-slug value): it is stamped into the campaign-name Project segment, and a UUID
// there breaks the data-pipeline's slug-based attribution join. Goa does not enforce
// path-param patterns, so this is guarded app-side.
func TestBriefService_CreateBriefAndCampaigns_RejectNonSlugProjectID(t *testing.T) {
	uuid := "a09410d0-0ec0-11ea-8e8f-416e2d8da950"
	for _, bad := range []string{uuid, "CNCF", "cncf_x", "-cncf", "with space", "foo--bar", "cncf-"} {
		s := newTestBriefService(newFakeBriefRepo())
		if _, err := s.CreateBrief(context.Background(), &briefs.CreateBriefPayload{
			ProjectID: bad, Brief: &briefs.BriefInput{ProgramType: "events", EventSlug: "kubecon"},
		}); err == nil {
			t.Errorf("CreateBrief must reject non-slug project_id %q", bad)
		} else if _, ok := err.(*briefs.BadRequestError); !ok {
			t.Errorf("CreateBrief(%q) error = %T, want *BadRequestError", bad, err)
		}
		if _, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
			ProjectID: bad, BriefID: "b1", Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads"}},
		}); err == nil {
			t.Errorf("CreateCampaigns must reject non-slug project_id %q", bad)
		} else if _, ok := err.(*briefs.BadRequestError); !ok {
			t.Errorf("CreateCampaigns(%q) error = %T, want *BadRequestError", bad, err)
		}
	}
	// A valid slug must PASS the slug guard (it fails later for a different reason —
	// unknown brief — proving the guard itself didn't reject it).
	s := newTestBriefService(newFakeBriefRepo())
	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "nope", Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads"}},
	})
	if err != nil && strings.Contains(err.Error(), "canonical") {
		t.Errorf("a valid slug must pass the slug guard, got %v", err)
	}
}

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

// parseBriefIfMatch accepts a bare version and a strong quoted entity-tag, and
// rejects non-numeric input AND weak validators (If-Match requires strong
// comparison per RFC 7232).
func TestParseBriefIfMatch_AcceptsQuotedETag(t *testing.T) {
	cases := map[string]int64{`3`: 3, `"3"`: 3, ` "42" `: 42}
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
	for _, weak := range []string{`W/"3"`, `w/"3"`} {
		if _, err := parseBriefIfMatch(&weak); err == nil {
			t.Errorf("parseBriefIfMatch(%q) = nil error, want weak-tag rejection", weak)
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

// versionGuardedJobRepo models the atomic approve-re-check that
// CreateJobForApprovedBrief performs in SQL: it creates a job only if the brief
// in the shared store is still 'approved' at the expected version. It lets a test
// exercise the approve→dispatch TOCTOU guard without a real database.
type versionGuardedJobRepo struct {
	fakeJobRepo
	store *fakeBriefRepo
}

func (r *versionGuardedJobRepo) CreateJobForApprovedBrief(_ context.Context, briefID string, expectedVersion int64) (*model.CampaignJob, error) {
	// Re-verify approval atomically with the create, mirroring the real repo's
	// SELECT ... FOR UPDATE re-check inside the job-insert transaction: any
	// concurrent replace/archive that bumped the version or changed the status
	// fails the guard.
	var stillApproved bool
	for _, b := range r.store.briefs {
		if b.ID == briefID && b.Status == model.BriefApproved && b.Version == expectedVersion {
			stillApproved = true
			break
		}
	}
	if !stillApproved {
		// Mirror the real repo: the approve→dispatch guard fires with the
		// state-conflict sentinel (not the generic uniqueness ErrConflict).
		return nil, domain.ErrStaleApproval
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(briefID)
}

// TestBriefService_CreateCampaigns_TOCTOURaceFailsClosed verifies the
// approve→dispatch race is closed: if a concurrent replace resets the brief to
// draft (and bumps its version) AFTER CreateCampaigns reads it as approved but
// BEFORE the job is created, the request FAILS (409 conflict) rather than
// launching paid campaigns from the stale "approved" snapshot.
func TestBriefService_CreateCampaigns_TOCTOURaceFailsClosed(t *testing.T) {
	repo := newFakeBriefRepo()
	// The brief is approved at version 5 when CreateCampaigns reads it.
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefApproved, Version: 5,
	}
	camps := &fakeCampaignRepo{}
	jobs := &versionGuardedJobRepo{
		fakeJobRepo: fakeJobRepo{jobs: map[string]*model.CampaignJob{}},
		store:       repo,
	}
	orch := NewOrchestrator(camps, jobs, nil)
	s := NewBriefService(repo, camps, jobs, orch)

	// Model the concurrent replace committing in the window: it fires AFTER
	// CreateCampaigns reads the brief as approved@v5 (GetBrief returns a snapshot)
	// but BEFORE the guarded job create re-checks the store. It resets the brief to
	// draft and bumps the version, exactly as ReplaceBrief does, so the guarded
	// create must now fail (409) rather than dispatch from the stale snapshot.
	repo.onGet = func() {
		repo.briefs[briefKey("cncf", "b1")].Status = model.BriefDraft
		repo.briefs[briefKey("cncf", "b1")].Version = 6
	}

	_, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads"}},
	})
	ce, ok := err.(*briefs.ConflictError)
	if !ok {
		t.Fatalf("expected *briefs.ConflictError (concurrent replace closed the TOCTOU race), got %T (%v)", err, err)
	}
	// The message must describe the version/approval conflict accurately (refresh
	// and re-approve), NOT the misleading uniqueness "already exists" — a client
	// needs to know to re-approve and retry.
	if !strings.Contains(ce.Message, "no longer approved") || !strings.Contains(ce.Message, "re-approve") {
		t.Errorf("conflict message = %q, want it to describe the stale-approval remedy (re-approve and retry)", ce.Message)
	}
	if strings.Contains(ce.Message, "already exists") {
		t.Errorf("conflict message = %q, must not misdescribe a version conflict as a uniqueness one", ce.Message)
	}
	if len(jobs.jobs) != 0 {
		t.Errorf("a job was created despite the brief no longer being approved: %d jobs", len(jobs.jobs))
	}
}

// TestBriefService_CreateCampaigns_ApprovedAtVersionSucceeds verifies the guard
// does not over-reject: when the brief is still approved at the read version, the
// job is created normally.
func TestBriefService_CreateCampaigns_ApprovedAtVersionSucceeds(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefApproved, Version: 5,
	}
	camps := &fakeCampaignRepo{}
	jobs := &versionGuardedJobRepo{
		fakeJobRepo: fakeJobRepo{jobs: map[string]*model.CampaignJob{}},
		store:       repo,
	}
	orch := NewOrchestrator(camps, jobs, nil)
	s := NewBriefService(repo, camps, jobs, orch)

	resp, err := s.CreateCampaigns(context.Background(), &briefs.CreateCampaignsPayload{
		ProjectID: "cncf", BriefID: "b1",
		Input: &briefs.CampaignCreateInput{Platforms: []string{"google-ads"}},
	})
	if err != nil {
		t.Fatalf("CreateCampaigns: %v", err)
	}
	if resp.JobID == "" {
		t.Error("expected a job id for a brief still approved at the read version")
	}
}

// campaignEditRepo is a minimal CampaignRepository for UpdateCampaign tests.
type campaignEditRepo struct {
	got *model.Campaign // the campaign passed to ReplaceCampaign
	cur *model.Campaign // the stored campaign returned by GetCampaign
	// indexPayloads records the co-committed index messages, so a test can assert the update
	// is indexed rather than only persisted.
	indexPayloads [][]byte
}

func (r *campaignEditRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	cp := *r.cur
	return &cp, nil
}
func (r *campaignEditRepo) GetCampaignByPlatform(context.Context, string, string, model.Provider) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}
func (r *campaignEditRepo) ClaimCampaignDispatch(context.Context, string, string, model.Provider, string) (bool, *model.Campaign, error) {
	return true, nil, nil
}
func (r *campaignEditRepo) DeleteDispatchClaim(context.Context, string, model.Provider) error {
	return nil
}
func (r *campaignEditRepo) UpsertCampaign(_ context.Context, c *model.Campaign, _ domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return c, nil
}
func (r *campaignEditRepo) ReplaceCampaign(_ context.Context, c *model.Campaign, _ int64, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	r.got = c
	return c, r.recordIndex(c, indexPayload)
}

// recordIndex runs the payload builder the way the real repo does — inside the "transaction",
// after the row is written — so a test can assert the update is actually indexed.
func (r *campaignEditRepo) recordIndex(c *model.Campaign, indexPayload domain.CampaignIndexPayloadFunc) error {
	if indexPayload == nil {
		return nil
	}
	payload, err := indexPayload(c)
	if err != nil {
		return err
	}
	r.indexPayloads = append(r.indexPayloads, payload)
	return nil
}
func (r *campaignEditRepo) DeleteCampaign(_ context.Context, _ string, _ string, _ string, _ int64, indexPayload domain.CampaignIndexPayloadFunc) error {
	if indexPayload != nil {
		// Record that the deletion was indexed, matching the real repo's behavior.
		payload, _ := indexPayload(&model.Campaign{})
		r.indexPayloads = append(r.indexPayloads, payload)
	}
	return nil
}

// UpdateCampaign must NOT wipe the stored config when the caller omits config, and it must
// leave the run status untouched (the caller round-trips the CURRENT status on a name edit;
// run-state changes go through the toggle, not this DB-only path).
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
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "new", Status: "active"}, // status unchanged; Config omitted
	})
	if err != nil {
		t.Fatalf("UpdateCampaign: %v", err)
	}
	if string(camps.got.ConfigSnapshot) != `{"budget":100}` {
		t.Errorf("config was overwritten: %s, want the stored {\"budget\":100}", camps.got.ConfigSnapshot)
	}
	if camps.got.CampaignName != "new" {
		t.Errorf("name not applied: %q", camps.got.CampaignName)
	}
	if camps.got.Status != "active" {
		t.Errorf("status must be preserved verbatim by the DB-only update, got %q want %q", camps.got.Status, "active")
	}
}

// UpdateCampaign must REFUSE a run-status change (active<->paused): persisting a run state
// without contacting the ad platform would recreate the DB/platform divergence the toggle
// endpoint exists to prevent. The refusal is a 400 that routes the caller to the toggle.
func TestBriefService_UpdateCampaign_RejectsRunStatusChange(t *testing.T) {
	camps := &campaignEditRepo{cur: &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Version: 2,
		CampaignName: "old", Status: "active",
	}}
	s := &BriefService{briefs: &fakeBriefRepo{briefs: map[string]*model.CampaignBrief{}}, campaigns: camps, jobs: newFakeJobRepo(), orch: NewOrchestrator(camps, newFakeJobRepo(), nil)}
	v := "2"
	_, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &v,
		Campaign: &briefs.CampaignUpdateInput{CampaignName: "old", Status: "paused"}, // active -> paused via DB path
	})
	var badReq *briefs.BadRequestError
	if !errors.As(err, &badReq) {
		t.Fatalf("a run-status change via update-campaign must be a 400 BadRequestError, got %T: %v", err, err)
	}
	if camps.got != nil {
		t.Error("ReplaceCampaign must NOT be called when the run-status change is rejected")
	}
}

// getJobTestService builds a BriefService whose job repo returns the given job
// verbatim from GetJob, for exercising the poll-response decoding path.
func getJobTestService(job *model.CampaignJob) *BriefService {
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	jobs.jobs[job.ID] = job
	orch := NewOrchestrator(camps, jobs, nil)
	return NewBriefService(repo, camps, jobs, orch)
}

// GetJob must NOT silently discard a persisted result that won't decode: a
// malformed result on a terminal succeeded/partial job would otherwise return a
// 200 poll response with NO per-platform results, masking corruption as success.
// It must surface a 500 InternalServerError instead.
func TestBriefService_GetJob_MalformedResultIsInternalError(t *testing.T) {
	s := getJobTestService(&model.CampaignJob{
		ID: "j1", BriefID: "b1", Status: model.JobSucceeded,
		Result: []byte(`{"not":"an array"}`), // valid JSON, wrong shape → unmarshal error
	})
	_, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
	if _, ok := err.(*briefs.InternalServerError); !ok {
		t.Fatalf("expected *briefs.InternalServerError for a malformed job result, got %T (%v)", err, err)
	}
}

// A terminal succeeded/partial job is an aggregate over per-platform outcomes, so
// an empty/absent result on that status means the row is corrupt. GetJob must
// surface a 500 rather than a 200 with no results (which would misrepresent
// corruption as a successful dispatch).
func TestBriefService_GetJob_TerminalWithoutResultsIsInternalError(t *testing.T) {
	for _, st := range []model.JobStatus{model.JobSucceeded, model.JobPartial} {
		s := getJobTestService(&model.CampaignJob{ID: "j1", BriefID: "b1", Status: st, Result: nil})
		_, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
		if _, ok := err.(*briefs.InternalServerError); !ok {
			t.Fatalf("status %s: expected *briefs.InternalServerError for a terminal job with no results, got %T (%v)", st, err, err)
		}
	}
}

// A succeeded/partial job whose stored Result is the raw JSON null or [] decodes
// to an EMPTY slice with len(j.Result) > 0, so it slips past the outer length
// guard. GetJob must still treat this as corruption on a terminal-aggregate status
// and surface a 500, not a 200 with zero per-platform results.
func TestBriefService_GetJob_EmptyDecodedResultIsInternalError(t *testing.T) {
	for _, raw := range []string{`null`, `[]`} {
		for _, st := range []model.JobStatus{model.JobSucceeded, model.JobPartial} {
			s := getJobTestService(&model.CampaignJob{
				ID: "j1", BriefID: "b1", Status: st, Result: []byte(raw),
			})
			_, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
			if _, ok := err.(*briefs.InternalServerError); !ok {
				t.Fatalf("raw %q status %s: expected *briefs.InternalServerError for an empty decoded result on a terminal job, got %T (%v)", raw, st, err, err)
			}
		}
	}
}

// The orchestrator persists a skipped platform with BOTH skipped=true AND a raw
// internal error string ("skipped: another concurrent dispatch owns..."). GetJob
// must check the skipped case before the error case so it surfaces the explicit
// "not a failure" message rather than leaking the internal string.
func TestBriefService_GetJob_SkippedWithInternalErrorSurfacesNonFailure(t *testing.T) {
	s := getJobTestService(&model.CampaignJob{
		ID: "j1", BriefID: "b1", Status: model.JobSucceeded,
		Result: []byte(`[{"platform":"google-ads","ok":true,"campaign_id":"pc-1"},{"platform":"linkedin-ads","ok":false,"skipped":true,"error":"skipped: another concurrent dispatch owns this platform"}]`),
	})
	resp, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var li *briefs.PlatformResult
	for _, r := range resp.Result {
		if r.Platform == "linkedin-ads" {
			li = r
		}
	}
	if li == nil {
		t.Fatal("linkedin-ads result missing")
	}
	if li.Error == nil || !strings.Contains(*li.Error, "not a failure") {
		t.Errorf("skipped platform must surface the friendly non-failure message, got %v", li.Error)
	}
	if li.Error != nil && strings.Contains(*li.Error, "another concurrent dispatch owns") {
		t.Errorf("skipped platform leaked the internal error string: %v", *li.Error)
	}
}

// A valid persisted result decodes into typed per-platform results, and a failed
// job carrying only an error (no results — a legitimate finalize-marshal-failure
// outcome) is NOT treated as corruption: it returns 200 with the error surfaced.
func TestBriefService_GetJob_ValidResultsAndFailedErrorOnly(t *testing.T) {
	// Valid result on a succeeded job round-trips into typed results.
	s := getJobTestService(&model.CampaignJob{
		ID: "j1", BriefID: "b1", Status: model.JobSucceeded,
		Result: []byte(`[{"platform":"google-ads","ok":true,"campaign_id":"pc-1"}]`),
	})
	resp, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
	if err != nil {
		t.Fatalf("GetJob (valid result): %v", err)
	}
	if len(resp.Result) != 1 || resp.Result[0].Platform != "google-ads" || !resp.Result[0].OK {
		t.Errorf("decoded result = %+v, want one ok google-ads result", resp.Result)
	}

	// A failed job with only an error message (no results) is a legitimate outcome
	// (e.g. the finalize marshal failed), not corruption → 200 with the error.
	s2 := getJobTestService(&model.CampaignJob{
		ID: "j2", BriefID: "b1", Status: model.JobFailed, Result: nil, Error: "failed to serialize job result",
	})
	resp2, err := s2.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j2"})
	if err != nil {
		t.Fatalf("GetJob (failed, error-only): %v", err)
	}
	if resp2.Error == nil || *resp2.Error != "failed to serialize job result" {
		t.Errorf("failed job error = %v, want the surfaced error message", resp2.Error)
	}
}

// TestBriefService_GetJob_SkippedSurfacesNonFailure verifies a skipped platform
// (ok=false, skipped=true) on a succeeded job is surfaced with an explicit
// non-failure message rather than an unexplained ok=false that reads as a failure.
func TestBriefService_GetJob_SkippedSurfacesNonFailure(t *testing.T) {
	s := getJobTestService(&model.CampaignJob{
		ID: "j1", BriefID: "b1", Status: model.JobSucceeded,
		Result: []byte(`[{"platform":"google-ads","ok":true,"campaign_id":"pc-1"},{"platform":"linkedin-ads","ok":false,"skipped":true}]`),
	})
	resp, err := s.GetJob(context.Background(), &briefs.GetJobPayload{ProjectID: "cncf", JobID: "j1"})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var li *briefs.PlatformResult
	for _, r := range resp.Result {
		if r.Platform == "linkedin-ads" {
			li = r
		}
	}
	if li == nil {
		t.Fatal("linkedin-ads result missing")
	}
	if li.OK {
		t.Errorf("skipped platform OK = true, want false")
	}
	if li.Error == nil || !strings.Contains(*li.Error, "skipped") || !strings.Contains(*li.Error, "not a failure") {
		t.Errorf("skipped platform must surface a non-failure 'skipped' message, got %v", li.Error)
	}
}

// --- status toggle -----------------------------------------------------------

// toggleCampaignRepo is a minimal CampaignRepository for the toggle handler: it serves one
// campaign from GetCampaign and records the ReplaceCampaign it receives.
type toggleCampaignRepo struct {
	fakeCampaignRepo
	got      *model.Campaign
	replaced *model.Campaign
	getErr   error
	// indexPayloads records the co-committed index messages for the toggle path.
	indexPayloads [][]byte
}

func (r *toggleCampaignRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	cp := *r.got
	return &cp, nil
}
func (r *toggleCampaignRepo) ReplaceCampaign(_ context.Context, c *model.Campaign, _ int64, indexPayload domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	r.replaced = c
	if indexPayload == nil {
		return c, nil
	}
	payload, err := indexPayload(c)
	if err != nil {
		return nil, err
	}
	r.indexPayloads = append(r.indexPayloads, payload)
	return c, nil
}

// stubToggler implements PlatformDispatcher + StatusToggler, recording the toggle call.
type stubToggler struct {
	err     error
	gotID   string
	gotStat string
}

func (d *stubToggler) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("unused")
}
func (d *stubToggler) ToggleStatus(_ context.Context, _ string, _ model.Provider, campaign *model.Campaign, status string) error {
	if campaign != nil {
		d.gotID = campaign.PlatformCampaignID
	}
	d.gotStat = status
	return d.err
}

func newToggleService(camp *model.Campaign, tog *stubToggler) (*BriefService, *toggleCampaignRepo) {
	repo := newFakeBriefRepo()
	camps := &toggleCampaignRepo{got: camp}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: tog})
	s := NewBriefService(repo, camps, jobs, orch)
	// A real (non-Noop) publisher: a Noop service deliberately enqueues nothing, which is the
	// disabled-deployment path rather than the behaviour these tests exercise.
	s.SetIndexer(&failingIndexer{})
	orch.SetIndexer(&failingIndexer{})
	return s, camps
}

func TestBriefService_ToggleCampaignStatus_PlatformThenPersist(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 3,
	}
	tog := &stubToggler{}
	s, camps := newToggleService(camp, tog)
	im := "3"
	res, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	if err != nil {
		t.Fatalf("ToggleCampaignStatus: %v", err)
	}
	// The platform was called with the stored upstream id + requested status.
	if tog.gotID != "t3_c" || tog.gotStat != model.CampaignRunPaused {
		t.Errorf("platform toggle got (%q,%q), want (t3_c,paused)", tog.gotID, tog.gotStat)
	}
	// The row was persisted with the new status AFTER the platform confirmed.
	if camps.replaced == nil || camps.replaced.Status != model.CampaignRunPaused {
		t.Errorf("persisted status = %+v, want paused", camps.replaced)
	}
	if res.Status != model.CampaignRunPaused {
		t.Errorf("result status = %q, want paused", res.Status)
	}
}

func TestBriefService_ToggleCampaignStatus_PlatformFailLeavesRowUntouched(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 1,
	}
	tog := &stubToggler{err: errors.New("reddit 500")}
	s, camps := newToggleService(camp, tog)
	im := "1"
	if _, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	}); err == nil {
		t.Fatal("expected an error when the platform rejects the toggle")
	}
	// Critically: the DB row must NOT be updated when the platform call failed.
	if camps.replaced != nil {
		t.Errorf("row was persisted despite a platform failure: %+v", camps.replaced)
	}
}

func TestBriefService_ToggleCampaignStatus_StaleIfMatchSkipsPlatform(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 5,
	}
	tog := &stubToggler{}
	s, _ := newToggleService(camp, tog)
	im := "2" // stale (row is version 5)
	if _, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunActive,
	}); err == nil {
		t.Fatal("expected a precondition error on a stale If-Match")
	}
	if tog.gotID != "" {
		t.Error("a stale If-Match must fail BEFORE the platform is called")
	}
}

func TestBriefService_ToggleCampaignStatus_NotProvisionedIs409(t *testing.T) {
	// A campaign with no upstream platform id (still creating / ambiguous create) must be a
	// 409 client error, NOT a 503 "platform rejected" — the platform is never called.
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "", Status: "pending", Version: 1,
	}
	tog := &stubToggler{}
	s, camps := newToggleService(camp, tog)
	im := "1"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a ConflictError (409) for an unprovisioned campaign, got %T: %v", err, err)
	}
	if tog.gotID != "" {
		t.Error("the platform must NOT be called for an unprovisioned campaign")
	}
	if camps.replaced != nil {
		t.Error("the row must not be modified for an unprovisioned campaign")
	}
}

// unconfirmedErr implements the Unconfirmed() behavioral interface the toggle handler
// checks (mirrors the dispatch layer's unconfirmedToggleError).
type unconfirmedErr struct{}

func (unconfirmedErr) Error() string     { return "unconfirmed" }
func (unconfirmedErr) Unconfirmed() bool { return true }

func TestBriefService_ToggleCampaignStatus_UnconfirmedIsSurfaced(t *testing.T) {
	// An UNCONFIRMED platform outcome (the PATCH may have applied) must be reported as
	// unconfirmed (verify-before-retry), NOT as a flat "not modified", and must NOT write
	// the DB row (it might already be right, or not).
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: "created", Version: 1,
	}
	tog := &stubToggler{err: unconfirmedErr{}}
	s, camps := newToggleService(camp, tog)
	im := "1"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	su, ok := err.(*briefs.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("expected a 503 ConnServiceUnavailableError, got %T: %v", err, err)
	}
	if !strings.Contains(su.Message, "unconfirmed") {
		t.Errorf("message = %q, want it to say the change is unconfirmed", su.Message)
	}
	if camps.replaced != nil {
		t.Error("the row must NOT be written on an unconfirmed outcome")
	}
}

func TestBriefService_ToggleCampaignStatus_DegradedNotToggleable(t *testing.T) {
	// A created_degraded campaign WITH a real upstream id must NOT be toggled: toggling
	// would activate an incomplete campaign and overwrite the reconciliation marker. It is
	// a 409, the platform is never called, and the row is untouched.
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: model.CampaignStatusCreatedDegraded, Version: 1,
	}
	tog := &stubToggler{}
	s, camps := newToggleService(camp, tog)
	im := "1"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a 409 ConflictError for a degraded campaign, got %T: %v", err, err)
	}
	if tog.gotID != "" {
		t.Error("the platform must NOT be called for a non-toggleable (degraded) campaign")
	}
	if camps.replaced != nil {
		t.Error("the row (and its degraded reconciliation marker) must NOT be overwritten")
	}
}

func TestBriefService_ToggleCampaignStatus_PendingWithIDNotToggleable(t *testing.T) {
	// A pending ambiguous orphan can carry a non-empty upstream id (campaign POST succeeded,
	// a later step failed). The toggle must reject it (409), not activate the incomplete
	// campaign — the empty-id check alone would miss this.
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
		PlatformCampaignID: "t3_c", Status: model.CampaignStatusPending, Version: 1,
	}
	tog := &stubToggler{}
	s, _ := newToggleService(camp, tog)
	im := "1"
	_, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunActive,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a 409 for a pending campaign with an id, got %T: %v", err, err)
	}
	if tog.gotID != "" {
		t.Error("the platform must NOT be called for a pending campaign")
	}
}

// newIndexTestBriefService wires a BriefService with live collaborators around repo.
func newIndexTestBriefService(repo *fakeBriefRepo) *BriefService {
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	s := NewBriefService(nil, nil, nil, nil)
	orch := NewOrchestrator(camps, jobs, nil)
	s.SetBackend(repo, camps, jobs, orch)
	// Inject a real (non-Noop) publisher: these tests assert INDEXING behaviour, and a service
	// left on the default Noop deliberately enqueues nothing — that is the disabled-deployment
	// path (NATS_URL=""), not the one under test here.
	s.SetIndexer(&failingIndexer{})
	orch.SetIndexer(&failingIndexer{})
	return s
}

// failingIndexer records calls and models a publisher whose delivery fails. Publish has no
// error return by contract, so a real failure is invisible to the caller — which is exactly
// the property under test.
type failingIndexer struct{ calls int }

func (f *failingIndexer) Publish(context.Context, indexer.Transaction) { f.calls++ }
func (f *failingIndexer) Close()                                       {}

// TestCreateBrief_IndexingNeverFailsTheWrite pins the contract that a broker problem can never
// become a write failure: the database is the source of truth, and indexing costs
// discoverability at worst. If indexing could fail a write, a NATS outage would become a
// campaign-service outage.
//
// The MECHANISM changed with the outbox — a create no longer publishes directly at all, it
// co-commits an outbox row that the relay delivers — so the assertion is now that the write
// succeeds and enqueued its message while the broker is failing, and that NOTHING was published
// on the request path. That is stronger than the old "exactly one publish": a direct publish
// here would be the ordering hazard the outbox exists to remove.
func TestCreateBrief_IndexingNeverFailsTheWrite(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo)
	idx := &failingIndexer{}
	s.SetIndexer(idx)

	res, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("a broken indexer must not fail the write: %v", err)
	}
	if res == nil {
		t.Fatal("expected the created brief to be returned")
	}
	if len(repo.indexPayloads) != 1 {
		t.Fatalf("expected the create to co-commit exactly one index message, got %d", len(repo.indexPayloads))
	}
	if idx.calls != 0 {
		t.Errorf("a brief write must publish only via the outbox relay, got %d direct publishes", idx.calls)
	}
	// The co-committed message must describe a CREATE of this brief, not merely exist.
	var msg struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(repo.indexPayloads[0], &msg); err != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", err)
	}
	if msg.Action != indexer.ActionCreated {
		t.Errorf("action = %q, want %q", msg.Action, indexer.ActionCreated)
	}
}

// TestBriefService_DefaultsToNoopIndexer guards the nil path: every write calls publishIndex
// unconditionally, so a service constructed without SetIndexer must not panic.
func TestBriefService_DefaultsToNoopIndexer(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo) // no SetIndexer call

	if _, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "no-indexer"},
	}); err != nil {
		t.Fatalf("a service with no indexer configured must still write: %v", err)
	}
}

// TestDeleteBrief_CoCommitsAnArchivedTombstone pins the archive path against leaving a stale
// search document. Archiving is a SOFT delete: if it doesn't emit an index event the brief
// keeps its pre-archive _source and goes on matching searches forever, and archiving is
// TERMINAL — there is no later write to repair it.
//
// The message is now CO-COMMITTED to the outbox rather than published on the request path, so
// the assertions read the enqueued payload. Everything the old direct-publish test pinned still
// holds: the action must be a DELETE (republishing as an update leaves the document findable),
// the data must be the bare object id (the indexer type-asserts delete data to a string and
// rejects an object with "expected string"), and the object type must route to the brief index.
func TestDeleteBrief_CoCommitsAnArchivedTombstone(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo)

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// A failing indexer proves the tombstone does not depend on the broker being up: the row
	// is what carries it, and the relay delivers it later.
	idx := &failingIndexer{}
	s.SetIndexer(idx)
	before := len(repo.indexPayloads)

	if err := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); err != nil {
		t.Fatalf("DeleteBrief: %v", err)
	}

	if len(repo.indexPayloads) != before+1 {
		t.Fatalf("archiving must co-commit exactly one index message, got %d (a stale search document survives)",
			len(repo.indexPayloads)-before)
	}
	if idx.calls != 0 {
		t.Errorf("the archive must publish only via the relay, got %d direct publishes", idx.calls)
	}

	var got indexer.Transaction
	if uerr := json.Unmarshal(repo.indexPayloads[len(repo.indexPayloads)-1], &got); uerr != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", uerr)
	}
	// Archiving is a soft DELETE: republishing it as an update would leave the document
	// findable, which is the whole failure this test guards.
	if got.Action != indexer.ActionDeleted {
		t.Errorf("action = %q, want %q — an archived brief must be removed from the index", got.Action, indexer.ActionDeleted)
	}
	if got.IndexingConfig == nil || got.IndexingConfig.ObjectID != created.ID {
		t.Errorf("indexing_config.object_id = %+v, want %q", got.IndexingConfig, created.ID)
	}
	// A DELETE carries the bare object id, not a document: the indexer type-asserts delete
	// data to a string and rejects an object with "expected string", so passing a document
	// means the archived brief is never removed from search.
	id, ok := got.Data.(string)
	if !ok {
		t.Fatalf("co-committed data = %T, want the bare object id string", got.Data)
	}
	if id != created.ID {
		t.Errorf("co-committed id = %q, want %q", id, created.ID)
	}
	// The stored payload must carry NO credential: the outbox is JSONB retained for audit with
	// no pruning, so a token written here would persist as a live credential indefinitely.
	if auth := got.Headers["authorization"]; auth != "" {
		t.Errorf("stored payload carries an authorization header (%q); the relay must stamp it at publish time", auth)
	}
}

// TestDeleteBrief_ArchiveReturnsTheCommittedRow pins that the archive and the read of its
// result are ONE statement. That is what closes the read-then-archive race: a concurrent
// ReplaceBrief/Approve committing between a separate read and the archive would make the
// archive apply to the newer row while the index received the older snapshot.
//
// It asserts on the REPOSITORY CONTRACT (ArchiveBrief returns the row it committed) rather
// than on published version numbers. An in-memory fake hands back the same pointer for both
// the read and the archive, so racy and correct implementations produce identical published
// versions — a version-based assertion passes either way. (Verified: an earlier version of
// this test did exactly that and stayed green against a deliberately racy DeleteBrief.) The
// real guarantee lives in the SQL — UPDATE ... RETURNING — and in the port's signature, which
// no longer makes a separate read possible.
func TestDeleteBrief_ArchiveReturnsTheCommittedRow(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo)

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	// The port returns the archived row, so a caller cannot publish a separately-read snapshot.
	archived, aerr := repo.ArchiveBrief(ctx, "cncf", created.ID, nil)
	if aerr != nil {
		t.Fatalf("ArchiveBrief: %v", aerr)
	}
	if archived == nil {
		t.Fatal("ArchiveBrief must return the committed row, not just an error")
	}
	if archived.Status != model.BriefArchived {
		t.Errorf("returned status = %q, want %q", archived.Status, model.BriefArchived)
	}

	// Archiving twice must be ErrNotFound: the real query guards on status <> 'archived', so a
	// second archive commits nothing and therefore has no row to return.
	if _, second := repo.ArchiveBrief(ctx, "cncf", created.ID, nil); !errors.Is(second, domain.ErrNotFound) {
		t.Errorf("second archive = %v, want ErrNotFound (nothing was committed, so nothing to index)", second)
	}
}

// TestIndexedDocsUseSnakeCase pins the INDEXED wire shape.
//
// The generated goa types carry no json tags, so publishing them directly emitted Go field
// names ("ProjectID", "EventSlug") instead of the snake_case the HTTP API uses. Such a document
// indexes cleanly and then matches nothing for a consumer filtering on the API's field names —
// indistinguishable from indexing being broken.
func TestIndexedDocsUseSnakeCase(t *testing.T) {
	url := "https://events.lfx.dev/k"
	raw, err := json.Marshal(briefDoc(&briefs.Brief{
		ID: "b1", ProjectID: "cncf", ProgramType: "events",
		EventSlug: "kubecon-eu-2026", URL: &url, Status: "approved", Version: 3,
	}))
	if err != nil {
		t.Fatalf("marshal brief doc: %v", err)
	}
	for _, want := range []string{`"project_id":"cncf"`, `"event_slug":"kubecon-eu-2026"`, `"program_type":"events"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("brief doc missing %s\ngot: %s", want, raw)
		}
	}
	for _, bad := range []string{`"ProjectID"`, `"EventSlug"`, `"ProgramType"`} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("brief doc leaks the Go field name %s: consumers filtering on the API shape match nothing\ngot: %s", bad, raw)
		}
	}

	pcid := "pc-9"
	raw, err = json.Marshal(campaignDoc(&briefs.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: "hubspot",
		PlatformCampaignID: &pcid, CampaignName: "n", Status: "created", Version: 1,
	}))
	if err != nil {
		t.Fatalf("marshal campaign doc: %v", err)
	}
	for _, want := range []string{`"project_id":"cncf"`, `"brief_id":"b1"`, `"platform_campaign_id":"pc-9"`, `"campaign_name":"n"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("campaign doc missing %s\ngot: %s", want, raw)
		}
	}
	for _, bad := range []string{`"ProjectID"`, `"BriefID"`, `"PlatformCampaignID"`} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("campaign doc leaks the Go field name %s\ngot: %s", bad, raw)
		}
	}

	// A nil optional must not become "null" in the document.
	raw, _ = json.Marshal(campaignDoc(&briefs.Campaign{ID: "c2"}))
	if strings.Contains(string(raw), "null") {
		t.Errorf("nil optionals must be omitted, not emitted as null\ngot: %s", raw)
	}
}

// TestBriefDoc_CarriesRevisableContent pins that the indexed projection includes the fields an
// edit actually changes. The Query Service serves revision history from these documents, so a
// projection limited to identity fields would make a copy-only revision indistinguishable from
// a no-op: the version increments and nothing visible differs.
func TestBriefDoc_CarriesRevisableContent(t *testing.T) {
	doc := briefDoc(&briefs.Brief{
		ID: "b1", ProjectID: "cncf", ProgramType: "events", EventSlug: "kubecon",
		Status: "approved", Version: 2,
		Platforms:    []string{"hubspot"},
		EventDetails: map[string]any{"eventName": "KubeCon"},
		Copy:         map[string]any{"headline": "Join us"},
		Keywords:     []any{"cloud"},
		Targeting:    map[string]any{"geo": "KR"},
	})

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{"platforms", "event_details", "copy", "keywords", "targeting"} {
		if !strings.Contains(string(raw), `"`+want+`"`) {
			t.Errorf("indexed brief is missing %q — revision history would show a new version with nothing changed\ngot: %s", want, raw)
		}
	}
	if !strings.Contains(string(raw), "Join us") {
		t.Errorf("the revised copy must reach the index\ngot: %s", raw)
	}

	// Absent optionals stay out of the document rather than appearing as null.
	raw, _ = json.Marshal(briefDoc(&briefs.Brief{ID: "b2"}))
	if strings.Contains(string(raw), "null") {
		t.Errorf("absent optionals must be omitted, not null\ngot: %s", raw)
	}
}

// newBriefServiceWithRepo builds a BriefService with live collaborators around repo,
// matching how the container injects them once the DB pool is ready.
func newBriefServiceWithRepo(t *testing.T, repo *fakeBriefRepo) *BriefService {
	t.Helper()
	s := NewBriefService(nil, nil, nil, nil)
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	s.SetBackend(repo, camps, jobs, NewOrchestrator(camps, jobs, nil))
	return s
}

// TestFindBrief_ReturnsSavedBriefForEventSlug covers the re-paste path: a brief was already
// generated and saved for this event, so the lookup returns it (with its AI-generated
// copy/keywords/targeting and any later edits) instead of the caller regenerating.
func TestFindBrief_ReturnsSavedBriefForEventSlug(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-eu-2026",
		Status: model.BriefDraft,
		Copy:   json.RawMessage(`{"headlines":["Join KubeCon EU 2026"]}`),
	}
	s := newBriefServiceWithRepo(t, repo)

	got, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "kubecon-eu-2026"})
	if err != nil {
		t.Fatalf("FindBrief: %v", err)
	}
	if got.ID != "b1" {
		t.Errorf("brief id = %q, want b1", got.ID)
	}
}

// TestFindBrief_NotFoundIsTheFirstGenerationCase pins the ordinary first-time path: no brief
// exists for this event yet, so the lookup 404s and the caller generates one. This must be a
// clean NotFound, not a server error — it is the COMMON case, not a failure.
func TestFindBrief_NotFoundIsTheFirstGenerationCase(t *testing.T) {
	ctx := context.Background()
	s := newBriefServiceWithRepo(t, newFakeBriefRepo())

	_, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "brand-new-event"})
	if err == nil {
		t.Fatal("expected a not-found error for an event with no brief")
	}
	var nf *briefs.NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("want a 404 NotFoundError (first-generation case), got %T: %v", err, err)
	}
}

// TestFindBrief_ArchivedBriefDoesNotMatch mirrors the partial unique index: archiving frees
// the slug, so a re-paste after archiving must 404 and generate afresh rather than resurrect
// the archived brief.
func TestFindBrief_ArchivedBriefDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-eu-2026", Status: model.BriefArchived,
	}
	s := newBriefServiceWithRepo(t, repo)

	if _, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: "kubecon-eu-2026"}); err == nil {
		t.Fatal("an archived brief must not be returned — the slug is free for a new brief")
	}
}

// TestFindBrief_HandlesLongSlugs checks the SERVICE layer passes a long slug straight through
// to the repo without truncating or rejecting it.
//
// This test alone does NOT pin the absence of a MaxLength on the contract: it calls FindBrief
// directly, and a design-level cap is enforced by the generated DECODER, which this bypasses.
// TestFindBriefDecoder_RejectsEmptySlugButNotLongOnes is what binds that.
func TestFindBrief_HandlesLongSlugs(t *testing.T) {
	ctx := context.Background()
	longSlug := strings.Repeat("a", 512)
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: longSlug, Status: model.BriefDraft,
	}
	s := newBriefServiceWithRepo(t, repo)

	got, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "cncf", EventSlug: longSlug})
	if err != nil {
		t.Fatalf("a slug the create contract accepts must be recallable: %v", err)
	}
	if got.ID != "b1" {
		t.Errorf("brief id = %q, want b1", got.ID)
	}
}

// TestFindBriefDecoder_RejectsEmptySlugButNotLongOnes pins the find-brief QUERY-PARAM contract
// at the layer that actually enforces it: the generated decoder.
//
// The load-bearing half is the LONG slug. A MaxLength MUST NOT be (re)introduced on this
// lookup: BriefInput.event_slug is uncapped and the column is unbounded TEXT, so a cap
// here would make a brief the CREATE contract accepted permanently unrecallable — the caller
// would get a validation error instead of its saved brief, then collide on re-create against
// the UNIQUE(project_id, event_slug) index. The service-level test above cannot catch that,
// because a cap lives in design/brief.go and is generated into DecodeFindBriefRequest, not
// into the service method. Verified binding: adding MaxLength(64) to design/brief.go and
// regenerating fails this test.
//
// The empty-slug half is a guard, not a proof. Because event_slug is a REQUIRED query param,
// goa rejects "" with a MissingFieldError from Required() alone — so this assertion still
// passes if MinLength(1) is dropped from the design. MinLength(1) is therefore belt-and-braces
// on THIS endpoint; the constraint that genuinely does work is the one on BriefInput
// (a JSON body field, where Required() only checks key presence), which
// TestBriefInput_RejectsEmptyEventSlug pins.
func TestFindBriefDecoder_RejectsEmptySlugButNotLongOnes(t *testing.T) {
	mux := goahttp.NewMuxer()
	decode := briefsserver.DecodeFindBriefRequest(mux, goahttp.RequestDecoder)

	// Route through the muxer so mux.Vars(r) resolves {project_id}; decoding a raw
	// httptest request would silently yield an empty project_id and not exercise the
	// real path. The handler runs synchronously inside ServeHTTP, so capturing the
	// routed request without a lock is safe.
	var routed *http.Request
	mux.Handle(http.MethodGet, "/projects/{project_id}/briefs", func(_ http.ResponseWriter, rr *http.Request) {
		routed = rr
	})
	newReq := func(t *testing.T, slug string) *http.Request {
		t.Helper()
		routed = nil
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
			"/projects/cncf/briefs?event_slug="+url.QueryEscape(slug), nil))
		if routed == nil {
			t.Fatal("request was not routed; the decoder would not see path params")
		}
		return routed
	}

	// An empty slug must be rejected: it can never match a stored row. (Satisfied by
	// Required() alone — see the doc comment.)
	if _, err := decode(newReq(t, "")); err == nil {
		t.Error("an empty event_slug must be rejected by the decoder")
	}

	// A very long slug must still decode. A MaxLength added to design/brief.go and regenerated
	// would fail here — which is the regression this test exists to catch.
	longSlug := strings.Repeat("a", 512)
	got, err := decode(newReq(t, longSlug))
	if err != nil {
		t.Fatalf("a long slug must decode (no MaxLength on the contract), got: %v", err)
	}
	if got.EventSlug != longSlug {
		t.Errorf("event_slug was altered by decoding: len = %d, want %d", len(got.EventSlug), len(longSlug))
	}
	if got.ProjectID != "cncf" {
		t.Errorf("project_id = %q, want cncf", got.ProjectID)
	}
}

// TestFindBrief_IsScopedToProject guards tenancy: the same event slug under a DIFFERENT
// project must not leak across.
func TestFindBrief_IsScopedToProject(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", EventSlug: "kubecon-eu-2026", Status: model.BriefDraft,
	}
	s := newBriefServiceWithRepo(t, repo)

	if _, err := s.FindBrief(ctx, &briefs.FindBriefPayload{ProjectID: "other-foundation", EventSlug: "kubecon-eu-2026"}); err == nil {
		t.Fatal("a brief must not be visible to a different project")
	}
}

// TestBriefInput_RejectsEmptyEventSlug pins that the CREATE contract and the find-by-slug
// LOOKUP contract agree on event_slug.
//
// They originally did not. goa's Required() checks only that the JSON key is PRESENT, so an
// explicit "" satisfied it, and the TEXT NOT NULL column accepts it — meaning a brief with an
// empty slug was creatable, occupied the UNIQUE(project_id, event_slug) index, and could never
// be recalled through find-brief (whose own MinLength(1) rejects the request with a 400
// instead of the documented 404/200).
//
// Asserting on the GENERATED validator is the point: the constraint lives in design/brief.go,
// so this fails if someone drops MinLength(1) there and regenerates.
func TestBriefInput_RejectsEmptyEventSlug(t *testing.T) {
	empty := ""
	programType := "events"

	err := briefsserver.ValidateBriefInputRequestBody(&briefsserver.BriefInputRequestBody{
		ProgramType: &programType,
		EventSlug:   &empty,
	})
	if err == nil {
		t.Fatal("an empty event_slug must be rejected at create: it is creatable but permanently unrecallable")
	}
	if !strings.Contains(err.Error(), "event_slug") {
		t.Errorf("error should name the offending field, got: %v", err)
	}

	// A real slug still passes — the constraint must not reject ordinary input.
	slug := "kubecon-eu-2026"
	if verr := briefsserver.ValidateBriefInputRequestBody(&briefsserver.BriefInputRequestBody{
		ProgramType: &programType,
		EventSlug:   &slug,
	}); verr != nil {
		t.Errorf("a real slug must still validate, got: %v", verr)
	}
}

// TestBriefResponse_StillDecodesLegacyEmptySlug pins the OTHER half of the empty-slug contract:
// requests reject an empty event_slug, but RESPONSES must still carry one.
//
// The constraint originally lived on BriefInput, which the Brief response type Reference()s —
// and goa copies validations through Reference, so it landed in all five response validators
// too. Any already-persisted empty-slug row then became undecodable by generated clients,
// breaking even get-brief for exactly the rows the create-side fix exists to prevent going
// forward. Hence the separate BriefInput and BriefData types.
func TestBriefResponse_StillDecodesLegacyEmptySlug(t *testing.T) {
	// A response body carrying the legacy empty slug must validate.
	empty := ""
	id := "b1"
	projectID := "cncf"
	programType := "events"
	status := "approved"
	var version int64 = 1

	if err := briefsclient.ValidateGetBriefResponseBody(&briefsclient.GetBriefResponseBody{
		ID: &id, ProjectID: &projectID, ProgramType: &programType,
		EventSlug: &empty, Status: &status, Version: &version,
	}); err != nil {
		t.Fatalf("a legacy empty-slug row must still be readable, got: %v", err)
	}

	// ...while the WRITE path still rejects it.
	if err := briefsserver.ValidateBriefInputRequestBody(&briefsserver.BriefInputRequestBody{
		ProgramType: &programType, EventSlug: &empty,
	}); err == nil {
		t.Error("the create/update payload must still reject an empty event_slug")
	}
}

// nonTogglerDispatcher is a PlatformDispatcher that does NOT implement StatusToggler, which
// is exactly the shape of the hubspot (email) adapter: it stages a draft and has no run state.
type nonTogglerDispatcher struct{}

func (nonTogglerDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("not used in this test")
}

// TestToggleUnsupported_EmailGetsADistinctMessage drives the REAL handler path — repo,
// orchestrator, and a dispatcher lacking StatusToggler — and asserts the message a caller
// actually receives. An earlier version of this test only re-asserted Provider.Kind(), so the
// email-specific message could have been deleted and it would have stayed green.
func TestToggleUnsupported_EmailGetsADistinctMessage(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		platform    model.Provider
		wantContain string
	}{
		// Email: nothing to pause BY DESIGN. The message must say so, or an operator reads a
		// generic "not supported" as a missing feature and files a bug.
		{"email channel", model.ProviderHubSpot, "email channel"},
		// A paid platform with no toggler wired keeps the generic message — there it really
		// IS unimplemented, and claiming "email has no run state" would be wrong.
		{"paid platform", model.ProviderRedditAds, "not supported for this campaign's platform"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeBriefRepo()
			repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
				ID: "b1", ProjectID: "cncf", EventSlug: "ev", Status: model.BriefApproved,
			}
			camps := &fakeCampaignRepo{}
			camps.byID = map[string]*model.Campaign{
				"c1": {
					ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: tc.platform,
					PlatformCampaignID: "up-1", Status: model.CampaignStatusCreated, Version: 1,
				},
			}
			jobs := newFakeJobRepo()
			// The dispatcher exists (so it is not "no dispatcher registered") but does not
			// implement StatusToggler — the real hubspot situation.
			orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
				tc.platform: nonTogglerDispatcher{},
			})
			s := NewBriefService(nil, nil, nil, nil)
			s.SetBackend(repo, camps, jobs, orch)

			etag := `"1"` // matches the campaign's Version, satisfying the If-Match gate
			_, err := s.ToggleCampaignStatus(ctx, &briefs.ToggleCampaignStatusPayload{
				ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
				Status: model.CampaignRunPaused, IfMatch: &etag,
			})
			if err == nil {
				t.Fatal("expected a 400 when the platform has no toggle capability")
			}
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("want a BadRequestError (400), got %T: %v", err, err)
			}
			if !strings.Contains(bad.Message, tc.wantContain) {
				t.Errorf("message = %q, want it to contain %q", bad.Message, tc.wantContain)
			}
		})

	}
}

// TestDeleteBrief_EnqueuesTheIndexMessageWithTheArchive pins the outbox co-commit for the
// TERMINAL write. Archiving has no "next write" to repair the index, so if the post-commit
// publish is dropped the brief stays searchable forever. The outbox row is written in the SAME
// transaction as the archive, so the relay can deliver it even if the process dies immediately
// after the commit.
func TestDeleteBrief_EnqueuesTheIndexMessageWithTheArchive(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo)

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026", ProgramType: "events"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	if derr := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); derr != nil {
		t.Fatalf("DeleteBrief: %v", derr)
	}

	if len(repo.lastIndexPayload) == 0 {
		t.Fatal("no index message was enqueued with the archive: a dropped publish would leave the brief searchable forever")
	}

	var msg map[string]any
	if uerr := json.Unmarshal(repo.lastIndexPayload, &msg); uerr != nil {
		t.Fatalf("the enqueued payload must be a valid index message: %v", uerr)
	}
	// The payload is frozen at write time under the CURRENT contract, so the relay never
	// re-derives it — assert the parts the indexer requires.
	if msg["action"] != indexer.ActionDeleted {
		t.Errorf("action = %v, want %q (an archived brief must be REMOVED from the index)", msg["action"], indexer.ActionDeleted)
	}
	if msg["data"] != created.ID {
		t.Errorf("delete data = %v, want the bare object id %q", msg["data"], created.ID)
	}
	if _, ok := msg["indexing_config"]; !ok {
		t.Error("the enqueued message must carry indexing_config or the indexer drops it")
	}
}

// TestBriefWrites_AllGoThroughTheOutbox pins the property that closes the resurrection race.
//
// While SOME brief writes published directly after commit and only the archive went through the
// outbox, the two paths could not be ordered against each other: a replace could commit, stall
// before its publish, and land its update AFTER the archive had been replayed and its row
// retired — putting a deleted brief back in the index with no pending tombstone left to repair
// it. The publisher's per-object lock cannot prevent this; it is process-local and only orders
// calls in the order they arrive, which says nothing about a replica that stalled.
//
// The fix is structural rather than a tighter lock: every mutation co-commits its message, so
// each brief has ONE ordered sequence carried by the table — which is also what makes it correct
// across replicas. This test therefore asserts on the MECHANISM (nothing publishes directly),
// because that is the only thing that actually rules the interleaving out.
func TestBriefWrites_AllGoThroughTheOutbox(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	s := newIndexTestBriefService(repo)
	idx := &failingIndexer{}
	s.SetIndexer(idx)

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}

	v1 := "1"
	if _, uerr := s.UpdateBrief(ctx, &briefs.UpdateBriefPayload{
		ProjectID: "cncf", BriefID: created.ID, IfMatch: &v1,
		Brief: &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	}); uerr != nil {
		t.Fatalf("UpdateBrief: %v", uerr)
	}

	// The fake stores the replacement brief as-is, so its version is 0 after the replace.
	// Approve gates on the CURRENT version; a mismatch here would be a precondition failure,
	// which is not what this test is about.
	v0 := "0"
	if _, aerr := s.ApproveBrief(ctx, &briefs.ApproveBriefPayload{
		ProjectID: "cncf", BriefID: created.ID, IfMatch: &v0,
	}); aerr != nil {
		t.Fatalf("ApproveBrief: %v", aerr)
	}

	if derr := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); derr != nil {
		t.Fatalf("DeleteBrief: %v", derr)
	}

	if len(repo.indexPayloads) != 4 {
		t.Fatalf("create, replace, approve and archive must each co-commit one index message, got %d",
			len(repo.indexPayloads))
	}
	if idx.calls != 0 {
		t.Errorf("no brief write may publish on the request path (got %d): a direct publish cannot be "+
			"ordered against an outbox replay, which is how an archived brief got resurrected", idx.calls)
	}

	// The sequence must END in a delete. That is the ordering the outbox guarantees and the
	// direct-publish path could not: whatever else happened, the brief finishes archived.
	var last indexer.Transaction
	if uerr := json.Unmarshal(repo.indexPayloads[len(repo.indexPayloads)-1], &last); uerr != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", uerr)
	}
	if last.Action != indexer.ActionDeleted {
		t.Errorf("final index action = %q, want %q — the archive must be the last event for the brief",
			last.Action, indexer.ActionDeleted)
	}
}

// TestCampaignWrites_UpdateAndToggleCoCommitTheirIndex closes the last direct-publish path.
//
// Creates went through the outbox while UpdateCampaign and ToggleCampaignStatus still published
// after ReplaceCampaign. Mixing the two cannot be ordered: a replayed create could land AFTER a
// newer update or toggle and overwrite it in the index, leaving search stale until some later
// write happened to repair it — the same interleaving already fixed for briefs.
//
// The toggle case carries a second hazard. Its write is deliberately detached (persistCtx) so a
// cancelled request still records a platform change that already happened; publishing on the
// REQUEST context dropped exactly those index events, leaving the status right in the database
// and stale in search for the requests most likely to need reconciling. Co-committing means the
// row and its message share one fate.
func TestCampaignWrites_UpdateAndToggleCoCommitTheirIndex(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		cur := &model.Campaign{
			ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
			CampaignName: "old", Status: "created", Version: 1,
		}
		repo := &campaignEditRepo{cur: cur}
		s := &BriefService{briefs: newFakeBriefRepo(), campaigns: repo, jobs: newFakeJobRepo(),
			orch: NewOrchestrator(repo, newFakeJobRepo(), nil)}
		s.SetIndexer(&failingIndexer{})
		im := "1"

		if _, err := s.UpdateCampaign(context.Background(), &briefs.UpdateCampaignPayload{
			ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
			Campaign: &briefs.CampaignUpdateInput{CampaignName: "new", Status: "created"},
		}); err != nil {
			t.Fatalf("UpdateCampaign: %v", err)
		}
		assertOneCampaignIndexMessage(t, repo.indexPayloads)
	})

	t.Run("status toggle", func(t *testing.T) {
		camp := &model.Campaign{
			ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderRedditAds,
			PlatformCampaignID: "t3_c", Status: "created", Version: 1,
		}
		s, camps := newToggleService(camp, &stubToggler{})
		im := "1"
		if _, err := s.ToggleCampaignStatus(context.Background(), &briefs.ToggleCampaignStatusPayload{
			ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im, Status: model.CampaignRunPaused,
		}); err != nil {
			t.Fatalf("ToggleCampaignStatus: %v", err)
		}
		assertOneCampaignIndexMessage(t, camps.indexPayloads)
	})
}

// assertOneCampaignIndexMessage checks that exactly one message was co-committed, that it
// describes an update, and that it carries NO caller credential — the relay stamps a service
// token at publish time, and the outbox is retained for audit with no pruning, so a JWT written
// there would persist as a live credential.
func assertOneCampaignIndexMessage(t *testing.T, payloads [][]byte) {
	t.Helper()
	if len(payloads) != 1 {
		t.Fatalf("expected exactly one co-committed index message, got %d", len(payloads))
	}
	var msg indexer.Transaction
	if err := json.Unmarshal(payloads[0], &msg); err != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", err)
	}
	if msg.Action != indexer.ActionUpdated {
		t.Errorf("action = %q, want %q", msg.Action, indexer.ActionUpdated)
	}
	// NOT asserted: msg.ObjectType(). It is unexported state that does not survive the JSON
	// round-trip by design — the indexer derives the type from the SUBJECT, and the relay routes
	// from the outbox row's object_type column rather than the payload.
	if auth := msg.Headers["authorization"]; auth != "" {
		t.Errorf("stored payload carries an authorization header (%q); the relay must stamp it at publish time", auth)
	}
}

// TestUpdateBrief_CoCommitsIndexMessage pins that an update operation co-commits an index message
// the same way CreateBrief and ArchiveBrief do. A future refactor that drops the index-payload
// argument from the ReplaceBrief call would compile and pass the existing suite; this assertion
// catches that regression before it makes updates unsearchable.
func TestUpdateBrief_CoCommitsIndexMessage(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	// Pre-populate a brief to update
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefDraft, Version: 1,
		EventSlug: "kubecon-eu-2026", ProgramType: model.ProgramEvents,
	}
	s := newIndexTestBriefService(repo)
	s.SetIndexer(&failingIndexer{})

	ver := "1"
	updated, err := s.UpdateBrief(ctx, &briefs.UpdateBriefPayload{
		ProjectID: "cncf", BriefID: "b1", IfMatch: &ver,
		Brief: &briefs.BriefInput{
			EventSlug:   "kubecon-north-america-2026",
			ProgramType: "events",
		},
	})
	if err != nil {
		t.Fatalf("UpdateBrief: %v", err)
	}
	if updated == nil {
		t.Fatal("expected the updated brief to be returned")
	}
	if len(repo.indexPayloads) == 0 {
		t.Fatal("expected UpdateBrief to co-commit an index message")
	}
	// Assertion on the last payload: if there were multiple, we want the update's message, not an earlier one
	var msg struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(repo.indexPayloads[len(repo.indexPayloads)-1], &msg); err != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", err)
	}
	if msg.Action != indexer.ActionUpdated {
		t.Errorf("action = %q, want %q", msg.Action, indexer.ActionUpdated)
	}
}

// TestApproveBrief_CoCommitsIndexMessage pins that an approve operation co-commits an index
// message. A future refactor that drops the index-payload argument from the Approve call would
// compile and pass the existing suite; this assertion catches that regression before it makes
// approvals unsearchable.
func TestApproveBrief_CoCommitsIndexMessage(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	// Pre-populate a brief to approve
	repo.briefs[briefKey("cncf", "b1")] = &model.CampaignBrief{
		ID: "b1", ProjectID: "cncf", Status: model.BriefDraft, Version: 2,
		EventSlug: "kubecon-eu-2026", ProgramType: model.ProgramEvents,
	}
	s := newIndexTestBriefService(repo)
	s.SetIndexer(&failingIndexer{})

	ver := "2"
	approved, err := s.ApproveBrief(ctx, &briefs.ApproveBriefPayload{
		ProjectID: "cncf", BriefID: "b1", IfMatch: &ver,
	})
	if err != nil {
		t.Fatalf("ApproveBrief: %v", err)
	}
	if approved == nil {
		t.Fatal("expected the approved brief to be returned")
	}
	if len(repo.indexPayloads) == 0 {
		t.Fatal("expected ApproveBrief to co-commit an index message")
	}
	// Assertion on the last payload
	var msg struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(repo.indexPayloads[len(repo.indexPayloads)-1], &msg); err != nil {
		t.Fatalf("co-committed payload is not valid JSON: %v", err)
	}
	if msg.Action != indexer.ActionUpdated {
		t.Errorf("action = %q, want %q", msg.Action, indexer.ActionUpdated)
	}
}

// TestDisabledIndexing_EnqueuesNothing pins the source-level answer to unbounded outbox growth.
//
// Pending rows are NEVER pruned — they are undelivered work and this service has no reindex
// path, so discarding one is unrecoverable (a terminal brief archive has no later write to
// repair it). That makes it essential that rows which can never be delivered are not written in
// the first place. When indexing is DELIBERATELY disabled (NATS_URL="" → the Noop publisher),
// the payload builder is nil and the repo skips the enqueue entirely.
//
// A missing CREDENTIAL is deliberately NOT treated this way: that is a provisioning gap, the
// rows are real work, and the relay drains them once the token lands.
func TestDisabledIndexing_EnqueuesNothing(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()
	// DisableIndexing is the CONFIG signal the container sets when NATS_URL is empty — the same
	// call production makes. Deliberately not "construct without SetIndexer": that state also
	// arises from a wiring bug or a broker outage, and those must still enqueue.
	s := NewBriefService(repo, camps, jobs, NewOrchestrator(camps, jobs, nil))
	s.SetIndexer(&failingIndexer{}) // a real publisher, to prove the gate is the CONFIG flag
	s.DisableIndexing()

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("a disabled indexer must not fail the write: %v", err)
	}
	if derr := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); derr != nil {
		t.Fatalf("nor the archive: %v", derr)
	}

	if n := len(repo.indexPayloads); n != 0 {
		t.Errorf("wrote %d outbox row(s) that can never be delivered; pending rows are never "+
			"pruned, so this grows without bound", n)
	}
}

// TestBrokerDown_StillEnqueues is the counterpart to TestDisabledIndexing_EnqueuesNothing, and
// guards the distinction the whole gate rests on.
//
// NewNATSPublisher returns a Noop for BOTH an empty NATS_URL and an unreachable broker. If the
// enqueue were gated on "is the publisher a Noop", a pod that happened to start during a broker
// restart would skip the outbox for its ENTIRE life — the publisher is built once and never
// re-dialled — and those writes would be lost permanently, since pending rows are never pruned
// and there is no reindex path. That is strictly worse than the unbounded growth the gate exists
// to prevent.
//
// So the gate keys on CONFIG (DisableIndexing), and a service with a dead publisher but indexing
// configured must still write its outbox row. The relay delivers it once NATS reconnects.
func TestBrokerDown_StillEnqueues(t *testing.T) {
	ctx := context.Background()
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{}
	jobs := newFakeJobRepo()

	s := NewBriefService(repo, camps, jobs, NewOrchestrator(camps, jobs, nil))
	// Exactly what the container does when NATS is unreachable at boot: a Noop publisher, but
	// NO DisableIndexing, because NATS_URL is configured.
	s.SetIndexer(indexer.Noop{})
	if !s.IndexerIsNoop() {
		t.Fatal("precondition: the publisher must be a Noop, modelling an unreachable broker")
	}

	created, err := s.CreateBrief(ctx, &briefs.CreateBriefPayload{
		ProjectID: "cncf",
		Brief:     &briefs.BriefInput{EventSlug: "kubecon-eu-2026"},
	})
	if err != nil {
		t.Fatalf("CreateBrief: %v", err)
	}
	if derr := s.DeleteBrief(ctx, &briefs.DeleteBriefPayload{
		ProjectID: "cncf", BriefID: created.ID,
	}); derr != nil {
		t.Fatalf("DeleteBrief: %v", derr)
	}

	if n := len(repo.indexPayloads); n != 2 {
		t.Errorf("wrote %d outbox rows, want 2: a broker outage must not skip the enqueue, or "+
			"the write is lost forever once the pod's publisher never recovers", n)
	}
}

// --- get campaign metrics -----------------------------------------------------

func newMetricsService(camp *model.Campaign, disp PlatformDispatcher) *BriefService {
	repo := newFakeBriefRepo()
	camps := &fakeCampaignRepo{byID: map[string]*model.Campaign{camp.ID: camp}}
	jobs := newFakeJobRepo()
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{camp.Platform: disp})
	return NewBriefService(repo, camps, jobs, orch)
}

func TestBriefService_GetCampaignMetrics_HappyPath(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "ga-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	disp := &metricsOnlyDispatcher{metrics: &model.CampaignMetrics{
		CampaignID: "ga-1", Window: model.MetricsWindowLast30Days, Impressions: 100, Clicks: 10, CostMicros: 5000000, Ctr: 0.1,
	}}
	s := newMetricsService(camp, disp)
	window := "last_30_days"
	res, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Window: &window,
	})
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if res.CampaignID != "c1" || res.PlatformCampaignID != "ga-1" || res.Window != "last_30_days" ||
		res.Impressions != 100 || res.Clicks != 10 || res.CostMicros != 5000000 || res.Ctr != 0.1 {
		t.Errorf("unexpected result: %+v", res)
	}
	if disp.gotWindow != model.MetricsWindowLast30Days {
		t.Errorf("dispatcher got window %q, want last_30_days", disp.gotWindow)
	}
}

func TestBriefService_GetCampaignMetrics_DefaultWindowIsLast30Days(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "ga-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	disp := &metricsOnlyDispatcher{metrics: &model.CampaignMetrics{
		CampaignID: "ga-1", Window: model.MetricsWindowLast30Days, Impressions: 200, Clicks: 20, CostMicros: 9000000, Ctr: 0.1,
	}}
	s := newMetricsService(camp, disp)
	res, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
	})
	if err != nil {
		t.Fatalf("GetCampaignMetrics: %v", err)
	}
	if disp.gotWindow != model.MetricsWindowLast30Days {
		t.Errorf("dispatcher got window %q, want last_30_days", disp.gotWindow)
	}
	if res.Window != "last_30_days" {
		t.Errorf("result Window = %q, want last_30_days", res.Window)
	}
}

func TestBriefService_GetCampaignMetrics_PlatformUnsupportedIs400(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "ga-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	s := newMetricsService(camp, nonMetricsDispatcher{})
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadRequestError (400), got %T: %v", err, err)
	}
}

func TestBriefService_GetCampaignMetrics_NotProvisionedIs409(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "", Status: "pending", Version: 1,
	}
	s := newMetricsService(camp, &metricsOnlyDispatcher{})
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a ConflictError (409) for an unprovisioned campaign, got %T: %v", err, err)
	}
}

func TestBriefService_GetCampaignMetrics_PlatformFailureIs503(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "ga-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	disp := &metricsOnlyDispatcher{err: errors.New("google ads 500")}
	s := newMetricsService(camp, disp)
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1",
	})
	var unavailable *briefs.ConnServiceUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected a ConnServiceUnavailableError (503), got %T: %v", err, err)
	}
}

func TestBriefService_GetCampaignMetrics_WindowUnsupportedIs400(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderTwitterAds,
		PlatformCampaignID: "x-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	disp := &metricsOnlyDispatcher{err: fmt.Errorf("x ads: %w", ErrMetricsWindowUnsupported)}
	s := newMetricsService(camp, disp)
	window := "last_30_days"
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Window: &window,
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadRequestError (400) for a platform rejecting this window, got %T: %v", err, err)
	}
}

func TestBriefService_GetCampaignMetrics_InvalidWindowIs400(t *testing.T) {
	camp := &model.Campaign{
		ID: "c1", ProjectID: "cncf", BriefID: "b1", Platform: model.ProviderGoogleAds,
		PlatformCampaignID: "ga-1", Status: model.CampaignStatusCreated, Version: 1,
	}
	s := newMetricsService(camp, &metricsOnlyDispatcher{})
	window := "not_a_real_window"
	_, err := s.GetCampaignMetrics(context.Background(), &briefs.GetCampaignMetricsPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", Window: &window,
	})
	var bad *briefs.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("expected a BadRequestError (400) for an invalid window, got %T: %v", err, err)
	}
}

// ─── DeleteCampaign ───

// deleteCampaignRepo records the DeleteCampaign call and returns a configurable error,
// so the service-layer tests can pin how each repo sentinel is mapped to the API.
type deleteCampaignRepo struct {
	fakeCampaignRepo
	err error
	// called records the arguments of the last DeleteCampaign call.
	called          bool
	gotProject      string
	gotBrief        string
	gotCampaign     string
	gotExpectedVers int64
	// gotIndexPayload is the JSON the builder produced when invoked with the deleted
	// row, mirroring what the real repo does before it commits (see
	// CampaignRepo.DeleteCampaign's enqueueCampaignIndex call). nil if the builder
	// itself was nil (indexing disabled) or was never invoked.
	gotIndexPayload []byte
}

func (r *deleteCampaignRepo) DeleteCampaign(_ context.Context, projectID, briefID, id string, expectedVersion int64, indexPayload domain.CampaignIndexPayloadFunc) error {
	r.called = true
	r.gotProject, r.gotBrief, r.gotCampaign, r.gotExpectedVers = projectID, briefID, id, expectedVersion
	if r.err == nil && indexPayload != nil {
		payload, perr := indexPayload(&model.Campaign{
			ID: id, BriefID: briefID, ProjectID: projectID, Status: model.CampaignStatusDeleted,
		})
		if perr != nil {
			return perr
		}
		r.gotIndexPayload = payload
	}
	return r.err
}

func newDeleteService(repoErr error) (*BriefService, *deleteCampaignRepo) {
	camps := &deleteCampaignRepo{err: repoErr}
	jobs := newFakeJobRepo()
	return NewBriefService(newFakeBriefRepo(), camps, jobs, NewOrchestrator(camps, jobs, nil)), camps
}

// A successful delete forwards the parsed If-Match version to the repo, which is what
// makes the delete optimistically concurrent: without it a client could delete a
// campaign that changed (e.g. finished dispatching) since it was read.
func TestBriefService_DeleteCampaign_ForwardsVersionAndScope(t *testing.T) {
	s, camps := newDeleteService(nil)
	im := "7"
	if err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
	}); err != nil {
		t.Fatalf("DeleteCampaign: %v", err)
	}
	if !camps.called {
		t.Fatal("repo DeleteCampaign was not called")
	}
	if camps.gotExpectedVers != 7 {
		t.Errorf("expectedVersion = %d, want 7 (the If-Match value must gate the delete)", camps.gotExpectedVers)
	}
	// Project and brief must both be forwarded: they scope the delete for tenant
	// isolation, so a campaign UUID alone cannot delete across projects.
	if camps.gotProject != "cncf" || camps.gotBrief != "b1" || camps.gotCampaign != "c1" {
		t.Errorf("scope = (%q,%q,%q), want (cncf,b1,c1)", camps.gotProject, camps.gotBrief, camps.gotCampaign)
	}
}

// If-Match is REQUIRED. A delete is destructive and unrecoverable through the API (the
// row becomes invisible), so an unconditional delete must be refused with 428 and must
// never reach the repo.
func TestBriefService_DeleteCampaign_RequiresIfMatch(t *testing.T) {
	s, camps := newDeleteService(nil)
	err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: nil,
	})
	var pre *briefs.PreconditionRequiredError
	if !errors.As(err, &pre) {
		t.Fatalf("err = %#v, want PreconditionRequiredError (428)", err)
	}
	if camps.called {
		t.Error("the repo was called despite a missing If-Match; the delete must be refused before it reaches persistence")
	}
}

// TestBriefService_DeleteCampaign_EnqueuesTheDeletedTombstone pins that the service
// passes a REAL builder producing an ActionDeleted tombstone for the deleted campaign,
// not just some indexPayload value. Without this, deleteCampaignRepo.DeleteCampaign's
// indexPayload argument could be dropped or built with the wrong action and every
// other test here would still pass, leaving a successful 204 with the old live
// campaign still visible in Query Service.
func TestBriefService_DeleteCampaign_EnqueuesTheDeletedTombstone(t *testing.T) {
	s, camps := newDeleteService(nil)
	im := "1"
	if err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
	}); err != nil {
		t.Fatalf("DeleteCampaign: %v", err)
	}

	if len(camps.gotIndexPayload) == 0 {
		t.Fatal("no index message was built for the delete: a dropped builder would leave the campaign searchable forever")
	}

	var msg map[string]any
	if uerr := json.Unmarshal(camps.gotIndexPayload, &msg); uerr != nil {
		t.Fatalf("the enqueued payload must be a valid index message: %v", uerr)
	}
	if msg["action"] != indexer.ActionDeleted {
		t.Errorf("action = %v, want %q (a deleted campaign must be REMOVED from the index)", msg["action"], indexer.ActionDeleted)
	}
	if msg["data"] != "c1" {
		t.Errorf("delete data = %v, want the bare object id %q", msg["data"], "c1")
	}
}

// A mid-dispatch campaign surfaces as a 409 whose message says the dispatch is in
// flight and to retry. mapBriefErr would otherwise render domain.ErrConflict as "the
// resource already exists" — a uniqueness message that misdescribes the state and
// gives the caller nothing actionable.
func TestBriefService_DeleteCampaign_PendingIsActionableConflict(t *testing.T) {
	s, _ := newDeleteService(domain.ErrConflict)
	im := "1"
	err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
		ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
	})
	var conflict *briefs.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %#v, want ConflictError (409)", err)
	}
	if strings.Contains(conflict.Message, "already exists") {
		t.Errorf("message = %q; the generic uniqueness wording misdescribes a mid-dispatch campaign", conflict.Message)
	}
	if !strings.Contains(conflict.Message, "dispatch") {
		t.Errorf("message = %q, want it to explain that a dispatch is in flight", conflict.Message)
	}
}

// The remaining repo sentinels keep their standard mappings, so a deleted/absent
// campaign is a 404 and a stale ETag is a 412 (not, say, a 500).
func TestBriefService_DeleteCampaign_MapsRepoErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		s, _ := newDeleteService(domain.ErrNotFound)
		im := "1"
		err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
			ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
		})
		var nf *briefs.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("err = %#v, want NotFoundError (404)", err)
		}
	})
	t.Run("stale version", func(t *testing.T) {
		s, _ := newDeleteService(domain.ErrPreconditionFailed)
		im := "1"
		err := s.DeleteCampaign(context.Background(), &briefs.DeleteCampaignPayload{
			ProjectID: "cncf", BriefID: "b1", CampaignID: "c1", IfMatch: &im,
		})
		var pf *briefs.PreconditionFailedError
		if !errors.As(err, &pf) {
			t.Fatalf("err = %#v, want PreconditionFailedError (412)", err)
		}
	})
}
