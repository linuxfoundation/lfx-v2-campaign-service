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

// Container holds all application dependencies.
type Container struct {
	Config      *config.Config
	Service     svc.Service
	Connections connsvc.Service
	Briefs      briefsvc.Service

	pool *postgres.Pool
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
		slog.Warn("database URL not set; connection endpoints will return 503 Service Unavailable")
		c.Service = service.NewCampaignService(nil)
		// Wire the connection service with a nil repo so its routes are still
		// mounted and return the typed 503 ServiceUnavailable advertised by the
		// OpenAPI contract, rather than a bare 404 from unmounted routes.
		c.Connections = service.NewConnectionService(nil, nil)
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
	dispatchers := map[model.Provider]service.PlatformDispatcher(nil)
	if len(dispatchers) == 0 {
		slog.Warn("no platform dispatchers registered; campaign creation will record jobs but perform no upstream dispatch")
	}
	orch := service.NewOrchestrator(briefRepo, campaignRepo, jobRepo, dispatchers)
	c.Briefs = service.NewBriefService(briefRepo, campaignRepo, jobRepo, orch)

	// The health service's readiness depends on the database pool (Readyz).
	c.Service = service.NewCampaignService(pool)

	if host := cfg.RedactedDatabaseHost(); host != "" {
		slog.Info("dependency container initialized", "database", host)
	} else {
		slog.Info("dependency container initialized")
	}
	return c, nil
}

// Close releases any resources held by the container.
func (c *Container) Close() error {
	if c.pool != nil {
		c.pool.Close()
	}
	return nil
}
