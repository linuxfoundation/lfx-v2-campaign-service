// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShutdownBudgetComposes verifies the two graceful-shutdown phases sum to at
// most the overall budget, so the sequential srv.Shutdown then Container.Close
// can never overrun DefaultShutdownTimeout (which would risk a SIGKILL
// mid-drain). This guards the invariant the init() in container.go panics on.
func TestShutdownBudgetComposes(t *testing.T) {
	// The container-close phase reserves drain + post-cancel grace.
	assert.Equal(t, dispatchDrainTimeout+service.CancelGracePeriod, ContainerCloseTimeout)
	// The HTTP phase gets a positive share of the remaining budget.
	assert.Positive(t, HTTPShutdownTimeout, "HTTP shutdown phase must have a positive budget")
	// The two phases together stay within the overall budget.
	assert.LessOrEqual(t, HTTPShutdownTimeout+ContainerCloseTimeout, constants.DefaultShutdownTimeout)
}

// blockingDispatcher blocks until its context is cancelled, so Orchestrator.Shutdown
// hits the drain deadline and returns an error — letting the test prove Close
// propagates (rather than swallows) that error.
type blockingDispatcher struct{ started chan struct{} }

func (d *blockingDispatcher) Dispatch(ctx context.Context, _ *model.CampaignBrief, p model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return &model.Campaign{PlatformCampaignID: "pc-" + string(p)}, nil
}

// stubJobRepo is a minimal in-memory JobRepository for the Close test.
type stubJobRepo struct {
	mu   sync.Mutex
	seq  int
	jobs map[string]*model.CampaignJob
}

func newStubJobRepo() *stubJobRepo { return &stubJobRepo{jobs: map[string]*model.CampaignJob{}} }

func (r *stubJobRepo) create() (*model.CampaignJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	j := &model.CampaignJob{ID: "job-" + string(rune('a'+r.seq)), Status: model.JobQueued}
	r.jobs[j.ID] = j
	return j, nil
}
func (r *stubJobRepo) CreateJob(context.Context, string) (*model.CampaignJob, error) {
	return r.create()
}
func (r *stubJobRepo) CreateJobForApprovedBrief(context.Context, string, int64) (*model.CampaignJob, error) {
	return r.create()
}
func (r *stubJobRepo) GetJob(context.Context, string, string) (*model.CampaignJob, error) {
	return nil, domain.ErrNotFound
}
func (r *stubJobRepo) UpdateJobStatus(context.Context, string, model.JobStatus, []byte, string) error {
	return nil
}
func (r *stubJobRepo) FailStuckJobs(context.Context, string) (int64, error) { return 0, nil }

// stubCampaignRepo is a minimal in-memory CampaignRepository for the Close test.
type stubCampaignRepo struct{}

func (stubCampaignRepo) GetCampaign(context.Context, string, string, string) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}
func (stubCampaignRepo) GetCampaignByPlatform(context.Context, string, string, model.Provider) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}
func (stubCampaignRepo) ClaimCampaignDispatch(context.Context, string, string, model.Provider, string) (bool, *model.Campaign, error) {
	return true, &model.Campaign{Status: "pending"}, nil
}
func (stubCampaignRepo) DeleteDispatchClaim(context.Context, string, model.Provider) error {
	return nil
}
func (stubCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign) (*model.Campaign, error) {
	return c, nil
}
func (stubCampaignRepo) ReplaceCampaign(context.Context, *model.Campaign, int64) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}

// TestClose_PropagatesShutdownError verifies Container.Close returns (does not
// swallow) the orchestrator shutdown error when a dispatch is still running at
// drain time, so the caller's "container close error" branch can observe that
// dispatches were still running when the pool was closed. The pool must still be
// closed regardless (here the pool is nil, exercising the error-propagation path
// without a real database).
func TestClose_PropagatesShutdownError(t *testing.T) {
	disp := &blockingDispatcher{started: make(chan struct{}, 1)}
	orch := service.NewOrchestrator(stubCampaignRepo{}, newStubJobRepo(), map[model.Provider]service.PlatformDispatcher{
		model.ProviderGoogleAds: disp,
	})
	brief := &model.CampaignBrief{ID: "b1", ProjectID: "cncf"}
	if _, err := orch.Start(context.Background(), brief, brief.Version, []model.Provider{model.ProviderGoogleAds}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-disp.started // dispatch is in-flight and will block until its ctx is cancelled

	c := &Container{orch: orch} // nil pool: exercise the error path without a DB

	// A short outer budget forces the drain to time out and the grace to expire
	// with the dispatch still blocked, so Shutdown returns a non-nil error.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Close(ctx)
	if err == nil {
		t.Fatal("Close returned nil; a shutdown timeout (dispatches still running) must be observable to the caller")
	}
}

func TestNewContainer_NoDatabase(t *testing.T) {
	cfg := &config.Config{
		Host: "*",
		Port: "8080",
	}

	cont, err := NewContainer(cfg)
	require.NoError(t, err)
	require.NotNil(t, cont)
	assert.NotNil(t, cont.Service)
	assert.NotNil(t, cont.Connections)
	require.NoError(t, cont.Close(context.Background()))
}

func TestNewContainer_UnsupportedEngine(t *testing.T) {
	cfg := &config.Config{
		Host:       "*",
		Port:       "8080",
		PGHost:     "localhost",
		PGPort:     "5432",
		PGUser:     "app",
		PGDatabase: "campaign",
		PGEngine:   "mysql",
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported database engine")
	assert.NotContains(t, err.Error(), "password=")
}

func TestNewContainer_IncompletePGSettings(t *testing.T) {
	cfg := &config.Config{
		Host:   "*",
		Port:   "8080",
		PGHost: "localhost",
		PGUser: "app",
		// missing PGDatabase / password → validation error
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database configuration")
	assert.Contains(t, err.Error(), "PGDATABASE")
	assert.Contains(t, err.Error(), "PGPASSWORD")
	assert.NotContains(t, err.Error(), "password=")
}
