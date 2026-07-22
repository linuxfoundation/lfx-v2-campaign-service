// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validEncryptionKey is a base64-encoded 32-byte AES-256 key for tests (not a
// secret; all-zero bytes).
func validEncryptionKey() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

// shrinkDBTimers shrinks the DB-init timers for the duration of a test so the
// cold-start-retry path doesn't wait real seconds.
func shrinkDBTimers(t *testing.T) {
	t.Helper()
	origTimeout, origInterval := startupDBTimeout, dbRetryInterval
	startupDBTimeout = 200 * time.Millisecond
	dbRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() {
		startupDBTimeout = origTimeout
		dbRetryInterval = origInterval
	})
}

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

// TestRegisterDispatchers_RegistersReddit guards the one line that makes this PR's
// production fix real: the reddit dispatcher must be mapped to ProviderRedditAds. A
// regression that drops it from the map would silently restore the "no dispatcher
// registered" behavior with every other test still green (the adapter is unit-tested
// by instantiating RedditDispatcher directly, which bypasses the map). registerDispatchers
// only stores its args, so nil repo/encryptor build the map without a deref.
func TestRegisterDispatchers_RegistersReddit(t *testing.T) {
	m := registerDispatchers(nil, nil)
	_, ok := m[model.ProviderRedditAds]
	assert.True(t, ok, "ProviderRedditAds must be registered — this is the wiring the PR adds")
}

// TestLogMissingDispatchers_SurfacesGaps verifies logMissingDispatchers actually
// flags a known ad provider that has no adapter, so the startup gap stays visible. It
// asserts on the EMITTED LOG OUTPUT (captured via a buffer-backed default handler), not
// a recomputed copy of the function's loop — so it fails if logMissingDispatchers were
// gutted to a no-op (per @dealako's review).
func TestLogMissingDispatchers_SurfacesGaps(t *testing.T) {
	// Capture the ACTUAL slog output (not a recomputed copy of the loop) so the test
	// verifies logMissingDispatchers's behavior — it would fail if the function were
	// gutted to a no-op. Swap the default logger to a buffer-backed handler for the call.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m := registerDispatchers(nil, nil) // registers reddit (+ linkedin/meta/twitter as they land)
	logMissingDispatchers(m)

	out := buf.String()
	assert.Contains(t, out, "no dispatcher registered", "the warning message must be emitted when providers are missing")
	// The registered provider (reddit) must NOT be logged as missing...
	assert.NotContains(t, out, string(model.ProviderRedditAds), "reddit is registered, so it must not be logged as missing")
	// ...and at least one genuinely-unregistered known provider MUST be named.
	assert.Contains(t, out, string(model.ProviderMicrosoftAds), "an unregistered known provider (microsoft-ads) must be surfaced in the log")
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
	assert.NotNil(t, cont.Briefs)
	// The audiences service is wired with a nil repo so its routes stay mounted and
	// return the typed 503 advertised by the contract, not a bare 404. Prove that by
	// exercising a handler and asserting the typed ServiceUnavailable error.
	require.NotNil(t, cont.Audiences)
	_, aerr := cont.Audiences.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "proj-1", BriefID: "brief-1", Audience: &audiences.AudienceInput{Platform: "meta"},
	})
	var unavail *audiences.ConnServiceUnavailableError
	require.ErrorAs(t, aerr, &unavail, "audiences must return the typed 503 when no DB is configured")

	// Late-binding: once a backend is set (as the cold-start retry does), the same
	// handler stops returning 503 and reaches the repo.
	cont.Audiences.(audienceBackendSetter).SetBackend(fakeAudienceRepo{})
	got, aerr := cont.Audiences.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "proj-1", BriefID: "brief-1", Audience: &audiences.AudienceInput{Platform: "meta"},
	})
	require.NoError(t, aerr, "after SetBackend the audiences handler must reach the repo")
	require.NotNil(t, got)

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

// TestNewContainer_UnreachableDBBootsIn503Mode verifies the cold-start fix: when
// the database is configured but unreachable, NewContainer does NOT fail — it
// returns a wired container (503 mode) so the process boots, and a background
// goroutine retries. This is what makes the startupProbe budget real.
func TestNewContainer_UnreachableDBBootsIn503Mode(t *testing.T) {
	shrinkDBTimers(t)
	cfg := &config.Config{
		Host: "*",
		Port: "8080",
		// Port 1 has nothing listening → connection refused (transient, retryable).
		DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
		CredentialEncryptionKey: validEncryptionKey(),
	}

	cont, err := NewContainer(cfg)
	require.NoError(t, err, "an unreachable DB must NOT fail startup — boot in 503 mode")
	require.NotNil(t, cont)
	assert.NotNil(t, cont.Service, "campaign service must be wired (reports not-ready)")
	assert.NotNil(t, cont.Connections, "connection service must be wired (returns 503)")
	assert.NotNil(t, cont.Briefs, "brief service must be wired in 503 mode (its routes return 503, not a nil panic)")
	// The audiences service must also be wired in 503 mode and return the typed 503
	// (not a nil-repo panic) until the cold-start retry late-binds a real backend.
	require.NotNil(t, cont.Audiences, "audiences service must be wired in 503 mode")
	_, aerr := cont.Audiences.CreateAudience(context.Background(), &audiences.CreateAudiencePayload{
		ProjectID: "proj-1", BriefID: "brief-1", Audience: &audiences.AudienceInput{Platform: "meta"},
	})
	var unavail *audiences.ConnServiceUnavailableError
	require.ErrorAs(t, aerr, &unavail, "during a cold start audiences must return the typed 503")
	// The health service must report NOT ready while the pool is still coming up
	// (distinct from no-DB mode, which reports ready).
	assert.False(t, cont.Service.(interface{ ServiceReady() bool }).ServiceReady(),
		"during a cold start /readyz must report not-ready, not OK")
	// Close must stop the background goroutine cleanly (no hang, no panic).
	require.NoError(t, cont.Close(context.Background()))
}

// TestNewContainer_BadEncryptionKeyFailsFast verifies a config error (not a
// transient DB problem) still fails fast — the process should exit, not boot.
func TestNewContainer_BadEncryptionKeyFailsFast(t *testing.T) {
	shrinkDBTimers(t)
	cfg := &config.Config{
		Host:                    "*",
		Port:                    "8080",
		DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
		CredentialEncryptionKey: "not-a-valid-base64-32-byte-key",
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential encryptor")
}

// TestNewContainer_MalformedDSNFailsFast verifies a keyword-form DATABASE_URL (a
// deterministic config error no retry can fix) fails fast rather than entering the
// 503-mode retry loop — distinct from a transient unreachable DB, which boots 503.
func TestNewContainer_MalformedDSNFailsFast(t *testing.T) {
	shrinkDBTimers(t)
	cfg := &config.Config{
		Host: "*",
		Port: "8080",
		// A keyword DSN migrations can't consume — deterministic, not transient.
		DatabaseURL:             "host=127.0.0.1 user=app dbname=campaign",
		CredentialEncryptionKey: validEncryptionKey(),
	}

	cont, err := NewContainer(cfg)
	assert.Nil(t, cont, "a malformed DSN must fail fast, not boot in 503 mode")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database configuration")
}

// TestNotReady verifies the cold-start health placeholder always reports
// not-ready (so /readyz stays 503 until the real pool is swapped in).
func TestNotReady(t *testing.T) {
	assert.False(t, notReady{}.Ready(context.Background()))
}

// fakeAudienceRepo is a minimal domain.AudienceRepository for the container's
// late-binding assertion: CreateAudience echoes the row back so the handler's
// success path (audienceResult) runs without a real database.
type fakeAudienceRepo struct{}

func (fakeAudienceRepo) CreateAudience(_ context.Context, a *model.CampaignAudience) (*model.CampaignAudience, error) {
	return a, nil
}

func (fakeAudienceRepo) GetAudience(_ context.Context, _, _, _ string) (*model.CampaignAudience, error) {
	return &model.CampaignAudience{}, nil
}

func (fakeAudienceRepo) ListAudiences(_ context.Context, _, _ string) ([]*model.CampaignAudience, error) {
	return nil, nil
}

func (fakeAudienceRepo) UpdateAudience(_ context.Context, a *model.CampaignAudience, _ int64) (*model.CampaignAudience, error) {
	return a, nil
}
