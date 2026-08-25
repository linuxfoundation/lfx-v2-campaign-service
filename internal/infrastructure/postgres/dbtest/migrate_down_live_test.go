// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5 driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/dbtest"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres/migrations"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
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
		// SafeDSNErrFor(dsn, ...), not SafeDSNErr. This function is handed an EXPLICIT
		// dsn -- the scratch DSN from freshDatabase, which is TEST_DATABASE_URL with the
		// database name swapped. SafeDSNErr would compare against the ENVIRONMENT's
		// DSN(), and the scratch database name is precisely the identifier that differs,
		// so a golang-migrate init failure naming it would be cleared as "not a
		// configured value" and printed in full. That is the same wrong-DSN seam this
		// package already fixed in connectAndMigrate; the redactor must be handed the
		// DSN this call actually used.
		t.Fatalf("init migrator: %s", dbtest.SafeDSNErrFor(dsn, err))
	}
	return m
}

// scratchReaper drops every scratch database a test created, in ONE test-level cleanup
// under ONE deadline.
//
// The per-call shape this replaces registered a separate t.Cleanup per database, each with
// its own fresh 30s budget. That is correct per database and wrong in aggregate:
// TestLiveMigrationsGoDownAndUpAgain provisions one database per migration version, so at
// 28 migrations the cleanups could wait ~29 x 30s serially. `go test` is run without a
// -timeout override, so Go's 10-minute default fires first and the run dies at the opaque
// suite timeout -- which names neither the unreachable server nor the databases left
// behind. Bounding each step did not bound the teardown.
//
// One deadline for the whole reap fixes that, and one reconnect is the other half: the
// per-call version dialled the server again for every database, so an unreachable server
// paid the connect timeout 29 times instead of once.
type scratchReaper struct {
	mu    sync.Mutex
	names []string
}

func (r *scratchReaper) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
}

// reap drops every registered database. Errorf, not Logf: this test provisions a database
// per migration version, so a silently-skipped drop does not cost one stray database, it
// accumulates a whole run's worth against the persistent local harness -- and a Logf leaves
// that invisible in a green run. The contract is that a green run leaves nothing behind, so
// failing to drop is a test failure, not a note.
func (r *scratchReaper) reap(t *testing.T) {
	r.mu.Lock()
	names := append([]string(nil), r.names...)
	r.names = nil
	r.mu.Unlock()
	if len(names) == 0 {
		return
	}

	// ONE deadline for the whole reap, and NOT derived from the test's ctx: by the time
	// Cleanup runs the test is over and that context is cancelled, so a reap inheriting it
	// would fail instantly without dropping anything.
	ctx, cancel := dbtest.CleanupContext()
	defer cancel()

	conn, err := pgx.Connect(ctx, dbtest.DSN())
	if err != nil {
		t.Errorf("cleanup: reconnect to drop %d scratch database(s) (%s): %s; they are "+
			"still present and this run did not leave the server as it found it",
			len(names), strings.Join(names, ", "), dbtest.SafeDSNErr(err))
		return
	}
	defer func() { _ = conn.Close(ctx) }()

	for i, name := range names {
		// One shared deadline means a stalled drop stops the reap rather than letting
		// each subsequent database pay its own full budget. Report the ones actually
		// SKIPPED: names[i:], not the whole registered list. An earlier version passed
		// len(names) here, which counted databases this loop had already dropped as
		// still present and named none of them -- contradicting the very thing this
		// message exists to say.
		if err := ctx.Err(); err != nil {
			remaining := names[i:]
			t.Errorf("cleanup: the %ds reap budget expired with %d of %d scratch "+
				"database(s) still present (%s); they were not dropped and this run did "+
				"not leave the server as it found it",
				int(dbtest.CleanupTimeout.Seconds()), len(remaining), len(names),
				strings.Join(remaining, ", "))
			return
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q WITH (FORCE)", name)); err != nil {
			t.Errorf("cleanup: drop %s: %v; the scratch database is still present and this "+
				"run did not leave the server as it found it", name, err)
		}
	}
}

// scratchDatabases returns the per-test reaper, registering its single cleanup on first use.
func scratchDatabases(t *testing.T) *scratchReaper {
	t.Helper()
	if r, ok := scratchReapers.Load(t); ok {
		return r.(*scratchReaper)
	}
	r := &scratchReaper{}
	actual, loaded := scratchReapers.LoadOrStore(t, r)
	r = actual.(*scratchReaper)
	if !loaded {
		t.Cleanup(func() {
			r.reap(t)
			scratchReapers.Delete(t)
		})
	}
	return r
}

// scratchReapers keys a reaper by the *testing.T that owns it, so a helper called from
// several tests cannot merge their databases into one another's teardown.
var scratchReapers sync.Map

// TestScratchReaperRegistersOneCleanupForEveryDatabase pins the aggregate bound.
//
// Per-database deadlines are correct per database and wrong in aggregate: this file
// provisions one scratch database per migration version, so 28 migrations meant ~29
// cleanups that could each wait its own full budget -- about 14.5 minutes serially, past
// go test's 10-minute default, so the run died at the opaque suite timeout instead of
// reporting the databases it left behind. Bounding each step did not bound the teardown.
//
// An earlier version of this test was VACUOUS and review caught it: it built a local
// scratchReaper, asserted on CleanupContext in isolation, and cleared r.names by
// assignment, never calling scratchDatabases or reap. Reverting the aggregate fix left it
// green, so it pinned nothing it claimed. This drives the REAL registration path through a
// subtest and counts the cleanups that path installs -- the number the whole fix is about.
//
// It is a unit test of the registration SHAPE, and the shape is all it reaches. Three
// mutations were run against it, and the third SURVIVES -- recorded here rather than left
// for the next reader to discover:
//
//   - scratchDatabases returns a fresh reaper per call  -> FAILS (same-reaper arm)
//   - scratchDatabases registers no cleanup at all      -> FAILS (map-entry arm)
//   - the cleanup is registered but never calls reap()  -> PASSES, still uncaught
//
// The third survives because this test must drain the synthetic names before the reap can
// dial a real server, so "the list is empty afterwards" cannot distinguish the reap having
// run from the drain having run. Whether reap() actually DROPS anything is pinned by no
// unit test; it needs a live database, and that is what the live down-migration test
// exercises in CI. The stalled-server behaviour is likewise unreachable here.
//
// An earlier version of this test asserted a cleanup COUNT it incremented itself, which was
// 1 by construction whether the helper registered one cleanup, many, or none. The comment
// above it claimed the opposite. Both are gone; what replaced them is the map-entry check,
// which only the helper's own cleanup can satisfy.
func TestScratchReaperRegistersOneCleanupForEveryDatabase(t *testing.T) {
	t.Parallel()

	// The registered cleanup is observed through its EFFECT rather than through a counter
	// the test increments itself. An earlier version registered its own st.Cleanup and
	// counted that, which always yielded 1 whether scratchDatabases had installed one
	// cleanup, many, or none -- review caught it, and removing the helper's registration
	// entirely left that arm green.
	//
	// reap() drains the reaper, so "did the registered cleanup run?" is answered by
	// whether the list is empty afterwards, which only the helper's own cleanup can do.
	var r *scratchReaper
	var reaped []string
	drained := -1

	var subT *testing.T
	t.Run("registration", func(st *testing.T) {
		subT = st
		r = scratchDatabases(st)

		for i := range 29 {
			name := fmt.Sprintf("down_scratch_%d", i)
			r.add(name)
			reaped = append(reaped, name)
		}
		// Drain before the reap runs. The registered reap is real and its first act is a
		// connect against TEST_DATABASE_URL; these names are synthetic, so letting it
		// proceed would either fail the subtest on an unrelated connect error or, worse,
		// issue DROP DATABASE against a live server.
		//
		// Cleanups run LIFO, so this one -- registered after the helper's -- runs FIRST
		// and hands the reap an empty list, which is the same early return a test that
		// created nothing takes. `drained` records that the names were still present at
		// that moment, which is what proves the assertions below ran against a populated
		// reaper rather than an already-empty one.
		st.Cleanup(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			drained = len(r.names)
			r.names = nil
		})

		// Every database registered with the SAME reaper: one registration, not 29.
		if got := scratchDatabases(st); got != r {
			t.Errorf("scratchDatabases returned a different reaper on the second call; "+
				"each database would then carry its own cleanup and its own budget, "+
				"which is the ~29x30s teardown this exists to prevent (%p vs %p)", got, r)
		}
		r.mu.Lock()
		held := len(r.names)
		r.mu.Unlock()
		if held != 29 {
			t.Errorf("reaper holds %d names, want 29; the names must accumulate into ONE "+
				"reap rather than one cleanup each", held)
		}
	})

	if len(reaped) != 29 {
		t.Fatalf("test set up %d names, want 29", len(reaped))
	}
	// All 29 were still held when the subtest ended, so they accumulated into the single
	// reaper rather than being dropped one cleanup at a time.
	if drained != 29 {
		t.Errorf("the reaper held %d names when the subtest ended, want 29; the databases "+
			"must accumulate into ONE reap rather than one cleanup each", drained)
	}
	// scratchDatabases must have registered a cleanup at all. reap() clears the map entry
	// for this *testing.T, so a helper that registered nothing leaves it behind -- which
	// is the regression a self-incremented counter could not see.
	if _, still := scratchReapers.Load(subT); still {
		t.Error("scratchDatabases left its reaper registered after the subtest finished; " +
			"it never installed a cleanup, so nothing would ever drop the scratch databases")
	}

	// The aggregate budget is ONE CleanupTimeout for all of them, not one each. This is
	// the arithmetic that made the per-call shape fail: 29 x 30s exceeds go test's
	// 10-minute default, so the run died at the suite timeout rather than reporting.
	perDatabaseWorst := time.Duration(len(reaped)) * dbtest.CleanupTimeout
	if perDatabaseWorst <= 10*time.Minute {
		t.Skipf("the aggregate bound is only interesting while the per-database worst case "+
			"(%v) exceeds go test's 10m default; CleanupTimeout or the migration count "+
			"changed, so re-derive this test", perDatabaseWorst)
	}
	if dbtest.CleanupTimeout >= perDatabaseWorst {
		t.Errorf("one reap budget (%v) is not smaller than the per-database worst case (%v)",
			dbtest.CleanupTimeout, perDatabaseWorst)
	}
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
		// SafeDSNErr, not %v. pgx returns *pgconn.ConnectError, whose Error() formats
		// "failed to connect to `user=%s database=%s`" straight from the Config it
		// parsed out of the DSN -- the configured username and database, verbatim, in
		// a message this line prints to the CI log.
		t.Fatalf("connect to create a scratch database: %s", dbtest.SafeDSNErr(err))
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
	scratchDatabases(t).add(name)

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
		t.Fatalf("Up on a fresh database: %s", dbtest.SafeDSNErrFor(dsn, err))
	}
	topVersion, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read version after Up: %s", dbtest.SafeDSNErrFor(dsn, err))
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
			t.Fatalf("Down one step from version %d: %s — a down migration that cannot "+
				"run is a rollback that fails during an incident, and nothing else in the "+
				"suite executes these files", version, dbtest.SafeDSNErrFor(dsn, err))
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
		t.Fatalf("after Down the version is not nil (err = %s); the schema did not return "+
			"to zero, so at least one down migration left state behind",
			dbtest.SafeDSNErrFor(dsn, err))
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
		t.Fatalf("Up after a full Down: %s — the down set left the database in a state "+
			"the up set cannot be re-applied to, so a rollback would be one-way",
			dbtest.SafeDSNErrFor(dsn, err))
	}
	reVersion, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("read version after the second Up: %s", dbtest.SafeDSNErrFor(dsn, err))
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
		t.Fatalf("reference database: migrate up to version %d: %s", version,
			dbtest.SafeDSNErrFor(dsn, err))
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
		// Redacted against the DSN THIS function was handed, which is a scratch DSN
		// rewritten from TEST_DATABASE_URL and carries the same credential.
		t.Fatalf("connect to read the schema: %s", dbtest.SafeDSNErrFor(dsn, err))
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
// it through the wrapper, so the DISCARD arm handles this too: SafeDSNErr does not unwrap
// the cause and re-render it, it drops the whole error and returns dsnUnparseableMsg —
// which is the point, since a *url.Error's own text quotes the fragment it choked on and
// that fragment can carry the credential. The wrapper text is what a reader sees, and
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
				// `got` is the rewritten DSN and carries whatever credential tc.dsn
				// carried, and pgx's ParseConfigError echoes the connection string.
				t.Fatalf("pgxpool.ParseConfig(rewritten): %s", dbtest.SafeDSNErrFor(got, err))
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
// This test needs no database, and it now names its DSN EXPLICITLY rather than inheriting
// whatever the runner exported. That is not tidying. SafeDSNErr clears a message by checking
// it against the identifiers in the configured DSN, so while it read the environment this
// test's result depended on a value it did not choose: its fixtures say `host=localhost` —
// the commonest hostname there is — so a developer or CI job whose DSN also said `localhost`
// saw the helper correctly withhold a message naming the configured host, and read that
// correct behaviour as a regression. Passing the DSN through SafeDSNErrFor makes the
// assertion depend on the helper alone, which is what it was always trying to measure, and
// it keeps the test parallel: pinning via t.Setenv would have forced it serial and mutated
// process-global state that this file's parallel tests read.
func TestSafeDSNErrKeepsDriverTextForNonURLErrors(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line

	// Passed EXPLICITLY, and deliberately sharing nothing with the fixture messages below,
	// so a message is kept or withheld on its own merits rather than on a coincidence with
	// whatever DSN the runner happens to export.
	const pinnedDSN = "host=pinned.invalid port=5432 user=pinneduser password=pinnedpw dbname=pinneddb" // secretlint-disable-line

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

			got := dbtest.SafeDSNErrFor(pinnedDSN, tc.err)
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

// TestSafeDSNErrWithholdsKeywordDSNIdentifiers covers the THIRD leak arm in this
// redaction, and the first one that is not URL-shaped at all.
//
// The two arms before it were both about a *url.Error: first that `%v` on one embeds the
// whole DSN, then that unwrapping to its cause still quotes the fragment net/url choked
// on. Both were fixed by withholding, and both left the fallback arm — every error that
// is NOT a *url.Error — reading `redact.URLUserinfo(err.Error())`.
//
// That backstop only understands URL-shaped `user:pass@` text, and a DSN has a second
// legal shape. pgx accepts keyword/value connection strings and builds its errors out of
// them, so the identifiers arrive in a form the redactor does not recognise and are
// returned verbatim. Two channels do it, and they are independent:
//
//   - pgx SPLICES the DSN into its own messages. `*pgconn.ConnectError` formats
//     "failed to connect to `user=%s database=%s`" from Config; `*pgconn.ParseConfigError`
//     prints the whole connection string through pgx's `redactPW`, which masks the
//     password and keeps the username. Neither unwraps to a *url.Error.
//   - PostgreSQL QUOTES the identifiers back in its own diagnostics, with no involvement
//     from the DSN's formatting at all: `role "u" does not exist`, `database "d" does not
//     exist`, `password authentication failed for user "u"`.
//
// The second channel is why this test does not simply assert that pgx's two templates are
// scrubbed. A denylist of message shapes cannot cover text the server generated, and this
// redaction has already been defeated twice by fixes that pattern-matched the shapes then
// in evidence. So the assertion is the invariant itself: no identifier from the configured
// DSN appears in the output, whatever produced the message.
//
// The username is asserted absent alongside the password deliberately. This package's
// contract — stated on pkg/redact and pinned by the *url.Error tests above — is that
// userinfo goes ENTIRELY, because a username issued alongside a password is half of one
// credential rather than a public identifier.
//
// This test needs no database and touches no process-global state. It calls the pure helper
// directly and passes the DSN it means as an argument, so it stays parallel; nothing connects.
func TestSafeDSNErrWithholdsKeywordDSNIdentifiers(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	const username = "ciuser"
	const database = "campaigndb"
	const host = "db.internal"

	// The keyword/value form, which is exactly the shape redact.URLUserinfo cannot read.
	dsn := "host=" + host + " port=5432 user=" + username + " password=" + password + " dbname=" + database

	cases := []struct {
		name string
		err  error
		// the identifier this case demonstrates leaking, so a case cannot quietly stop
		// exercising the arm it guards.
		leaks string
	}{
		{
			name:  "pgx ConnectError splices user and database",
			err:   errors.New("failed to connect to `user=" + username + " database=" + database + "`: dial error: connection refused"),
			leaks: username,
		},
		{
			name:  "pgx ParseConfigError keeps the username after its own redactPW",
			err:   errors.New("cannot parse `host=" + host + " user=" + username + " password=xxxxx dbname=" + database + "`: failed to configure TLS (sslmode is invalid)"),
			leaks: username,
		},
		{
			name:  "a keyword DSN carrying the password verbatim",
			err:   errors.New("host=" + host + " user=" + username + " password=" + password + " dbname=" + database),
			leaks: password,
		},
		{
			name:  "the server quotes the role back (28000)",
			err:   errors.New(`server error: FATAL: role "` + username + `" does not exist (SQLSTATE 28000)`),
			leaks: username,
		},
		{
			name:  "the server quotes the role back on auth failure (28P01)",
			err:   errors.New(`server error (FATAL: password authentication failed for user "` + username + `" (SQLSTATE 28P01))`),
			leaks: username,
		},
		{
			name:  "the server quotes the database back (3D000)",
			err:   errors.New(`server error (FATAL: database "` + database + `" does not exist (SQLSTATE 3D000))`),
			leaks: database,
		},
		{
			name:  "wrapped, so the leak is not at the top level",
			err:   fmt.Errorf("init migrator: %w", errors.New("failed to connect to `user="+username+" database="+database+"`: connection refused")),
			leaks: username,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The control: the UNFIXED rendering leaks. redact.URLUserinfo is what the
			// fallback arm used to return, so running the raw message through it is
			// exactly the old behaviour. If this stops holding, the case no longer
			// demonstrates the leak it guards and the assertions below prove nothing.
			if old := redact.URLUserinfo(tc.err.Error()); !strings.Contains(old, tc.leaks) {
				t.Fatalf("precondition failed: the URL-shaped redactor did not pass %q "+
					"through, so this case cannot demonstrate the leak it guards", tc.leaks)
			}

			got := dbtest.SafeDSNErrFor(dsn, tc.err)

			for _, id := range []string{password, username, database, host} {
				if strings.Contains(got, id) {
					t.Errorf("SafeDSNErr = %q, want no identifier from %s; it echoed %q",
						got, dbtest.EnvDatabaseURL, id)
				}
			}
			// It must still be actionable. A redaction that leaks nothing and says
			// nothing trades one defect for another, so the output has to name the
			// variable an operator should go and look at.
			if !strings.Contains(got, dbtest.EnvDatabaseURL) {
				t.Errorf("SafeDSNErr = %q, want it to still name %s so the operator knows "+
					"which value to inspect", got, dbtest.EnvDatabaseURL)
			}
		})
	}
}

// TestSafeDSNErrWithholdsEachIdentifierIndependently pins that EVERY field of the
// configured DSN is compared, not just the ones that happen to travel together.
//
// The table above cannot see this. Its messages are realistic, and a realistic pgx error
// names several identifiers at once — `user=u database=d` carries both — so dropping any
// single field from the comparison still leaves the message caught by one of the others.
// Mutations removing cfg.Host and cfg.Password from the compared set both SURVIVED that
// table for exactly this reason: a test whose cases each trip several assertions cannot
// tell which assertion is load-bearing.
//
// So each case here names ONE identifier and nothing else, which is the only shape that
// fails when that identifier's comparison is removed. The host and the database matter as
// much as the user: an internal hostname is infrastructure detail an operator should not
// have to paste into a public CI log, and this package's contract is that the DSN's
// contents do not appear, not that its password does not.
func TestSafeDSNErrWithholdsEachIdentifierIndependently(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	const username = "ciuser"
	const database = "campaigndb"
	const host = "db.internal"

	dsn := "host=" + host + " port=5432 user=" + username + " password=" + password + " dbname=" + database

	// Each message mentions its identifier and NOTHING else from the DSN, so it is caught
	// only by that identifier's own comparison.
	cases := []struct{ name, only string }{
		{"user alone", username},
		{"database alone", database},
		{"host alone", host},
		{"password alone", password},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := errors.New("server error: FATAL: something went wrong near " + tc.only)

			got := dbtest.SafeDSNErrFor(dsn, err)
			if strings.Contains(got, tc.only) {
				t.Errorf("SafeDSNErr = %q, want %q withheld; every field of %s is compared, "+
					"not only the ones that travel alongside another", got, tc.only,
					dbtest.EnvDatabaseURL)
			}
		})
	}
}

// TestSafeDSNErrWithholdsWhenTheDSNCannotBeParsed pins the direction the unparseable case
// fails in.
//
// dsnIdentifiersPresent clears a message by proving it contains none of the configured
// identifiers. When the DSN does not parse there are no identifiers to prove that with, so
// nothing can be cleared and the sentinel has to apply. Returning false there instead —
// "we found no identifiers, so the message is fine" — reads like the same sentence and is
// the opposite guarantee: it restores the verbatim passthrough in precisely the case where
// the configured value is least well understood. That mutation compiles and SURVIVED the
// tables above, because none of their cases drives an unparseable DSN.
func TestSafeDSNErrWithholdsWhenTheDSNCannotBeParsed(t *testing.T) {
	t.Parallel()

	const username = "ciuser"

	// A DSN pgconn.ParseConfig rejects outright.
	const badDSN = "host=h user=u sslmode=notamode"

	if _, err := pgconn.ParseConfig(badDSN); err == nil {
		t.Fatal("precondition failed: the DSN parses, so this test no longer exercises " +
			"the unparseable branch it guards")
	}

	got := dbtest.SafeDSNErrFor(badDSN, errors.New(`server error: FATAL: role "`+username+`" does not exist`))
	if !strings.Contains(got, dbtest.EnvDatabaseURL) {
		t.Errorf("SafeDSNErr = %q, want the sentinel naming %s: with an unparseable DSN "+
			"there are no identifiers to clear a message with, so nothing can be proven "+
			"safe to print", got, dbtest.EnvDatabaseURL)
	}
}

// TestSafeDSNErrKeepsDiagnosticsWhenAFieldIsEmpty pins the guard that keeps an absent DSN
// field from swallowing every message in the package.
//
// The local setup is peer auth: a DSN with no password at all, which leaves Config.Password
// empty. `strings.Contains(msg, "")` is true for EVERY string, so comparing an empty field
// would report that every error names the credential, and SafeDSNErr would answer every
// call — including a plain "connection refused" — with the sentinel. That is the failure
// this whole helper exists to avoid, reached from the other side: a redaction that leaks
// nothing and diagnoses nothing.
//
// Dropping the `id != ""` test compiles and SURVIVES every table above, because they all
// configure a DSN in which every compared field is populated.
func TestSafeDSNErrKeepsDiagnosticsWhenAFieldIsEmpty(t *testing.T) {
	t.Parallel()

	// Peer auth: a user and a database, no password.
	const peerDSN = "host=/var/run/postgresql user=peer dbname=campaign_test"

	got := dbtest.SafeDSNErrFor(peerDSN, errors.New("connection refused"))
	if !strings.Contains(got, "connection refused") {
		t.Errorf("SafeDSNErr = %q, want the driver's text preserved: an EMPTY DSN field "+
			"matches every message, so comparing it would withhold every error this "+
			"package prints", got)
	}
}

// TestSafeDSNErrReadsTheConfiguredDSN pins the one thing the explicit-DSN tests cannot see:
// that the environment-reading wrapper actually forwards the CONFIGURED value.
//
// Every other test here calls SafeDSNErrFor with a DSN it names itself, which is what keeps
// them parallel and free of process-global state. The cost is that they say nothing about
// SafeDSNErr, and the harness calls SafeDSNErr — so replacing DSN() with "" in the wrapper
// compiles, keeps all of them green, and silently restores the verbatim passthrough on the
// only call path that runs in CI. That mutation SURVIVED until this test existed.
//
// This is the one case that must touch the environment, so it is deliberately serial and
// deliberately minimal: it asserts the wiring, not the redaction.
func TestSafeDSNErrReadsTheConfiguredDSN(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it, and this test exists precisely to check the
	// environment read. It is the ONLY test in this file that needs that.
	const username = "envwireduser"

	t.Setenv(dbtest.EnvDatabaseURL,
		"host=env.invalid port=5432 user="+username+" password=envpw dbname=envdb") // secretlint-disable-line

	got := dbtest.SafeDSNErr(errors.New(`server error: FATAL: role "` + username + `" does not exist`))
	if strings.Contains(got, username) {
		t.Errorf("SafeDSNErr = %q, want the identifier withheld; the wrapper must pass the "+
			"CONFIGURED DSN to SafeDSNErrFor, or the harness's own call path redacts "+
			"nothing", got)
	}
}

// TestScratchDatabaseHelpersRedactConnectErrors pins that this file's own connect sites do
// not undo the redaction the rest of the PR installs.
//
// The helpers here call pgx.Connect with a DSN and, until this test existed, printed the
// failure with %v. pgx returns *pgconn.ConnectError, whose Error() formats
// "failed to connect to `user=%s database=%s`" straight from the Config it parsed out of
// that DSN — so the configured username and database went to the CI log from a test file
// whose whole subject is keeping them out of it. The redaction being correct in dbtest.go
// says nothing about the call sites; only rendering a real failure does.
//
// It drives a REAL pgx connect against an unreachable port rather than a hand-written
// error, so the message is whatever pgx actually produces rather than what this test
// imagines it produces.
func TestScratchDatabaseHelpersRedactConnectErrors(t *testing.T) {
	t.Parallel()

	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	const username = "scratchuser"
	const database = "scratchdb"

	// Port 1 refuses immediately, so this needs no database and cannot hang.
	dsn := "postgres://" + username + ":" + password + "@127.0.0.1:1/" + database + "?sslmode=disable"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := pgx.Connect(ctx, dsn)
	if err == nil {
		t.Fatal("precondition failed: the connect succeeded, so this test no longer " +
			"exercises the connect-failure branch it guards")
	}

	// The control: the UNREDACTED rendering leaks, so the assertion below is not vacuous.
	if raw := fmt.Sprintf("%v", err); !strings.Contains(raw, username) {
		t.Fatalf("precondition failed: pgx's error did not name the user, so this case " +
			"cannot demonstrate the leak it guards")
	}

	got := dbtest.SafeDSNErrFor(dsn, err)
	for _, id := range []string{password, username, database} {
		if strings.Contains(got, id) {
			t.Errorf("SafeDSNErrFor = %q, want no identifier from the DSN; it echoed %q — "+
				"this is what the helpers in this file print on a failed connect", got, id)
		}
	}
}

// TestSafeDSNErrDoesNotOverMatchEmbeddedIdentifiers pins the boundary half of the
// redaction contract: withhold every message that REPRODUCES a configured identifier, and
// keep every message that merely embeds it inside a longer word.
//
// Both halves are failures, and only one of them is a leak. The DSN here is CI's:
// user and password "postgres", which is a substring of "postgresql". Under a raw
// strings.Contains the "keeps" table below was withheld wholesale -- the sentinel replaced
// ordinary protocol prose that named no credential at all, in exactly the configuration
// this package is meant to keep readable.
//
// The messages are the driver's real shapes, not invented ones: pgx formats
// "failed to connect to `user=%s database=%s`", Postgres formats
// `password authentication failed for user "%s"` and `role "%s" does not exist`.
func TestSafeDSNErrDoesNotOverMatchEmbeddedIdentifiers(t *testing.T) {
	t.Parallel()

	// user and password are both "postgres" -- the CI default, and the case that makes
	// the embedding question real.
	const dsn = "postgres://postgres:postgres@127.0.0.1:5432/campaign_test?sslmode=disable" // secretlint-disable-line -- CI-default fixture; identical user and password is the case under test

	// WITHHELD: each of these names a configured identifier as a value.
	withheld := []string{
		"failed to connect to `user=postgres database=campaign_test`: connection refused",
		`password authentication failed for user "postgres"`,
		`role "postgres" does not exist`,
		`FATAL: database "campaign_test" does not exist`,
	}
	for _, msg := range withheld {
		got := dbtest.SafeDSNErrFor(dsn, errors.New(msg))
		if got == msg {
			t.Errorf("SafeDSNErrFor = %q, want it WITHHELD; the message reproduces an "+
				"identifier from the DSN as a value", got)
		}
	}

	// KEPT: none of these reproduces an identifier. "postgresql" merely embeds the
	// user/password "postgres"; it is a different token and leaks nothing.
	kept := []string{
		"unsupported postgresql wire protocol version 2",
		"server closed the connection unexpectedly (postgresql protocol error)",
		"dial tcp: lookup postgresql.svc: no such host",
		"connection refused",
		"context deadline exceeded",
	}
	for _, msg := range kept {
		got := dbtest.SafeDSNErrFor(dsn, errors.New(msg))
		if got != msg {
			t.Errorf("SafeDSNErrFor = %q, want the driver's text %q preserved; "+
				"%q embeds a configured identifier inside a longer word but reproduces "+
				"no credential, and withholding it costs the diagnosis for nothing",
				got, msg, msg)
		}
	}

	// A bounded echo immediately following an embedded one must still be caught: the
	// scan advances one byte per attempt rather than skipping a whole match.
	both := `postgresql driver: role "postgres" does not exist`
	if got := dbtest.SafeDSNErrFor(dsn, errors.New(both)); got == both {
		t.Errorf("SafeDSNErrFor = %q, want it withheld; an embedded occurrence must not "+
			"mask a genuine bounded echo later in the same message", got)
	}
}

// TestSafeDSNErrWithholdsFallbackHosts pins the multi-host DSN case.
//
// pgconn.ParseConfig puts only the FIRST host in Config.Host; a comma-separated DSN parses
// its remaining hosts into Config.Fallbacks. pgx dials each in turn and names the one that
// failed, using originalHostname -- the host as written in the DSN. So a comparison against
// Config.Host alone cleared every error that named only a secondary host, and it printed in
// full.
//
// The primary case is asserted alongside the fallbacks, because a fix that swapped the
// comparison to the fallbacks instead of adding them would pass a fallback-only test.
func TestSafeDSNErrWithholdsFallbackHosts(t *testing.T) {
	t.Parallel()

	// Three hosts: one primary, two fallbacks. .invalid keeps them unresolvable, so this
	// DSN cannot collide with any harness value. //nolint:gosec // synthetic fixture
	const dsn = "postgres://multiuser:multipw@primary.invalid:5432," + //nolint:gosec // secretlint-disable-line -- synthetic multi-host fixture; .invalid hosts are unresolvable
		"secondary.invalid:5433,third.invalid:5434/multidb?sslmode=disable"

	// Every host in the DSN must be withheld, not just the first.
	for _, host := range []string{"primary.invalid", "secondary.invalid", "third.invalid"} {
		msg := `failed to connect to host "` + host + `": connection refused`
		got := dbtest.SafeDSNErrFor(dsn, errors.New(msg))
		if strings.Contains(got, host) {
			t.Errorf("SafeDSNErrFor = %q, want the host %q withheld; ParseConfig keeps "+
				"secondary hosts in Config.Fallbacks and pgx names the host that failed, "+
				"so comparing against Config.Host alone leaks every fallback", got, host)
		}
	}

	// The other identifiers still hold on a multi-host DSN: the fallback loop must ADD to
	// the comparison, not replace it.
	for _, id := range []string{"multiuser", "multipw", "multidb"} {
		msg := `failed to connect to ` + "`user=" + id + "`"
		if got := dbtest.SafeDSNErrFor(dsn, errors.New(msg)); strings.Contains(got, id) {
			t.Errorf("SafeDSNErrFor = %q, want %q withheld on a multi-host DSN", got, id)
		}
	}

	// Still diagnosable: a message naming no host in the DSN keeps its text.
	const clean = "connection refused"
	if got := dbtest.SafeDSNErrFor(dsn, errors.New(clean)); got != clean {
		t.Errorf("SafeDSNErrFor = %q, want %q preserved; adding fallback hosts to the "+
			"comparison must not withhold messages that name none of them", got, clean)
	}
}

// TestNoConnectSiteRendersItsErrorRaw is a SOURCE assertion, and it is deliberate.
//
// The rest of this file asserts on rendered strings, on the stated principle that a leak is
// a property of what an error formats to rather than of which verb produced it. That
// principle still holds — but it can only be applied where a test can REACH the failure.
// The connect sites in this file fire when a live database becomes unreachable mid-run,
// which no unit test can arrange, so a rendered-output test cannot cover them: reverting one
// to %v leaves every behavioural test in the package green.
//
// A behaviour that cannot be observed can still be pinned at its source, and that is the
// honest instrument here. This reads the file and requires every pgx.Connect / ParseConfig
// error to be rendered through the package's redaction rather than with %v. It is a weaker
// guarantee than a rendered-output assertion and is not a substitute for one anywhere the
// output CAN be rendered — it exists exactly where that option does not.
//
// It asks TWO questions, because a redactor CALL is not evidence of redaction. The second
// exists because the leak this package was opened to fix survived five review rounds while
// a redactor was plainly present at the leaking site: connectAndMigrate called SafeDSNErr,
// which compares against the ENVIRONMENT's DSN(), while the function had been handed an
// explicit one. The identifier that differed was the one that leaked.
//
//  1. Is the error rendered through a redactor at all, rather than with %v?
//
// Its mutation coverage, recorded because each of these once PASSED against an earlier
// version of this guard and every one of those versions looked correct:
//
//   - `%v` in place of a redactor                       -> question 1
//   - SafeDSNErr where the call used an explicit DSN    -> question 2
//   - SafeDSNErrFor handed dbtest.DSN() instead of dsn  -> question 2 (right fn, wrong arg)
//   - renaming the `dsn` parameter to `databaseURL`     -> defeated the name-based version
//   - `databaseURL := dbtest.DSN()` aliased into
//     withDatabase, then formatted with `%v`            -> defeated the name-based liveDSNArg
//   - forcing every pairing onto the environment branch -> explicitPairs self-test
//
// Three of those six turned on SPELLING, which is why nothing in this guard is allowed to
// ask what an identifier is called: bareErrArgs takes the identifier the call BOUND,
// dsnCarriers is computed by data flow, and the expected DSN is read from the call's own
// argument.
//
//  2. Does the redactor compare against the DSN the failing call actually USED? The
//     expected value is read from the DSN-bearing call's own argument, so it does not
//     depend on what any parameter is spelled. A call that used the environment's DSN()
//     is satisfied by either form; a call that used any other value requires
//     SafeDSNErrFor handed THAT value. A bare SafeDSNErr on an explicit DSN silently
//     redacts against a different secret than the call used.
func TestNoConnectSiteRendersItsErrorRaw(t *testing.T) {
	t.Parallel()

	// Parsed, not scanned. Two successive line-window versions of this guard missed a
	// site each: the first used a fixed 5-line lookahead and a four-line comment pushed
	// a Fatalf out of range; the second scanned the whole `if` block but still only
	// inspected the single line holding t.Fatalf/t.Errorf, so a call whose argument sat
	// on a continuation line was never checked. Both failures share one cause -- line
	// proximity only approximates the question. The AST asks it directly: for every
	// t.Fatalf/t.Errorf reachable from a DSN-bearing call's error handling, is the error
	// value passed through a redactor, or handed over bare?
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "migrate_down_live_test.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse own source: %v", err)
	}

	// Every call that receives a DSN (or a value rewritten from one) and can therefore
	// fail with an error built out of it. migrate.NewWithSourceInstance and withDatabase
	// are here because they are handed migrateURL(t, dsn) / dbtest.DSN(): the AST walk
	// found both when a mutation at one of them survived a list that named only the pgx
	// entry points, so the call set, not just the matching, was too narrow.
	dsnCalls := map[string]bool{
		"pgx.Connect":                   true,
		"pgxpool.ParseConfig":           true,
		"pgxpool.New":                   true,
		"postgres.NewPool":              true,
		"migrate.NewWithSourceInstance": true,
	}
	// The migrator's METHODS are DSN-bearing too, which the original call set missed
	// because it only listed things that take a DSN as an argument. golang-migrate holds
	// the DSN inside the driver and reconnects through database/sql, so Up/Steps/Migrate/
	// Version can each surface pgx's *pgconn.ConnectError -- the same error the connect
	// sites redact. Rendered against a closed port, the driver produced
	// "failed to connect to `user=leakuser database=leakdb`" verbatim, so seven %v sites
	// here were printing the credential the rest of this file exists to withhold.
	//
	// Matched on the METHOD name against the migrator receiver, since the value is bound
	// as `m` by newMigrator rather than produced by a call this map could name.
	migratorMethods := map[string]bool{
		"Up": true, "Down": true, "Steps": true, "Migrate": true, "Version": true,
		"Force": true,
	}
	// The only renderings that count as safe passage for an error value.
	redactors := map[string]bool{"SafeDSNErr": true, "SafeDSNErrFor": true}

	// The DSN-taking redactor, which is the only one correct inside a function that was
	// handed an explicit DSN. Kept separate from `redactors` because the two questions
	// differ: `redactors` answers "is this rendered safely at all", this answers "is it
	// rendered against the RIGHT secret".
	const dsnRedactor = "SafeDSNErrFor"

	// selName renders a call's function as "pkg.Func" (or "Func") for matching.
	selName := func(e ast.Expr) string {
		switch f := e.(type) {
		case *ast.SelectorExpr:
			if x, ok := f.X.(*ast.Ident); ok {
				return x.Name + "." + f.Sel.Name
			}
			return f.Sel.Name
		case *ast.Ident:
			return f.Name
		}
		return ""
	}

	// dsnCarriers holds every identifier in the file that carries a live DSN, computed by
	// DATA FLOW rather than by spelling.
	//
	// The seed is a call to DSN() or a function parameter whose type is string in a
	// function that also performs a DSN-bearing call; from there assignment propagates.
	// The name-based version this replaces asked whether an identifier was literally
	// spelled `dsn`, which is the same spelling heuristic this guard has now been burned
	// by three times: `databaseURL := dbtest.DSN()` followed by
	// withDatabase(databaseURL, name) made the call invisible, and a raw %v of its error
	// passed the guard while the other sites held every coverage counter above zero.
	// Verified against this file before the rewrite.
	dsnCarriers := map[string]bool{}
	for {
		grew := false
		mark := func(name string) {
			if name != "" && name != "_" && !dsnCarriers[name] {
				dsnCarriers[name] = true
				grew = true
			}
		}
		// carriesDSN reports whether an expression evaluates to a live DSN: a DSN() call,
		// an identifier already known to carry one, or a rewrite over either.
		var carriesDSN func(ast.Expr) bool
		carriesDSN = func(e ast.Expr) bool {
			switch x := e.(type) {
			case *ast.Ident:
				return dsnCarriers[x.Name]
			case *ast.CallExpr:
				if strings.HasSuffix(selName(x.Fun), ".DSN") || selName(x.Fun) == "DSN" {
					return true
				}
				// A rewrite (migrateURL, withDatabase) carries a DSN if any argument does.
				for _, a := range x.Args {
					if carriesDSN(a) {
						return true
					}
				}
			}
			return false
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				// x := <dsn-valued expr>, positionally.
				for i, rhs := range st.Rhs {
					if i < len(st.Lhs) && carriesDSN(rhs) {
						if id, ok := st.Lhs[i].(*ast.Ident); ok {
							mark(id.Name)
						}
					}
				}
			case *ast.FuncDecl:
				// A string parameter of a function that itself performs a DSN-bearing
				// call is handed a DSN by its callers. newMigrator(t, dsn) and
				// schemaObjects(ctx, t, dsn) both qualify under any spelling.
				if st.Type.Params == nil || st.Body == nil {
					return true
				}
				performsDSNCall := false
				ast.Inspect(st.Body, func(n ast.Node) bool {
					if c, ok := n.(*ast.CallExpr); ok && dsnCalls[selName(c.Fun)] {
						performsDSNCall = true
					}
					return !performsDSNCall
				})
				if !performsDSNCall {
					return true
				}
				for _, f := range st.Type.Params.List {
					if id, ok := f.Type.(*ast.Ident); !ok || id.Name != "string" {
						continue
					}
					for _, nm := range f.Names {
						mark(nm.Name)
					}
				}
			}
			return true
		})
		if !grew {
			break
		}
	}

	// withDatabase is DSN-bearing only when it is handed the CONFIGURED DSN. The
	// table-driven test at TestWithDatabase feeds it hardcoded literals ("u:p@h"), whose
	// echo leaks nothing, and flagging those would train the reader to ignore this guard.
	// So it qualifies by argument, not by name -- and "is this argument a DSN" is now
	// answered from dsnCarriers rather than from how the identifier is spelled.
	liveDSNArg := func(call *ast.CallExpr) bool {
		for _, a := range call.Args {
			switch x := a.(type) {
			case *ast.CallExpr:
				if strings.HasSuffix(selName(x.Fun), ".DSN") || selName(x.Fun) == "DSN" {
					return true
				}
			case *ast.Ident:
				if dsnCarriers[x.Name] {
					return true
				}
			}
		}
		return false
	}

	// migratorVars holds identifiers bound by newMigrator, so a method call on one counts
	// as DSN-bearing without needing the DSN to appear at that call site.
	// The DSN each migrator was BUILT from, kept alongside the variable rather than as a
	// bare bool. A method call carries its DSN in the receiver, so there is no argument for
	// dsnArgOf to read -- without this the pairing question saw an empty `want` and skipped
	// every migrator site, and swapping SafeDSNErrFor(dsn, err) for SafeDSNErr(err) on one
	// of them passed the guard. Verified before this was added.
	migratorVars := map[string]ast.Expr{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		c, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || selName(c.Fun) != "newMigrator" || len(c.Args) == 0 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			// newMigrator(t, dsn): the DSN is the last argument.
			migratorVars[id.Name] = c.Args[len(c.Args)-1]
		}
		return true
	})

	// isDSNCall reports whether an expression contains a call that takes a DSN.
	isDSNCall := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				name := selName(c.Fun)
				if dsnCalls[name] {
					found = true
				}
				if name == "withDatabase" && liveDSNArg(c) {
					found = true
				}
				// m.Up(), m.Version(), ... on a migrator built from a DSN.
				if se, ok := c.Fun.(*ast.SelectorExpr); ok && migratorMethods[se.Sel.Name] {
					if id, ok := se.X.(*ast.Ident); ok {
						if _, isMigrator := migratorVars[id.Name]; isMigrator {
							found = true
						}
					}
				}
			}
			return !found
		})
		return found
	}

	// bareErrArgs collects identifiers passed DIRECTLY as an argument to the format
	// call -- the leak shape. An identifier nested inside a redactor is not direct.
	// bareErrArgs collects occurrences of the BOUND error identifiers that reach the
	// format call without passing through a redactor. It descends into nested calls
	// rather than looking only at top-level arguments, because fmt.Sprintf("%v", err)
	// leaks exactly as much as err does — wrapping an error in a non-redactor call
	// renders it just the same. Descent stops at a redactor: everything below
	// SafeDSNErr/SafeDSNErrFor is already safe.
	//
	// Safety is a property of each OCCURRENCE, not of the identifier, so the walk decides
	// per position: t.Fatalf("%s: %v", SafeDSNErr(err), err) is a finding on its second
	// argument even though the first is redacted.
	//
	// `bound` comes from the ASSIGNMENT that produced the error, never from how it is
	// spelled. An earlier version asked whether a name was "err" or ended in Err/Error,
	// which is a claim about spelling rather than about the value: renaming the result to
	// `failure` and passing it raw left this empty and the guard green. The AST knows
	// which identifier the DSN-bearing call actually bound, so it is asked instead.
	bareErrArgs := func(call *ast.CallExpr, bound map[string]bool) []string {
		var bare []string
		for _, a := range call.Args {
			ast.Inspect(a, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					name := selName(c.Fun)
					if i := strings.LastIndex(name, "."); i >= 0 {
						name = name[i+1:]
					}
					if redactors[name] {
						return false // redacted below this point
					}
					return true
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if !bound[id.Name] {
					return true
				}
				bare = append(bare, id.Name)
				return true
			})
		}
		return bare
	}

	// boundErrs returns the identifiers a DSN-bearing statement assigns. Both the plain
	// `x, err := call(...)` form and an `if x, err := call(...); err != nil` initializer
	// bind here, and the LAST result is the error by Go convention — the same convention
	// `go vet`'s errcheck relies on. Taking the last result rather than a named one is
	// what makes the check independent of spelling.
	boundErrs := func(stmt ast.Stmt) map[string]bool {
		bound := map[string]bool{}
		record := func(as *ast.AssignStmt) {
			if len(as.Rhs) != 1 || !isDSNCall(as.Rhs[0]) || len(as.Lhs) == 0 {
				return
			}
			if id, ok := as.Lhs[len(as.Lhs)-1].(*ast.Ident); ok && id.Name != "_" {
				bound[id.Name] = true
			}
		}
		switch st := stmt.(type) {
		case *ast.AssignStmt:
			record(st)
		case *ast.IfStmt:
			if as, ok := st.Init.(*ast.AssignStmt); ok {
				record(as)
			}
		}
		return bound
	}

	// dsnArgOf returns the SOURCE TEXT of the DSN argument a DSN-bearing call was handed.
	//
	// This is the question the guard actually needs answered, and an earlier version asked
	// a proxy for it: it looked for a parameter literally spelled `dsn` on the enclosing
	// function. That is a claim about SPELLING, and it failed exactly as the spelling-based
	// version of bareErrArgs did before it -- renaming newMigrator's parameter to
	// `databaseURL` and reverting its formatter to SafeDSNErr removed the function from the
	// map entirely, and the wrong-DSN regression passed while another function kept the
	// coverage counter nonzero. Verified against this file before the rewrite.
	//
	// The DSN a call used is not a property of any name: it is the argument at the call
	// site. pgx.Connect(ctx, X) and migrate.NewWithSourceInstance(_, _, migrateURL(t, X))
	// both USED X, so X is what their error must be redacted against. Reading it from the
	// call is independent of how anything is spelled.
	dsnArgOf := func(n ast.Node) (ast.Expr, bool) {
		var arg ast.Expr
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || found {
				return !found
			}
			// A migrator METHOD carries its DSN in the receiver, so the expected value is
			// the DSN newMigrator was handed rather than any argument at this call.
			if se, ok := c.Fun.(*ast.SelectorExpr); ok && migratorMethods[se.Sel.Name] {
				if id, ok := se.X.(*ast.Ident); ok {
					if built, isMigrator := migratorVars[id.Name]; isMigrator {
						arg = built
						found = true
						return false
					}
				}
			}
			name := selName(c.Fun)
			if !dsnCalls[name] && (name != "withDatabase" || !liveDSNArg(c)) {
				return true
			}
			// WHICH argument carries the DSN is a property of each function's
			// signature, so it is stated per call rather than guessed. Every entry in
			// dsnCalls takes it LAST -- pgx.Connect(ctx, dsn), pgxpool.New(ctx, dsn),
			// pgxpool.ParseConfig(dsn), postgres.NewPool(ctx, dsn),
			// NewWithSourceInstance(name, src, url) -- but withDatabase takes it FIRST,
			// as withDatabase(dsn, name). Reading the last argument there resolved to
			// `name`, the scratch database name, and reported a correct
			// SafeDSNErr(DSN()) call as mispaired.
			idx := len(c.Args) - 1
			if name == "withDatabase" {
				idx = 0
			}
			if idx < 0 || idx >= len(c.Args) {
				return true
			}
			arg = c.Args[idx]
			found = true
			return false
		})
		return arg, found
	}

	// dsnRoot reduces a DSN argument to the identifier it ultimately reads, so a call
	// wrapped in a rewrite still names its source. migrateURL(t, dsn) -> dsn;
	// dbtest.DSN() -> the sentinel below.
	const envDSNSentinel = "\x00env"
	var dsnRoot func(ast.Expr) string
	dsnRoot = func(e ast.Expr) string {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.CallExpr:
			if strings.HasSuffix(selName(x.Fun), ".DSN") || selName(x.Fun) == "DSN" {
				return envDSNSentinel
			}
			// A rewrite like migrateURL(t, dsn) or withDatabase(dsn, name): the DSN it
			// carries is the FIRST argument that resolves to a DSN, in source order.
			//
			// Order matters and the environment must not be demoted. withDatabase's
			// signature is (dsn, name), so a "prefer any identifier over the sentinel"
			// pass picked `name` -- the scratch database NAME, not a DSN at all -- and
			// reported the correct SafeDSNErr(DSN()) call as mispaired. Taking the first
			// resolving argument keeps withDatabase(dbtest.DSN(), name) reading as the
			// environment and migrateURL(t, dsn) reading as dsn, because in each the DSN
			// genuinely comes first among the arguments that resolve at all.
			for _, a := range x.Args {
				// `t` is threaded through these helpers as a *testing.T, never as a
				// DSN: migrateURL(t, dsn) resolved to `t` on a first-argument scan and
				// reported the fixed newMigrator call as mispaired. It is the only such
				// value in this file, and skipping it by name is honest -- the
				// alternative is type resolution, which needs go/types and a full
				// package load for one identifier.
				if id, ok := a.(*ast.Ident); ok && id.Name == "t" {
					continue
				}
				if r := dsnRoot(a); r != "" {
					return r
				}
			}
		}
		return ""
	}

	// mispairedRedactors reports redactor calls that redact against a DSN other than the
	// one the failing call used.
	//
	//   - the call used the ENVIRONMENT's DSN()  -> either redactor is correct.
	//   - the call used any other value          -> SafeDSNErrFor handed THAT value, only.
	mispairedRedactors := func(call *ast.CallExpr, want string) []string {
		var bad []string
		ast.Inspect(call, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := selName(c.Fun)
			if i := strings.LastIndex(name, "."); i >= 0 {
				name = name[i+1:]
			}
			if !redactors[name] {
				return true
			}
			if want == envDSNSentinel {
				return true // the call read the environment; both forms compare it
			}
			if name != dsnRedactor {
				bad = append(bad, name+" (redacts against the environment DSN(), but the "+
					"call used "+want+")")
				return true
			}
			if len(c.Args) == 0 {
				return true
			}
			if got := dsnRoot(c.Args[0]); got != want {
				bad = append(bad, name+" handed "+exprText(fset, c.Args[0])+", but the call used "+want)
			}
			return true
		})
		return bad
	}

	checked := 0
	pairChecked := 0
	explicitPairs := 0
	// Walk every statement that performs a DSN-bearing call, then inspect the error
	// handling that follows it in the same block.
	ast.Inspect(file, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, stmt := range block.List {
			if !isDSNCall(stmt) {
				continue
			}
			// The identifier the call BOUND, so the check below is about that value
			// rather than about what it was named.
			bound := boundErrs(stmt)

			// The handler is the `if err != nil { ... }` (or `if err == nil`) that
			// follows, plus the call statement itself if it is inline.
			var handlers []ast.Node
			if ifs, ok := stmt.(*ast.IfStmt); ok {
				handlers = append(handlers, ifs.Body)
			}
			if i+1 < len(block.List) {
				if ifs, ok := block.List[i+1].(*ast.IfStmt); ok {
					handlers = append(handlers, ifs.Body)
					// `x, err := call(); if err != nil` binds in the assignment above,
					// not in the if -- so the handler's own initializer is folded in
					// only when it is the one holding the DSN call.
					for k := range boundErrs(block.List[i+1]) {
						bound[k] = true
					}
				}
			}
			// A site whose error is discarded (`_`) or never bound has nothing to
			// render raw, so there is nothing to check and nothing to report.
			if len(bound) == 0 {
				continue
			}
			// The DSN this statement's call actually used, read from the call site.
			want := ""
			if arg, ok := dsnArgOf(stmt); ok {
				want = dsnRoot(arg)
			}
			for _, h := range handlers {
				ast.Inspect(h, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					name := selName(call.Fun)
					if name != "t.Fatalf" && name != "t.Errorf" && name != "t.Logf" {
						return true
					}
					checked++
					// Question 2: the redactor is present -- is it aimed at the right
					// secret? `want` is read from the DSN-bearing call itself, so this
					// does not depend on what any parameter is named.
					if want != "" {
						pairChecked++
						if want != envDSNSentinel {
							explicitPairs++
						}
						for _, bad := range mispairedRedactors(call, want) {
							pos := fset.Position(call.Pos())
							shown := want
							if shown == envDSNSentinel {
								shown = "the environment DSN()"
							}
							t.Errorf("line %d redacts against the WRONG DSN: %s\n  %s\nthe "+
								"call used %s, and an explicit DSN differs from the "+
								"environment's (the scratch database name above all), so "+
								"redacting against the other one clears the identifier that "+
								"actually leaks; use dbtest.SafeDSNErrFor(%s, err)",
								pos.Line, bad, exprText(fset, call), shown, shown)
						}
					}
					for _, bare := range bareErrArgs(call, bound) {
						pos := fset.Position(call.Pos())
						t.Errorf("line %d renders a DSN-bearing connect error raw (%s passed "+
							"directly to %s):\n  %s\npgx builds *pgconn.ConnectError and "+
							"*pgconn.ParseConfigError out of the DSN, so this prints the "+
							"configured user, database and host; use dbtest.SafeDSNErr / "+
							"SafeDSNErrFor", pos.Line, bare, name, exprText(fset, call))
					}
					return true
				})
			}
		}
		return true
	})

	// The guard is only evidence if it actually inspected something. A refactor that
	// renames a helper or restructures a handler could silently empty this walk, and a
	// guard that checks nothing passes for the same reason a correct one does.
	if checked == 0 {
		t.Fatal("guard inspected no t.Fatalf/t.Errorf in any connect-error handler; " +
			"the walk no longer matches this file's shape and is not pinning anything")
	}
	// The pairing question has its own self-test for the same reason the first one does:
	// it is answered only inside functions holding an explicit dsn parameter, and a
	// refactor that renames that parameter would empty this walk silently, leaving a
	// guard that passes because it asked nothing.
	if pairChecked == 0 {
		t.Fatal("guard checked no redactor/DSN pairing; no DSN-bearing call's error handler " +
			"was inspected, so the wrong-DSN seam this file exists to pin is not covered")
	}
	// A pairing on a call that read the ENVIRONMENT is trivially satisfied by either
	// redactor, so counting only `pairChecked` would let the EXPLICIT-DSN sites disappear
	// while a sibling kept the counter nonzero. That is not hypothetical: renaming
	// newMigrator's parameter removed it from an earlier, spelling-based version of this
	// guard, and schemaObjects alone held the count up while the regression passed. The
	// explicit sites are the ones the seam lives on, so they are counted separately.
	if explicitPairs == 0 {
		t.Fatal("guard checked no pairing on a call that used an EXPLICIT DSN; every " +
			"inspected site read the environment, so the wrong-DSN seam is not being " +
			"pinned even though the walk found handlers")
	}
	t.Logf("inspected %d formatting calls in connect-error handlers "+
		"(%d redactor/DSN pairings, %d on an explicit DSN)", checked, pairChecked, explicitPairs)
}

// exprText renders an AST node back to source for a failure message.
func exprText(fset *token.FileSet, n ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
