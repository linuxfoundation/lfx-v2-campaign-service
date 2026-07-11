// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// fakeJobRepo records job status transitions.
type fakeJobRepo struct {
	mu      sync.Mutex
	jobs    map[string]*model.CampaignJob
	counter int
}

func newFakeJobRepo() *fakeJobRepo { return &fakeJobRepo{jobs: map[string]*model.CampaignJob{}} }

func (r *fakeJobRepo) CreateJob(_ context.Context, briefID string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	id := "job-" + string(rune('a'+r.counter))
	j := &model.CampaignJob{ID: id, BriefID: briefID, Status: model.JobQueued}
	r.jobs[id] = j
	return j, nil
}

func (r *fakeJobRepo) GetJob(_ context.Context, _, id string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, errors.New("not found")
	}
	// Return a snapshot so callers don't race with concurrent UpdateJobStatus.
	cp := *j
	return &cp, nil
}

func (r *fakeJobRepo) UpdateJobStatus(_ context.Context, id string, status model.JobStatus, result []byte, jobErr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[id]
	j.Status = status
	j.Result = result
	j.Error = jobErr
	return nil
}

func (r *fakeJobRepo) FailStuckJobs(_ context.Context, jobErr string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, j := range r.jobs {
		if j.Status == model.JobQueued || j.Status == model.JobRunning {
			j.Status = model.JobFailed
			j.Error = jobErr
			n++
		}
	}
	return n, nil
}

// fakeCampaignRepo records upserted campaigns and simulates the claim table.
type fakeCampaignRepo struct {
	mu       sync.Mutex
	upserted []*model.Campaign
	// existing maps briefID+"|"+platform to a pre-existing campaign, letting a
	// test simulate a brief already dispatched to a platform (idempotency guard).
	existing map[string]*model.Campaign
	// byPlatformErr, when set, is returned by GetCampaignByPlatform to simulate a
	// transient lookup failure.
	byPlatformErr error
	// claimErr, when set, is returned by ClaimCampaignDispatch.
	claimErr error
}

func (r *fakeCampaignRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

func (r *fakeCampaignRepo) GetCampaignByPlatform(_ context.Context, briefID string, platform model.Provider) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byPlatformErr != nil {
		return nil, r.byPlatformErr
	}
	if c, ok := r.existing[briefID+"|"+string(platform)]; ok {
		return c, nil
	}
	return nil, domain.ErrNotFound
}

// ClaimCampaignDispatch simulates INSERT ... ON CONFLICT DO NOTHING: if an entry
// for (brief, platform) already exists it's a conflict (not claimed) returning
// the existing row; otherwise it inserts a pending placeholder and claims.
func (r *fakeCampaignRepo) ClaimCampaignDispatch(_ context.Context, projectID, briefID string, platform model.Provider, jobID string) (bool, *model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimErr != nil {
		return false, nil, r.claimErr
	}
	key := briefID + "|" + string(platform)
	if c, ok := r.existing[key]; ok {
		return false, c, nil
	}
	pending := &model.Campaign{ProjectID: projectID, BriefID: briefID, Platform: platform, JobID: &jobID, Status: "pending"}
	if r.existing == nil {
		r.existing = map[string]*model.Campaign{}
	}
	r.existing[key] = pending
	return true, pending, nil
}

func (r *fakeCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Version = 1
	r.upserted = append(r.upserted, c)
	return c, nil
}
func (r *fakeCampaignRepo) ReplaceCampaign(context.Context, *model.Campaign, int64) (*model.Campaign, error) {
	return nil, errors.New("unused")
}

// okDispatcher always succeeds.
type okDispatcher struct{}

func (okDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// failDispatcher always fails.
type failDispatcher struct{}

func (failDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, errors.New("boom")
}

// nilDispatcher returns (nil, nil) — a misbehaving dispatcher that must be
// handled as a failure rather than panicking on the ownership stamp.
type nilDispatcher struct{}

func (nilDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil //nolint:nilnil // deliberately exercises the nil-campaign guard
}

func waitForTerminal(t *testing.T, jobs *fakeJobRepo, id string) *model.CampaignJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, _ := jobs.GetJob(context.Background(), "", id)
		if j.Status.Terminal() {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not reach terminal status")
	return nil
}

func TestOrchestrator_AllSucceed(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
	if len(camps.upserted) != 2 {
		t.Errorf("upserted %d campaigns, want 2", len(camps.upserted))
	}
}

func TestOrchestrator_PartialFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: failDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobPartial {
		t.Errorf("status = %s, want partial", j.Status)
	}
}

func TestOrchestrator_NoDispatcherFails(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil) // no dispatchers
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
}

func TestOrchestrator_NilCampaignFailsWithoutPanic(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: nilDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (nil campaign must not persist)", len(camps.upserted))
	}
}

// countingDispatcher records how many times Dispatch is called, to prove the
// idempotency guard skips the upstream create.
type countingDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *countingDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_SkipsAlreadyDispatchedPlatform verifies that a brief already
// carrying a campaign with an upstream id for a platform does NOT re-invoke the
// platform's create API (which would spend money on a duplicate).
func TestOrchestrator_SkipsAlreadyDispatchedPlatform(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "existing-c1", PlatformCampaignID: "pc-google-ads"},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, err := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (existing campaign must be reused)", calls)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (no re-create)", len(camps.upserted))
	}
	// The reuse path must report the upstream platform campaign id (not the DB
	// row id), so campaign_id means the same thing as on the create path.
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want it to carry the upstream campaign id pc-google-ads", j.Result)
	}
	if strings.Contains(string(j.Result), "existing-c1") {
		t.Errorf("result = %s, must not leak the DB row id existing-c1", j.Result)
	}
}

// TestOrchestrator_ClaimErrorIsFailure verifies that a failure to claim the
// dispatch slot is recorded as a platform failure and the dispatcher is never
// called (so no create can duplicate).
func TestOrchestrator_ClaimErrorIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{claimErr: errors.New("db down")}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (must not create when the claim failed)", calls)
	}
}

// TestOrchestrator_AlreadyClaimedPendingSkips verifies that when another worker
// holds the pending claim (no upstream id yet), this worker does not dispatch.
func TestOrchestrator_AlreadyClaimedPendingSkips(t *testing.T) {
	jobs := newFakeJobRepo()
	// Seed a pending claim (no upstream id) for the pair, so ClaimCampaignDispatch
	// returns not-claimed with a still-pending row.
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", Status: "pending", PlatformCampaignID: ""},
	}}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	// A single in-progress platform aggregates to failed (not ok), and the
	// dispatcher must not have been called.
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (another worker holds the claim)", calls)
	}
	if !strings.Contains(string(j.Result), "already in progress") {
		t.Errorf("result = %s, want an in-progress message", j.Result)
	}
}

func TestAggregateStatus(t *testing.T) {
	cases := []struct {
		name    string
		results []platformResult
		want    model.JobStatus
	}{
		{"all ok", []platformResult{{OK: true}, {OK: true}}, model.JobSucceeded},
		{"all fail", []platformResult{{OK: false}, {OK: false}}, model.JobFailed},
		{"mixed", []platformResult{{OK: true}, {OK: false}}, model.JobPartial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := aggregateStatus(tc.results); got != tc.want {
				t.Errorf("aggregateStatus = %s, want %s", got, tc.want)
			}
		})
	}
}

// emptyIDDispatcher returns a non-nil campaign with no upstream id — a
// misbehaving dispatcher that must be recorded as a failure, not ok.
type emptyIDDispatcher struct{}

func (emptyIDDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "", Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_EmptyUpstreamIDIsFailure verifies a dispatched campaign with
// no PlatformCampaignID is reported as a failure (not ok) and not persisted.
func TestOrchestrator_EmptyUpstreamIDIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: emptyIDDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d, want 0 (empty upstream id must not persist)", len(camps.upserted))
	}
}

// TestOrchestrator_ReusesExistingWhenDispatcherGone verifies the idempotency
// guard runs before dispatcher resolution: an already-persisted platform is
// reported ok on retry even if its dispatcher is no longer registered.
func TestOrchestrator_ReusesExistingWhenDispatcherGone(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderGoogleAds): {ID: "c1", PlatformCampaignID: "pc-google-ads"},
	}}
	// No dispatchers registered at all.
	orch := NewOrchestrator(camps, jobs, nil)
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (existing campaign reused despite no dispatcher)", j.Status)
	}
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want the reused upstream id", j.Result)
	}
}

// panicDispatcher panics — a misbehaving dispatcher that must not crash the
// process; the orchestrator must record it as a failure.
type panicDispatcher struct{}

func (panicDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	panic("boom in dispatcher")
}

// TestOrchestrator_RecoversFromDispatcherPanic verifies a panicking dispatcher
// is recovered and recorded as a failure rather than crashing the goroutine.
func TestOrchestrator_RecoversFromDispatcherPanic(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: panicDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	// The panic value must not leak into the client-facing result.
	if strings.Contains(string(j.Result), "boom in dispatcher") {
		t.Errorf("result leaked the panic value: %s", j.Result)
	}
}

// persistErrCampaignRepo fails UpsertCampaign with a raw DB-like error.
type persistErrCampaignRepo struct{ fakeCampaignRepo }

func (r *persistErrCampaignRepo) UpsertCampaign(context.Context, *model.Campaign) (*model.Campaign, error) {
	return nil, errors.New("pq: duplicate key value violates unique constraint \"campaigns_pkey\"")
}

// TestOrchestrator_PersistErrorIsSanitized verifies a raw persistence error is
// not surfaced verbatim in the client-facing job result.
func TestOrchestrator_PersistErrorIsSanitized(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &persistErrCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	if strings.Contains(string(j.Result), "pq:") || strings.Contains(string(j.Result), "constraint") {
		t.Errorf("result leaked raw DB error: %s", j.Result)
	}
	// The message is sanitized but the upstream id is preserved so the orphaned
	// campaign isn't lost.
	if !strings.Contains(string(j.Result), "failed to record it") {
		t.Errorf("result = %s, want the sanitized message", j.Result)
	}
	if !strings.Contains(string(j.Result), "pc-google-ads") {
		t.Errorf("result = %s, want the upstream id preserved for reconciliation", j.Result)
	}
}

// claimCountingCampaignRepo records that each dispatch went through the claim.
type claimCountingCampaignRepo struct {
	fakeCampaignRepo
	cmu    sync.Mutex
	claims int
}

func (r *claimCountingCampaignRepo) ClaimCampaignDispatch(ctx context.Context, projectID, briefID string, p model.Provider, jobID string) (bool, *model.Campaign, error) {
	r.cmu.Lock()
	r.claims++
	r.cmu.Unlock()
	return r.fakeCampaignRepo.ClaimCampaignDispatch(ctx, projectID, briefID, p, jobID)
}

// TestOrchestrator_DispatchGoesThroughClaim verifies each per-platform dispatch
// claims the (brief, platform) single-flight slot.
func TestOrchestrator_DispatchGoesThroughClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &claimCountingCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.cmu.Lock()
	defer camps.cmu.Unlock()
	if camps.claims != 2 {
		t.Errorf("ClaimCampaignDispatch called %d times, want 2 (one per platform)", camps.claims)
	}
}

// blockingDispatcher blocks until released, to test shutdown draining.
type blockingDispatcher struct {
	started chan struct{}
	release chan struct{}
}

func (d *blockingDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	close(d.started)
	<-d.release
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestOrchestrator_ShutdownDrainsInFlight verifies Shutdown waits for an
// in-flight dispatch to finish before returning.
func TestOrchestrator_ShutdownDrainsInFlight(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	disp := &blockingDispatcher{started: make(chan struct{}), release: make(chan struct{})}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started // dispatch is now in-flight

	shutdownReturned := make(chan error, 1)
	go func() {
		shutdownReturned <- orch.Shutdown(context.Background())
	}()

	// Shutdown must NOT return while the dispatch is blocked.
	select {
	case <-shutdownReturned:
		t.Fatal("Shutdown returned before in-flight dispatch finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(disp.release) // let dispatch complete
	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after dispatch completed")
	}
}

// TestOrchestrator_StartRejectedAfterShutdown verifies Start refuses new work
// once Shutdown has been initiated.
func TestOrchestrator_StartRejectedAfterShutdown(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: okDispatcher{},
	})
	if err := orch.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	if _, err := orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil); err == nil {
		t.Fatal("expected Start to be rejected after Shutdown")
	}
}

// TestFailStuckJobs verifies the recovery scan fails only non-terminal jobs.
func TestFailStuckJobs(t *testing.T) {
	jobs := newFakeJobRepo()
	jobs.jobs["j-queued"] = &model.CampaignJob{ID: "j-queued", Status: model.JobQueued}
	jobs.jobs["j-running"] = &model.CampaignJob{ID: "j-running", Status: model.JobRunning}
	jobs.jobs["j-done"] = &model.CampaignJob{ID: "j-done", Status: model.JobSucceeded}

	n, err := jobs.FailStuckJobs(context.Background(), "restarted")
	if err != nil {
		t.Fatalf("FailStuckJobs: %v", err)
	}
	if n != 2 {
		t.Errorf("failed %d jobs, want 2 (queued+running)", n)
	}
	if jobs.jobs["j-done"].Status != model.JobSucceeded {
		t.Errorf("terminal job was altered: %s", jobs.jobs["j-done"].Status)
	}
	if jobs.jobs["j-queued"].Status != model.JobFailed || jobs.jobs["j-running"].Status != model.JobFailed {
		t.Error("non-terminal jobs were not failed")
	}
}

// TestOrchestrator_ShutdownCancelsOnTimeout verifies that when the drain deadline
// expires, Shutdown cancels the in-flight run's context (rather than leaving it
// running against a closing pool).
func TestOrchestrator_ShutdownCancelsOnTimeout(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	dctx := <-ctxSeen

	// Drain with an already-past deadline so Shutdown times out immediately and
	// cancels the run context.
	deadctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = orch.Shutdown(deadctx)

	select {
	case <-dctx.Done():
		// good: the dispatch context was cancelled by Shutdown's timeout path.
	case <-time.After(time.Second):
		t.Error("dispatch context was not cancelled after drain timeout")
	}
	close(disp.release)
}

type ctxCapturingDispatcher struct {
	started chan struct{}
	release chan struct{}
	ctxSeen chan context.Context
}

func (d *ctxCapturingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.ctxSeen <- ctx
	close(d.started)
	<-d.release
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}
