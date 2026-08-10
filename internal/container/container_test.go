// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/indexer"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/eventurl"
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
	// The container-close phase reserves EVERY term Close actually spends: the sweeper-stop
	// wait, the index relay's stop wait, the dispatch drain, the post-cancel grace, the
	// index publisher's connection drain, AND the UNCONFIRMED lock cooldown stop wait.
	// Omitting any one of these understates the phase and lets the two phases sum past
	// DefaultShutdownTimeout — the SIGKILL-mid-drain this budget exists to prevent.
	assert.Equal(t, sweeperStopTimeout+relayStopTimeout+dispatchDrainTimeout+service.CancelGracePeriod+indexer.DrainTimeout+cooldownStopTimeout, ContainerCloseTimeout)
	// The HTTP phase gets a positive share of the remaining budget.
	assert.Positive(t, HTTPShutdownTimeout, "HTTP shutdown phase must have a positive budget")
	// The two phases together stay within the overall budget.
	assert.LessOrEqual(t, HTTPShutdownTimeout+ContainerCloseTimeout, constants.DefaultShutdownTimeout)
}

// TestClose_CooldownStopSpendsExactlyItsReservedTerm pins the hand-off that makes
// cooldownStopTimeout an honest budget term rather than a comment.
//
// TestShutdownBudgetComposes above proves cooldownStopTimeout is RESERVED. That is only half
// the invariant: what Close actually spends is whatever value it hands to
// StopCooldownsForShutdown, and that value also becomes each woken release's own unlock
// deadline (postgres.shutdownReleaseBound). Hand down a different constant — or none, in
// which case the release falls back to its ordinary 5s lockReleaseTimeout — and Close returns
// after 250ms while a cooldown connection stays checked out for up to 5s. pgxpool.Close then
// blocks on that connection for the difference, OUTSIDE ContainerCloseTimeout. Nothing else
// catches it: the budget arithmetic still balances, every postgres-side test still passes
// (they call StopCooldownsForShutdown directly with their own timeout), and the overrun only
// shows up as a SIGKILL mid-drain in production.
//
// Ordering is the second half. The signal is useless after the fact, so it must precede
// pool.Close(); an unsignalled cooldown holds its connection for the full
// unconfirmedLockCooldown (30s) and pool.Close waits out every second of it.
//
// Asserted on the source of Close because both properties are structural — there is no DB
// harness here, and a behavioural test would have to observe a real pooled connection.
func TestClose_CooldownStopSpendsExactlyItsReservedTerm(t *testing.T) {
	src, err := os.ReadFile("container.go")
	require.NoError(t, err)

	const start = "func (c *Container) Close("
	i := strings.Index(string(src), start)
	require.NotEqual(t, -1, i, "Container.Close not found; update this test if the method was renamed")
	body := string(src)[i:]
	// Bound at the next top-level func so a later method cannot satisfy these on Close's behalf.
	if j := strings.Index(body[len(start):], "\nfunc "); j != -1 {
		body = body[:len(start)+j]
	}

	stop := strings.Index(body, "postgres.StopCooldownsForShutdown(cooldownStopTimeout)")
	require.NotEqual(t, -1, stop,
		"Close must signal cooldowns with cooldownStopTimeout itself — the term reserved in "+
			"ContainerCloseTimeout — since that argument becomes each release's unlock deadline")
	// The statement, not the several comments above that name it: matching the bare text
	// would find the first mention (a comment about the sweeper) and compare against that.
	closePool := strings.Index(body, "\n\t\tpool.Close()\n")
	require.NotEqual(t, -1, closePool, "Close must close the pool")
	assert.Less(t, stop, closePool,
		"cooldowns must be signalled BEFORE pool.Close(): pgxpool.Close blocks until every "+
			"checked-out connection is returned, and an unsignalled cooldown holds one for the "+
			"full unconfirmedLockCooldown")
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
func (stubCampaignRepo) ClaimCampaignDispatch(context.Context, string, string, model.Provider, string, *model.Actor) (bool, *model.Campaign, error) {
	return true, &model.Campaign{Status: "pending"}, nil
}
func (stubCampaignRepo) DeleteDispatchClaim(context.Context, string, model.Provider) error {
	return nil
}
func (stubCampaignRepo) UpsertCampaign(_ context.Context, c *model.Campaign, _ domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return c, nil
}
func (stubCampaignRepo) ReplaceCampaign(context.Context, *model.Campaign, int64, domain.CampaignLockToken, domain.CampaignIndexPayloadFunc) (*model.Campaign, error) {
	return nil, domain.ErrNotFound
}
func (stubCampaignRepo) DeleteCampaign(context.Context, string, string, string, int64, *model.Actor, domain.CampaignIndexPayloadFunc) error {
	return nil
}
func (stubCampaignRepo) ClaimCampaignVersion(context.Context, string, string, string, int64) (*model.Campaign, domain.CampaignLockToken, error) {
	return nil, domain.CampaignLockToken{}, domain.ErrNotFound
}
func (stubCampaignRepo) ReleaseCampaignLock(context.Context, domain.CampaignLockToken) error {
	return nil
}
func (stubCampaignRepo) ReleaseCampaignLockAfterCooldown(domain.CampaignLockToken, time.Duration) {}

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

func (fakeAudienceRepo) CreateAudienceForApprovedBrief(_ context.Context, a *model.CampaignAudience, _ int64) (*model.CampaignAudience, error) {
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

// TestNewAudienceBuilder_SnowflakeOptional pins that an unconfigured or misconfigured warehouse
// does NOT block audience building. Snowflake only enriches an audience with past editions; the
// country-scoped group needs no warehouse, so failing here would take the whole email channel
// down for a read-only lookup.
//
// It also guards the typed-nil trap: assigning a nil *snowflake.Client into the interface would
// make `snow == nil` false and produce a nil-dereference on first use instead of the intended
// degrade.
func TestNewAudienceBuilder_SnowflakeOptional(t *testing.T) {
	cases := map[string]*config.Config{
		"nothing configured": {},
		"partial config":     {SnowflakeAccount: "acct", SnowflakeUser: "usr"}, // no key
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			b, client := newAudienceBuilder(nil, nil, cfg)
			require.NotNil(t, b, "a builder must always be returned: HubSpot list creation does not need Snowflake")
			// The client must be nil when Snowflake is not fully configured, otherwise
			// it remains unclosed and leaks connections.
			require.Nil(t, client, "client should only be non-nil when all Snowflake config fields are present")

			// The degrade path must yield no editions and NO error — the caller records the
			// narrower scope rather than failing the build.
			names, err := b.ResolvePastEditions(context.Background(), "KubeCon", "Korea", "2026")
			require.NoError(t, err, "an unconfigured warehouse is not an error")
			assert.Empty(t, names)
		})
	}
}

// TestNewContainer_AllPathsInjectIndexer pins the wiring bug that made PR #60 publish
// nothing: SetIndexer was called ONLY on the 503-mode path, while the healthy fast path
// and the no-database path each constructed their own BriefService and silently kept the
// Noop. Every path must end up with a real publisher, so asserting one path is not enough
// — the bug lived precisely in the path that wasn't checked.
//
// A reachable NATS server is NOT required: newIndexPublisher returns a live *NATSPublisher
// after a failed dial (it reconnects in the background), so "not Noop" is the correct
// signal that wiring happened, independent of broker availability.
func TestNewContainer_AllPathsInjectIndexer(t *testing.T) {
	const unreachableNATS = "nats://127.0.0.1:14222"

	t.Run("no-database path", func(t *testing.T) {
		cont, err := NewContainer(&config.Config{Host: "*", Port: "8080", NATSUrl: unreachableNATS})
		require.NoError(t, err)
		t.Cleanup(func() { _ = cont.Close(context.Background()) })

		bs, ok := cont.Briefs.(*service.BriefService)
		require.True(t, ok, "briefs service must be the concrete *BriefService")
		assert.False(t, bs.IndexerIsNoop(), "no-database path kept the Noop indexer: it would serve traffic and index nothing")
	})

	t.Run("503-mode path", func(t *testing.T) {
		// An unreachable DB boots in 503 mode and takes the background-retry path.
		shrinkDBTimers(t)
		cont, err := NewContainer(&config.Config{
			Host: "*", Port: "8080", NATSUrl: unreachableNATS,
			// Port 1 has nothing listening → connection refused (transient, retryable).
			DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
			CredentialEncryptionKey: validEncryptionKey(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = cont.Close(context.Background()) })

		bs, ok := cont.Briefs.(*service.BriefService)
		require.True(t, ok, "briefs service must be the concrete *BriefService")
		assert.False(t, bs.IndexerIsNoop(), "503-mode path kept the Noop indexer")
	})
}

// TestNewContainer_AllPathsInjectTheTokenVerifier is the indexer test's security twin and
// fails louder: an unwired verifier REFUSES every request to that service. Goa wires each
// service's JWTAuth separately and the degraded boot paths build their services
// independently, so a missed injection only shows up by asking all three on every path.
func TestNewContainer_AllPathsInjectTheTokenVerifier(t *testing.T) {
	const unreachableNATS = "nats://127.0.0.1:14222"

	assertAll := func(t *testing.T, cont *Container) {
		t.Helper()
		for name, s := range map[string]interface{ HasTokenVerifier() bool }{
			"briefs":      cont.Briefs.(*service.BriefService),
			"connections": cont.Connections.(*service.ConnectionService),
			"audiences":   cont.Audiences.(*service.AudienceService),
		} {
			assert.True(t, s.HasTokenVerifier(),
				"%s was constructed without a token verifier: it will reject every request", name)
		}
	}

	t.Run("no-database path", func(t *testing.T) {
		cont, err := NewContainer(&config.Config{Host: "*", Port: "8080", NATSUrl: unreachableNATS})
		require.NoError(t, err)
		t.Cleanup(func() { _ = cont.Close(context.Background()) })
		assertAll(t, cont)
	})

	t.Run("503-mode path", func(t *testing.T) {
		shrinkDBTimers(t)
		cont, err := NewContainer(&config.Config{
			Host: "*", Port: "8080", NATSUrl: unreachableNATS,
			DatabaseURL:             "postgres://app@127.0.0.1:1/campaign?sslmode=disable",
			CredentialEncryptionKey: validEncryptionKey(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = cont.Close(context.Background()) })
		assertAll(t, cont)
	})
}

// stubTokenVerifier stands in for auth.Verifier. It is never called: these tests assert
// only that a verifier REACHED the service, which is the wiring failure that would
// otherwise refuse every request.
type stubTokenVerifier struct{}

func (stubTokenVerifier) VerifyActor(context.Context, string) (*model.Actor, error) {
	return &model.Actor{Username: "stub"}, nil
}

// TestNewServices_LivePathInjectsTheTokenVerifier closes the one path the test above
// cannot reach. NewContainer only runs wireLiveBackends with a REAL pool, so the
// no-database and 503-mode subtests leave the live wiring — the path every deployment
// actually takes — unasserted; a missed injection there would ship while both subtests
// stayed green.
//
// Calling the three helpers directly proves the same guarantee without a database,
// because wireLiveBackends constructs each service through exactly these helpers and
// nothing else (container.go:680, :707, :708). That is what makes this equivalent rather
// than merely adjacent: if a future edit inlines service.NewXService there instead, the
// helper rule is broken and this test is no longer standing in for anything — which is
// precisely what the godoc on each helper exists to prevent.
func TestNewServices_LivePathInjectsTheTokenVerifier(t *testing.T) {
	c := &Container{tokenVerifier: stubTokenVerifier{}, indexPublisher: indexer.Noop{}}
	for name, s := range map[string]interface{ HasTokenVerifier() bool }{
		"briefs":      c.newBriefService(nil, nil, nil, nil),
		"connections": c.newConnectionService(nil, nil),
		"audiences":   c.newAudienceService(nil, nil),
	} {
		assert.True(t, s.HasTokenVerifier(),
			"%s: the live wiring path built it without a verifier; it would reject every request", name)
	}

	// The injection must PROPAGATE the container's field, not merely set something
	// non-nil: a helper that built its own verifier would pass the check above and still
	// leave a deployment verifying against the wrong JWKS.
	c = &Container{indexPublisher: indexer.Noop{}}
	assert.False(t, c.newBriefService(nil, nil, nil, nil).HasTokenVerifier(),
		"a container with no verifier must not yield a service that claims one")
}

// TestNewBriefService_InjectsSharedPublisher covers the live fast path's constructor
// directly. The fast path needs a real pool, so exercising NewContainer for it would
// require a database; calling the helper proves the same guarantee — that the helper
// every path now routes through actually injects — without one.
func TestNewBriefService_InjectsSharedPublisher(t *testing.T) {
	c := &Container{indexPublisher: indexer.Noop{}}
	assert.True(t, c.newBriefService(nil, nil, nil, nil).IndexerIsNoop(),
		"a Noop publisher must pass through as a Noop")

	live, err := indexer.NewNATSPublisher("nats://127.0.0.1:14222")
	require.NotNil(t, live)
	_ = err // a dial failure still yields a usable publisher; that is not what this asserts
	c = &Container{indexPublisher: live}
	t.Cleanup(live.Close)
	assert.False(t, c.newBriefService(nil, nil, nil, nil).IndexerIsNoop(),
		"the container's real publisher must reach the BriefService")
}

// TestNewOrchestrator_InjectsSharedPublisher covers the orchestrator half of the same
// wiring guarantee. Campaign CREATES are persisted by the orchestrator (dispatchOne),
// so an orchestrator that kept the Noop would leave every newly created campaign
// unsearchable until a later update republished it — a gap invisible from BriefService.
func TestNewOrchestrator_InjectsSharedPublisher(t *testing.T) {
	c := &Container{indexPublisher: indexer.Noop{}}
	assert.True(t, c.newOrchestrator(nil, nil, nil).IndexerIsNoop(),
		"a Noop publisher must pass through as a Noop")

	live, err := indexer.NewNATSPublisher("nats://127.0.0.1:14222")
	require.NotNil(t, live)
	_ = err // a dial failure still yields a usable publisher; that is not what this asserts
	t.Cleanup(live.Close)
	c = &Container{indexPublisher: live}
	assert.False(t, c.newOrchestrator(nil, nil, nil).IndexerIsNoop(),
		"the container's real publisher must reach the Orchestrator")
}

// TestClose_StopsARelayInstalledByALateInitRetry pins the ORDER of the two shutdown steps.
//
// On the 503 cold-start path the DB-init goroutine is what installs the relay. Close therefore
// has to cancel and JOIN that goroutine before it reads c.indexRelay — reading first loses the
// race: a retry that succeeds in the gap starts a relay nothing ever stops, and it keeps reading
// the outbox straight through the pool.Close that follows.
//
// The test reproduces the gap directly: the goroutine installs the relay only after Close has
// begun, so a Close that read the field up front would see nil and stop nothing.
func TestClose_StopsARelayInstalledByALateInitRetry(t *testing.T) {
	relay := indexer.NewRelay(&stubOutbox{}, indexer.Noop{}, "token")

	c := &Container{}
	_, cancel := context.WithCancel(context.Background())
	c.cancelInit = cancel
	c.initDone = make(chan struct{})

	closeStarted := make(chan struct{})
	go func() {
		defer close(c.initDone)
		<-closeStarted // the retry lands mid-Close, exactly where the race lives
		c.setIndexRelay(relay)
	}()

	close(closeStarted)
	require.NoError(t, c.Close(context.Background()))

	// Stop is idempotent via sync.Once, so a relay Close already stopped reports done
	// immediately. A relay Close never saw would still be running its ticker.
	assert.True(t, relayStopped(relay), "Close must stop a relay installed by a late init retry")
}

// stubOutbox is an outbox that never has pending work. The relay under test is never expected to
// publish anything — only to be STOPPED — so the reads just need to succeed.
type stubOutbox struct{}

func (stubOutbox) DrainPendingIndexMessages(
	context.Context, int, func(context.Context, *model.OutboxMessage) error,
) (int, error) {
	return 0, nil
}
func (stubOutbox) PrunePublishedIndexMessages(context.Context, time.Duration, int) (int64, error) {
	return 0, nil
}

// relayStopped reports whether Stop has already run, without exporting relay internals: a second
// Stop on an already-stopped relay returns immediately, while one on a live relay would block
// for its full wait.
func relayStopped(r *indexer.Relay) bool {
	done := make(chan struct{})
	go func() {
		r.Stop(time.Second)
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// TestNewContainer_MalformedNATSURLIsFatal pins that an unusable NATS configuration fails boot
// rather than degrading to a silent Noop.
//
// The publisher is built with RetryOnFailedConnect, so an ordinary broker outage does NOT reach
// the error branch — it returns a reconnecting publisher that heals itself. Reaching it means the
// config can never work, and no retry will fix it.
//
// Carrying on with a Noop there is the worst outcome available: NATS_URL is non-empty, so the
// enqueue gate stays OPEN and every write co-commits an outbox row into a table this process can
// never drain — and whose pending rows are deliberately never pruned. The service would report
// healthy while accumulating undeliverable work forever. Failing fast surfaces a config error as
// a config error, exactly as invalid database settings already do.
func TestNewContainer_MalformedNATSURLIsFatal(t *testing.T) {
	cfg := &config.Config{NATSUrl: "://not-a-url"}

	_, err := NewContainer(cfg)

	require.Error(t, err, "an unusable NATS URL must fail boot, not degrade to a Noop")
	assert.Contains(t, err.Error(), "nats configuration",
		"the error must name the misconfigured subsystem so an operator knows what to fix")
	// The raw URL must not leak: NATS_URL can carry credentials.
	assert.NotContains(t, err.Error(), "not-a-url@", "a credential-bearing URL must stay redacted")
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
		{"bare claim", &model.Campaign{Version: 1}, "verify upstream platform before deleting: a paid campaign may exist (a retained claim does not prove the provider was never called)"},
		{"upserted after claim", &model.Campaign{Version: 2}, "verify upstream platform before deleting: a paid campaign may exist (dispatch recorded a partial or ambiguous result)"},
		{"carries a platform id", &model.Campaign{Version: 1, PlatformCampaignID: "pc-1"}, "verify upstream platform before deleting: a paid campaign may exist (dispatch recorded a partial or ambiguous result)"},
		{"carries a result blob", &model.Campaign{Version: 1, Result: []byte(`{"x":1}`)}, "verify upstream platform before deleting: a paid campaign may exist (dispatch recorded a partial or ambiguous result)"},
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

// TestNewAudienceBuilder_UnusableKeyIsAnOutage_NotSilence pins the DIFFERENCE between the two
// degrades, which is the part that actually costs an audience.
//
// A malformed or rotated SNOWFLAKE_PRIVATE_KEY must not look like "no warehouse configured".
// Both leave the resolver unusable, but only this one means a returning event's past-registrant
// groups were LOST. If the construction error is dropped, ResolvePastEditions answers (nil, nil),
// BuildAudience takes its success branch, and the stored summary claims a first-time event —
// every signal reports success while a returning KubeCon silently ships a country-only audience.
//
// The service-side consequence of this error (the "NARROWER THAN INTENDED" note) is pinned
// separately in audience.TestBuildPlan_WarehouseErrorGetsTheOutageNote.
func TestNewAudienceBuilder_UnusableKeyIsAnOutage_NotSilence(t *testing.T) {
	cfg := &config.Config{
		SnowflakeAccount: "acct", SnowflakeUser: "usr",
		SnowflakePrivateKey: "-----BEGIN PRIVATE KEY-----\nnot-a-key\n-----END PRIVATE KEY-----", // secretlint-disable-line -- non-key fixture asserting an unusable key degrades
	}
	b, client := newAudienceBuilder(nil, nil, cfg)
	require.NotNil(t, b, "boot must not fail: a read-only enrichment cannot take down dispatch")
	// An unusable key means the client is never created; the builder is degraded but not nil.
	require.Nil(t, client, "client should be nil when key initialization fails")

	names, err := b.ResolvePastEditions(context.Background(), "KubeCon", "Korea", "2026")
	require.Error(t, err,
		"a CONFIGURED but unusable warehouse must report an error; returning (nil, nil) makes it "+
			"indistinguishable from an unconfigured deployment and loses the audience silently")
	assert.Contains(t, err.Error(), "snowflake is configured but unusable",
		"the error must name the cause so the stored summary tells an operator what to fix")
	assert.Empty(t, names)
}

// TestNewAudienceService_InjectsBuilder pins the wiring guarantee for the audience service, the
// same one PR #60 had to fix for the indexer: SetBuilder is opt-in, so a construction path that
// skips it compiles and serves while the build endpoint returns 503 forever.
//
// It asserts on the SERVICE's own view (BuilderIsSet) rather than on a BuildAudience error: a
// nil repo short-circuits before the builder is consulted, so both wired and unwired services
// return the same typed 503 and an error-based assertion passes vacuously. (Verified: an
// earlier version of this test did exactly that and stayed green with SetBuilder removed.)
func TestNewAudienceService_InjectsBuilder(t *testing.T) {
	b, _ := newAudienceBuilder(nil, nil, &config.Config{})
	c := &Container{audienceBuilder: b}
	require.NotNil(t, c.audienceBuilder)

	s := c.newAudienceService(nil, nil)
	require.NotNil(t, s)
	assert.True(t, s.BuilderIsSet(),
		"the container's builder must reach the service; without it BuildAudience returns 503 forever")

	// And a container with no builder must NOT claim one — the degrade is real, not implied.
	assert.False(t, (&Container{}).newAudienceService(nil, nil).BuilderIsSet())
}

// TestAudienceService_ColdStartBindsAllBuildDeps pins the cold-start late-binding. The audience
// service is constructed in 503 mode with NO brief repo and NO builder, so a retry path that
// binds only the audience repo leaves BuildAudience returning 503 forever — on a pod that looks
// completely healthy. My first cut did exactly that.
//
// The audienceBackendSetter interface now names all three setters, so a path that forgets one
// fails to satisfy it at COMPILE time rather than at 3am.
func TestAudienceService_ColdStartBindsAllBuildDeps(t *testing.T) {
	// The concrete service must satisfy the full late-bind contract.
	var ab audienceBackendSetter = service.NewAudienceService(nil)

	ab.SetBackend(fakeAudienceRepo{})
	ab.SetBriefRepo(nil)
	b, _ := newAudienceBuilder(nil, nil, &config.Config{})
	ab.SetBuilder(b)

	s, ok := ab.(*service.AudienceService)
	require.True(t, ok)
	assert.True(t, s.BuilderIsSet(),
		"the cold-start path must bind the builder; binding only the audience repo leaves BuildAudience 503 forever")
}

// TestSetBuilder_IgnoresNil guards the degraded deployment: with no HubSpot/Snowflake
// configured the container's builder is nil, and binding it must leave the service reporting
// "not configured" rather than storing a nil interface that panics on first use.
func TestSetBuilder_IgnoresNil(t *testing.T) {
	s := service.NewAudienceService(nil)
	s.SetBuilder(nil)
	assert.False(t, s.BuilderIsSet(), "a nil builder must not register as configured")
}

// TestStuckClaimRemediation_AlwaysRequiresUpstreamCheck is the load-bearing half of the
// remediation contract, and it is deliberately stated as an invariant over EVERY row shape
// rather than as per-case strings.
//
// A bare version-1 row is NOT evidence that the provider was never called. dispatchOne retains
// the claim without upserting on (nil, nil), on an empty upstream id, and on a non-pre-create
// (nil, err) — all of which leave version 1, no platform id, no result blob, while a paid
// campaign may already exist upstream. Guidance that let an operator clear such a row after
// merely confirming no worker is running would authorize a duplicate paid create.
func TestStuckClaimRemediation_AlwaysRequiresUpstreamCheck(t *testing.T) {
	shapes := []*model.Campaign{
		{Version: 1},                             // bare claim: the ambiguous retained-claim shape
		{Version: 2},                             // upserted after claim
		{Version: 1, PlatformCampaignID: "pc-1"}, // partial with an upstream id
		{Version: 1, Result: []byte(`{"x":1}`)},  // id-less partial carrying a reconcile blob
		{Version: 9, PlatformCampaignID: "pc-2", Result: []byte(`{}`)}, // everything at once
	}
	for _, c := range shapes {
		got := stuckClaimRemediation(c)
		assert.Contains(t, got, "verify upstream platform",
			"every stuck claim must require an upstream check: the schema cannot prove a paid campaign does not exist (version=%d, id=%q, result=%d bytes)",
			c.Version, c.PlatformCampaignID, len(c.Result))
		assert.NotContains(t, got, "safe to delete",
			"no stuck claim may be reported as safe to delete")
	}
}

// fixedScanner returns a canned batch of stuck claims.
type fixedScanner struct{ rows []*model.Campaign }

func (f *fixedScanner) StuckDispatchClaims(context.Context, int) ([]*model.Campaign, error) {
	return f.rows, nil
}

// stuckClaims builds n distinct 'pending' rows.
func stuckClaims(n int) []*model.Campaign {
	out := make([]*model.Campaign, 0, n)
	for i := range n {
		out = append(out, &model.Campaign{BriefID: fmt.Sprintf("b-%d", i), Version: 1})
	}
	return out
}

// summaryFor runs one scan against rows and returns the parsed "stuck dispatch claims detected"
// summary record, plus how many per-claim detail lines were emitted.
func summaryFor(t *testing.T, rows []*model.Campaign) (map[string]any, int) {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	scanStuckDispatchClaims(context.Background(), &fixedScanner{rows: rows}, "in test")

	var summary map[string]any
	details := 0
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		switch rec["msg"] {
		case "stuck dispatch claims detected in test":
			summary = rec
		case "stuck dispatch claim":
			details++
		}
	}
	return summary, details
}

// TestScanStuckDispatchClaims_TruncationIsHonest pins the one contract that decides whether an
// operator sees the real size of an incident. The repo deliberately queries DefaultStuckClaimLimit+1
// so a saturated cap is DISTINGUISHABLE from an exact count; if the scan reported a flat
// count=100 with truncated=false, a crash-loop stranding thousands of claims would read as a
// bounded 100-row problem. count must therefore never exceed the cap, and truncated must be
// true exactly when the repo returned more than the cap.
func TestScanStuckDispatchClaims_TruncationIsHonest(t *testing.T) {
	cases := []struct {
		name          string
		returned      int
		wantCount     float64
		wantTruncated bool
	}{
		{"under the cap", 3, 3, false},
		{"exactly the cap", postgres.DefaultStuckClaimLimit, float64(postgres.DefaultStuckClaimLimit), false},
		// The repo queries limit+1, so this is what "at least the cap" looks like on the wire.
		{"cap saturated", postgres.DefaultStuckClaimLimit + 1, float64(postgres.DefaultStuckClaimLimit), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary, details := summaryFor(t, stuckClaims(tc.returned))
			require.NotNil(t, summary, "a non-empty scan must emit a summary line")

			assert.Equal(t, tc.wantTruncated, summary["truncated"],
				"truncated must be true exactly when the repo returned more rows than the cap, or a saturated scan understates the incident")
			assert.Equal(t, tc.wantCount, summary["count"],
				"count must be clamped to the cap and never report the +1 probe row")
			// An ABSOLUTE bound, deliberately not `<= maxStuckClaimDetailLogs`: comparing the
			// output against the very constant that produced it holds for any value of that
			// constant, so it would not notice the cap being raised to something that floods
			// the log during an incident. A stuck-claim burst is exactly when logging volume
			// matters, and the summary line already carries the true total.
			assert.LessOrEqual(t, details, 10,
				"per-claim detail logging must stay bounded regardless of how many rows came back")
			assert.Equal(t, float64(details), summary["reported"],
				"reported must equal the number of detail lines actually emitted")
		})
	}
}

// TestScanStuckDispatchClaims_SilentWhenClean keeps the diagnostic from being noise: a clean
// scan is the normal state on every replica every 5 minutes, so it must log nothing at all.
func TestScanStuckDispatchClaims_SilentWhenClean(t *testing.T) {
	summary, details := summaryFor(t, nil)
	assert.Nil(t, summary, "a clean scan must not emit a summary line")
	assert.Zero(t, details, "a clean scan must not emit detail lines")
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

// fakeConnRepo is a ConnectionRepository that is present but never consulted. The wiring
// test below needs a NON-NIL repo so the connection service gets past its storage check and
// reaches the orchestrator check — the one this test is actually about.
type fakeConnRepo struct{}

func (fakeConnRepo) Get(context.Context, string, model.Provider) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}
func (fakeConnRepo) Create(context.Context, *model.Connection) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}
func (fakeConnRepo) Update(context.Context, *model.Connection, int64) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}
func (fakeConnRepo) SetCredential(context.Context, string, model.Provider, []byte, *model.Actor) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}
func (fakeConnRepo) UpdateWithCredential(context.Context, *model.Connection, []byte, int64) (*model.Connection, error) {
	return nil, domain.ErrNotFound
}
func (fakeConnRepo) Delete(context.Context, string, model.Provider, *model.Actor) error {
	return domain.ErrNotFound
}

// fakeEncryptor is a no-op Encryptor, present for the same reason as fakeConnRepo.
type fakeEncryptor struct{}

func (fakeEncryptor) Encrypt(plaintext []byte) ([]byte, error)  { return plaintext, nil }
func (fakeEncryptor) Decrypt(ciphertext []byte) ([]byte, error) { return ciphertext, nil }

// TestConnectionService_AccountDiscoveryNeedsTheOrchestrator pins the wiring that both
// container startup paths perform and that nothing else covered: the fast path
// (NewContainer with a live pool) and the cold-start retry both call SetOrchestrator on the
// connection service, and account discovery is permanently 503 until one of them runs.
//
// The service-level tests all inject an orchestrator by hand, so deleting EITHER container
// call site leaves every one of them green while every deployed pod answers 503 forever.
//
// The repo is bound first and deliberately: with a nil repo the 503 comes from the STORAGE
// check and the orchestrator check is never reached, so a test that skipped SetBackend would
// pass no matter what SetOrchestrator did. The two 503s are told apart by their messages.
func TestConnectionService_AccountDiscoveryNeedsTheOrchestrator(t *testing.T) {
	// Both injection sites bind through backendSetter, so the concrete service must satisfy
	// it. A signature change breaks this line rather than one call site.
	var bs backendSetter = service.NewConnectionService(nil, nil)
	bs.SetBackend(fakeConnRepo{}, fakeEncryptor{})

	svc, ok := bs.(*service.ConnectionService)
	require.True(t, ok)

	// Pre-state: storage bound, orchestrator never injected. This is exactly what a pod
	// looks like if either container call site is removed.
	_, err := svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
	var unavail *conn.ConnServiceUnavailableError
	require.ErrorAs(t, err, &unavail,
		"without SetOrchestrator, account discovery must report the typed 503")
	require.Contains(t, unavail.Message, "account discovery",
		"the 503 must come from the missing ORCHESTRATOR, not from missing storage")

	// Post-injection: the same call now reaches the orchestrator. That orchestrator has no
	// dispatchers, so it answers 400 unsupported — a DIFFERENT typed error, which is the
	// point: only reaching the orchestration layer can produce it.
	bs.SetOrchestrator(&service.Orchestrator{})
	_, err = svc.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{ProjectID: "p"})
	var badReq *conn.BadRequestError
	require.ErrorAs(t, err, &badReq,
		"after SetOrchestrator the call must reach the orchestrator, not short-circuit on the nil check")
}

// TestContainer_BothStartupPathsInjectTheOrchestrator asserts that each of the two container
// startup paths actually performs the injection the test above proves is necessary.
//
// The test above drives the SERVICE: it binds the backend and calls SetOrchestrator by hand,
// so it proves discovery is 503 without the injection — but it never executes a line of
// container.go, and deleting either call site leaves it green. Both sites need a live
// pgxpool to reach in a unit test, so this asserts on the source, the way the shutdown-order
// invariants in this package are asserted. Each function body is bounded at the next
// top-level func so its sibling cannot satisfy the assertion on its behalf.
func TestContainer_BothStartupPathsInjectTheOrchestrator(t *testing.T) {
	src, err := os.ReadFile("container.go")
	require.NoError(t, err)

	body := func(sig string) string {
		i := strings.Index(string(src), sig)
		require.NotEqual(t, -1, i, "%s not found; update this test if it was renamed", sig)
		rest := string(src)[i+len(sig):]
		if j := strings.Index(rest, "\nfunc "); j != -1 {
			rest = rest[:j]
		}
		return rest
	}

	// The fast path: NewContainer reached a live pool and wires everything synchronously.
	assert.Contains(t, body("func (c *Container) wireLiveBackends("), "SetOrchestrator(orch)",
		"the live-backend path must inject the orchestrator into the connection service; without "+
			"it every pod that starts with a reachable DB answers 503 to account discovery forever")
	// The cold-start path: the DB was unreachable at boot, the container came up in 503 mode,
	// and a background goroutine binds the backends when the retry finally connects. It is
	// the easier one to forget precisely because it is not the path anyone runs locally.
	assert.Contains(t, body("func (c *Container) retryDatabaseInit("), "SetOrchestrator(orch)",
		"the cold-start retry must inject the orchestrator too; otherwise a pod that booted "+
			"before the database was reachable serves 503s for account discovery for its whole life")
}

// TestEventFetcherNAT64PrefixesReachTheDialer closes the END of the config chain.
//
// eventurl's own tests prove WithNAT64Prefixes reaches the dial hook, and config's tests
// prove EVENT_URL_NAT64_PREFIXES parses into Config.EventURLNAT64Prefixes. Neither says
// anything about the join in between, and that join is exactly one line
// (`eventFetcher` passing the slice on). Drop it — return a bare `eventurl.NewFetcher()`
// unconditionally — and every other test in both packages still passes while the only
// control an operator has over this SSRF class is silently dead.
//
// The failure that would hide behind that is not a degradation. On a cluster with a
// network-specific NAT64 prefix, an undeclared prefix means the TRANSLATOR makes the IPv4
// connection, so an address encoding 169.254.169.254 satisfies every check inside this
// process and the fetch reaches the cloud metadata endpoint.
//
// No socket is involved on the passing path: the address is refused inside the dialer's
// Control hook, before connect. The context deadline bounds the FAILING path, where the
// address is (correctly, for an unconfigured fetcher) allowed through to a real dial.
func TestEventFetcherNAT64PrefixesReachTheDialer(t *testing.T) {
	t.Setenv(constants.EnvEventURLNAT64Prefixes, "2a01:4f8:1::/48")

	cfg := config.LoadConfig()
	require.Equal(t, []string{"2a01:4f8:1::/48"}, cfg.EventURLNAT64Prefixes,
		"the env var did not parse; the rest of this test would prove nothing")

	c := &Container{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 2a01:4f8:1:a9fe:a9:fe00:: embeds 169.254.169.254 at the /48 layout (RFC 6052 splits
	// the address around the reserved octet at bits 64-71 for every prefix shorter than /96).
	_, err := c.eventFetcher().Fetch(ctx, "http://[2a01:4f8:1:a9fe:a9:fe00::]/event")
	require.ErrorIs(t, err, eventurl.ErrEventURLForbidden,
		"the configured prefix never reached the dial guard: an encoded metadata address "+
			"was allowed out of the process")

	// The well-known prefix is unconditional and must survive alongside the configured one.
	_, err = c.eventFetcher().Fetch(ctx, "http://[64:ff9b::a9fe:a9fe]/event")
	require.ErrorIs(t, err, eventurl.ErrEventURLForbidden,
		"configuring a prefix dropped the unconditional 64:ff9b::/96 decoding")
}

// TestEventFetcherWithoutConfigStillGuards pins that the unconfigured path is a real
// Fetcher and not a nil one — eventFetcher has two returns, and only one is exercised
// above.
func TestEventFetcherWithoutConfigStillGuards(t *testing.T) {
	for name, c := range map[string]*Container{
		"nil config":   {},
		"empty config": {Config: &config.Config{}},
	} {
		t.Run(name, func(t *testing.T) {
			f := c.eventFetcher()
			require.NotNil(t, f)
			// The well-known prefix is decoded with no configuration at all.
			_, err := f.Fetch(context.Background(), "http://[64:ff9b::a9fe:a9fe]/event")
			require.ErrorIs(t, err, eventurl.ErrEventURLForbidden)
		})
	}
}
