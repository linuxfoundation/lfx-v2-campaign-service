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

	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
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

// notReady is a ReadinessChecker that always reports not-ready. It is wired as
// the health dependency during a cold start so /readyz returns 503 (not OK)
// until the real pool is swapped in — distinct from the no-database mode, where
// no dependency is wired and /readyz reports ready.
type notReady struct{}

func (notReady) Ready(context.Context) bool { return false }

// Container holds all application dependencies.
type Container struct {
	Config      *config.Config
	Service     svc.Service
	Connections connsvc.Service

	// mu guards pool, which the background DB-init goroutine sets once the pool
	// opens and Close reads on shutdown.
	mu   sync.Mutex
	pool *postgres.Pool

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
		slog.Warn("database URL not set; connection endpoints will return 503 Service Unavailable")
		c.Service = service.NewCampaignService(nil)
		// Wire the connection service with a nil repo so its routes are still
		// mounted and return the typed 503 ServiceUnavailable advertised by the
		// OpenAPI contract, rather than a bare 404 from unmounted routes.
		c.Connections = service.NewConnectionService(nil, nil)
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

	// Fast path: one synchronous attempt. On success, wire the live pool now.
	if pool, initErr := initDatabase(context.Background(), cfg.DatabaseURL); initErr == nil {
		c.setPool(pool)
		repo := postgres.NewConnectionRepo(pool)
		c.Connections = service.NewConnectionService(repo, enc)
		c.Service = service.NewCampaignService(pool)
		if host := cfg.RedactedDatabaseHost(); host != "" {
			slog.Info("dependency container initialized", "database", host)
		} else {
			slog.Info("dependency container initialized")
		}
		return c, nil
	} else {
		slog.Warn("database not ready at startup; booting in 503 mode and retrying in the background",
			"error", initErr.Error())
	}

	// Transient failure: boot in 503 mode. Wire the health dependency to notReady
	// (so /readyz reports 503, unlike the no-database mode) and the connection
	// service with a nil repo. The background goroutine swaps in the live
	// pool/repo once it opens, flipping both to healthy.
	campaign := service.NewCampaignService(notReady{})
	connections := service.NewConnectionService(nil, enc)
	c.Service = campaign
	c.Connections = connections

	initCtx, cancel := context.WithCancel(context.Background())
	c.cancelInit = cancel
	c.initDone = make(chan struct{})
	go c.retryDatabaseInit(initCtx, cfg, enc, campaign, connections)

	return c, nil
}

// retryDatabaseInit keeps attempting migration+pool-open until it succeeds or the
// context is cancelled (shutdown). On success it swaps the live pool into both
// services so /readyz flips to healthy and the connection endpoints go live.
func (c *Container) retryDatabaseInit(ctx context.Context, cfg *config.Config, enc domain.Encryptor, r readinessSetter, b backendSetter) {
	defer close(c.initDone)

	for attempt := 1; ; attempt++ {
		pool, err := initDatabase(ctx, cfg.DatabaseURL)
		if err == nil {
			c.setPool(pool)
			b.SetBackend(postgres.NewConnectionRepo(pool), enc)
			r.SetReadinessDep(pool)
			if host := cfg.RedactedDatabaseHost(); host != "" {
				slog.Info("database now ready; connection endpoints live", "database", host, "attempts", attempt)
			} else {
				slog.Info("database now ready; connection endpoints live", "attempts", attempt)
			}
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

// Close releases any resources held by the container and stops the background
// DB-init goroutine if one is running.
func (c *Container) Close() error {
	if c.cancelInit != nil {
		c.cancelInit()
		<-c.initDone
	}
	c.mu.Lock()
	pool := c.pool
	c.mu.Unlock()
	if pool != nil {
		pool.Close()
	}
	return nil
}
