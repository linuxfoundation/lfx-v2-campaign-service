// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package container provides dependency injection for the application.
package container

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// startupDBTimeout bounds ONE database migration+pool-open attempt. It is a var
// (not a const) only so tests can shrink it; production never changes it.
var startupDBTimeout = 15 * time.Second

// dbRetryInterval is the pause between background DB-init attempts during a cold
// start. A var for the same test-only reason.
var dbRetryInterval = 3 * time.Second

// setBackends is the interface the container needs to late-bind the database once
// the pool opens. Both service types implement it.
type readinessSetter interface {
	SetReadinessDep(service.ReadinessChecker)
}
type backendSetter interface {
	SetBackend(domain.ConnectionRepository, domain.Encryptor)
}

// briefBackendSetter is the interface the container needs to late-bind the brief
// service's repos + orchestrator after a cold-start retry opens the pool. *BriefService
// implements it. Kept separate from backendSetter (whose SetBackend has a different
// signature) so the retry path can wire both.
type briefBackendSetter interface {
	SetBackend(domain.BriefRepository, domain.CampaignRepository, domain.JobRepository, *service.Orchestrator)
}

// notReady is a ReadinessChecker that always reports not-ready. It is wired as
// the health dependency during a cold start so /readyz returns 503 (not OK)
// until the real pool is swapped in — distinct from the no-database mode, where
// no dependency is wired and /readyz reports ready.
type notReady struct{}

func (notReady) Ready(context.Context) bool { return false }

// dispatchDrainTimeout bounds how long Container.Close waits for in-flight
// campaign dispatch to finish before the pool is closed. Together with the
// orchestrator's post-cancel grace (service.CancelGracePeriod) it forms
// ContainerCloseTimeout, which is reserved out of the overall graceful-shutdown
// budget (constants.DefaultShutdownTimeout) so the HTTP-drain phase and this
// phase can't sum past it and get SIGKILLed. Validated by the init() below.
//
// Sized so dispatchDrainTimeout + CancelGracePeriod leaves a positive HTTP-drain
// budget: CancelGracePeriod grew to cover the post-provider persist AND the
// terminal finalize write (both detached, both must complete during grace), so
// the drain window is trimmed to keep the total within DefaultShutdownTimeout.
const dispatchDrainTimeout = 6 * time.Second

// ContainerCloseTimeout is the wall-clock budget for Container.Close: the
// orchestrator drain (dispatchDrainTimeout) plus its post-cancel grace
// (service.CancelGracePeriod). The server budgets the HTTP-shutdown phase and
// this container-close phase separately (see HTTPShutdownTimeout), so the total
// graceful shutdown is a true sum bounded by constants.DefaultShutdownTimeout.
const ContainerCloseTimeout = dispatchDrainTimeout + service.CancelGracePeriod

// HTTPShutdownTimeout is the wall-clock budget for draining in-flight HTTP
// handlers before the container is closed. It is whatever remains of the overall
// graceful-shutdown budget after the container-close phase is reserved, so the
// two sequential phases can never sum past DefaultShutdownTimeout (which would
// otherwise risk a SIGKILL mid-drain — the orchestrator's grace timer is
// wall-clock, not bound by a shared context).
const HTTPShutdownTimeout = constants.DefaultShutdownTimeout - ContainerCloseTimeout

// HandlerDrainTimeout is a SEPARATE, dedicated budget for waiting on in-flight
// handler goroutines to return AFTER a forced srv.Close(). It must not be derived
// from the HTTP-shutdown context's remaining time: when srv.Shutdown times out,
// that context is already exhausted, so a "remaining budget" would be zero and
// the wait would return immediately — defeating the tracker exactly when a
// straggler is running. Reserving a small fixed slice guarantees a bounded wait
// in that case. It is carved from HTTPShutdownTimeout so the total still fits.
const HandlerDrainTimeout = 2 * time.Second

func init() {
	if dispatchDrainTimeout+service.CancelGracePeriod > constants.DefaultShutdownTimeout {
		panic("dispatchDrainTimeout + service.CancelGracePeriod exceeds DefaultShutdownTimeout")
	}
	// The HTTP phase must have a positive budget once the container-close phase
	// is reserved; otherwise HTTP handlers would get no drain window at all.
	if HTTPShutdownTimeout <= 0 {
		panic("HTTPShutdownTimeout is non-positive: ContainerCloseTimeout consumes the entire DefaultShutdownTimeout")
	}
}

// Container holds all application dependencies.
type Container struct {
	Config      *config.Config
	Service     svc.Service
	Connections connsvc.Service
	Briefs      briefsvc.Service

	// mu guards pool, which the background DB-init goroutine sets once the pool
	// opens and Close reads on shutdown.
	mu   sync.Mutex
	pool *postgres.Pool
	orch *service.Orchestrator

	// cancelInit stops the background DB-init goroutine (nil when none runs).
	cancelInit context.CancelFunc
	// initDone is closed when the background goroutine exits, so Close can wait
	// for it (nil when no goroutine runs).
	initDone chan struct{}
}

// NewContainer creates and wires all application dependencies.
//
// If a database URL is configured it runs migrations and opens the pool. On
// SUCCESS everything is wired against the live pool immediately. On a TRANSIENT
// failure (database unreachable / migration deadline) the container does NOT
// fail the process: it boots the services in 503 mode (health reports not-ready,
// connection endpoints return the typed 503) and retries migration+pool in the
// BACKGROUND, swapping the live pool in once it opens. This is what makes the
// deployment's ~90s startupProbe budget real: /readyz stays 503 during a DB cold
// start and the pod is kept alive, rather than the process exiting at the first
// 15s attempt and crash-looping.
//
// Config errors that a retry cannot fix (invalid database settings, a bad
// credential-encryption key) still fail fast — those return an error and the
// process exits.
//
// If no database URL is configured, the connection service is wired with a nil
// repo so its routes stay mounted and return the typed 503 ServiceUnavailable
// from the OpenAPI contract instead of a bare 404; the health endpoints report
// ready in that mode.
func NewContainer(cfg *config.Config) (*Container, error) {
	slog.Info("initializing dependency container")

	if err := cfg.ValidateDatabaseSettings(); err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}

	c := &Container{Config: cfg}

	if cfg.DatabaseURL == "" {
		slog.Warn("database URL not set; connection and brief/campaign endpoints will return 503 Service Unavailable")
		c.Service = service.NewCampaignService(nil)
		// Wire the connection + brief services with nil repos so their routes are
		// still mounted and return the typed 503 ServiceUnavailable advertised by
		// the OpenAPI contract, rather than a bare 404 from unmounted routes.
		c.Connections = service.NewConnectionService(nil, nil)
		c.Briefs = service.NewBriefService(nil, nil, nil, nil)
		slog.Info("dependency container initialized (no database)")
		return c, nil
	}

	// A bad credential-encryption key is a config error, not a transient DB
	// problem, so fail fast (a retry can't fix it).
	enc, err := crypto.NewAESGCMFromBase64(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("init credential encryptor: %w", err)
	}

	// A malformed DATABASE_URL (e.g. a keyword DSN migrations can't consume) is a
	// deterministic config error that NO retry can fix — fail fast rather than
	// entering the background retry loop and 503-looping forever. (Transient
	// DB-unavailability, by contrast, IS handled by the retry path below.)
	if err := postgres.ValidateMigrationDSN(cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}

	// Fast path: one synchronous attempt. On success, wire everything now.
	pool, initErr := initDatabase(context.Background(), cfg.DatabaseURL)
	switch {
	case initErr == nil:
		c.setPool(pool)
		c.wireLiveBackends(pool, enc, cfg)
		if host := cfg.RedactedDatabaseHost(); host != "" {
			slog.Info("dependency container initialized", "database", host)
		} else {
			slog.Info("dependency container initialized")
		}
		return c, nil
	case postgres.IsPermanentMigrationErr(initErr):
		// A dirty schema (or other permanent migration state) can NEVER be cleared by
		// retrying — it needs an operator to inspect and force the version. Fail fast
		// so the failure is loud (pod crash) rather than a silent 503 loop that burns
		// the startup-probe budget and then restarts to the same broken state.
		return nil, fmt.Errorf("database migration is in a permanent-failure state (needs manual recovery): %w", initErr)
	default:
		slog.Warn("database not ready at startup; booting in 503 mode and retrying in the background",
			"error", initErr.Error())
	}

	// Transient failure: boot in 503 mode. Wire the health dependency to notReady
	// (so /readyz reports 503, unlike the no-database mode), the connection service
	// with a nil repo, and the brief service with nil repos (its routes stay mounted
	// and return the typed 503). The background goroutine late-binds the live
	// pool/repos into ALL THREE (connection, brief, health readiness) once it opens,
	// so brief + job routes go live without a pod restart.
	campaign := service.NewCampaignService(notReady{})
	connections := service.NewConnectionService(nil, enc)
	briefs := service.NewBriefService(nil, nil, nil, nil)
	c.Service = campaign
	c.Connections = connections
	c.Briefs = briefs

	initCtx, cancel := context.WithCancel(context.Background())
	c.cancelInit = cancel
	c.initDone = make(chan struct{})
	go c.retryDatabaseInit(initCtx, cfg, enc, campaign, connections, briefs)

	return c, nil
}

// wireLiveBackends wires all services against a live pool: the connection service
// (repo + encryptor), the brief service and its async orchestrator (brief/campaign/
// job repos; no dispatchers registered yet, so a startup warning notes campaigns
// record jobs but perform no upstream dispatch), and the campaign/health service so
// /readyz reflects DB connectivity. It also recovers jobs orphaned by a prior pod
// restart and starts the periodic recovery sweeper. Shared by the fast path (and,
// once brief/orchestrator late-binding exists, reusable from the retry path).
func (c *Container) wireLiveBackends(pool *postgres.Pool, enc domain.Encryptor, cfg *config.Config) {
	repo := postgres.NewConnectionRepo(pool)
	c.Connections = service.NewConnectionService(repo, enc)
	briefRepo := postgres.NewBriefRepo(pool)
	campaignRepo := postgres.NewCampaignRepo(pool)
	jobRepo := postgres.NewJobRepo(pool)
	// No platform dispatchers are registered yet; campaign creation records a
	// job whose platforms report "no dispatcher" until the per-platform adapters
	// land (LFXV2-2636..2640). The orchestration flow (job lifecycle,
	// persistence) is exercised end to end regardless. Log a startup warning so
	// this gap is visible in production logs rather than silently producing jobs
	// that always finish "failed" with "no dispatcher registered".
	dispatchers := map[model.Provider]service.PlatformDispatcher{}
	if len(dispatchers) == 0 {
		slog.Warn("no platform dispatchers registered; campaign creation will record jobs but perform no upstream dispatch")
	}
	orch := service.NewOrchestrator(campaignRepo, jobRepo, dispatchers)
	c.orch = orch
	c.Briefs = service.NewBriefService(briefRepo, campaignRepo, jobRepo, orch)

	// Recover jobs orphaned by a previous pod's restart: a queued/running job's
	// dispatch goroutine lived only in that process, so fail them forward now
	// rather than leaving them non-terminal forever. Bounded by the startup
	// deadline; a failure here is logged but not fatal (the service can still run).
	recoverCtx, cancel := context.WithTimeout(context.Background(), startupDBTimeout)
	defer cancel()
	if n, rerr := jobRepo.FailStuckJobs(recoverCtx, "job did not complete before a service restart"); rerr != nil {
		slog.Warn("failed to recover stuck jobs on startup", "error", rerr)
	} else if n > 0 {
		slog.Info("recovered stuck jobs on startup", "count", n)
	}

	// The startup scan can't recover a job orphaned by a crash younger than the
	// stale cutoff (too new to look stuck at boot, never re-examined). A periodic
	// sweep catches those; it stops on Shutdown via the orchestrator's root ctx.
	orch.StartRecoverySweeper()

	// The health service's readiness depends on the database pool (Readyz).
	c.Service = service.NewCampaignService(pool)
}

// retryDatabaseInit keeps attempting migration+pool-open until it succeeds or the
// context is cancelled (shutdown). On success it late-binds the live pool into ALL
// the mounted services — connection, brief (repos + orchestrator), and health
// readiness — and runs the same stuck-job recovery + periodic sweeper the fast path
// does, so /readyz flips to healthy AND the connection + brief/job routes go live
// without a pod restart.
func (c *Container) retryDatabaseInit(ctx context.Context, cfg *config.Config, enc domain.Encryptor, r readinessSetter, b backendSetter, bb briefBackendSetter) {
	defer close(c.initDone)

	for attempt := 1; ; attempt++ {
		pool, err := initDatabase(ctx, cfg.DatabaseURL)
		if err == nil {
			c.setPool(pool)
			// Late-bind the connection service.
			b.SetBackend(postgres.NewConnectionRepo(pool), enc)
			// Late-bind the brief service (repos + orchestrator) so brief/job routes go
			// live, and run the stuck-job recovery + start the periodic sweeper — the
			// same work wireLiveBackends does on the fast path.
			briefRepo := postgres.NewBriefRepo(pool)
			campaignRepo := postgres.NewCampaignRepo(pool)
			jobRepo := postgres.NewJobRepo(pool)
			dispatchers := map[model.Provider]service.PlatformDispatcher{}
			if len(dispatchers) == 0 {
				slog.Warn("no platform dispatchers registered; campaign creation will record jobs but perform no upstream dispatch")
			}
			orch := service.NewOrchestrator(campaignRepo, jobRepo, dispatchers)
			// Safe without a lock: Close() waits on <-c.initDone (closed when this
			// goroutine returns) before it reads c.orch, so this write happens-before
			// that read.
			c.orch = orch
			bb.SetBackend(briefRepo, campaignRepo, jobRepo, orch)
			recoverCtx, cancelRecover := context.WithTimeout(context.Background(), startupDBTimeout)
			if n, rerr := jobRepo.FailStuckJobs(recoverCtx, "job did not complete before a service restart"); rerr != nil {
				slog.Warn("failed to recover stuck jobs on startup", "error", rerr)
			} else if n > 0 {
				slog.Info("recovered stuck jobs on startup", "count", n)
			}
			cancelRecover()
			orch.StartRecoverySweeper()
			// Flip readiness LAST, so /readyz only reports healthy after the brief
			// service is fully wired (avoids a window where /readyz is OK but brief
			// routes still 503).
			r.SetReadinessDep(pool)
			if host := cfg.RedactedDatabaseHost(); host != "" {
				slog.Info("database now ready; connection + brief endpoints live", "database", host, "attempts", attempt)
			} else {
				slog.Info("database now ready; connection + brief endpoints live", "attempts", attempt)
			}
			return
		}
		// A permanent migration state (dirty schema) will never clear by retrying, so
		// stop the loop and surface it loudly. /readyz stays at 503 with no live pool,
		// but the ERROR log makes the reason unambiguous instead of an endless silent
		// "will retry" stream — an operator must force the migration version.
		if postgres.IsPermanentMigrationErr(err) {
			slog.Error("background database initialization hit a permanent migration failure (needs manual recovery); stopping retries",
				"attempt", attempt, "error", err.Error())
			return
		}
		slog.Warn("background database initialization attempt failed; will retry",
			"attempt", attempt, "retryIn", dbRetryInterval.String(), "error", err.Error())

		select {
		case <-ctx.Done():
			slog.Info("stopping background database initialization (shutdown)")
			return
		case <-time.After(dbRetryInterval):
		}
	}
}

// setPool stores the live pool under the lock (Close reads it on shutdown).
func (c *Container) setPool(pool *postgres.Pool) {
	c.mu.Lock()
	c.pool = pool
	c.mu.Unlock()
}

// initDatabase runs migrations and opens the pool within a single bounded
// attempt. golang-migrate's Up() takes no context, so it is bounded by running
// it under the same deadline. Returns the live pool or an error.
func initDatabase(parent context.Context, dsn string) (*postgres.Pool, error) {
	ctx, cancel := context.WithTimeout(parent, startupDBTimeout)
	defer cancel()

	// Open the pool FIRST: NewPool does a context-bounded Ping (pool.go), so when the
	// database is unreachable this fails fast within the deadline. golang-migrate's
	// Up() takes no context and blocks until the DB responds, so running it against a
	// down database would hang past the deadline — and because the caller retries,
	// each hung attempt would leak another migration goroutine and race concurrent
	// migrations. Gating Migrate behind a successful (reachable) Ping ensures Migrate
	// only runs when the DB is actually up, where it connects immediately, so no
	// migration goroutine is ever left blocked and retries never overlap.
	pool, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}

	// Migrate only after a reachable ping (above), so it connects immediately rather
	// than blocking against a down DB. It can still run long on a reachable DB if a
	// migration is slow or lock-blocked, and golang-migrate's Up() takes no context,
	// so bound it with the startup deadline: run it under migrateMu (only ONE
	// migration ever runs at a time, so a retry can't start a second while a prior is
	// still finishing) and return on the deadline. On timeout the in-flight migration
	// keeps running under the lock; the next retry blocks on migrateMu until it
	// finishes rather than launching an overlapping one.
	migrateDone := make(chan error, 1)
	go func() {
		migrateMu.Lock()
		defer migrateMu.Unlock()
		migrateDone <- postgres.Migrate(dsn)
	}()
	select {
	case mErr := <-migrateDone:
		if mErr != nil {
			pool.Close()
			return nil, fmt.Errorf("run migrations: %w", mErr)
		}
	case <-ctx.Done():
		pool.Close()
		return nil, fmt.Errorf("run migrations: %w", ctx.Err())
	}
	return pool, nil
}

// migrateMu serializes golang-migrate runs so a retry never starts a second
// migration while a prior (possibly deadline-abandoned) one is still finishing.
var migrateMu sync.Mutex

// Close releases any resources held by the container. It first stops the background
// DB-init goroutine (if a cold start is still retrying), then drains in-flight
// campaign dispatch so a dispatch that already created an upstream campaign isn't
// cut off before it persists, THEN closes the database pool.
//
// Orchestrator.Shutdown runs two separately-budgeted phases: a clean drain
// bounded by dispatchDrainTimeout, then (only if that elapses) a post-cancel
// grace bounded by CancelGracePeriod. Both must fit within ctx, so ctx MUST
// carry the full ContainerCloseTimeout (= dispatchDrainTimeout +
// CancelGracePeriod), not just the drain timeout — otherwise the grace phase
// would have zero budget and the pool could close while a just-cancelled
// dispatch is still finalizing job/campaign state.
func (c *Container) Close(ctx context.Context) error {
	// Stop the background DB-init goroutine first (if the container booted in 503
	// mode and is still retrying), and wait for it to exit so it can't open/swap a
	// pool after we've decided to shut down.
	if c.cancelInit != nil {
		c.cancelInit()
		<-c.initDone
	}
	// Capture the orchestrator shutdown error but do NOT early-return on it: the
	// pool must still be closed even if the drain timed out with dispatches still
	// running. Returning the error (rather than swallowing it) makes a shutdown
	// failure — dispatches still running when the pool was closed — observable to
	// the caller's "container close error" branch and its logs.
	var shutdownErr error
	if c.orch != nil {
		if err := c.orch.Shutdown(ctx, dispatchDrainTimeout); err != nil {
			slog.Warn("timed out draining in-flight dispatch on shutdown", "error", err)
			shutdownErr = fmt.Errorf("drain in-flight dispatch: %w", err)
		}
	}
	c.mu.Lock()
	pool := c.pool
	c.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	return shutdownErr
}
