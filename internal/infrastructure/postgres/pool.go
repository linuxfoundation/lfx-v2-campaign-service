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
	"time"

	"github.com/exaring/otelpgx"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	// pgxv5 is aliased because golang-migrate's driver above already occupies the
	// identifier `pgx` (see the pgx.Postgres reference at the foot of this file).
	pgxv5 "github.com/jackc/pgx/v5"
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
//
// On success it verifies the schema carries no INVALID index (checkNoInvalidIndexes) —
// the one failure a migration can leave behind that a re-run reports as success.
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
	// dsn, NOT migrateURL: pgxURL rewrites the scheme to golang-migrate's INTERNAL
	// pgx5://, which pgx itself cannot parse.
	return checkNoInvalidIndexes(dsn)
}

// invalidIndexCheckTimeout bounds the post-migration catalog read. It is a single
// indexed lookup against a database the migrator has just finished using, so the budget
// only needs to cover a connect plus one round trip.
const invalidIndexCheckTimeout = 10 * time.Second

// ErrInvalidIndex reports that the schema carries an index Postgres has marked INVALID.
// It is PERMANENT: no amount of retrying rebuilds the index, and the service must not
// serve while it stands, because an invalid index enforces NOTHING. For a UNIQUE index
// that is a lost constraint, and the code above it goes on believing the database is
// arbitrating something it has stopped arbitrating.
var ErrInvalidIndex = errors.New("schema carries an INVALID index")

// ErrMissingRequiredIndex reports that an index the schema RELIES ON for correctness is
// absent (or present-but-invalid, which is the same fact). Like ErrInvalidIndex it is
// PERMANENT: golang-migrate has already recorded the migration that creates it as clean,
// so no re-run will rebuild it — the version has to be forced back first. It exists
// because the recovery for ErrInvalidIndex, done halfway, produces exactly this state.
var ErrMissingRequiredIndex = errors.New("schema is missing an index it relies on for correctness")

// invalidIndexQuery lists indexes in the connection's schema that Postgres has marked
// invalid. Only CREATE INDEX CONCURRENTLY produces one: a plain CREATE INDEX rolls its
// failure back, while a CONCURRENTLY build that fails leaves the index PRESENT and
// invalid. The planner then refuses to use it and a unique index stops enforcing
// uniqueness — silently, since every catalog lookup by NAME still finds it.
const invalidIndexQuery = `SELECT c.relname
	FROM pg_index i
	JOIN pg_class c ON c.oid = i.indexrelid
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE NOT i.indisvalid AND n.nspname = current_schema()
	ORDER BY c.relname`

// checkNoInvalidIndexes runs after a successful migration and refuses to report success
// while the schema carries an invalid index.
//
// This exists because `CREATE INDEX CONCURRENTLY IF NOT EXISTS` cannot recover from its
// own failure. A failed CONCURRENTLY build leaves the index present under the intended
// name and marked invalid; golang-migrate marks the version dirty. The operator then
// reconciles the data and forces the version back, and the re-run finds the NAME, does
// nothing, and reports success — so the version goes clean over an index that enforces
// nothing. Every test that looks the index up by name still passes.
//
// A test cannot cover that: it is production CATALOG state, and the migration tests run
// on a fresh database where the debris does not exist. The check has to be in the runner,
// on the path production takes. It is also deliberately not scoped to one index name —
// an invalid index is never an intended state, and every future CONCURRENTLY migration
// gets this for free rather than needing its own bespoke assertion.
//
// Running it on every boot, including the ErrNoChange path, is the point: a pod that
// starts against a schema whose lease index is invalid must refuse rather than quietly
// accept the duplicate builds the index was added to prevent.
func checkNoInvalidIndexes(dsn string) error {
	// Migrate takes no context (golang-migrate's Up() does not either). This is a
	// single catalog read against a database the migration just finished using, so a
	// short independent deadline is enough and keeps a hung read from wedging boot.
	ctx, cancel := context.WithTimeout(context.Background(), invalidIndexCheckTimeout)
	defer cancel()

	conn, err := pgxv5.Connect(ctx, dsn)
	if err != nil {
		// Connectivity, not a schema verdict: leave it retryable rather than
		// wrapping it in the permanent sentinel.
		return fmt.Errorf("check for invalid indexes: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	rows, err := conn.Query(ctx, invalidIndexQuery)
	if err != nil {
		return fmt.Errorf("check for invalid indexes: %w", err)
	}
	names, err := pgxv5.CollectRows(rows, pgxv5.RowTo[string])
	if err != nil {
		return fmt.Errorf("check for invalid indexes: %w", err)
	}
	if len(names) > 0 {
		return fmt.Errorf("%w: %s. Left by a failed CREATE INDEX CONCURRENTLY: it "+
			"enforces nothing and the planner will not use it, yet a re-run of the "+
			"migration that creates it finds the NAME and reports success. To recover: "+
			"DROP each index listed, then `migrate force <version-1>` so the migration "+
			"that creates it will RUN again — dropping alone leaves the version clean, "+
			"and the next boot then succeeds with the index absent entirely",
			ErrInvalidIndex, strings.Join(names, ", "))
	}

	// And the absence case, which is what the recovery above walks straight into if only
	// half of it is done. Dropping the debris leaves migration 18 recorded CLEAN, so Up()
	// returns ErrNoChange, the scan above finds nothing invalid, and boot succeeds against
	// a schema with no uniqueness at all — the same silent loss the scan exists to catch,
	// reached by following the remedy. Detection therefore cannot be "no invalid index";
	// it has to be "the index that enforces the invariant is PRESENT and valid".
	//
	// A hand-maintained list is the cost of that, and it is bounded: an entry belongs here
	// only for an index whose absence is SILENT — a unique index standing in for a
	// constraint. A performance index going missing is slow, not wrong, and does not
	// belong. The live test TestMigrateRefusesADroppedRequiredIndex fails if an entry
	// names an index no migration creates, so a stale name cannot sit here unnoticed.
	missing, err := missingRequiredIndexes(ctx, conn)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s. This index is the ONLY thing enforcing its invariant — "+
			"without it the writes it serializes all succeed and the damage is silent. "+
			"Recover with `migrate force <version-1>` so the migration that creates it "+
			"runs again; do not start the service against this schema",
			ErrMissingRequiredIndex, strings.Join(missing, ", "))
	}
	return nil
}

// requiredIndexes are indexes whose ABSENCE is silent: each one stands in for a constraint,
// so with it gone every write it was serializing succeeds and nothing reports a problem.
// See checkNoInvalidIndexes for why membership is deliberately narrow.
var requiredIndexes = []string{
	// migration 000018: at most one audience per (brief, platform) in `building`.
	"uq_campaign_audiences_brief_platform_building",
}

const requiredIndexQuery = `SELECT c.relname
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_index i ON i.indexrelid = c.oid
	WHERE c.relname = ANY($1) AND n.nspname = current_schema() AND i.indisvalid`

// missingRequiredIndexes returns the requiredIndexes absent from the schema. An index that
// exists but is INVALID counts as missing: it enforces nothing, so the two are the same
// fact to a caller deciding whether the invariant holds.
func missingRequiredIndexes(ctx context.Context, conn *pgxv5.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, requiredIndexQuery, requiredIndexes)
	if err != nil {
		return nil, fmt.Errorf("check for required indexes: %w", err)
	}
	found, err := pgxv5.CollectRows(rows, pgxv5.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("check for required indexes: %w", err)
	}
	present := make(map[string]struct{}, len(found))
	for _, n := range found {
		present[n] = struct{}{}
	}
	var missing []string
	for _, want := range requiredIndexes {
		if _, ok := present[want]; !ok {
			missing = append(missing, want)
		}
	}
	return missing, nil
}

// IsPermanentMigrationErr reports whether a Migrate error is a PERMANENT state that
// retrying can never clear on its own: a dirty schema (migrate.ErrDirty), which
// golang-migrate sets when a prior migration failed partway and leaves the
// schema_migrations row marked dirty; or ErrInvalidIndex, the debris of a failed
// CREATE INDEX CONCURRENTLY. It requires an operator to inspect and `force`
// the version; a boot loop that just re-runs Migrate will hit ErrDirty forever. The
// caller uses this to fail fast (surface the error) instead of 503-looping silently.
// A connectivity/lock/deadline failure is NOT permanent and is deliberately excluded
// so it still retries.
func IsPermanentMigrationErr(err error) bool {
	var dirty migrate.ErrDirty
	// ErrInvalidIndex joins it: an invalid index is catalog debris no retry rebuilds.
	// Retrying would boot-loop in 503 while the schema silently enforces nothing —
	// which is worse than the dirty case, because the version reads CLEAN.
	// ErrMissingRequiredIndex is permanent for the same reason and one step further along:
	// the version is already clean, so Up() returns ErrNoChange forever and the index is
	// never rebuilt. Only `migrate force` moves it.
	return errors.As(err, &dirty) ||
		errors.Is(err, ErrInvalidIndex) ||
		errors.Is(err, ErrMissingRequiredIndex)
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
