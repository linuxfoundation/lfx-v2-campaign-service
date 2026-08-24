// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5 driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

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
		t.Fatalf("init migrator: %s", dbtest.SafeDSNErr(err))
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
	//
	// TEMPLATE template0 explicitly. Without it Postgres clones template1, which is an
	// ORDINARY database a developer or another tool may have added objects to — and this
	// test asserts a fully-empty public schema at version zero after the down migrations
	// run. A stray table in template1 would fail that baseline even though every down
	// migration is correct, and the failure would point at the migrations rather than at
	// the template. template0 is guaranteed pristine, so the assertion measures only what
	// the migrations did.
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q TEMPLATE template0", name)); err != nil {
		// SKIP rather than FAIL on a missing privilege, because this test asks for MORE than
		// the documented harness contract. dbtest's contract is a database the package "may
		// freely modify", which a database OWNER without the cluster-level CREATEDB role fully
		// satisfies — and every other live test in this package works in exactly that setup.
		// Failing here would turn a conforming configuration into a red build over a test that
		// simply cannot run there, and the error would read as a migration defect.
		//
		// Only the privilege case is skipped. Any other CREATE DATABASE failure is still a real
		// failure and is reported as one.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
			t.Skipf("scratch-database provisioning needs the CREATEDB role, which the harness "+
				"contract does not require (TEST_DATABASE_URL only promises a database this "+
				"package may modify): %v", err)
		}
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
	rewritten, err := withDatabase(dbtest.DSN(), name)
	if err != nil {
		t.Fatalf("parse %s: %s", dbtest.EnvDatabaseURL, dbtest.SafeDSNErr(err))
	}
	return rewritten
}

// withDatabase points a DSN at a different database, changing nothing else.
//
// Split out of freshDatabase so it can be tested without a server: the shapes that break
// it are properties of the DSN STRING, and a live test could only ever exercise whichever
// single form the developer's TEST_DATABASE_URL happens to take.
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	// The PATH is not the only place a DSN names its database. pgx reads `dbname` (and
	// its alias `database`) from the QUERY and applies it AFTER the path, so a
	// TEST_DATABASE_URL written `postgres://h/ignored?dbname=campaign_test` would leave
	// this "scratch" DSN pointing straight back at the shared database -- verified
	// against pgxpool.ParseConfig, which reports Database="fromquery" for both spellings.
	// Every migration would then run on the developer's real test database instead of a
	// throwaway, and the down-to-zero would drop its schema. Strip both.
	//
	// Only these two keys are removed, and only when present: everything else in the
	// query is exactly what must survive -- sslmode above all, plus any connect_timeout
	// or application_name the operator set.
	if q := u.Query(); q.Has("dbname") || q.Has("database") {
		q.Del("dbname")
		q.Del("database")
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
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
// The final Up is not decoration, but be precise about WHICH failure it catches, because the
// intuitive reading is backwards. It does NOT detect over-removal: after a down-to-zero the
// up set replays from scratch, so an object a down file dropped too early is simply recreated
// by its own up file and the re-Up passes. Over-removal is caught by the PER-VERSION snapshot
// comparison above, which checks each intermediate state against the schema its own up
// produced.
//
// What the re-Up catches is the opposite: RESIDUE. An object a down file failed to drop, whose
// KIND schemaObjects does not enumerate — so the emptiness check above cannot see it — and
// which a later create then collides with. That is a real gap the snapshots miss, because they
// can only compare the object kinds they know to look for.
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

	// And the set applies cleanly a second time. This catches RESIDUE, not over-removal: a
	// replay from zero recreates anything a down file dropped too eagerly, so over-removal is
	// the per-version snapshots' job. What fails here is an object left BEHIND whose kind
	// schemaObjects does not enumerate — invisible to the emptiness check above — that a
	// later CREATE then collides with.
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
	// Ownership IS selected, joined in from pg_depend. An earlier version of this comment
	// claimed ownership was "carried by the column's DEFAULT expression, which the column
	// arm already renders as nextval('index_outbox_id_seq'::regclass)" — that is false, and
	// it is false in the direction that disarms the test. Postgres records OWNED BY as a
	// separate pg_depend entry (deptype 'a', the auto-dependency from sequence to column),
	// entirely independent of the DEFAULT. `ALTER SEQUENCE ... OWNED BY NONE` and
	// `OWNED BY <other column>` both leave the DEFAULT byte-identical, so the column arm
	// renders exactly the same string either way. Measured on PG16: with 000011's down file
	// reassigning index_outbox_id_seq to index_outbox.object_id, the whole exact-inverse
	// comparison PASSED before this join was added. Ownership is what makes a serial
	// sequence get dropped with its table, so a down file that detaches one leaves an
	// orphan sequence behind after a rollback — precisely the class this test exists for.
	//
	// LEFT JOIN, not JOIN: a standalone CREATE SEQUENCE has no owning column, and an inner
	// join would drop it from the snapshot entirely, making an unremoved standalone
	// sequence invisible. Unowned renders as ' OWNED BY NONE', which is also the string
	// that differs from an owned one.
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
		SELECT 'sequence:' || s.sequencename || ' ' || s.data_type ||
		       ' START ' || s.start_value ||
		       ' MIN ' || s.min_value ||
		       ' MAX ' || s.max_value ||
		       ' INCREMENT ' || s.increment_by ||
		       ' CACHE ' || s.cache_size ||
		       CASE WHEN s.cycle THEN ' CYCLE' ELSE ' NO CYCLE' END ||
		       ' OWNED BY ' || COALESCE(o.owner, 'NONE')
		FROM pg_sequences s
		LEFT JOIN (
		        SELECT d.objid,
		               a.attrelid::regclass::text || '.' || a.attname AS owner
		        FROM pg_depend d
		        JOIN pg_attribute a
		          ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		        WHERE d.classid = 'pg_class'::regclass
		          AND d.refclassid = 'pg_class'::regclass
		          AND d.deptype = 'a'
		          AND d.refobjsubid > 0
		) o ON o.objid = (quote_ident(s.schemaname) || '.' ||
		                  quote_ident(s.sequencename))::regclass
		WHERE s.schemaname = 'public'
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

// TestSafeDSNErrKeepsCredentialsOutOfOutput pins the property that makes every DSN-
// bearing error site in this package safe to print: the RENDERED string carries the
// diagnosis and not the credential.
//
// It asserts on the rendered output rather than on the source of the call sites,
// because the leak is a property of what the error FORMATS TO, not of which verb the
// format string uses. A `%v` on a plain error is fine; a `%v` on a *url.Error is not.
// Only the rendered bytes can tell those apart.
//
// This test needs no database. The failure it guards is reachable only when the DSN is
// malformed, which is exactly when the live tests cannot run — so a live-gated test
// could never cover it.
func TestSafeDSNErrKeepsCredentialsOutOfOutput(t *testing.T) {
	t.Parallel()

	// A password chosen to be unmistakable in output, and a user name that is half of
	// the same credential (pkg/redact drops userinfo entirely, not just the password).
	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	const username = "ciuser"

	cases := []struct {
		name string
		// how the DSN is made unparseable; each is a distinct net/url failure mode
		dsn string
		// every fragment of the DSN that must NOT appear in the output. net/url's
		// causes quote the fragment they choked on, so these cover the parser's own
		// message as well as the raw URL.
		mustNotContain []string
		// the secret this case's DSN carries. Empty means the shared password; the
		// malformed-userinfo case overrides it because its password is what is
		// malformed. The precondition below asserts THIS value leaks unfixed, so a
		// case cannot silently stop demonstrating the leak it guards.
		secret string
	}{
		{
			name:           "invalid percent escape",
			dsn:            "postgres://" + username + ":" + password + "@db.%zz:5432/app",
			mustNotContain: []string{"%zz", "db.%zz"},
		},
		{
			name:           "invalid port",
			dsn:            "postgres://" + username + ":" + password + "@db.internal:no-port/app",
			mustNotContain: []string{"no-port", "db.internal"},
		},
		{
			name:           "unclosed IPv6 bracket",
			dsn:            "postgres://" + username + ":" + password + "@[::1:5432/app",
			mustNotContain: []string{"[::1", "missing"},
		},
		{
			// The case that unwrapping alone does NOT cover: the malformed bytes are
			// inside the PASSWORD, so the parser's own cause quotes a slice of the
			// credential. Here the password contains "%zz" and the cause is
			// `invalid URL escape "%zz"` -- no URL, and still a leak.
			name:           "malformed escape inside the password",
			dsn:            "postgres://" + username + ":s3cr3t%zzpw@db.internal:5432/app",
			mustNotContain: []string{"%zz", "s3cr3t"},
			secret:         "s3cr3t%zzpw",
		},
		{
			name:           "control character",
			dsn:            "postgres://" + username + ":" + password + "@db.internal:5432/app\x7f",
			mustNotContain: []string{"db.internal"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := url.Parse(tc.dsn)
			if err == nil {
				t.Fatalf("url.Parse(%q) succeeded; this case no longer exercises a parse failure "+
					"and the test has stopped testing what it claims", tc.name)
			}

			// The control: the UNFIXED rendering leaks. If this stops holding, the
			// premise of the fix is gone and the assertions below prove nothing.
			secret := tc.secret
			if secret == "" {
				secret = password
			}
			if raw := fmt.Sprintf("%v", err); !strings.Contains(raw, secret) {
				t.Fatalf("precondition failed: formatting the raw error with %%v did not embed "+
					"%q, so this case cannot demonstrate the leak it guards", secret)
			}

			got := dbtest.SafeDSNErr(err)
			if strings.Contains(got, password) {
				t.Errorf("SafeDSNErr leaked the password into its output: %q", got)
			}
			if strings.Contains(got, username) {
				t.Errorf("SafeDSNErr leaked the user half of the credential into its output: %q", got)
			}
			// Nothing derived from the input may appear -- including the PARSER'S
			// message, which quotes the fragment it choked on. That fragment can be
			// part of the credential: a password of "%zz" yields `invalid URL escape
			// "%zz"`, which is the whole secret, and a longer password containing
			// "%zz" leaks that slice of itself. Unwrapping alone does not fix this,
			// which is why the cause is discarded rather than unwrapped.
			for _, frag := range tc.mustNotContain {
				if strings.Contains(got, frag) {
					t.Errorf("SafeDSNErr = %q, want no fragment of the input; it echoed %q",
						got, frag)
				}
			}

			// It must still be actionable: the operator has to learn that the DSN is
			// what failed to parse, or the message says nothing usable at all.
			if !strings.Contains(got, "does not parse") {
				t.Errorf("SafeDSNErr = %q, want it to still report that the DSN does not "+
					"parse — a redactor that says nothing is not actionable", got)
			}
		})
	}
}

// TestSafeDSNErrRedactsAWrappedMigratorError covers the second leak shape, which is not
// a *url.Error at the top level.
//
// migrate.NewWithSourceInstance reaches database.Open, which parses the URL and wraps the
// resulting *url.Error as "failed to open database: parse %q: ...". errors.As still finds
// it, so the unwrap arm handles this too — but the wrapper text is what a reader sees, and
// this pins that the credential does not ride along inside it.
func TestSafeDSNErrRedactsAWrappedMigratorError(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	inner := &url.Error{
		Op:  "parse",
		URL: "pgx5://ciuser:" + password + "@db.%zz:5432/app",
		Err: errors.New(`invalid URL escape "%zz"`),
	}
	wrapped := fmt.Errorf("failed to open database: %w", inner)

	if raw := fmt.Sprintf("%v", wrapped); !strings.Contains(raw, password) {
		t.Fatalf("precondition failed: the wrapped error does not embed the password, so this " +
			"case cannot demonstrate the leak it guards")
	}

	got := dbtest.SafeDSNErr(wrapped)
	if strings.Contains(got, password) {
		t.Errorf("SafeDSNErr leaked the password from a wrapped migrator error: %q", got)
	}
	// The parser's message goes too, not just the URL: it quotes the fragment it choked
	// on, which can be a slice of the credential.
	if strings.Contains(got, "%zz") {
		t.Errorf("SafeDSNErr = %q, want no fragment of the input; it echoed the parser's "+
			"quoted fragment", got)
	}
	if !strings.Contains(got, "does not parse") {
		t.Errorf("SafeDSNErr = %q, want it to still report that the DSN does not parse", got)
	}
}

// TestWithDatabaseRepointsTheDSN asserts on the database pgx RESOLVES, not on the
// rewritten string.
//
// That distinction is the whole finding. Editing `u.Path` produces a DSN that LOOKS
// repointed -- the path says the scratch name -- while pgx applies the query's `dbname`
// afterwards and connects to the original database anyway. A string assertion on the path
// passes in exactly the case that is broken, so the assertion has to go through the same
// parser the migrator uses.
func TestWithDatabaseRepointsTheDSN(t *testing.T) {
	t.Parallel()

	const scratch = "scratch_db"

	cases := []struct {
		name string
		dsn  string
	}{
		{name: "database in the path", dsn: "postgres://u:p@h:5432/original?sslmode=disable"},
		{name: "dbname in the query overrides the path", dsn: "postgres://u:p@h:5432/original?dbname=original&sslmode=disable"},
		{name: "database alias in the query", dsn: "postgres://u:p@h:5432/original?database=original&sslmode=disable"},
		{name: "both aliases present", dsn: "postgres://u:p@h:5432/original?dbname=original&database=original&sslmode=disable"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := withDatabase(tc.dsn, scratch)
			if err != nil {
				t.Fatalf("withDatabase(%q): %v", tc.name, err)
			}

			cfg, err := pgxpool.ParseConfig(got)
			if err != nil {
				t.Fatalf("pgxpool.ParseConfig(rewritten): %v", err)
			}
			if cfg.ConnConfig.Database != scratch {
				t.Errorf("pgx resolves database %q, want %q — the rewritten DSN still points at "+
					"the shared database, so every migration would run there and the down-to-zero "+
					"would drop its schema (rewritten: %q)", cfg.ConnConfig.Database, scratch, got)
			}

			// Everything the DSN did NOT name a database with must survive. sslmode is the
			// one that fails loudly in CI, where the connection is over TCP.
			if cfg.ConnConfig.TLSConfig != nil {
				t.Errorf("sslmode=disable did not survive the rewrite: TLSConfig is non-nil "+
					"(rewritten: %q)", got)
			}
			if cfg.ConnConfig.User != "u" {
				t.Errorf("user did not survive the rewrite: got %q (rewritten: %q)", cfg.ConnConfig.User, got)
			}
			if cfg.ConnConfig.Password != "p" {
				t.Errorf("password did not survive the rewrite (rewritten: %q)", got)
			}
		})
	}
}

// TestSafeDSNErrKeepsDriverTextForNonURLErrors pins the OTHER half of SafeDSNErr's
// contract: the sentinel replacement is reserved for *url.Error, and every other cause
// keeps its own text.
//
// The two existing SafeDSNErr tests both drive *url.Error inputs, so they constrain only
// the redacting arm. That leaves the fallback arm free: replacing
// `redact.URLUserinfo(err.Error())` with a constant compiles, vets, and keeps both of
// them green, because neither ever reaches it. A redaction that answers every error with
// a fixed string leaks nothing and diagnoses nothing — the failure mode the whole helper
// exists to avoid, arrived at from the opposite direction.
//
// A connection refused, an authentication failure and a missing database are the errors
// an operator actually meets, and none of them is a *url.Error. Their text is what names
// the problem, and it does not carry the credential: the DSN appears in these causes only
// when net/url quoted the fragment it choked on, which is precisely the *url.Error case
// handled above.
//
// The assertion is therefore two-sided. It requires the driver's own words to survive AND
// the credential to stay out, so neither a leak nor a silent constant can pass it.
//
// This test needs no database: it calls the pure helper directly.
func TestSafeDSNErrKeepsDriverTextForNonURLErrors(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "connection refused",
			err:  errors.New("failed to connect to `host=localhost user=ciuser database=app`: dial error: connection refused"),
			want: "connection refused",
		},
		{
			name: "authentication failed",
			err:  errors.New(`server error (FATAL: password authentication failed for user "ciuser" (SQLSTATE 28P01))`),
			want: "password authentication failed",
		},
		{
			name: "database does not exist",
			err:  errors.New(`server error (FATAL: database "app" does not exist (SQLSTATE 3D000))`),
			want: "does not exist",
		},
		{
			name: "wrapped non-url cause",
			err:  fmt.Errorf("init migrator: %w", errors.New("connection refused")),
			want: "connection refused",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := dbtest.SafeDSNErr(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("SafeDSNErr = %q, want it to preserve the driver's text %q; a "+
					"redaction that answers every cause with a fixed string diagnoses "+
					"nothing", got, tc.want)
			}
			// The sentinel is reserved for *url.Error; reaching it here would mean the
			// helper had stopped distinguishing the two shapes.
			if strings.Contains(got, "does not parse as a URL") {
				t.Errorf("SafeDSNErr = %q, want the *url.Error sentinel NOT to be used "+
					"for a cause that is not a *url.Error", got)
			}
			if strings.Contains(got, password) {
				t.Errorf("SafeDSNErr leaked the password: %q", got)
			}
		})
	}
}
