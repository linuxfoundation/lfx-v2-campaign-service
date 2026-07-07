// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package postgres provides the PostgreSQL connection pool, migration runner,
// and repository implementations for the campaign service.
package postgres

import (
	"context"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// NewPool opens a pgx connection pool for the given DSN and verifies
// connectivity with a ping.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Ready reports whether the pool can reach the database. Used by the readiness
// probe.
func (p *Pool) Ready(ctx context.Context) bool {
	return p.Ping(ctx) == nil
}

// Migrate applies all pending up migrations from the embedded migration files.
// It is safe to call on every startup; already-applied migrations are skipped.
func Migrate(dsn string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open migration source: %w", err)
	}
	// golang-migrate expects the pgx5 driver via the "pgx5://" URL scheme.
	m, err := migrate.NewWithSourceInstance("iofs", src, pgxURL(dsn))
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// pgxURL ensures the DSN uses the pgx5 scheme golang-migrate's driver expects.
// A standard "postgres://" or "postgresql://" DSN is rewritten to "pgx5://".
func pgxURL(dsn string) string {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if len(dsn) >= len(prefix) && dsn[:len(prefix)] == prefix {
			return "pgx5://" + dsn[len(prefix):]
		}
	}
	return dsn
}

// ensure the pgx5 migrate driver is linked.
var _ = pgx.Postgres{}
