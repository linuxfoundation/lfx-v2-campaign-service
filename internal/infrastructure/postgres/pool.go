// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package postgres provides the PostgreSQL connection pool, migration runner,
// and repository implementations for the campaign service.
package postgres

import (
	"context"
	"errors"
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

// dsnParseError is a DSN-free wrapper for a pgx ParseConfig failure. pgx's
// ParseConfigError DOES redact the password before rendering (verified), but that
// redaction is a best-effort dependency detail we don't want a credential-bearing
// DATABASE_URL to rely on: NewContainer propagates this error and main logs it, so a
// regression in pgx's redaction (or an exotic DSN shape it doesn't cover) would leak
// the secret into logs. Error() therefore renders only a STATIC message; the original
// parser error is retained via Unwrap for errors.Is/As, not for display.
type dsnParseError struct {
	context string
	err     error
}

func (e *dsnParseError) Error() string {
	// Deliberately does NOT include e.err (which quotes the DSN) or the DSN itself.
	return e.context + ": invalid DATABASE_URL (redacted; check host/port/params)"
}
func (e *dsnParseError) Unwrap() error { return e.err }

// NewPool opens an instrumented pgx connection pool for the given DSN and
// verifies connectivity with a ping.
func NewPool(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, &dsnParseError{context: "parse database config", err: err}
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
// It is safe to call on every startup when the schema is CLEAN: already-applied
// migrations are skipped. It does NOT silently re-run a PARTIALLY-applied migration —
// golang-migrate marks such a schema dirty (migrate.ErrDirty, surfaced by
// IsPermanentMigrationErr) and refuses to proceed until an operator forces the
// version, since partial migration SQL is not assumed idempotent.
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
		// golang-migrate marks the schema version dirty (SetVersion(target, true))
		// BEFORE running a migration's SQL, then returns the raw SQL/driver error if
		// that SQL fails — NOT ErrDirty. So on the FIRST failing migration Up()
		// returns the underlying error while the schema is already dirty, and only a
		// SUBSEQUENT Up() (which hits the dirty pre-check) surfaces ErrDirty. Without
		// this, the caller misclassifies that first permanent failure as transient,
		// boots 503, and only fails fast on the next retry. Re-check the dirty state
		// here so a newly-dirtied schema fails fast on the very first attempt.
		var dirtyErr migrate.ErrDirty
		if !errors.As(err, &dirtyErr) {
			if v, dirty, verr := m.Version(); verr == nil && dirty {
				return fmt.Errorf("apply migrations (schema left dirty): %w", migrate.ErrDirty{Version: int(v)})
			}
		}
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// IsPermanentMigrationErr reports whether a Migrate error is a PERMANENT state that
// retrying can never clear on its own — today, a dirty schema (migrate.ErrDirty),
// which golang-migrate sets when a prior migration failed partway and leaves the
// schema_migrations row marked dirty. It requires an operator to inspect and `force`
// the version; a boot loop that just re-runs Migrate will hit ErrDirty forever. The
// caller uses this to fail fast (surface the error) instead of 503-looping silently.
// A connectivity/lock/deadline failure is NOT permanent and is deliberately excluded
// so it still retries.
func IsPermanentMigrationErr(err error) bool {
	var dirty migrate.ErrDirty
	return errors.As(err, &dirty)
}

// ValidateMigrationDSN reports whether dsn is in the URL form migrations require,
// WITHOUT connecting. It checks BOTH that the DSN has a URL scheme golang-migrate
// can consume (not a keyword "host=… user=…" DSN) AND that it actually parses as a
// pgx config — a syntactically broken URL like "postgres://[bad" passes the prefix
// check but would fail deep in NewPool/Migrate, so we reject it up front. A keyword
// or malformed DSN is a deterministic config error no retry can fix, so callers use
// this to fail fast rather than entering a retry loop that can never succeed.
func ValidateMigrationDSN(dsn string) error {
	if _, err := pgxURL(dsn); err != nil {
		return err
	}
	// Also verify the URL actually parses with the SAME parser NewPool uses, so a
	// syntactically broken URL like "postgres://[bad" is caught here rather than
	// deep in NewPool. pgxURL above already rejected every scheme NewPool can't
	// open (keyword DSNs and the internal "pgx5://"), so anything reaching here is
	// a postgres/postgresql URL that pgxpool.ParseConfig must accept.
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		// DSN-free wrapper: this message is surfaced to callers/logs, and the DSN
		// carries the DB password. See dsnParseError.
		return &dsnParseError{context: "DATABASE_URL is not a parseable postgres URL", err: err}
	}
	return nil
}

// pgxURL converts a URL-scheme DATABASE_URL to the "pgx5://" scheme
// golang-migrate's driver expects. A "postgres://" / "postgresql://" DSN is
// rewritten. Any other form is rejected with a clear error: a keyword DSN
// ("host=… user=…") has no URL scheme golang-migrate can parse, and a raw
// "pgx5://" DSN — golang-migrate's INTERNAL scheme — is NOT accepted as input.
// NewPool opens the same DATABASE_URL via pgxpool.ParseConfig, which cannot
// parse "pgx5://", so passing it through here would let a "pgx5://" URL clear
// ValidateMigrationDSN and then fail every pool open as a "transient" error,
// retrying the 503 cold-start loop forever with no fail-fast. The only
// legitimate source of "pgx5://" is this function's own translation.
func pgxURL(dsn string) (string, error) {
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if strings.HasPrefix(dsn, prefix) {
			return "pgx5://" + strings.TrimPrefix(dsn, prefix), nil
		}
	}
	return "", fmt.Errorf("DATABASE_URL must be a postgres:// or postgresql:// URL; keyword DSNs and the internal pgx5:// scheme are not supported")
}

// ensure the pgx5 migrate driver is linked.
var _ = pgx.Postgres{}
