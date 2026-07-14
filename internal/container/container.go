// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package container provides dependency injection for the application.
package container

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// startupDBTimeout bounds the database connection/ping during container init so
// an unreachable or slow database fails the pod fast (and lets Kubernetes
// restart it) rather than wedging startup indefinitely.
const startupDBTimeout = 15 * time.Second

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

	pool *postgres.Pool
	orch *service.Orchestrator
}

// NewContainer creates and wires all application dependencies.
//
// If a database URL is configured, it runs migrations, opens the pool, and
// wires the connection service against it. Otherwise the connection service is
// still wired (with a nil repo) so its routes stay mounted and return the typed
// 503 ServiceUnavailable from the OpenAPI contract instead of a bare 404; only
// the health endpoints are backed by real data in that mode.
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

	enc, err := crypto.NewAESGCMFromBase64(cfg.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("init credential encryptor: %w", err)
	}

	startupCtx, cancel := context.WithTimeout(context.Background(), startupDBTimeout)
	defer cancel()

	// golang-migrate's Up() takes no context, so bound it with the same startup
	// deadline: run it in a goroutine and fail fast if the database is
	// unreachable, rather than letting migration wedge boot indefinitely.
	migrateErr := make(chan error, 1)
	go func() { migrateErr <- postgres.Migrate(cfg.DatabaseURL) }()
	select {
	case err := <-migrateErr:
		if err != nil {
			return nil, fmt.Errorf("run migrations: %w", err)
		}
	case <-startupCtx.Done():
		return nil, fmt.Errorf("run migrations: %w", startupCtx.Err())
	}

	pool, err := postgres.NewPool(startupCtx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", err)
	}
	c.pool = pool

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
	if n, rerr := jobRepo.FailStuckJobs(startupCtx, "job did not complete before a service restart"); rerr != nil {
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

	if host := cfg.RedactedDatabaseHost(); host != "" {
		slog.Info("dependency container initialized", "database", host)
	} else {
		slog.Info("dependency container initialized")
	}
	return c, nil
}

// Close releases any resources held by the container. It first drains in-flight
// campaign dispatch so a dispatch that already created an upstream campaign
// isn't cut off before it persists, THEN closes the database pool.
//
// Orchestrator.Shutdown runs two separately-budgeted phases: a clean drain
// bounded by dispatchDrainTimeout, then (only if that elapses) a post-cancel
// grace bounded by CancelGracePeriod. Both must fit within ctx, so ctx MUST
// carry the full ContainerCloseTimeout (= dispatchDrainTimeout +
// CancelGracePeriod), not just the drain timeout — otherwise the grace phase
// would have zero budget and the pool could close while a just-cancelled
// dispatch is still finalizing job/campaign state.
func (c *Container) Close(ctx context.Context) error {
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
	if c.pool != nil {
		c.pool.Close()
	}
	return shutdownErr
}
