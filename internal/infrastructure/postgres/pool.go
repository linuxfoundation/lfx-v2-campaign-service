// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package postgres provides the PostgreSQL connection pool, migration runner,
// and repository implementations for the campaign service.
package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/exaring/otelpgx"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

var tracerName = "github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"

// readyTracer returns the current global tracer so tests can install an
// in-memory provider without fighting a package-init Tracer binding.
func readyTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// NewPool opens an instrumented pgx connection pool for the given DSN and
// verifies connectivity with a ping.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		// Close in the background so a failed ping returns immediately rather
		// than blocking startup while the pool drains (an unreachable DB in k8s
		// can otherwise wedge boot until the liveness probe restarts the pod).
		go pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Ready reports whether the pool can reach the database. Used by the readiness
// probe. Emits an explicit health span because /readyz is excluded from
// otelhttp and pgxpool.Ping does not go through otelpgx's Query/Exec hooks.
func (p *Pool) Ready(ctx context.Context) bool {
	return p.checkReady(ctx, p.Ping)
}

// checkReady runs ping under a postgres.ready span. ping is injectable so unit
// tests can cover success/failure without a live database.
func (p *Pool) checkReady(ctx context.Context, ping func(context.Context) error) bool {
	ctx, span := readyTracer().Start(ctx, "postgres.ready",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system", "postgresql")),
	)
	defer span.End()

	if err := ping(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "database ping failed")
		return false
	}
	span.SetStatus(codes.Ok, "")
	return true
}

// Migrate applies all pending up migrations from the embedded migration files.
// It is safe to call on every startup; already-applied migrations are skipped.
func Migrate(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open migration source: %w", err)
	}
	// golang-migrate's pgx5 driver requires a URL-scheme DSN (pgx5://…). A
	// keyword/DSN string ("host=… user=…") cannot be consumed here, so it is
	// rejected with a clear error rather than silently failing driver
	// selection. (pgxpool.New accepts both forms, but Migrate needs a URL.)
	migrateURL, err := pgxURL(dsn)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// pgxURL converts a URL-scheme DSN to the "pgx5://" scheme golang-migrate's
// driver expects. A "postgres://" / "postgresql://" DSN is rewritten; an
// ValidateMigrationDSN reports whether dsn is in the URL form migrations require,
// WITHOUT connecting. A keyword DSN ("host=… user=…") is a deterministic config
// error that no retry can fix, so callers use this to fail fast up front rather
// than entering a retry loop that can never succeed.
func ValidateMigrationDSN(dsn string) error {
	_, err := pgxURL(dsn)
	return err
}

// already-"pgx5://" DSN is passed through. A keyword DSN ("host=… user=…") has
// no URL scheme golang-migrate can parse and is rejected with a clear error.
func pgxURL(dsn string) (string, error) {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(dsn, prefix) {
			return "pgx5://" + strings.TrimPrefix(dsn, prefix), nil
		}
	}
	if strings.HasPrefix(dsn, "pgx5://") {
		return dsn, nil
	}
	return "", fmt.Errorf("DATABASE_URL must be a URL DSN (postgres://…) for migrations; keyword DSNs are not supported")
}

// ensure the pgx5 migrate driver is linked.
var _ = pgx.Postgres{}
