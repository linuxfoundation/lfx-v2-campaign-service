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

// registeredProviders is the set of providers registerDispatchers wires up on this
// branch (one entry lands per adapter PR: reddit, linkedin, meta, twitter, googleads, hubspot). Each is
// guarded so dropping its map entry — the production wiring each PR adds — fails a test rather
// than silently restoring "no dispatcher registered" (the adapters are unit-tested by
// direct instantiation, which bypasses the map).
var registeredProviders = []model.Provider{
	model.ProviderRedditAds,
	model.ProviderLinkedInAds,
	model.ProviderMetaAds,
	model.ProviderTwitterAds,
	model.ProviderGoogleAds,
	model.ProviderHubSpot,
	model.ProviderMicrosoftAds,
}

// TestRegisterDispatchers_RegistersProviders asserts EXACTLY the expected providers map to a
// dispatcher — every one is present AND no extra/fewer. The len(m) equality means dropping a
// wiring line (or adding an unlisted one) fails this test, not just a missing-key check.
// registerDispatchers only stores its args, so nil repo/encryptor build the map without a deref.
func TestRegisterDispatchers_RegistersProviders(t *testing.T) {
	m := registerDispatchers(nil, nil, nil)
	for _, p := range registeredProviders {
		_, ok := m[p]
		assert.True(t, ok, "%s must be registered — this is the wiring its PR adds", p)
	}
	// Exact membership: catches BOTH a dropped wiring (fewer entries) and an unlisted new
	// wiring (more entries), so registeredProviders stays the single source of truth.
	assert.Equal(t, len(registeredProviders), len(m),
		"registerDispatchers must register exactly the expected providers; update registeredProviders when adding/removing a wiring")
}

// TestLogMissingDispatchers_SurfacesGaps verifies logMissingDispatchers actually
// flags a known ad provider that has no adapter, so the startup gap stays visible. It asserts on the
// EMITTED slog output (buffer-captured), so it fails if the function were a no-op.
func TestLogMissingDispatchers_SurfacesGaps(t *testing.T) {
	// Capture the ACTUAL slog output (not a recomputed copy of the loop) so the test
	// verifies logMissingDispatchers's behavior — it would fail if the function were
	// gutted to a no-op (per @dealako's review). Swap the default logger for the call.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Feed a map with one provider deliberately REMOVED rather than relying on a real gap:
	// adapters keep landing, so a test that asserts "provider X is still unregistered" rots
	// the moment X ships. A synthetic gap keeps proving the function is not a no-op forever.
	full := registerDispatchers(nil, nil, nil)
	gapped := make(map[model.Provider]service.PlatformDispatcher, len(full))
	for p, d := range full {
		if p == model.ProviderRedditAds {
			continue // the synthetic gap
		}
		gapped[p] = d
	}
	logMissingDispatchers(gapped)

	out := buf.String()
	assert.Contains(t, out, "no dispatcher registered", "the warning must be emitted when a provider is missing")
	assert.Contains(t, out, string(model.ProviderRedditAds), "the missing provider must be named in the log")
	// The missing provider must land in the field matching its KIND, so an operator can filter
	// a missing paid platform (budget unspent) from a missing email channel (no drafts staged)
	// on a field rather than substring-matching a formatted string.
	assert.Contains(t, out, "missing_paid_ads=", "the paid-ads field must carry the missing ad platform")
	assert.Contains(t, out, "missing_email=[]", "the email field must be present and EMPTY — only a paid platform is missing here")
	for p := range gapped {
		assert.NotContains(t, out, string(p), "%s is registered, so it must not be logged as missing", p)
	}
}

// TestLogMissingDispatchers_SilentWhenComplete is the counterpart to the gap test: with every
// dispatchable provider wired, the startup path must emit NOTHING. A warning that fires on a
// healthy boot is noise operators learn to filter, which is how a real gap later goes unseen.
//
// It builds a SYNTHETIC complete map rather than asserting on the real one. An earlier version
// guarded with t.Skip until every provider had an adapter — which meant it never ran at all
// (microsoft's is still in flight), making it a green checkmark that asserted nothing. This
// mirrors the synthetic-gap approach of the sibling test and runs today.
func TestLogMissingDispatchers_SilentWhenComplete(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	complete := make(map[model.Provider]service.PlatformDispatcher, len(dispatchableProviders))
	for _, p := range dispatchableProviders {
		// The value is never invoked — logMissingDispatchers only tests key presence.
		complete[p] = nil
	}
	logMissingDispatchers(complete)
	assert.Empty(t, buf.String(), "no warning may be emitted when every provider has a dispatcher")
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

// countingScanner records how many scans ran, and can report a claim that only becomes
// visible AFTER the first scan — modelling the exact gap this sweeper exists to close.
type countingScanner struct {
	mu      sync.Mutex
	calls   int
	scanned chan struct{}
}

func (s *countingScanner) StuckDispatchClaims(context.Context, int) ([]*model.Campaign, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case s.scanned <- struct{}{}:
	default:
	}
	return nil, nil
}

func (s *countingScanner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestStuckClaimSweeper_RescansAfterStartup pins the fix for the startup-only gap: a claim
// stranded seconds before a rolling deploy is YOUNGER than stuckClaimReportAge, so the new
// pod's boot scan skips it. Without a periodic re-scan nothing ever looks again and the row
// silently blocks every future dispatch for its (brief_id, platform).
//
// The assertion is deliberately "a scan happened that the startup path did not perform" —
// that is the whole behavioural difference, and a startup-only implementation can never
// satisfy it no matter how long the test waits.
func TestStuckClaimSweeper_RescansAfterStartup(t *testing.T) {
	orig := stuckClaimSweepInterval
	stuckClaimSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { stuckClaimSweepInterval = orig })

	sc := &countingScanner{scanned: make(chan struct{}, 1)}
	c := &Container{}
	c.startStuckClaimSweeper(sc)
	t.Cleanup(func() {
		c.cancelSweep()
		<-c.sweepDone
	})

	select {
	case <-sc.scanned:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper never re-scanned: a claim stranded after startup would stay invisible forever")
	}
	assert.Positive(t, sc.count(), "expected at least one periodic scan")
}

// TestStuckClaimSweeper_StopsOnCancel proves the sweeper is bounded by the container's
// lifecycle: Close must not hang waiting for it, and it must not keep querying a closing pool.
func TestStuckClaimSweeper_StopsOnCancel(t *testing.T) {
	orig := stuckClaimSweepInterval
	stuckClaimSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { stuckClaimSweepInterval = orig })

	sc := &countingScanner{scanned: make(chan struct{}, 1)}
	c := &Container{}
	c.startStuckClaimSweeper(sc)
	<-sc.scanned // it is running

	c.cancelSweep()
	select {
	case <-c.sweepDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper did not exit on cancel; Container.Close would hang")
	}
}

// blockingScanner blocks until its context is cancelled, modelling a scan stuck on a slow or
// unavailable database.
type blockingScanner struct{ entered chan struct{} }

func (b *blockingScanner) StuckDispatchClaims(ctx context.Context, _ int) ([]*model.Campaign, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestScanStuckDispatchClaims_RespectsParentCancel pins that the scan derives from its parent
// context. On the cold-start path the scan runs inside the init goroutine, which Close waits
// on via <-c.initDone. If it used context.Background() instead, a scan blocked in the database
// would be uninterruptible and Close would overrun its bounded shutdown budget by up to
// stuckClaimScanTimeout — the same reasoning that already governs the FailStuckJobs call.
func TestScanStuckDispatchClaims_RespectsParentCancel(t *testing.T) {
	sc := &blockingScanner{entered: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		scanStuckDispatchClaims(ctx, sc, "at startup")
	}()

	<-sc.entered // the scan is blocked in the "database"
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the scan ignored its parent context: Close would block until stuckClaimScanTimeout expires")
	}
}

// TestStuckClaimRemediation_NeverSaysSafe pins the operator-facing signal. 'pending' cannot
// distinguish an abandoned claim from one whose dispatch is still running, so no row can be
// reported as safe to delete — the only honest distinction is how much verification is owed.
func TestStuckClaimRemediation_NeverSaysSafe(t *testing.T) {
	cases := []struct {
		name string
		c    *model.Campaign
		want string
	}{
		{"bare claim", &model.Campaign{Version: 1}, "verify no dispatch is in flight before deleting"},
		{"upserted after claim", &model.Campaign{Version: 2}, "verify upstream platform before deleting: a paid campaign may exist"},
		{"carries a platform id", &model.Campaign{Version: 1, PlatformCampaignID: "pc-1"}, "verify upstream platform before deleting: a paid campaign may exist"},
		{"carries a result blob", &model.Campaign{Version: 1, Result: []byte(`{"x":1}`)}, "verify upstream platform before deleting: a paid campaign may exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stuckClaimRemediation(tc.c)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "safe to delete",
				"no stuck claim may be reported as safe to delete: a dispatch may still be in flight")
		})
	}
}

// wedgedScanner ignores context cancellation until released, modelling a driver already
// inside a statement that cannot unwind promptly.
type wedgedScanner struct {
	entered chan struct{}
	release chan struct{}
}

func (w *wedgedScanner) StuckDispatchClaims(context.Context, int) ([]*model.Campaign, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release // deliberately ignores ctx
	return nil, nil
}

// TestClose_DoesNotWaitForWedgedSweeper pins the bound on the sweeper stop. Cancelling
// sweeperCtx interrupts a scan but does not guarantee it RETURNS — a driver mid-statement can
// take until stuckClaimScanTimeout to unwind. That wait sits before the dispatch drain, so an
// unbounded one would spend the drain's budget on a diagnostic and starve in-flight campaign
// creation, the phase that actually matters.
func TestClose_DoesNotWaitForWedgedSweeper(t *testing.T) {
	orig := stuckClaimSweepInterval
	stuckClaimSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { stuckClaimSweepInterval = orig })

	ws := &wedgedScanner{entered: make(chan struct{}, 1), release: make(chan struct{})}
	c := &Container{} // nil pool and orch: isolate the sweeper wait
	c.startStuckClaimSweeper(ws)
	<-ws.entered // wedged inside a scan
	t.Cleanup(func() { close(ws.release) })

	start := time.Now()
	require.NoError(t, c.Close(context.Background()))
	elapsed := time.Since(start)

	// Generous slack for scheduling, but far below stuckClaimScanTimeout (5s) — the point
	// is that Close gave up rather than waiting out the wedged scan.
	if elapsed > time.Second {
		t.Fatalf("Close took %s waiting for a wedged sweeper (scan timeout is %s): the dispatch drain's budget was spent on a diagnostic",
			elapsed, stuckClaimScanTimeout)
	}
}

// TestClose_CancelsSweeperBeforeClosingPool pins the ORDERING that makes the bounded wait
// safe. StuckDispatchClaims runs a pgxpool.Query, which holds a pooled CONNECTION until its
// rows close, and pgxpool.Close "blocks until all connections are returned to pool and
// closed". So merely giving up on <-sweepDone does not bound shutdown: pool.Close() would
// block on the very same scan, just later.
//
// What actually releases the connection is cancelling sweeperCtx — pgx aborts the in-flight
// statement on cancellation and returns the connection. This test asserts the sweeper's
// context is already cancelled by the time Close reaches the pool, which is the property the
// bounded wait depends on. (It cannot use a real pool: postgres.Pool embeds a concrete
// *pgxpool.Pool, so there is no seam to substitute one.)
func TestClose_CancelsSweeperBeforeClosingPool(t *testing.T) {
	orig := stuckClaimSweepInterval
	stuckClaimSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { stuckClaimSweepInterval = orig })

	observed := make(chan context.Context, 1)
	sc := &ctxCapturingScanner{seen: observed}

	c := &Container{}
	c.startStuckClaimSweeper(sc)

	scanCtx := <-observed // the ctx a real query would be running under
	require.NoError(t, scanCtx.Err(), "precondition: the scan context starts live")

	require.NoError(t, c.Close(context.Background()))

	// After Close returns, the scan's context MUST be cancelled — that cancellation is what
	// aborts the statement and returns the connection, so pool.Close() cannot deadlock on it.
	require.Error(t, scanCtx.Err(),
		"Close must cancel the sweeper's context: without it the scan keeps a pooled connection and pool.Close() blocks")
	assert.ErrorIs(t, scanCtx.Err(), context.Canceled)
}

// ctxCapturingScanner hands the caller the context its "query" would run under, so a test can
// assert that context is cancelled by shutdown.
type ctxCapturingScanner struct{ seen chan context.Context }

func (s *ctxCapturingScanner) StuckDispatchClaims(ctx context.Context, _ int) ([]*model.Campaign, error) {
	select {
	case s.seen <- ctx:
	default:
	}
	<-ctx.Done() // model a query that unwinds only when its context is cancelled
	return nil, ctx.Err()
}
