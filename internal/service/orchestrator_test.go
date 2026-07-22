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
	mu             sync.Mutex
	jobs           map[string]*model.CampaignJob
	counter        int
	failStuckCalls int
}

func newFakeJobRepo() *fakeJobRepo { return &fakeJobRepo{jobs: map[string]*model.CampaignJob{}} }

func (r *fakeJobRepo) CreateJob(_ context.Context, briefID string) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(briefID)
}

// createLocked inserts a queued job. Caller must hold r.mu.
func (r *fakeJobRepo) createLocked(briefID string) (*model.CampaignJob, error) {
	r.counter++
	id := "job-" + string(rune('a'+r.counter))
	j := &model.CampaignJob{ID: id, BriefID: briefID, Status: model.JobQueued}
	r.jobs[id] = j
	return j, nil
}

// CreateJobForApprovedBrief mirrors the unconditional create for the orchestrator
// tests (which don't wire a brief store, so the approval guard is exercised
// separately by the brief-service TOCTOU test with its own version-aware fake).
func (r *fakeJobRepo) CreateJobForApprovedBrief(_ context.Context, briefID string, _ int64) (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createLocked(briefID)
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

func (r *fakeJobRepo) failStuckCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failStuckCalls
}

func (r *fakeJobRepo) FailStuckJobs(_ context.Context, jobErr string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStuckCalls++
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

func (r *fakeCampaignRepo) GetCampaignByPlatform(_ context.Context, _ string, briefID string, platform model.Provider) (*model.Campaign, error) {
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

func (r *fakeCampaignRepo) DeleteDispatchClaim(_ context.Context, briefID string, platform model.Provider) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := briefID + "|" + string(platform)
	if c, ok := r.existing[key]; ok && c.Status == "pending" {
		delete(r.existing, key)
	}
	return nil
}

func (r *fakeCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign) (*model.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c.Version = 1
	r.upserted = append(r.upserted, c)
	// Mirror the real ON CONFLICT (brief_id, platform) DO UPDATE: the (brief,
	// platform) row is updated in place, so a subsequent lookup sees the new
	// platform_campaign_id/status.
	if r.existing == nil {
		r.existing = map[string]*model.Campaign{}
	}
	r.existing[c.BriefID+"|"+string(c.Platform)] = c
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

// waitForFinalized waits until the run's finalize write has landed (a non-empty
// result recorded), regardless of whether the resulting status is terminal. A
// job whose only outcomes are single-flight SKIPs finalizes to a non-terminal
// 'running' status (its skipped pairs are owned by another dispatch), so such a
// job never satisfies waitForTerminal — this helper observes its finalize instead.
func waitForFinalized(t *testing.T, jobs *fakeJobRepo, id string) *model.CampaignJob {
	t.Helper()
	for i := 0; i < 100; i++ {
		j, _ := jobs.GetJob(context.Background(), "", id)
		if len(j.Result) > 0 {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job result was never finalized")
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
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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

// TestOrchestrator_IdempotencyLookupErrorIsFailure verifies that a REAL DB error
// from the idempotency lookup (GetCampaignByPlatform) — anything other than
// ErrNotFound — is surfaced as a platform failure and the dispatcher is never
// called. Otherwise a transient read failure would be treated like "no existing
// campaign" and dispatch could duplicate an existing-but-unloaded campaign.
func TestOrchestrator_IdempotencyLookupErrorIsFailure(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{byPlatformErr: errors.New("db connection reset")}
	disp := &countingDispatcher{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (a lookup error must not fall through to dispatch)", calls)
	}
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (no create on a lookup error)", len(camps.upserted))
	}
}

// TestOrchestrator_AlreadyClaimedPendingSkips verifies that when another worker
// holds the pending claim (no upstream id yet), this worker does not dispatch and
// the skip is NOT recorded as a terminal failure: a single skipped platform is a
// deferral to the owning dispatch, so the job terminalizes as succeeded (not
// failed, which would be spurious; not left running, which the staleness sweeper
// would later fail), rather than the old behavior of falsely failing it.
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	// A skipped platform (owned by a concurrent dispatch) is a deferral, not a
	// failure — the job terminalizes as SUCCEEDED (not stuck-running, which the
	// recovery sweeper would later fail; not failed, which would be spurious).
	j := waitForFinalized(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (a lone skipped platform is a deferral, terminalizes succeeded)", j.Status)
	}
	disp.mu.Lock()
	calls := disp.calls
	disp.mu.Unlock()
	if calls != 0 {
		t.Errorf("Dispatch called %d times, want 0 (another worker holds the claim)", calls)
	}
	if !strings.Contains(string(j.Result), "another concurrent dispatch owns") {
		t.Errorf("result = %s, want a concurrent-owner skip message", j.Result)
	}
	if !strings.Contains(string(j.Result), "\"skipped\":true") {
		t.Errorf("result = %s, want the platform marked skipped:true", j.Result)
	}
}

// TestClaimCampaignDispatch_ConcurrentSingleWinner exercises the ACTUAL race the
// single-flight claim guards against: N goroutines calling ClaimCampaignDispatch
// for the SAME (brief, platform) at the same time. Exactly one must win
// (claimed=true) and every loser must cleanly observe claimed=false with no error
// and the SAME pending row — the ON CONFLICT (brief_id, platform) DO NOTHING
// arbitration the design leans on. The prior claim tests only pre-seed a claimed
// row and call Start once, so they never run two claimers concurrently.
func TestClaimCampaignDispatch_ConcurrentSingleWinner(t *testing.T) {
	// The fake repo models ON CONFLICT DO NOTHING under a mutex: first caller
	// inserts + returns claimed=true; every later caller sees the existing row and
	// returns claimed=false — the same arbitration Postgres provides.
	repo := &fakeCampaignRepo{}

	const n = 32
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		mu     sync.Mutex
		wins   int
		errs   int
		rowIDs = map[*model.Campaign]struct{}{}
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			claimed, row, err := repo.ClaimCampaignDispatch(
				context.Background(), "cncf", "b1", model.ProviderGoogleAds, "job1")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			if claimed {
				wins++
			}
			if row != nil {
				rowIDs[row] = struct{}{}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if errs != 0 {
		t.Errorf("got %d errors; every claimer (winner or loser) must return nil error", errs)
	}
	if wins != 1 {
		t.Errorf("exactly one goroutine must win the claim, got %d winners", wins)
	}
	if len(rowIDs) != 1 {
		t.Errorf("all claimers must observe the SAME pending row, got %d distinct rows", len(rowIDs))
	}
}

// TestOrchestrator_SkipDoesNotFailAlongsideSuccess verifies that when one
// platform succeeds and another is skipped (owned by a concurrent dispatch), the
// job is not falsely reported failed/partial: with a real success and no real
// failure, an outstanding skip (a deferral to the owner) terminalizes the job as
// succeeded rather than a spurious failure/partial or a stuck running state.
func TestOrchestrator_SkipDoesNotFailAlongsideSuccess(t *testing.T) {
	jobs := newFakeJobRepo()
	// LinkedIn is already held pending by another dispatch; Google Ads is free.
	camps := &fakeCampaignRepo{existing: map[string]*model.Campaign{
		"b1|" + string(model.ProviderLinkedInAds): {ID: "c1", Status: "pending", PlatformCampaignID: ""},
	}}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds:   okDispatcher{},
		model.ProviderLinkedInAds: okDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
	j := waitForFinalized(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded (a skip alongside a success terminalizes succeeded, not failed/partial/stuck)", j.Status)
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
		// A single-flight SKIP is a deferral to the owning dispatch, not a failure
		// and not this job's work to finish: it terminalizes as succeeded (not stuck
		// running, which the sweeper would later fail).
		{"only skipped", []platformResult{{Skipped: true}}, model.JobSucceeded},
		{"skip + ok", []platformResult{{OK: true}, {Skipped: true}}, model.JobSucceeded},
		// A real failure still surfaces even when another platform was skipped.
		{"skip + fail", []platformResult{{OK: false}, {Skipped: true}}, model.JobPartial},
		{"ok + fail + skip", []platformResult{{OK: true}, {OK: false}, {Skipped: true}}, model.JobPartial},
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
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
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds, model.ProviderLinkedInAds}, nil)
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
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started // dispatch is now in-flight

	shutdownReturned := make(chan error, 1)
	go func() {
		shutdownReturned <- orch.Shutdown(context.Background(), 5*time.Second)
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

// TestOrchestrator_ShutdownGraceHonorsContextCancel verifies that during the
// post-cancel grace wait, a caller CANCEL of ctx (not just its deadline) ends
// the wait promptly instead of blocking the full CancelGracePeriod.
func TestOrchestrator_ShutdownGraceHonorsContextCancel(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	<-ctxSeen

	// A cancelable ctx with no deadline; the dispatch never releases, so Shutdown
	// enters the grace wait. Cancelling ctx must unblock it well before the full
	// CancelGracePeriod.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(ctx, 10*time.Millisecond) }()

	// Let the drain window elapse so Shutdown is in the grace wait, then cancel.
	time.Sleep(40 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after ctx cancel during grace")
	}
	if elapsed := time.Since(start); elapsed >= CancelGracePeriod {
		t.Errorf("grace wait took %v after cancel; did not observe ctx cancellation", elapsed)
	}
	close(disp.release)
}

// TestOrchestrator_RecoverySweeperStopsOnShutdown verifies the background
// recovery sweeper is tracked by the wait group and stops promptly on Shutdown
// (it must not block the drain until a ticker fires), so Shutdown returns
// quickly with no in-flight dispatch.
func TestOrchestrator_RecoverySweeperStopsOnShutdown(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil)
	orch.StartRecoverySweeper()

	done := make(chan error, 1)
	go func() { done <- orch.Shutdown(context.Background(), 5*time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return promptly; sweeper likely blocked the drain")
	}
	// The sweeper interval (5m) is far longer than this test, so FailStuckJobs
	// must not have been called by a tick — it stopped on the stop signal.
	if c := jobs.failStuckCallCount(); c != 0 {
		t.Errorf("FailStuckJobs called %d times; sweeper should have stopped before any tick", c)
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
	if err := orch.Shutdown(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	if _, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil); err == nil {
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
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	dctx := <-ctxSeen

	// Drain with an already-past deadline so Shutdown times out immediately and
	// cancels the run context.
	deadctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = orch.Shutdown(deadctx, time.Millisecond)

	select {
	case <-dctx.Done():
		// good: the dispatch context was cancelled by Shutdown's timeout path.
	case <-time.After(time.Second):
		t.Error("dispatch context was not cancelled after drain timeout")
	}
	close(disp.release)
}

// TestOrchestrator_ShutdownGraceBoundedByContext verifies that when the drain
// deadline elapses while a dispatch is still stuck, the post-cancel grace wait
// does not exceed the caller's context budget (it must not add a full,
// wall-clock CancelGracePeriod on top of an already-expired deadline).
func TestOrchestrator_ShutdownGraceBoundedByContext(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	<-ctxSeen // drain the captured ctx so Dispatch can proceed to <-release

	// Short drain budget; the dispatch never releases, so Shutdown must hit the
	// deadline path and then wait at most the remaining budget for the grace,
	// NOT the full CancelGracePeriod (which is >> this budget).
	const budget = 50 * time.Millisecond
	deadctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_ = orch.Shutdown(deadctx, time.Millisecond)
	elapsed := time.Since(start)

	// Allow generous slack for scheduling, but it must be far below the full
	// wall-clock CancelGracePeriod that the old (unbounded) timer would impose.
	if elapsed >= CancelGracePeriod {
		t.Errorf("Shutdown waited %v (>= full CancelGracePeriod %v); grace not bounded by context", elapsed, CancelGracePeriod)
	}
	close(disp.release)
}

// TestOrchestrator_ShutdownGivesGraceWhenBudgetRemains verifies the two phases
// are budgeted separately: when the drain window elapses but the OUTER ctx still
// has budget, Shutdown actually spends a post-cancel grace waiting for the
// cancelled dispatch to unwind — it must NOT return immediately (which would let
// Container.Close close the pool mid-finalize). This guards the regression where
// Close passed a ctx limited to only the drain timeout, leaving zero grace.
func TestOrchestrator_ShutdownGivesGraceWhenBudgetRemains(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	ctxSeen := make(chan context.Context, 1)
	disp := &ctxCapturingDispatcher{started: make(chan struct{}), release: make(chan struct{}), ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	_, _ = orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-disp.started
	dctx := <-ctxSeen // the dispatch's own context, cancelled by rootCancel

	// Outer budget comfortably exceeds the tiny drain window, so after drain
	// times out there is real grace budget left.
	outerCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	graceObserved := make(chan struct{})
	go func() {
		// The dispatch releases only once it observes its context cancellation —
		// i.e. during the grace phase, proving grace actually ran.
		<-dctx.Done()
		close(graceObserved)
		close(disp.release)
	}()

	start := time.Now()
	_ = orch.Shutdown(outerCtx, 20*time.Millisecond)
	elapsed := time.Since(start)

	select {
	case <-graceObserved:
	default:
		t.Fatal("dispatch context was never cancelled; grace phase did not run")
	}
	// Shutdown must have waited past the drain window (the grace phase happened),
	// but well within the outer budget.
	if elapsed < 20*time.Millisecond {
		t.Errorf("Shutdown returned in %v, before the drain window elapsed; grace phase was skipped", elapsed)
	}
	if elapsed >= time.Second {
		t.Errorf("Shutdown waited %v, at/over the full outer budget", elapsed)
	}
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

// TestOrchestrator_NoDispatcherDoesNotLeavePendingClaim verifies that when no
// dispatcher is registered, no pending claim row is left behind (which would
// permanently block the pair).
func TestOrchestrator_NoDispatcherDoesNotLeavePendingClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil) // no dispatchers
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// No claim should have been inserted (dispatcher checked first), so existing
	// is empty and no pending row blocks the pair.
	if _, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]; ok {
		t.Error("a pending claim row was left behind for a platform with no dispatcher")
	}
}

// preCreateErrDispatcher fails with an error that signals no upstream create.
type preCreateErr struct{}

func (preCreateErr) Error() string          { return "invalid input" }
func (preCreateErr) NoUpstreamCreate() bool { return true }

type preCreateErrDispatcher struct{}

func (preCreateErrDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, preCreateErr{}
}

// TestOrchestrator_PreCreateErrorReleasesClaim verifies that a dispatcher error
// signalling no-upstream-create releases the claim (so the pair can be retried),
// unlike an ambiguous error which retains it.
func TestOrchestrator_PreCreateErrorReleasesClaim(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: preCreateErrDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	waitForTerminal(t, jobs, id)
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The pre-create error should have released the pending claim.
	if _, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]; ok {
		t.Error("pre-create dispatcher error should have released the pending claim")
	}
}

// partialResultDispatcher exercises the platform clients' partial-result
// contract: it returns a non-nil campaign carrying the created upstream id
// ALONGSIDE a (non-pre-create) error, as reddit/twitter clients do when the
// campaign POST succeeded but a later step failed.
type partialResultDispatcher struct{}

func (partialResultDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "pc-orphan-" + string(p), Status: "active", CampaignName: "n"},
		errors.New("ad group creation failed after campaign was created")
}

// TestOrchestrator_PartialDispatchErrorPersistsUpstreamID verifies that when
// Dispatch returns a partial campaign (a created upstream id) together with an
// error, the retained pending row is stamped with that upstream id so the
// orphaned upstream campaign is reconcilable — and the claim is NOT released.
func TestOrchestrator_PartialDispatchErrorPersistsUpstreamID(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderGoogleAds: partialResultDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobFailed {
		t.Errorf("status = %s, want failed", j.Status)
	}
	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The claim must be RETAINED (not released) — the upstream campaign may exist.
	row, ok := camps.existing["b1|"+string(model.ProviderGoogleAds)]
	if !ok {
		t.Fatal("pending claim should be retained after a partial dispatch error, not released")
	}
	// The retained row must now carry the orphaned upstream id (reconcilable) and
	// remain 'pending' (a recoverable orphan, not a completed campaign).
	if row.PlatformCampaignID != "pc-orphan-"+string(model.ProviderGoogleAds) {
		t.Errorf("retained row PlatformCampaignID = %q, want the orphaned upstream id", row.PlatformCampaignID)
	}
	if row.Status != "pending" {
		t.Errorf("retained row Status = %q, want pending (recoverable orphan)", row.Status)
	}
	// The partial campaign must have been persisted via UpsertCampaign.
	if len(camps.upserted) != 1 {
		t.Errorf("upserted %d campaigns, want 1 (partial upstream id persisted)", len(camps.upserted))
	}
}

// groupOrphanDispatcher models LinkedIn's group-created-but-campaign-failed case:
// it returns a non-nil campaign with an EMPTY PlatformCampaignID (deliberately, so
// the idempotency fast-path doesn't false-succeed) but a NON-EMPTY Result carrying
// the orphaned group id, alongside a retained (non-pre-create) error.
type groupOrphanDispatcher struct{}

func (groupOrphanDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{
			PlatformCampaignID: "", // no campaign was created — only the group
			CampaignName:       "Events | KubeCon | cncf",
			Status:             "group_created",
			Result:             json.RawMessage(`{"campaignGroupId":"urn:li:sponsoredCampaignGroup:500"}`),
		},
		errors.New("linkedin campaign creation incomplete (a campaign group may exist)")
}

// TestOrchestrator_GroupOrphanPartialIsPersisted verifies the fix for the HIGH
// group-orphan finding: a retained partial with an EMPTY PlatformCampaignID but a
// non-empty Result (the orphaned group id) must still be PERSISTED — previously it
// was dropped (persist only fired for a non-empty PlatformCampaignID), leaving the
// group orphan unrecorded and the pending claim blocking the pair with no trace.
func TestOrchestrator_GroupOrphanPartialIsPersisted(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderLinkedInAds: groupOrphanDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)

	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The claim must be RETAINED (the group exists — a blind retry could duplicate it).
	row, ok := camps.existing["b1|"+string(model.ProviderLinkedInAds)]
	if !ok {
		t.Fatal("pending claim should be retained after a group-orphan dispatch error")
	}
	// PlatformCampaignID stays empty (no campaign was created), which keeps the row
	// OUT of the idempotency fast-path (it keys on a non-empty id) so it can't
	// false-succeed. NOTE: the row is not automatically re-attempted either — a later
	// job's ClaimCampaignDispatch conflicts with this retained pending row and skips
	// dispatch; the row stays blocked pending reconciliation / resume support
	// (LFXV2-2665). This PR only makes the orphan RECORDED, not auto-resumed.
	if row.PlatformCampaignID != "" {
		t.Errorf("retained row PlatformCampaignID = %q, want empty (no campaign created)", row.PlatformCampaignID)
	}
	// The group orphan MUST be recorded: the Result blob carrying the group id must
	// have been persisted (this is the fix — previously dropped).
	if len(camps.upserted) != 1 {
		t.Fatalf("upserted %d campaigns, want 1 (the group orphan must be persisted, not dropped)", len(camps.upserted))
	}
	if !strings.Contains(string(row.Result), "sponsoredCampaignGroup:500") {
		t.Errorf("retained row Result must carry the orphaned group id for reconciliation, got %q", row.Result)
	}
	if row.Status != "pending" {
		t.Errorf("retained row Status = %q, want pending (recoverable orphan)", row.Status)
	}
}

// contentlessPartialDispatcher returns a non-nil campaign with NEITHER an upstream id
// NOR a Result blob, alongside a retained (non-pre-create) error — the exact case the
// widened persist guard must REJECT (nothing to record → "outcome unknown", no upsert).
type contentlessPartialDispatcher struct{}

func (contentlessPartialDispatcher) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return &model.Campaign{PlatformCampaignID: "", CampaignName: "n"}, // no id, no Result
		errors.New("dispatch failed with no recordable upstream detail")
}

// TestOrchestrator_ContentlessPartialIsNotPersisted pins the FALSE branch of the
// widened persist guard: a retained partial that carries neither an upstream id nor a
// Result blob has nothing worth recording, so it must NOT be upserted (it's logged as
// "outcome unknown" and the claim is retained). Without this, a future refactor that
// dropped the id/Result check from the guard would still pass every other test while
// silently writing anonymous, content-free pending rows.
func TestOrchestrator_ContentlessPartialIsNotPersisted(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{
		model.ProviderLinkedInAds: contentlessPartialDispatcher{},
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderLinkedInAds}, nil)
	waitForTerminal(t, jobs, id)

	camps.mu.Lock()
	defer camps.mu.Unlock()
	// The claim is RETAINED (outcome unknown; a blind retry could double-create).
	if _, ok := camps.existing["b1|"+string(model.ProviderLinkedInAds)]; !ok {
		t.Fatal("claim should be retained after a contentless partial error")
	}
	// But NOTHING is upserted — there is no orphan detail to record.
	if len(camps.upserted) != 0 {
		t.Errorf("upserted %d campaigns, want 0 (a content-free partial must not be persisted)", len(camps.upserted))
	}
}

// blockingSweepJobRepo blocks inside FailStuckJobs until its context is
// cancelled, letting a test prove that cancelling the sweeper's context
// interrupts an in-flight sweep promptly (rather than the sweep running to its
// own timeout against a closing pool).
type blockingSweepJobRepo struct {
	fakeJobRepo
	entered chan struct{}
}

func (r *blockingSweepJobRepo) FailStuckJobs(ctx context.Context, _ string) (int64, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	<-ctx.Done() // block until the sweeper's context is cancelled
	return 0, ctx.Err()
}

// TestOrchestrator_SweeperInterruptedOnShutdown verifies that a sweep already
// blocked in the DB is interrupted PROMPTLY when Shutdown cancels the sweeper's
// dedicated context, and that Shutdown still completes within budget. Uses a
// tiny sweep interval so a sweep starts quickly, and a repo whose FailStuckJobs
// blocks until its context is cancelled.
func TestOrchestrator_SweeperInterruptedOnShutdown(t *testing.T) {
	jobs := &blockingSweepJobRepo{
		fakeJobRepo: fakeJobRepo{jobs: map[string]*model.CampaignJob{}},
		entered:     make(chan struct{}, 1),
	}
	camps := &fakeCampaignRepo{}
	orch := NewOrchestrator(camps, jobs, nil)
	// Drive the sweeper on a very short interval so a sweep begins promptly. The
	// sweeper reads sweeperCtx, so overriding the interval doesn't affect the
	// cancellation path under test.
	orch.sweeperCtx, orch.sweeperCancel = context.WithCancel(context.Background())
	orch.wg.Add(1)
	go func() {
		defer orch.wg.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-orch.sweeperCtx.Done():
				return
			case <-ticker.C:
				sctx, cancel := context.WithTimeout(orch.sweeperCtx, jobFinalizeTimeout)
				_, _ = jobs.FailStuckJobs(sctx, "x")
				cancel()
			}
		}
	}()

	// Wait until a sweep is actually blocked inside FailStuckJobs.
	select {
	case <-jobs.entered:
	case <-time.After(time.Second):
		t.Fatal("sweep never started")
	}

	// Shutdown must cancel the sweeper's context (interrupting the blocked sweep)
	// and return quickly — well under the jobFinalizeTimeout the sweep would
	// otherwise wait for.
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- orch.Shutdown(context.Background(), 5*time.Second) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Shutdown err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return; blocked sweep was not interrupted")
	}
	if elapsed := time.Since(start); elapsed >= jobFinalizeTimeout {
		t.Errorf("Shutdown took %v (>= jobFinalizeTimeout %v); sweep was not interrupted promptly", elapsed, jobFinalizeTimeout)
	}
}

// ctxAssertingCampaignRepo asserts UpsertCampaign is invoked with a live (not
// cancelled) context — proving the post-provider persist runs on a context
// detached from the cancelled dispatch context.
type ctxAssertingCampaignRepo struct {
	fakeCampaignRepo
	upsertCtxErr error // context error observed inside UpsertCampaign
	upsertCalled chan struct{}
}

func (r *ctxAssertingCampaignRepo) UpsertCampaign(ctx context.Context, c *model.Campaign) (*model.Campaign, error) {
	r.mu.Lock()
	r.upsertCtxErr = ctx.Err()
	r.mu.Unlock()
	close(r.upsertCalled)
	return r.fakeCampaignRepo.UpsertCampaign(ctx, c)
}

// TestOrchestrator_PersistSurvivesDispatchCancel verifies that a provider result
// completing AFTER the dispatch context is cancelled (the phase-two shutdown
// grace) is still persisted: the upsert must run on a detached context, not the
// cancelled dispatch context, so the record of the created upstream campaign is
// not lost.
func TestOrchestrator_PersistSurvivesDispatchCancel(t *testing.T) {
	jobs := newFakeJobRepo()
	camps := &ctxAssertingCampaignRepo{upsertCalled: make(chan struct{})}
	ctxSeen := make(chan context.Context, 1)
	// The dispatcher returns its campaign only after observing cancellation, so the
	// persist step necessarily runs while the dispatch ctx is already cancelled.
	disp := &cancelThenReturnDispatcher{ctxSeen: ctxSeen}
	orch := NewOrchestrator(camps, jobs, map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: disp})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	id, _ := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil)
	<-ctxSeen // dispatch is in-flight

	// Drain with an already-past deadline so Shutdown immediately cancels the run's
	// context, but give a real outer budget so the grace phase lets it finish.
	outerCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = orch.Shutdown(outerCtx, time.Millisecond) }()

	// The upsert must be reached and must see a LIVE context (detached), then the
	// job reaches a terminal state with the campaign persisted.
	select {
	case <-camps.upsertCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("UpsertCampaign was never called; persist did not survive dispatch cancel")
	}
	camps.mu.Lock()
	upsertErr := camps.upsertCtxErr
	upsertCount := len(camps.upserted)
	camps.mu.Unlock()
	if upsertErr != nil {
		t.Errorf("UpsertCampaign ran on a cancelled context (%v); persist must use a detached context", upsertErr)
	}
	if upsertCount != 1 {
		t.Errorf("persisted %d campaigns, want 1 (created upstream campaign must be recorded)", upsertCount)
	}
	j := waitForTerminal(t, jobs, id)
	if j.Status != model.JobSucceeded {
		t.Errorf("status = %s, want succeeded", j.Status)
	}
}

// cancelThenReturnDispatcher waits until its context is cancelled, then returns a
// successful campaign — forcing the orchestrator's persist step to run while the
// dispatch context is already cancelled.
type cancelThenReturnDispatcher struct {
	ctxSeen chan context.Context
}

func (d *cancelThenReturnDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	d.ctxSeen <- ctx
	<-ctx.Done() // return only after Shutdown cancels the dispatch context
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p), Status: "active", CampaignName: "n"}, nil
}

// TestBriefETagIsQuoted verifies the emitted ETag is a quoted entity-tag.
func TestBriefETagIsQuoted(t *testing.T) {
	if got := briefETag(3); got != `"3"` {
		t.Errorf("briefETag(3) = %q, want \"3\"", got)
	}
	// And the parser round-trips it.
	v, err := parseBriefIfMatch(strPtr(briefETag(7)))
	if err != nil || v != 7 {
		t.Errorf("round-trip of briefETag(7) = %d, %v; want 7, nil", v, err)
	}
}

func strPtr(s string) *string { return &s }
