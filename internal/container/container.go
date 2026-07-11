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
)

// startupDBTimeout bounds the database connection/ping during container init so
// an unreachable or slow database fails the pod fast (and lets Kubernetes
// restart it) rather than wedging startup indefinitely.
const startupDBTimeout = 15 * time.Second

// dispatchDrainTimeout bounds how long shutdown waits for in-flight campaign
// dispatch to finish before the pool is closed. Kept under the server's overall
// shutdown budget so draining can't outlast the graceful-shutdown window.
const dispatchDrainTimeout = 20 * time.Second

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
// campaign dispatch (bounded by ctx and by dispatchDrainTimeout, whichever is
// sooner) so a dispatch that already created an upstream campaign isn't cut off
// before it persists, THEN closes the database pool. ctx is the shared
// graceful-shutdown deadline, so the drain can't run past the overall budget.
func (c *Container) Close(ctx context.Context) error {
	if c.orch != nil {
		drainCtx, cancel := context.WithTimeout(ctx, dispatchDrainTimeout)
		defer cancel()
		if err := c.orch.Shutdown(drainCtx); err != nil {
			slog.Warn("timed out draining in-flight dispatch on shutdown", "error", err)
		}
	}
	if c.pool != nil {
		c.pool.Close()
	}
	return nil
}
