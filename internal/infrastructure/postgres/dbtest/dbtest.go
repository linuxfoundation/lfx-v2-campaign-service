// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package dbtest gives the postgres package a LIVE database to test against.
//
// Every existing test in internal/infrastructure/postgres asserts over SQL *source
// text* — campaign_repo_test.go regexes both the ON CONFLICT clauses and the claim
// query. Those tests are worth having, but they can only check that
// the string still looks the way someone decided it should look. They cannot tell you
// whether PostgreSQL accepts the statement, whether an index the statement depends on
// still exists, or whether a fix actually changed the observable behaviour.
//
// That gap has a cost the review record already shows: the UPDATE ... RETURNING fix on
// the connection repo could not be revert-checked, because reverting it produced source
// text that no assertion here disagreed with. A test that cannot fail when the fix is
// removed is not evidence the fix works.
//
// # Opting in
//
// The value must be a postgres:// or postgresql:// URL. That is not a rule this package
// invents: postgres.Migrate rejects the keyword/value form ("host=h user=u dbname=d"), and
// Pool runs it through connectAndMigrate before any live test body, so a keyword DSN fails
// at the harness gate rather than inside a test. pgx itself accepts both forms -- which is
// why dsnIdentifiersPresent parses both -- but the migration driver does not.
//
// Set TEST_DATABASE_URL to a database this package may FREELY MODIFY:
//
//	TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/campaign_test?sslmode=disable' go test ./...
//
// Ownership of that one database is the whole contract — the cluster-level CREATEDB role is
// deliberately NOT required, so a plain database owner is a conforming setup. One test wants
// more than that: TestLiveMigrationsGoDownAndUpAgain provisions a scratch database per
// migration version (it must run every down file against the schema its own up produced, which
// cannot be done in the shared database). It SKIPS on insufficient_privilege rather than
// failing, so the extra capability is opt-in and its absence is never a red build.
//
// Unset, every helper here calls t.Skip. That keeps `go test ./...` working on a laptop
// with no database, which is why the variable is opt-in rather than required — but see
// verdict: on a CI runner, where a database was promised, Pool FAILS instead of skipping.
// A harness that silently skips everywhere is indistinguishable from no harness at all,
// and a skipped test reports success.
package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
)

// EnvDatabaseURL names the database the live tests run against.
const EnvDatabaseURL = "TEST_DATABASE_URL"

// EnvCI is set by GitHub Actions on every runner. It is what turns a skip from a local
// convenience into a failure — see verdict.
const EnvCI = "CI"

var (
	once     sync.Once
	pool     *pgxpool.Pool
	setupErr error
)

// DSN returns the configured database URL, or "" when the harness is not opted in.
func DSN() string { return strings.TrimSpace(os.Getenv(EnvDatabaseURL)) }

// CleanupTimeout bounds a single teardown step.
//
// 30s matches the pool-open budget in connectAndMigrate: it is long enough for a loaded CI
// runner to answer a DROP that is waiting on another session, and short enough that a wedged
// one is reported rather than waited on.
const CleanupTimeout = 30 * time.Second

// CleanupContext returns a context for teardown work, bounded by CleanupTimeout.
//
// Teardown must not inherit the test's context. By the time t.Cleanup runs the test is over,
// so a caller-derived context may already be cancelled or past its deadline, and the cleanup
// would fail instantly WITHOUT dropping anything -- the failure mode this exists to avoid, and
// the one that looks like a fix while leaving the rows behind. The root here is therefore
// Background: it carries no cancellation to inherit. (Elsewhere in this repo, where teardown
// hangs off a live request context, the same property is spelled context.WithoutCancel(ctx);
// Background is that spelling for work with no parent.)
//
// The DEADLINE is the half this adds. Teardown that connects or drops on an unbounded context
// cannot fail: if Postgres goes unreachable or a forced DROP DATABASE stalls behind another
// session, the cleanup blocks forever, its own t.Errorf is never reached, and the whole
// package waits for the suite-level timeout -- which reports a timeout in whatever test the
// runner happened to be in, naming neither the stall nor the database left behind. With a
// deadline the same stall surfaces as the cleanup's own error, against the right test, and the
// stray database is named.
//
// Callers must defer the returned cancel.
func CleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), CleanupTimeout)
}

// Pool returns a migrated pool against TEST_DATABASE_URL, skipping the test when the
// variable is unset.
//
// The schema is migrated ONCE per package run, not once per test: migrating is the slow
// part, and re-running it per test would push a full suite past any sensible timeout.
// Tests therefore share a schema and MUST NOT share rows — use UniqueID for every
// identifier a test writes, so two tests (or two runs against the same database) cannot
// collide. That is a deliberate trade: isolation by unique key is cheaper than isolation
// by schema, and it is the property the repos are built for anyway, since production
// runs many projects against one schema.
//
// The pool is never explicitly closed. That is deliberate and not an oversight: it is
// shared by every test in the package via sync.Once, so no single test may close it, and
// t.Cleanup on the FIRST caller would tear it down while later tests still hold it. The
// connections are released when the test binary exits, which for a process whose whole
// purpose is to end is the same thing.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := DSN()
	if reason, fatal := verdict(dsn, os.Getenv(EnvCI)); reason != "" {
		if fatal {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}

	once.Do(func() { pool, setupErr = connectAndMigrate(dsn) })
	if setupErr != nil {
		// Fatal, NOT Skip. Once a database was named, failing to reach or migrate it is
		// a broken harness, and a skip would hide exactly the CI misconfiguration this
		// package exists to make visible.
		t.Fatalf("live-database harness: %v", setupErr)
	}
	return pool
}

// verdict decides what an unconfigured harness MEANS, and it lives in its own function so
// the decision can be tested without a database — the one property a test of "the suite must
// not skip here" cannot have if it is written as a live test.
//
// Off CI, an absent database is a local convenience and the answer is skip: `go test ./...`
// has to work on a laptop. On a runner it is a broken build. The failure is raised HERE,
// inside Pool, rather than by a single sentinel test, because a sentinel only protects the
// package it sits in: the next live test written somewhere else would silently skip forever,
// and a skipped test reports success, so nothing would say so.
//
// Returns the message and whether it is fatal; an empty message means run.
func verdict(dsn, ci string) (reason string, fatal bool) {
	switch {
	case dsn != "":
		return "", false
	case ci != "":
		return EnvDatabaseURL + " is empty on a CI runner: the live-database suite would " +
			"skip entirely, reporting success without running. Check the postgres service " +
			"container and the env block in the build workflow.", true
	default:
		return EnvDatabaseURL + " is not set; skipping the live-database test", false
	}
}

func connectAndMigrate(dsn string) (*pgxpool.Pool, error) {
	if err := postgres.Migrate(dsn); err != nil {
		// The DSN is named, never printed -- and the ERROR has to be redacted for that
		// discipline to hold. A plain %w of an UNREDACTED migrate error would put
		// TEST_DATABASE_URL (with its CI `user:password@`) into the build log via Pool's
		// t.Fatalf.
		//
		// There is no SafeDSNErrFor here, and that is the fix for #178 rather than a
		// regression of it: postgres.Migrate now redacts at its own boundary, against the
		// dsn IT was handed, so every error it returns is already safe to print. Redacting
		// a second time here is not belt-and-braces, it is destructive -- the first pass
		// has already discarded the *url.Error (deliberately: see SafeDSNErr), so a second
		// pass can no longer tell the parse failure apart from a connect failure, and
		// reports the wrong one. TestConnectAndMigrateDoesNotEchoTheDSN caught exactly
		// that.
		//
		// The wrong-DSN seam the old comment here warned about -- redacting the ENVIRONMENT's
		// DSN() while holding an explicit, rewritten scratch dsn, which compared against the
		// wrong string and withheld nothing -- is now closed by construction rather than by
		// care at the call site: Migrate is handed one dsn and redacts against that one.
		// The NewPool arm below is now the same shape, for the same reason.
		//
		// The env var's NAME is this package's knowledge, not Migrate's, so it is added
		// here; Migrate's own rendering says "the database DSN" (see
		// dsnSourceDescription).
		return nil, fmt.Errorf("migrate %s: %s", EnvDatabaseURL, err)
	}
	// 30s covers a cold container that has just passed its health check but is still
	// warming, which is the slow case on a CI runner. It bounds the POOL open only;
	// Migrate above is intentionally unbounded, because a migration that hangs is a
	// defect worth seeing as a hung job with a stack, not as a timeout that names the
	// harness instead of the migration.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := postgres.NewPool(ctx, dsn)
	if err != nil {
		// No SafeDSNErrFor here either, and for the same reason as the migrate arm: as of
		// the NewPool sibling fix, NewPool redacts at its own boundary against the dsn IT
		// was handed, so every error it returns is already safe to print. What this arm
		// used to guard was real — NewPool's ping failure wrapped pgx's
		// *pgconn.ConnectError, whose Error() renders "failed to connect to
		// `user=%s database=%s`" straight from the parsed Config, and a %w put the
		// configured user and database into the build log via Pool's t.Fatalf — but it is
		// guarded at the source now.
		//
		// A second pass would be destructive in the same two ways the migrate arm's
		// comment sets out: it can no longer tell NewPool's parse arm (dsnParseError,
		// which keeps its own specific "invalid DATABASE_URL" text) from its connect arm,
		// and an unparseable dsn leaves dsnIdentifiersPresent nothing to compare against,
		// so it withholds an already-safe message unconditionally.
		//
		// NOT pinned by a test, and it should be read that way. Reaching this arm needs a
		// DSN that migrates successfully and THEN fails its ping, but postgres.Migrate
		// connects first, so any unreachable DSN fails above. Changing this line leaves
		// the suite green; the redaction it relies on is pinned in package postgres by
		// TestNewPoolDoesNotLeakIdentifiersWhenThePingFails.
		return nil, fmt.Errorf("open pool %s: %s", EnvDatabaseURL, err)
	}
	// postgres.Pool EMBEDS *pgxpool.Pool; handing back the embedded value keeps this
	// package from re-exporting the instrumented wrapper's surface to tests.
	return p.Pool, nil
}

// UniqueID returns an identifier no other call, in this run or any earlier one, produces.
//
// The shape is <kebab-test-name>-<suffix>-<random>, and both halves are load-bearing:
//
//   - The NAME prefix is for a human. When a constraint fires or a row is left behind,
//     the id in the error message says which test wrote it, and `... LIKE 'testlive%'`
//     finds the wreckage in psql.
//   - The RANDOM tail is for correctness, and the name alone is NOT enough for it. The
//     schema this package migrates carries real uniqueness — campaign_briefs is unique
//     on (project_id, event_slug) for any non-archived row — and the harness deliberately
//     does not drop the database between runs. A purely name-derived id therefore
//     collides with the row the PREVIOUS run inserted, which is not a hypothetical: it
//     failed `go test -count=2` outright, and, worse, it turned a schema revert-check
//     into a duplicate-key error at setup, so the assertion under examination never ran.
//     A test that cannot reach its own assertion twice in a row is not a test.
//
// The randomness comes from crypto/rand rather than a counter or a clock. A counter ties
// the value to execution order, so a -race or t.Parallel reordering writes different rows;
// a clock repeats across two runs inside the same resolution tick. Neither failure is one
// you would diagnose quickly from a 23505.
func UniqueID(t *testing.T, suffix string) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, t.Name())

	// 8 bytes is 64 bits. Every id in a run is drawn against every id in every previous
	// run, and at that width the collision probability stays negligible for far more rows
	// than a test database will ever hold.
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on any platform this builds for, and a
		// fallback here would be a silently-colliding id — the exact defect this function
		// exists to prevent. Fail loudly instead.
		t.Fatalf("dbtest: cannot generate a unique id: %v", err)
	}
	return strings.ToLower(name+"-"+suffix) + "-" + hex.EncodeToString(b[:])
}

// SafeDSNErr renders an error that may carry the DSN into a form safe to print.
//
// The DSN is the one value this package must never let reach a log. Locally it is a
// peer-auth URL with nothing in it, but in CI TEST_DATABASE_URL authenticates over TCP
// with a `user:password@` segment, and CI logs are visible to more people than the secret
// store is. Reporting the env-var NAME instead of its value is the discipline used
// throughout this package -- and on the paths below that discipline is defeated by the
// error itself, which repeats the input it was given.
//
// The implementation is postgres.SafeDSNErr. It moved there when the same leak was found on
// postgres.Migrate's OWN error paths (#178): the reasoning was never test-only -- it is the
// service's no-echo rule, and pkg/redact's package doc is explicit that two sites disagreeing
// about what "redacted" means is the bug that produced it. Every error shape this was built
// against, and every case where it deliberately keeps the driver's text, is documented on
// that function; this file keeps only the convenience of reading the environment.
//
// Exported so the harness in this package and the live tests in dbtest_test share ONE
// implementation.
func SafeDSNErr(err error) string {
	return SafeDSNErrFor(DSN(), err)
}

// SafeDSNErrFor is SafeDSNErr against an EXPLICIT DSN rather than the environment.
//
// It exists because reading the environment is a property of the CALLER's convenience, not
// of the redaction, and baking it in made the helper untestable without t.Setenv. Setenv
// mutates process-global state and Go therefore forbids it in a test with parallel
// ancestors, so every test of this behaviour had to be SERIAL -- one shared, mutable input
// standing between the package and running its redaction tests in parallel.
//
// That serialisation is safe, and it is worth being precise about why, because an earlier
// version of this comment claimed otherwise: `go test` runs top-level parallel tests only
// after every serial one has finished, so a serial Setenv window does NOT overlap the
// parallel tests that read the same variable through DSN(). TestSafeDSNErrReadsTheConfiguredDSN
// relies on exactly that and is correct. The cost of Setenv here is the ordering constraint,
// not a race.
//
// Taking the DSN as an argument removes the shared mutable state from the question
// entirely. The tests pass the value they mean, never touch the environment, and can all
// run in parallel; the harness keeps its one-argument convenience.
func SafeDSNErrFor(dsn string, err error) string {
	return postgres.SafeDSNErr(dsn, EnvDatabaseURL, err)
}

// BlobDigest renders a byte slice for a FAILURE MESSAGE: its length, and a truncated SHA-256.
//
// The tests that call it compare credential ciphertext and, in two places, decrypted plaintext.
// Printing either verbatim is out of the question, and length alone is not enough -- equal
// lengths with different bytes is exactly the failure the assertions exist to catch, since a
// rotation's "ciphertext-v3" is the same length as the "ciphertext-v2" it replaces -- so the
// digest is what makes a failure legible.
//
// The digest is TRUNCATED, and that is the whole reason this is a function rather than
// `sha256.Sum256(b)` written out at each site. A full unsalted SHA-256 next to an exact byte
// length is an offline verification oracle: for a low-entropy secret, anyone holding the log
// line can confirm a guess without touching the service. Against today's fixtures that costs
// nothing -- they are synthetic literals -- but these assertions are the shape the next
// credential test gets copied from, and two of the existing call sites already digest
// PLAINTEXT. Eight bytes is 2^64 of collision resistance, which no two fixtures in this suite
// will ever collide across, and is far too short to confirm a guess against.
//
// Exported so both live-test files share ONE rendering, for the reason SafeDSNErr is exported:
// two sites disagreeing about what "safe to print" means is how the first leak got in.
func BlobDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%d bytes sha256[:8]=%x", len(b), sum[:8])
}
