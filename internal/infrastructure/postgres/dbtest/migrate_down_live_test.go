// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5 driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
)

// migrateURL rewrites a postgres:// DSN onto golang-migrate's internal pgx5:// scheme.
//
// The postgres package has an unexported pgxURL that does exactly this, and this is a
// deliberate two-line restatement rather than a reason to export it. The rewrite is a
// property of golang-migrate's driver registry, not of this service, and widening the
// production API so a test can reach a one-line string operation is the trade the
// knowledge log previously (and wrongly) assumed the whole down-migration case required.
func migrateURL(t *testing.T, dsn string) string {
	t.Helper()
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if after, ok := strings.CutPrefix(dsn, prefix); ok {
			return "pgx5://" + after
		}
	}
	t.Fatalf("%s must be a postgres:// or postgresql:// URL to drive a migrator", dbtest.EnvDatabaseURL)
	return ""
}

// newMigrator builds a migrator over the EMBEDDED migration set, pointed at dsn.
//
// This is the whole of what driving Down requires, and it is why no production change is
// needed to test it: migrations.FS is already exported, and postgres.Migrate itself does
// nothing more than this before calling Up. The service's startup path calls only Up —
// that is a statement about the startup path, not about reachability.
func newMigrator(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("open migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL(t, dsn))
	if err != nil {
		t.Fatalf("init migrator: %v", err)
	}
	return m
}

// freshDatabase creates an EMPTY database of its own and returns a DSN for it.
//
// An isolated database is not a nicety here, it is the precondition. This test runs Down
// to version zero, which drops every table in the schema; against the shared
// TEST_DATABASE_URL that would delete the schema every other live test in the package is
// mid-flight against. The new database is dropped on cleanup, so a green run leaves
// nothing behind — and a FAILED run leaves it too, because the drop is unconditional and
// the wreckage of a half-migrated schema is not worth more than the certainty that the
// next run starts clean.
func freshDatabase(ctx context.Context, t *testing.T) string {
	t.Helper()

	admin, err := pgx.Connect(ctx, dbtest.DSN())
	if err != nil {
		t.Fatalf("connect to create a scratch database: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	// Lowercase and unquoted-identifier-safe: UniqueID emits [a-z0-9-], and a hyphen is
	// not legal in an unquoted identifier, so it becomes an underscore.
	name := strings.ReplaceAll(dbtest.UniqueID(t, "down"), "-", "_")
	// CREATE DATABASE cannot be parameterised, and the name is built from UniqueID
	// rather than from any input, so interpolation here is not a user-data path.
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// Errorf, not Logf. This test provisions a database per migration version, so a
		// silently-skipped drop does not cost one stray database, it accumulates a whole
		// run's worth against the persistent local harness — and a Logf leaves that
		// invisible in a green run. The contract is that a green run leaves nothing
		// behind, so failing to drop is a test failure, not a note.
		conn, err := pgx.Connect(cleanupCtx, dbtest.DSN())
		if err != nil {
			t.Errorf("cleanup: reconnect to drop %s: %v; the scratch database is still "+
				"present and this run did not leave the server as it found it", name, err)
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		if _, err := conn.Exec(cleanupCtx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
			t.Errorf("cleanup: drop %s: %v; the scratch database is still present and this "+
				"run did not leave the server as it found it", name, err)
		}
	})

	// Swap ONLY the database name, by editing the parsed URL's path. Rebuilding the DSN
	// from individual fields silently drops everything not named -- the password above
	// all, plus sslmode and any other query parameter. That is invisible locally, where
	// peer/trust auth needs no password, and fails in CI, which authenticates with a
	// user:password pair over TCP. So: parse, edit one field, re-render.
	u, err := url.Parse(dbtest.DSN())
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.EnvDatabaseURL, err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestLiveMigrationsGoDownAndUpAgain drives the embedded migration set to its top
// version, all the way back down to zero, and up again on a database of its own.
//
// The knowledge log for this ticket originally recorded the down path as unreachable —
// "testing it would mean widening the production API purely for a test". That was wrong,
// and the correction is the point of this test: `migrations.FS` is exported, so a
// migrator can be built against it directly without touching `postgres.Migrate`. The true
// statement is narrower: the SERVICE'S STARTUP PATH only ever calls Up.
//
// What this actually buys is the down SQL. Every migration in the tree ships a `.down.sql`
// that nothing executes, so a file that references a dropped column, reverses its
// statements in the wrong order, or is simply empty is indistinguishable from a correct
// one — until an operator runs it during an incident rollback, which is the worst moment
// to discover it. Down-to-zero is the strong form: it forces EVERY down file to run, and
// to run against the schema its own up file produced.
//
// The final Up is not decoration. A down file that drops more than its up created leaves
// a schema that looks empty but is not, and the re-Up is what catches it: the migration
// would fail on an object that should no longer exist.
func TestLiveMigrationsGoDownAndUpAgain(t *testing.T) {
	// Pool is called for its skip/fatal verdict on TEST_DATABASE_URL, which is the same
	// gate every other live test in this package sits behind. The pool itself is not
	// used — this test runs against a database it creates.
	_ = dbtest.Pool(t)
	ctx := context.Background()

	dsn := freshDatabase(ctx, t)

	m := newMigrator(t, dsn)
	defer func() { _, _ = m.Close() }()

	if err := m.Up(); err != nil {
		t.Fatalf("Up on a fresh database: %v", err)
	}
	topVersion, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read version after Up: %v", err)
	}
	if dirty {
		t.Fatalf("schema is dirty at version %d after a clean Up", topVersion)
	}

	// Step down ONE migration at a time rather than calling Down() once, and assert after
	// each step that the schema matches what that version's UP produced.
	//
	// This is not a stylistic choice, it is what makes the individual down files
	// observable. A single Down() to zero ends with every table dropped, and dropping a
	// table takes its indexes with it — so an index-only down migration (000026's
	// `DROP INDEX ... idx_campaign_jobs_retention`, for one) can be emptied entirely and
	// the final state is still identical. Verified: with that file replaced by a comment,
	// a single-Down version of this test stayed green, which is the mutation that put
	// this loop here.
	//
	// The reference state for each step is built by migrating a FRESH database UP to
	// that version — never by walking a shared reference down. Comparing "schema after
	// stepping down to N" against "schema after migrating up to N" is the strong form:
	// it pins the down file as the exact inverse of its up, rather than merely asserting
	// it did not error. See schemaAtVersion for why the upward construction is required.
	for version := topVersion; version > 0; version-- {
		if err := m.Steps(-1); err != nil {
			t.Fatalf("Down one step from version %d: %v — a down migration that cannot "+
				"run is a rollback that fails during an incident, and nothing else in the "+
				"suite executes these files", version, err)
		}

		// What the schema SHOULD look like now: the up set applied through version-1.
		want := schemaAtVersion(ctx, t, version-1)
		got := schemaObjects(ctx, t, dsn)
		if diff := objectDiff(want, got); diff != "" {
			t.Fatalf("stepping DOWN from version %d did not restore the schema that "+
				"migrating UP to version %d produces:\n%s\n"+
				"a down file must be the inverse of its up; an incomplete one makes a "+
				"rollback leave objects behind", version, version-1, diff)
		}
	}
	if _, _, err := m.Version(); err != migrate.ErrNilVersion {
		t.Fatalf("after Down the version is not nil (err = %v); the schema did not return "+
			"to zero, so at least one down migration left state behind", err)
	}

	// Every object the up set created is gone. Checking the version alone would pass for a
	// down file that updated the version and dropped nothing. The step loop above already
	// compared each intermediate state; this pins the endpoint, where the expected schema
	// is simply empty and needs no reference database to state.
	if remaining := schemaObjects(ctx, t, dsn); len(remaining) != 0 {
		t.Fatalf("after Down to zero these objects still exist: %v; a down migration "+
			"recorded its version without undoing its up", remaining)
	}

	// And the set applies cleanly a second time. This is what catches a down file that
	// dropped an object a LATER up migration does not recreate, or that dropped more than
	// its own up added.
	if err := m.Up(); err != nil {
		t.Fatalf("Up after a full Down: %v — the down set left the database in a state "+
			"the up set cannot be re-applied to, so a rollback would be one-way", err)
	}
	reVersion, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read version after the second Up: %v", err)
	}
	if dirty {
		t.Fatalf("schema is dirty at version %d after re-applying", reVersion)
	}
	if reVersion != topVersion {
		t.Fatalf("re-applied to version %d, want the original %d", reVersion, topVersion)
	}
}

// schemaAtVersion returns the schema the UP set produces at the given version, on a
// database of its own that is migrated UP to it from empty.
//
// The "from empty, upward" part is the whole point, and getting it wrong silently
// disarmed this test once already. The obvious implementation keeps ONE reference database
// and walks it down alongside the database under test — but that reference then executes
// the very down files being tested, so a broken down file corrupts both sides equally and
// the comparison comes back clean. Measured: with 000013's down emptied, a down-driven
// reference and an up-only reference diverge by exactly the index the mutation left
// behind, and the down-driven version of this helper reported no difference at all.
//
// So each call provisions a fresh database and applies only up migrations to it. That is
// the one construction that cannot inherit the defect it is supposed to detect. It costs a
// database per step, which is why this test is scoped to a rollback check rather than run
// per-package.
func schemaAtVersion(ctx context.Context, t *testing.T, version uint) []string {
	t.Helper()

	// Version 0 is the empty schema by definition — no database or migrator needed, and
	// asking golang-migrate to "migrate to 0" is not how it spells that anyway.
	if version == 0 {
		return nil
	}

	dsn := freshDatabase(ctx, t)
	m := newMigrator(t, dsn)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("reference database: migrate up to version %d: %v", version, err)
	}
	return schemaObjects(ctx, t, dsn)
}

// schemaObjects lists the public schema as comparable strings: tables, COLUMNS, indexes,
// constraints and SEQUENCES.
//
// All five kinds are here because each is the only witness to some migration's down file,
// and a snapshot that omits one lets that file be emptied without the test noticing.
// Every kind below was added in response to a mutation that SURVIVED a narrower version of
// this helper:
//
//   - tables alone missed 000026, an index-only down — a later DROP TABLE removes the
//     index anyway, so the end state was identical.
//   - adding indexes still missed 000021, whose down drops a COLUMN.
//   - adding columns still missed 000013, whose index is dropped by a neighbouring
//     migration regardless; the constraint 000014 restores is what distinguishes them.
//   - adding constraints still missed the SEQUENCE a BIGSERIAL creates. 000010's
//     `id BIGSERIAL PRIMARY KEY` creates `index_outbox_id_seq`, and a sequence's
//     increment, bounds, cache and cycle flag are carried by NO other object: a down
//     that restores the sequence with `INCREMENT BY 7` leaves every table, column,
//     index and constraint byte-identical, so the comparison passed while the restored
//     schema hands out different keys than the one it claims to be the exact inverse of.
//
// golang-migrate's own `schema_migrations` bookkeeping table is excluded: it survives Down
// by design, is not part of this service's schema, and its CONTENT is the version pointer
// the loop is testing rather than schema under test.
func schemaObjects(ctx context.Context, t *testing.T, dsn string) []string {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to read the schema: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Definitions, not names. A migration that redefines an object in place keeps the
	// name and changes the definition (000009 and 000014 both drop-and-recreate), and a
	// name-only compare would call that a no-op. Columns carry type and nullability for
	// the same reason: a down that restores a column as the wrong type has not restored it.
	//
	// The column arm reads pg_attribute rather than information_schema.columns, because
	// the latter cannot express the type it reports. Its `data_type` is the SQL-standard
	// type NAME with the modifier stripped: `NUMERIC(14,2)` and `NUMERIC(14,3)` both render
	// as the bare string `numeric`, and `VARCHAR(50)` and `VARCHAR(200)` both as `character
	// varying`. Measured on PG16. So the sentence above -- "a down that restores a column as
	// the wrong type has not restored it" -- was the one thing the old query could not
	// detect: campaigns.budget_amount is NUMERIC(14,2) (000002), and a down file restoring
	// it at NUMERIC(14,3) produced a snapshot BYTE-IDENTICAL to the reference.
	//
	// format_type(atttypid, atttypmod) is the modifier-carrying rendering, and the default
	// expression is joined in from pg_attrdef: a down that drops a DEFAULT changes what the
	// column does on every subsequent INSERT, and the old snapshot selected no default at
	// all, so that was invisible too.
	//
	// The sequence arm reads pg_sequences for the same reason the column arm reads
	// pg_attribute: the DEFINING properties, not the name. A BIGSERIAL column's sequence is
	// a schema object in its own right, and none of its parameters — increment, bounds,
	// cache, cycle — is observable through the table, column, index or constraint rows
	// above. `last_value` is deliberately NOT selected: it is the sequence's runtime
	// position, advanced by whatever rows a test inserted, so including it would make the
	// snapshot depend on activity rather than on schema and fail on a correct down file.
	// Ownership is likewise omitted; it is carried by the column's DEFAULT expression,
	// which the column arm already renders as `nextval('index_outbox_id_seq'::regclass)`.
	rows, err := conn.Query(ctx, `
		SELECT 'table:' || tablename
		FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		UNION ALL
		SELECT 'column:' || a.attrelid::regclass::text || '.' || a.attname || ' ' ||
		       format_type(a.atttypid, a.atttypmod) ||
		       CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END ||
		       COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
		FROM pg_attribute a
		JOIN pg_class cl ON cl.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = cl.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = 'public'
		  AND cl.relkind IN ('r', 'p')
		  AND cl.relname <> 'schema_migrations'
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		UNION ALL
		SELECT 'index:' || indexname || ' = ' || indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
		UNION ALL
		SELECT 'constraint:' || c.conrelid::regclass::text || '.' || c.conname || ' = ' ||
		       pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		WHERE c.connamespace = 'public'::regnamespace
		  AND c.conrelid::regclass::text <> 'schema_migrations'
		UNION ALL
		SELECT 'sequence:' || sequencename || ' ' || data_type ||
		       ' START ' || start_value ||
		       ' MIN ' || min_value ||
		       ' MAX ' || max_value ||
		       ' INCREMENT ' || increment_by ||
		       ' CACHE ' || cache_size ||
		       CASE WHEN cycle THEN ' CYCLE' ELSE ' NO CYCLE' END
		FROM pg_sequences
		WHERE schemaname = 'public'
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("read the schema: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a schema object: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema objects: %v", err)
	}
	return out
}

// objectDiff renders the symmetric difference of two sorted object lists, or "" when they
// match. Both sides are named in the output because "left behind by the down" and "not
// recreated by it" are different defects and the message has to say which one happened.
func objectDiff(want, got []string) string {
	inWant := make(map[string]bool, len(want))
	for _, o := range want {
		inWant[o] = true
	}
	inGot := make(map[string]bool, len(got))
	for _, o := range got {
		inGot[o] = true
	}

	var b strings.Builder
	for _, o := range got {
		if !inWant[o] {
			fmt.Fprintf(&b, "  UNEXPECTED (the down did not remove it): %s\n", o)
		}
	}
	for _, o := range want {
		if !inGot[o] {
			fmt.Fprintf(&b, "  MISSING (the down removed too much): %s\n", o)
		}
	}
	return b.String()
}
