// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package postgres provides the PostgreSQL connection pool, migration runner,
// and repository implementations for the campaign service.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	// pgxv5 is aliased because golang-migrate's driver above already occupies the
	// identifier `pgx` (see the pgx.Postgres reference at the foot of this file).
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// A CLEAN schema recorded at a version newer than any this image embeds (an image-only
// rollback after a later release migrated forward) is not migrated and its version is not
// rewritten; startup goes straight to the index checks below. See schemaIsNewerThanImage.
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

	// A CLEAN schema recorded at a version this image does not embed is the image-only
	// rollback case: a newer release migrated forward, then the image was reverted and the
	// schema was not. Up() cannot run there — golang-migrate's readUp checks that the
	// recorded version exists in THIS binary's source and fails with "no migration found
	// for version N" — so it is skipped, the recorded version is left exactly as it is, and
	// startup falls through to the index validation below. See schemaIsNewerThanImage for
	// the compatibility assumption this rests on. Every other state takes Up() unchanged,
	// a DIRTY newer schema included: Up() re-reads the version under its lock and refuses
	// it with ErrDirty exactly as before.
	newer, err := schemaIsNewerThanImage(m, src)
	if err != nil {
		return err
	}
	if newer {
		// dsn, NOT migrateURL — see the note at the foot of this function.
		return checkNoInvalidIndexes(dsn)
	}

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

// schemaIsNewerThanImage reports whether the database records a CLEAN migration version
// exactly one above the highest one embedded in this image, logging the fact when it does.
//
// The highest embedded version is read from the migration source itself rather than kept as
// a constant, so it cannot drift from the files when the next migration is added.
//
// The assumption it rests on is expand/contract, one release apart
// (internal/infrastructure/postgres/migrations/README.md): a release's schema changes must
// stay compatible with the PREVIOUS release's binary, because that binary is both what a
// rolling deploy still runs and what an image-only rollback returns to. Nothing in the
// migration chain marks a version as compatible or not, so this does not — and cannot —
// verify that the newer migrations kept that rule; it trusts it. What it does keep is the
// part that IS checkable: the newer schema must be clean, and checkNoInvalidIndexes still
// runs, so a newer release that dropped an index this image REQUIRES still refuses boot with
// ErrMissingRequiredIndex.
//
// The trust is bounded to that one hop (PR #260 review): only recorded == latest+1 takes this
// path. A clean version further ahead is a schema this release was never shown compatible
// with, so it falls through to Up(), which refuses it loudly ("no migration found for
// version N") exactly as it did before this path existed. A release that ships more than one
// migration therefore cannot be image-rolled-back past; that is the conservative side.
//
// Errors reading the recorded version are not reported here: the caller falls through to
// Up(), which reads it again under its lock and reports them as it always has. Only a
// failure to enumerate this image's own migrations is returned.
func schemaIsNewerThanImage(m *migrate.Migrate, src source.Driver) (bool, error) {
	recorded, dirty, err := m.Version()
	if err != nil || dirty {
		return false, nil
	}
	latest, err := highestEmbeddedVersion(src)
	if err != nil {
		return false, fmt.Errorf("read the embedded migration versions: %w", err)
	}
	if recorded != latest+1 {
		return false, nil
	}
	slog.Warn("database schema is newer than this image; skipping migrations and validating "+
		"indexes only, on the expand/contract assumption that a release stays compatible with "+
		"the next release's schema",
		"recorded_version", recorded, "image_latest_version", latest)
	return true, nil
}

// highestEmbeddedVersion walks the migration source to its last version.
func highestEmbeddedVersion(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, err
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, err
		}
		v = next
	}
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
// so no re-run will rebuild it. The remedy is the rebuild statement the error carries
// (`requiredIndex.createSQL`) — run it directly and leave the recorded version alone, which
// is already correct; the schema drifted underneath it. Forcing the version back was the
// original advice and is NOT safe here: see createSQL's comment for why replaying this
// migration range can leave the version dirty. It exists because the recovery for
// ErrInvalidIndex, done halfway, produces exactly this state.
var ErrMissingRequiredIndex = errors.New("schema is missing an index it relies on for correctness")

// ErrRequiredIndexMismatch reports an index that carries a required index's NAME while
// enforcing something else — non-unique, different keys, different predicate, different
// table. It is a separate sentinel from ErrMissingRequiredIndex because the recovery is
// different in a way an operator cannot guess: the impostor has to be DROPPED before the
// rebuild statement is run, since the migration's CREATE ... IF NOT EXISTS matches on the
// name alone and would skip again. Permanent for the same reason as its siblings.
var ErrRequiredIndexMismatch = errors.New("an index required for correctness exists under the right name with the wrong definition")

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
	// The OPTIONAL deparse probe runs FIRST, on its OWN connection under its OWN short budget,
	// so however slow it is (a slow server, a held lock) it cannot spend the REQUIRED checks'
	// deadline below, which only starts once the probe has returned. See probeRequiredIndexDeparse.
	expected := probeRequiredIndexDeparse(dsn)

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
			"migration that creates it finds the NAME and reports success. DROP each "+
			"index listed, THEN do what its annotation says. Dropping is the whole remedy "+
			"only for an index no migration creates; otherwise dropping alone leaves the "+
			"version recorded clean and the next boot succeeds with the index absent "+
			"entirely. Prefer a rebuild statement where one is given: it restores exactly "+
			"that index. A force replays every migration above the version named, and this "+
			"chain is not uniformly replay-safe",
			ErrInvalidIndex, describeInvalid(names))
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
	missing, wrong, err := checkRequiredIndexes(ctx, conn, expected)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w. Each index listed is the ONLY thing enforcing its "+
			"invariant — without it the writes it serializes all succeed and the damage "+
			"is silent. Run each statement below; then restart. Do not start the service "+
			"against this schema:\n%s",
			ErrMissingRequiredIndex, describeMissing(missing))
	}
	if len(wrong) > 0 {
		return fmt.Errorf("%w. An index of the right NAME that enforces something else "+
			"is worse than none: the migration's IF NOT EXISTS finds the name and skips, "+
			"so the real constraint is never built and every name-based check passes. "+
			"DROP each index listed and then run the statement beside it:\n%s",
			ErrRequiredIndexMismatch, strings.Join(wrong, "\n"))
	}
	return nil
}

// requiredIndexes are indexes whose ABSENCE is silent: each one stands in for a constraint,
// so with it gone every write it was serializing succeeds and nothing reports a problem.
// See checkNoInvalidIndexes for why membership is deliberately narrow.
//
// Each entry carries the index's DEFINITION, not just its name, and the reason is the
// same IF NOT EXISTS that produced the invalid-index case above. A name-only check has a
// hole an attacker does not need to find, because an ordinary mistake produces it: any
// index that happens to carry this name — a non-unique one, one on a superset of the
// keys, one with a different predicate — makes migration 000018's CREATE ... IF NOT
// EXISTS a silent no-op, and then satisfies a check that asks only whether the name is
// present and valid. Boot succeeds; concurrent builds are unconstrained; nothing reports
// it. The repo already guards exactly this for migration 000014, whose drop-guard pins
// uniqueness, key count, key names, relation and predicate rather than the name
// (TestMigration000014_GuardChecksIndexDefinition records the PostgreSQL 16.10 run where
// a same-named NON-unique index passed the name-only form). This is that guard, moved to
// the runner and applied to every index in the schema that the rule above admits.
//
// "Every index the rule admits" is the whole membership test, and it is worth stating
// because the first draft of this list held only the index 000018 creates — the one the
// change at hand was about. That is the natural scope for a change and the wrong scope for
// a guard: a check that covers one of eight identically-exposed indexes reads, from the
// boot log, exactly like a check that covers all of them. The other seven-plus were not
// judged less important; they were simply not what anyone was looking at.
var requiredIndexes = []requiredIndex{{
	// at most one audience per (brief, platform) in `building`.
	name:   audienceBuildLeaseIndex,
	table:  "campaign_audiences",
	unique: true,
	keys:   []string{"brief_id", "platform"},
	// As Postgres DEPARSES `WHERE status = 'building'`. Comparing against the deparsed
	// form rather than the source text is what makes an equivalent spelling — an
	// explicit ::text cast, extra whitespace — compare equal instead of tripping a
	// false alarm, on the same reasoning as 000014's guard.
	predicate: "(status = 'building'::text)",
}, {
	// at most one live campaign per (brief, platform, variant, slot_version) — 000037. The
	// arbiter of ClaimCampaignDispatch, UpsertCampaign and AdoptCampaign, and since 000047
	// dropped 000022's three-column uq_campaigns_brief_platform_variant_live, the ONLY index
	// standing between two concurrent claims of one slot version and two paid campaigns.
	// Membership here is what re-checks it at every boot. A migration-time guard (000014's,
	// 000023's) runs once, and an index lost afterwards — an operator clearing invalid-index
	// debris, a rebuild whose IF NOT EXISTS no-opped against a same-named leftover — leaves a
	// schema that boots clean with duplicate PAID campaigns as the first symptom.
	//
	// The slot_version column is what lets a DELIBERATE second campaign on one
	// (brief, platform, variant) slot coexist with the first. "An adopt binds only an empty
	// slot", which the three-column index used to enforce, is now enforced by the per-slot
	// advisory lock in campaign_repo.go — not by any index, so not by this check.
	name:   "uq_campaigns_brief_platform_variant_slot_version_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"brief_id", "platform", "variant", "slot_version"},
	// Deparsed form of `WHERE status <> 'deleted'`, character-identical to what 000023's
	// guard compared for the three-column index this one replaced.
	predicate: "(status <> 'deleted'::text)",
}, {
	// at most one live campaign per (platform, platform_campaign_id) — the guard that keeps
	// adoption from binding one upstream Google Ads campaign to two briefs. 000020 creates
	// it with no IF NOT EXISTS precisely because a same-named leftover would make the build
	// a no-op; this entry is the other half of that argument, re-asserting the DEFINITION at
	// every boot rather than only at migration time. Absent, adopt-campaign's pre-insert
	// lookup and the INSERT race freely and the service ends up managing the same paid
	// campaign from two briefs, each reporting its own status.
	name:   "uq_campaigns_platform_campaign_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"platform", "platform_campaign_id"},
	// Deparsed. The three conjuncts are parenthesised individually and the whole is wrapped
	// once — measured against PostgreSQL 16 rather than hand-derived, because a predicate
	// this shape is where an assumed spelling silently becomes a boot-time false alarm.
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'google-ads'::text))",
}, {
	// at most one live campaign per LinkedIn campaign id (migration 000042) — LinkedIn's own
	// binding guard, separate from 000020's because its reason differs: LinkedIn campaign ids
	// are GLOBALLY unique (urn:li:sponsoredCampaign:{id} carries no account), so the global key
	// is exact. Built without IF NOT EXISTS for 000020's reason, so this entry is again the
	// boot-time half of the argument.
	name:   "uq_campaigns_linkedin_campaign_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"platform", "platform_campaign_id"},
	// Deparsed, the same shape as 000020's entry.
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'linkedin-ads'::text))",
}, {
	// One live row per Meta campaign (000043). GLOBAL key: a Meta campaign is a Graph node with
	// a globally unique id. Built without IF NOT EXISTS for 000020's reason.
	name:   "uq_campaigns_meta_campaign_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"platform", "platform_campaign_id"},
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'meta-ads'::text))",
}, {
	// One live row per Microsoft campaign PER AD ACCOUNT (000044): Microsoft ids are
	// account-scoped, so the third key is the account extracted exactly as
	// microsoftCreationAccountID reads it (accountId, else the aid= of microsoftAdsUrl).
	// Captured from PostgreSQL 16's deparse (note the quoted "substring"); the boot check does
	// NOT depend on that spelling — it compares the server's own deparse of this definition
	// against the live index (serverDeparsedRequiredIndexes), so an upgrade cannot fail boot.
	name:   "uq_campaigns_microsoft_campaign_live",
	table:  "campaigns",
	unique: true,
	keys: []string{"platform", "platform_campaign_id",
		`COALESCE(NULLIF(result ->> 'accountId'::text, ''::text), NULLIF("substring"(result ->> 'microsoftAdsUrl'::text, '[?&]aid=([^&#]+)'::text), ''::text))`},
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'microsoft-ads'::text))",
}, {
	// One live row per Reddit campaign PER AD ACCOUNT (000045): Reddit does not document its
	// ids as global, so the account (result.accountId, redditCreationAccountID's field) keys it.
	name:   "uq_campaigns_reddit_campaign_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"platform", "platform_campaign_id", "NULLIF(result ->> 'accountId'::text, ''::text)"},
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'reddit-ads'::text))",
}, {
	// One live row per X campaign PER AD ACCOUNT (000046): X does not document its ids as
	// global, so the account (result.AccountID, twitterCreationAccountID's field) keys it.
	name:   "uq_campaigns_twitter_campaign_live",
	table:  "campaigns",
	unique: true,
	keys:   []string{"platform", "platform_campaign_id", "NULLIF(result ->> 'AccountID'::text, ''::text)"},
	predicate: "((status <> 'deleted'::text) AND (platform_campaign_id IS NOT NULL) AND " +
		"(platform = 'twitter-ads'::text))",
}, {
	// at most one LIVE brief per (project, event slug, delivery type, stage). 000003 does not
	// add this index alongside a constraint — it DROPs campaign_briefs_project_id_event_slug_key
	// and replaces it, so from 000003 onward the index is the only thing there is. Absent, two
	// briefs with the same identity coexist and every later lookup that assumes one picks
	// arbitrarily between them.
	//
	// 000030 widened the key from (project, event) and this entry moved with it. The narrow
	// version is deliberately NOT kept alongside: it would enforce one-brief-per-event, which is
	// the constraint 000030 exists to lift — an event carries a paid brief and an email series at
	// once. Leaving it here would refuse boot on exactly the schema the service now requires.
	name:      "uq_campaign_briefs_project_event_delivery_stage",
	table:     "campaign_briefs",
	unique:    true,
	keys:      []string{"project_id", "event_slug", "delivery_type", "stage"},
	predicate: "(status <> 'archived'::text)",
}}

// connectionSingletonIndexes is the same guarantee for the seven per-provider connection
// tables 000001 creates: one live connection per project, per provider.
//
// They are generated rather than written out because the seven differ in nothing but the
// table and the name — and writing them out seven times is how the eighth provider gets
// added to the schema and forgotten here. 000001 declares NO table-level UNIQUE constraint
// (its header says so, and grepping it confirms it), so each partial index is the sole
// enforcement behind ConnectionRepo.Create returning ErrConflict. With one gone, a second
// `active` connection for that project inserts cleanly, and which credentials a dispatch
// then resolves is decided by row order — against a table whose rows are ad-account
// credentials that spend money.
func connectionSingletonIndexes() []requiredIndex {
	// Ordered, not a map range: the missing-index error lists names in this order, and a
	// message whose contents reshuffle between boots is one an operator cannot diff.
	tables := []string{
		"google_ads_connections", "linkedin_ads_connections", "meta_ads_connections",
		"reddit_ads_connections", "twitter_ads_connections", "microsoft_ads_connections",
		"hubspot_connections",
	}
	out := make([]requiredIndex, 0, len(tables))
	for _, t := range tables {
		out = append(out, requiredIndex{
			// 000001 names each index uq_<table>_project. The existing
			// requiredIndexes test fails if this derivation ever stops matching what
			// the migration actually creates, so the convention cannot rot into a name
			// no migration owns.
			name:      "uq_" + t + "_project",
			table:     t,
			unique:    true,
			keys:      []string{"project_id"},
			predicate: "(status <> 'deleted'::text)",
		})
	}
	return out
}

func init() { requiredIndexes = append(requiredIndexes, connectionSingletonIndexes()...) }

// createSQL renders the statement that rebuilds this index, which is the recovery advice
// for a REQUIRED index — deliberately, instead of a version to force.
//
// Forcing was the original advice and it is not safe in general. `migrate force N` followed
// by Up() replays every migration ABOVE N, not just the one that creates the index, and this
// repo's migrations are not uniformly replay-safe: 000006 and 000007 carry bare
// `ALTER TABLE … ADD CONSTRAINT`, and PostgreSQL has no `ADD CONSTRAINT IF NOT EXISTS`, so a
// replay against a schema that already has the constraint fails with 42710 and leaves the
// version DIRTY. The advice happened to be sound while the list held only indexes from
// 000013 and 000018 — replaying 000014.. is all IF NOT EXISTS — and became unsound the
// moment entries from 000001 and 000003 joined it, because "force 0" means replaying
// everything. That is a property of the RANGE, not of the index, so it cannot be fixed by
// annotating more carefully.
//
// The index's own DDL has none of that surface: it restores exactly the invariant that is
// missing, touches nothing else, and needs no version change because the recorded version
// is already correct — the schema drifted underneath it. It is also checkable, which the
// force advice never was: TestRequiredIndexCreateSQL_RebuildsAnIndexTheCheckAccepts runs
// each statement against a live database and re-runs the checker.
func (r requiredIndex) createSQL() string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if r.unique {
		b.WriteString("UNIQUE ")
	}
	keys := make([]string, len(r.keys))
	for i, k := range r.keys {
		// A plain column key is an identifier; an EXPRESSION key (the account extracted from
		// result JSON, 000044-000046) must be parenthesised in CREATE INDEX. The stored key is
		// the deparsed form pg_get_indexdef renders, so the round trip holds either way.
		if identifierRe.MatchString(k) {
			keys[i] = k
		} else {
			keys[i] = "(" + k + ")"
		}
	}
	fmt.Fprintf(&b, "INDEX %s ON %s (%s)", r.name, r.table, strings.Join(keys, ", "))
	if r.predicate != "" {
		// The stored predicate is the DEPARSED form, which is itself valid SQL — it is
		// what pg_get_expr renders and what pg_get_indexdef embeds. Round-tripping it is
		// what keeps this statement and the equality check in checkRequiredIndexes from
		// drifting: an index built by this DDL necessarily deparses back to this string.
		fmt.Fprintf(&b, " WHERE %s", r.predicate)
	}
	return b.String()
}

// RequiredIndexNames returns the names of every index Migrate refuses to boot without, in
// the order the error reports them.
//
// It is exported for the live-database tests in the dbtest package, which are in a
// different package and so cannot reach the unexported list — and driving them off the real
// list is the whole point. A hand-written copy of these names in the test file is a claim of
// coverage that stops being true the moment a ninth entry is added, and it reads identically
// either way. RequiredIndexRebuildSQL is exported for the same reason.
func RequiredIndexNames() []string {
	out := make([]string, 0, len(requiredIndexes))
	for _, r := range requiredIndexes {
		out = append(out, r.name)
	}
	return out
}

// LatestMigrationVersion returns the highest migration version embedded in this image — the
// version whose successor (latest+1) is the only newer schema Migrate trusts without running
// Up(); a clean recorded version further ahead is refused through Up().
// Exported so the dbtest rollback test can record a version just past it rather than a
// hard-coded number that the next migration would silently turn into an embedded one.
func LatestMigrationVersion() (uint, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, fmt.Errorf("open migration source: %w", err)
	}
	defer func() { _ = src.Close() }()
	v, err := highestEmbeddedVersion(src)
	if err != nil {
		return 0, fmt.Errorf("read the embedded migration versions: %w", err)
	}
	return v, nil
}

// RequiredIndexRebuildSQL returns the statement that recreates the named required index,
// which is the recovery the boot error prints. Reports false for a name not in the list.
func RequiredIndexRebuildSQL(name string) (string, bool) {
	if i := slices.IndexFunc(requiredIndexes, func(r requiredIndex) bool { return r.name == name }); i >= 0 {
		return requiredIndexes[i].createSQL(), true
	}
	return "", false
}

// describeMissing renders one indented line per absent index: its name and the statement
// that rebuilds it.
func describeMissing(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		line := "  " + n
		if i := slices.IndexFunc(requiredIndexes, func(r requiredIndex) bool { return r.name == n }); i >= 0 {
			line += ": " + requiredIndexes[i].createSQL()
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// identifierRe matches a plain column name — what pg_get_indexdef renders for a column key, as
// opposed to an expression key.
var identifierRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// requiredIndex is an index whose absence — or silent replacement by something of the
// same name — leaves an invariant unenforced with no other symptom.
type requiredIndex struct {
	name   string
	table  string
	unique bool
	// keys are the index's key columns IN ORDER, each as pg_get_indexdef(index, n, true)
	// renders it: a bare column name, or the deparsed text of an expression key.
	keys      []string
	predicate string // deparsed; "" means the index must NOT be partial
}

// createIndexRe matches the CREATE statement for a named index in a migration file.
//
// It matches the CREATE specifically, not the name anywhere in the file: a migration that
// DROPs an index also contains its name, and reporting the dropping migration as the one to
// force back to would replay the drop. Where two migrations create the same name (a drop and
// a rebuild), the HIGHEST wins — that is the definition currently in force.
var createIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?"?([a-zA-Z0-9_]+)"?`)

// dropIndexRe matches a DROP INDEX statement in a migration file, read from the same
// executable SQL as createIndexRe (prose and dollar-quoted bodies stripped).
var dropIndexRe = regexp.MustCompile(
	`(?is)DROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?"?([a-zA-Z0-9_]+)"?`)

// lineCommentRe matches a `--` comment to end of line. Comments are stripped before the
// CREATE scan because these migrations DISCUSS the statements they are avoiding: 000009's
// header contains the phrase "a failed CREATE INDEX CONCURRENTLY", from which the regexp
// happily reads the index name `does`. Junk names are inert — no real index is called
// that — but a scan whose output depends on prose is one prose edit away from claiming a
// migration owns an index it never touches.
var lineCommentRe = regexp.MustCompile(`--[^\n]*`)

// dollarTagRe matches a dollar-quote delimiter ($$ or $tag$).
//
// Matching the delimiter and pairing it in code, rather than matching the whole block in
// one pattern, is forced: a `$tag$...$tag$` pattern needs a BACKREFERENCE to require the
// closing tag be the same as the opening one, and RE2 — Go's engine — has none. A
// tag-agnostic `\$\w*\$.*?\$\w*\$` would instead close one block on another block's
// OPENING delimiter and delete the executable statements between them.
var dollarTagRe = regexp.MustCompile(`\$[a-zA-Z0-9_]*\$`)

// executableSQL strips what the CREATE scan must not read: `--` prose, and the body of
// every dollar-quoted block.
//
// An UNTERMINATED block drops everything after it. That is the safe direction: the file
// would not run at all, so under-reporting ownership costs an annotation on a migration
// nobody can apply, while carrying on past it would read a half-quoted body as executable.
func executableSQL(body string) string {
	body = lineCommentRe.ReplaceAllString(body, "")
	var out strings.Builder
	for {
		open := dollarTagRe.FindStringIndex(body)
		if open == nil {
			out.WriteString(body)
			return out.String()
		}
		out.WriteString(body[:open[0]])
		tag := body[open[0]:open[1]]
		rest := body[open[1]:]
		end := strings.Index(rest, tag)
		if end < 0 {
			return out.String()
		}
		body = rest[end+len(tag):]
	}
}

// migrationIndexOwners maps an index name to the highest migration version whose up-file
// CREATEs it UNCONDITIONALLY, read once from the embedded migrations.
//
// "Unconditionally" is the load-bearing word, and it is why the body of a dollar-quoted
// block does not count. The remedy this map feeds is "DROP the index, then force back so
// the migration RUNS again" — which only recovers the index if that migration's CREATE
// fires against a schema where the index is ABSENT. A DO block exists precisely to make
// DDL conditional, and 000009's condition is "an INVALID copy is present": the operator's
// drop, the first half of the remedy, is exactly what makes it false. Counting that
// rebuild as ownership would send an operator to force 8, watch 000009 no-op, and boot
// clean with idx_campaigns_stuck_claims gone for good — the stuck-claim scan silently
// full-scanning forever, which is the very failure 000008 and 000009 exist to prevent.
//
// Skipping the block hands the same operator 000008 instead, whose plain
// `CREATE INDEX CONCURRENTLY ... IF NOT EXISTS` does fire once the name is gone. Forcing
// one version further back replays 000009 too, which then correctly no-ops.
//
// The general rule, which outlives this pair: ownership means "re-running this migration
// against a schema missing the index rebuilds it". A conditional create cannot promise
// that, so it is not ownership — it is repair, and repair is not a recovery target.
var migrationIndexOwners = sync.OnceValue(func() map[string]int {
	owners := map[string]int{}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		// Cannot happen for an embed.FS root, and this feeds an error MESSAGE — a boot
		// that already failed must not be turned into a panic by its own diagnostics.
		return owners
	}
	for _, e := range entries {
		base := e.Name()
		if !strings.HasSuffix(base, ".up.sql") {
			continue
		}
		v, cerr := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if cerr != nil {
			continue
		}
		body, rerr := migrations.FS.ReadFile(base)
		if rerr != nil {
			continue
		}
		for _, m := range createIndexRe.FindAllStringSubmatch(executableSQL(string(body)), -1) {
			if v > owners[m[1]] {
				owners[m[1]] = v
			}
		}
	}
	return owners
})

// migrationIndexDrops maps an index name to the highest migration version whose up-file DROPs
// it unconditionally, read like migrationIndexOwners. indexRecovery compares the two: an index
// dropped after its last CREATE is not part of the schema the migrations build.
var migrationIndexDrops = sync.OnceValue(func() map[string]int {
	drops := map[string]int{}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return drops // feeds an error MESSAGE only; see migrationIndexOwners
	}
	for _, e := range entries {
		base := e.Name()
		if !strings.HasSuffix(base, ".up.sql") {
			continue
		}
		v, cerr := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if cerr != nil {
			continue
		}
		body, rerr := migrations.FS.ReadFile(base)
		if rerr != nil {
			continue
		}
		for _, m := range dropIndexRe.FindAllStringSubmatch(executableSQL(string(body)), -1) {
			if v > drops[m[1]] {
				drops[m[1]] = v
			}
		}
	}
	return drops
})

// describeInvalid renders the invalid index names, annotating each one this repo's
// migrations create with the version to force back to.
//
// The scan is schema-wide on purpose — an invalid index is never an intended state, and
// scoping it to known names would miss the next CONCURRENTLY migration written after this
// code. The consequence is that a name here may be nothing to do with a migration: a
// hand-built index, an operator's experiment, another tool's. Telling that operator to
// force a version would replay unrelated DDL to fix an index no migration will recreate.
// So the version is attached per NAME, not to the sentence.
//
// Ownership is read from the MIGRATIONS, not from requiredIndexes, and the difference is
// load-bearing rather than stylistic. requiredIndexes is deliberately narrow — it lists only
// indexes whose ABSENCE is silent — so most migration-created indexes are legitimately
// missing from it, `idx_campaigns_stuck_claims` (000008) among them. Deriving ownership from
// that list would hand an operator holding an invalid copy of THAT index the "drop it, leave
// the schema version alone" advice, which deletes it permanently and boots clean with the
// stuck-claim scan full-scanning forever. Any list narrower than "every index a migration
// creates" produces that class of answer; the migrations are the only set that is not, and
// they stay correct for indexes added after this code was written.
//
// "Creates" means creates UNCONDITIONALLY — see migrationIndexOwners. A rebuild guarded on
// the debris the remedy tells the operator to delete is not a recovery target.
func describeInvalid(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, n+" ("+indexRecovery(n)+")")
	}
	return strings.Join(out, ", ")
}

// indexRecovery renders the recovery clause for ONE index name: the version to force so
// the migration that creates it runs again, or the drop-only advice for a name no
// migration owns.
//
// It is per-name and not per-message because more than one index can be reported at once
// and they need not share an owner: `uq_campaign_audiences_brief_platform_building` comes
// from 000018 and `uq_campaigns_platform_campaign_live` from 000020. A single
// `force <version-1>` sentence covering both is wrong for at least one of them — an
// operator who forces 17 replays 000018 and the campaigns index is still absent, with the
// error now silent because the message they followed said this was the remedy. Advice
// that is right for the first name in a list and wrong for the second is worse than no
// advice, because it is followed.
func indexRecovery(name string) string {
	// Where the DDL is known, it beats any force: it rebuilds THIS index and replays
	// nothing else. Forcing back to V and running Up() re-executes every migration above
	// V, and this chain is not uniformly replay-safe — 000006 and 000007 carry bare
	// `ALTER TABLE … ADD CONSTRAINT`, which PostgreSQL has no IF NOT EXISTS form of, so
	// they fail with 42710 and leave the version DIRTY. That hazard scales with the
	// DISTANCE forced back, which is why it was invisible while every annotated index came
	// from 000013 or later and became reachable the moment 000001's belonged here too.
	// The force branch below survives only for names this package has no DDL for.
	if i := slices.IndexFunc(requiredIndexes, func(r requiredIndex) bool { return r.name == name }); i >= 0 {
		return "rebuild with: " + requiredIndexes[i].createSQL()
	}
	if v, ok := migrationIndexOwners()[name]; ok {
		// An index a LATER migration drops is no recovery target either: forcing back to its
		// creator replays every migration up to the one that removes it again (000022's slot
		// index, dropped by 000047 — an invalid copy is what a failed 000047 down leaves), and
		// the distance makes the replay unsafe as described above.
		if d := migrationIndexDrops()[name]; d > v {
			return fmt.Sprintf("migration %06d drops this; drop it, leave the schema version alone", d)
		}
		return fmt.Sprintf("migration %06d: force %d", v, v-1)
	}
	return "no migration creates this; drop it, leave the schema version alone"
}

// requiredIndexQuery reads one index's definition. indisready joins indisvalid because the
// two fail apart: a CONCURRENTLY build that dies between its phases can leave an index
// marked valid but not ready, which enforces nothing on new writes.
const requiredIndexQuery = `SELECT
		i.indisunique,
		i.indisvalid AND i.indisready,
		i.indrelid = to_regclass($2),
		COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
		COALESCE((
			SELECT array_agg(pg_get_indexdef(i.indexrelid, k.ord, true) ORDER BY k.ord)
			FROM generate_series(1, i.indnkeyatts::int) AS k(ord)
		), '{}')
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_index i ON i.indexrelid = c.oid
	WHERE c.relname = $1 AND n.nspname = current_schema()`

// checkRequiredIndexes splits the required indexes into those ABSENT and those present
// under the right name with the wrong definition. The two are separated because their
// recovery differs: an absent index is rebuilt by running its own DDL
// (`requiredIndex.createSQL`) with the recorded migration version left alone, while an
// impostor must be DROPPED first — run the CREATE without dropping and it finds the name
// again and skips, leaving the operator where they started. An index that exists but is
// INVALID counts as absent: it enforces nothing, which is the same fact to a caller
// deciding whether the invariant holds, and its recovery is the drop-then-create above.
//
// expected is each index's definition as the server deparses it (probeRequiredIndexDeparse); an
// entry absent from it — or a nil map — compares against the stored text.
func checkRequiredIndexes(ctx context.Context, conn *pgxv5.Conn, expected map[string]deparsedIndex) (missing, wrong []string, err error) {
	for _, want := range requiredIndexes {
		var unique, live, rightTable bool
		var predicate string
		var keys []string
		row := conn.QueryRow(ctx, requiredIndexQuery, want.name, want.table)
		switch scanErr := row.Scan(&unique, &live, &rightTable, &predicate, &keys); {
		case errors.Is(scanErr, pgxv5.ErrNoRows), scanErr == nil && !live:
			missing = append(missing, want.name)
			continue
		case scanErr != nil:
			return nil, nil, fmt.Errorf("check for required indexes: %w", scanErr)
		}
		var defects []string
		if unique != want.unique {
			defects = append(defects, fmt.Sprintf("indisunique=%t, want %t", unique, want.unique))
		}
		if !rightTable {
			defects = append(defects, "on a different table than "+want.table)
		}
		// Compared against THIS server's deparse of the expected definition, not against the
		// stored text: see serverDeparsedRequiredIndexes.
		exp, ok := expected[want.name]
		if !ok {
			exp = deparsedIndex{keys: want.keys, predicate: want.predicate}
		}
		if !slices.Equal(keys, exp.keys) {
			defects = append(defects, fmt.Sprintf("keys %v, want %v", keys, exp.keys))
		}
		if predicate != exp.predicate {
			defects = append(defects, fmt.Sprintf("predicate %q, want %q", predicate, exp.predicate))
		}
		if len(defects) > 0 {
			// The remedy travels with the defects rather than in the message's closing
			// sentence, because two impostors reported together need not have the same
			// one. It is the index's own DDL, not a version to force — see createSQL.
			wrong = append(wrong, fmt.Sprintf("  %s (%s)\n    then: %s",
				want.name, strings.Join(defects, ", "), want.createSQL()))
		}
	}
	return missing, wrong, nil
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
	// never rebuilt. Only running the index's own DDL does — which is why the error carries
	// that statement (see createSQL) rather than telling the operator to force a version.
	return errors.As(err, &dirty) ||
		errors.Is(err, ErrInvalidIndex) ||
		errors.Is(err, ErrMissingRequiredIndex) ||
		errors.Is(err, ErrRequiredIndexMismatch)
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

// deparsedIndex is one index definition as a particular PostgreSQL server deparses it.
type deparsedIndex struct {
	keys      []string
	predicate string
}

// probeIndexQuery reads a probe index's deparsed keys and predicate by oid — the same renderings
// requiredIndexQuery reads for the real index (pg_get_indexdef per key, pg_get_expr for the
// predicate).
const probeIndexQuery = `SELECT
		COALESCE(pg_get_expr(i.indpred, i.indrelid), ''),
		COALESCE((
			SELECT array_agg(pg_get_indexdef(i.indexrelid, k.ord, true) ORDER BY k.ord)
			FROM generate_series(1, i.indnkeyatts::int) AS k(ord)
		), '{}')
	FROM pg_index i WHERE i.indexrelid = $1::regclass`

// probeBudget bounds the WHOLE optional deparse probe — connect, transaction and every probe
// statement. Short on purpose: the probe only improves the comparison, and spending longer on it
// than the required checks' own budget (invalidIndexCheckTimeout) would make an optional step the
// likeliest cause of a failed boot.
const probeBudget = 3 * time.Second

// probeStatementTimeout / probeLockTimeout bound each probe statement inside its transaction
// (SET LOCAL), so one statement waiting on a lock — CREATE TEMP TABLE ... (LIKE campaigns) takes
// a share lock on the source — gives up quickly instead of consuming the budget.
const (
	probeStatementTimeout = "1500ms"
	probeLockTimeout      = "500ms"
)

// probeRequiredIndexDeparse runs serverDeparsedRequiredIndexes on a DEDICATED connection under
// probeBudget. Every failure — connect included — degrades to the stored-text comparison (a nil
// map) with one WARN; it never returns an error, so it can only improve the boot check.
func probeRequiredIndexDeparse(dsn string) map[string]deparsedIndex {
	ctx, cancel := context.WithTimeout(context.Background(), probeBudget)
	defer cancel()
	var conn *pgxv5.Conn
	if err := probeStep(ctx, "connect", func() (err error) { conn, err = pgxv5.Connect(ctx, dsn); return err }); err != nil {
		slog.WarnContext(ctx, "required-index check: the deparse probe could not connect; comparing definitions as stored text",
			probeErrAttrs("connect", err)...)
		return nil
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return serverDeparsedRequiredIndexes(ctx, conn)
}

// probeRedactedDiagnostic is the ONLY text the probe's warnings carry about a failure. The raw
// error is never logged: a pgx connect or parse error repeats the DSN's identifiers (host, user,
// database — and a parse error can echo credential text), and PostgreSQL's own diagnostics quote
// the role and database (`password authentication failed for user "u"`). The same reasoning as
// dsnParseError and dbtest.SafeDSNErr: nothing derived from the input or the server's message is
// carried, only what identifies the failure's KIND.
const probeRedactedDiagnostic = "error text withheld: it can repeat DATABASE_URL identifiers or credentials"

// probeErrAttrs renders a probe failure for a log line: the step, the error's Go type, the
// SQLSTATE when it is a server error (a code, never the message), and the fixed diagnostic.
func probeErrAttrs(step string, err error) []any {
	attrs := []any{"step", step, "error_class", fmt.Sprintf("%T", err)}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		attrs = append(attrs, "sqlstate", pgErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		attrs = append(attrs, "timeout", true)
	}
	return append(attrs, "error", probeRedactedDiagnostic)
}

// probeFault, when set (tests only), is consulted before each probe-infrastructure step and
// makes that step fail with the returned error — standing in for a production role without the
// TEMPORARY privilege, a read-only default, or a hot standby.
var probeFault func(ctx context.Context, step string) error

// probeStep runs one probe-infrastructure step, honouring probeFault. ctx is the probe's own
// budget, handed to the hook so a test can stand in for a step that blocks until it runs out.
func probeStep(ctx context.Context, step string, run func() error) error {
	if probeFault != nil {
		if err := probeFault(ctx, step); err != nil {
			return err
		}
	}
	return run()
}

// serverDeparsedRequiredIndexes asks the RUNNING server how it deparses each required index's
// expected definition, so the boot check compares the server's rendering of BOTH sides rather
// than a live deparse against text captured on one PostgreSQL version. A major upgrade that
// changes how an expression key (000044-000046) or a predicate is rendered then changes both
// sides alike and cannot fail boot; a genuinely different definition still differs.
//
// How: inside ONE transaction that is always rolled back, it creates an empty TEMP copy of each
// table (`LIKE`, columns only — no data) and builds each expected index on it from createSQL's
// key list and predicate, then reads that probe's deparse.
//
// BEST-EFFORT, and never stricter than the text comparison it improves on. The probe needs things
// the boot check never needed before — a transaction, the TEMPORARY privilege, a writable session
// — and a production role may lack them (no TEMP grant, default_transaction_read_only, a hot
// standby). Any such infrastructure failure logs ONE warning naming the step and the error class
// and leaves the affected entries (the rest of the list) on their stored text, exactly the
// comparison made before this existed. A definition this server cannot parse falls back the same
// way, entry by entry. It therefore returns no error: the worst case is the old behaviour.
func serverDeparsedRequiredIndexes(ctx context.Context, conn *pgxv5.Conn) map[string]deparsedIndex {
	out := make(map[string]deparsedIndex, len(requiredIndexes))
	for _, r := range requiredIndexes {
		out[r.name] = deparsedIndex{keys: r.keys, predicate: r.predicate}
	}
	fallBack := func(step string, err error) map[string]deparsedIndex {
		slog.WarnContext(ctx, "required-index check: the deparse probe could not run; comparing the remaining definitions as stored text (a Postgres upgrade that re-renders them could then report a mismatch)",
			probeErrAttrs(step, err)...)
		return out
	}

	var tx pgxv5.Tx
	if err := probeStep(ctx, "begin", func() (err error) { tx, err = conn.Begin(ctx); return err }); err != nil {
		return fallBack("begin", err)
	}
	// Rollback on a context of its own: the probe's may already be spent, and an open
	// transaction holding temp objects must not outlive it.
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Second)
		defer rcancel()
		_ = tx.Rollback(rctx)
	}()

	if err := probeStep(ctx, "set timeouts", func() error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+probeStatementTimeout+"'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '"+probeLockTimeout+"'")
		return err
	}); err != nil {
		return fallBack("set timeouts", err)
	}

	probed := map[string]string{}
	for n, r := range requiredIndexes {
		tmp, ok := probed[r.table]
		if !ok {
			tmp = "required_index_probe_" + r.table
			if err := probeStep(ctx, "create temp table", func() error {
				_, err := tx.Exec(ctx, fmt.Sprintf("CREATE TEMP TABLE %s (LIKE %s)", tmp, r.table))
				return err
			}); err != nil {
				return fallBack("create temp table", err)
			}
			probed[r.table] = tmp
		}
		probe := fmt.Sprintf("required_index_probe_%d", n)
		ddl := strings.Replace(r.createSQL(), "INDEX "+r.name+" ON "+r.table+" ", "INDEX "+probe+" ON "+tmp+" ", 1)
		ddl = strings.Replace(ddl, "CREATE UNIQUE INDEX", "CREATE INDEX", 1)
		// A savepoint per probe, so a definition this server cannot parse falls back to the
		// stored text for that entry alone instead of aborting the whole transaction.
		if err := probeStep(ctx, "savepoint", func() error {
			_, err := tx.Exec(ctx, "SAVEPOINT required_index_probe")
			return err
		}); err != nil {
			return fallBack("savepoint", err)
		}
		if _, err := tx.Exec(ctx, ddl); err != nil {
			// About the DEFINITION, not the infrastructure: this entry keeps its stored text.
			if rerr := probeStep(ctx, "rollback to savepoint", func() error {
				_, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT required_index_probe")
				return err
			}); rerr != nil {
				return fallBack("rollback to savepoint", rerr)
			}
			continue
		}
		var d deparsedIndex
		if err := probeStep(ctx, "read probe", func() error {
			return tx.QueryRow(ctx, probeIndexQuery, "pg_temp."+probe).Scan(&d.predicate, &d.keys)
		}); err != nil {
			return fallBack("read probe", err)
		}
		out[r.name] = d
	}
	return out
}
