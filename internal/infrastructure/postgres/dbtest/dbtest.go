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
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/postgres"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
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
		// discipline to hold. postgres.Migrate reaches golang-migrate's database.Open,
		// which parses the URL and returns a *url.Error embedding it in full, so a plain
		// %w here would put TEST_DATABASE_URL (with its CI `user:password@`) into the
		// build log via Pool's t.Fatalf.
		//
		// On THIS path SafeDSNErr takes its *url.Error arm and discards the rendering
		// outright: net/url's causes quote the fragment they choked on, which can be a
		// slice of the credential, so nothing derived from the input is carried. The
		// string redactor is the fallback for causes that are NOT a *url.Error and does
		// not run here.
		return nil, fmt.Errorf("migrate %s: %s", EnvDatabaseURL, SafeDSNErr(err))
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
		return nil, fmt.Errorf("open pool: %w", err)
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
// Two error shapes do it. `url.Parse` fails with a `*url.Error`, whose Error() is
// `fmt.Sprintf("%s %q: %s", Op, URL, Err)` -- the whole raw URL, credentials included.
// `migrate.NewWithSourceInstance` reaches `database.Open`, which parses the URL and wraps
// that same `*url.Error` as "failed to open database: parse %q: ...". Both were verified
// against a password-bearing DSN, and both leaked it in full.
//
// For a *url.Error the cause is DISCARDED, not unwrapped. Unwrapping to `ue.Err` removes
// the URL and is most of the fix, but net/url's causes QUOTE THE FRAGMENT they choked on,
// and that fragment can be part of the credential: `postgres://u:%zz@h/db` yields
// `invalid URL escape "%zz"`, which is the entire password, and a longer password
// containing "%zz" leaks that slice of itself. A message that reproduces any part of an
// unvalidated value cannot honour the no-echo rule, so nothing derived from the input is
// carried. This is the same reasoning, and the same conclusion, as internal/platform/llm's
// proxy-URL constructor.
//
// Nothing a caller could use is lost: net/url's causes are unexported types or plain
// strings, so errors.Is/As reach nothing through them. The operator learns that the DSN
// does not parse and which variable to go and look at, which is the actionable part; the
// specific malformation is not worth a credential.
//
// Errors that are NOT a *url.Error keep their text, since they are driver and connection
// failures whose messages are diagnostic rather than echoes of the input -- but only after
// they are checked against the CONFIGURED DSN's own identifiers, because that premise does
// not hold on its own.
//
// redact.URLUserinfo was the backstop here and it is not sufficient, because it understands
// only URL-shaped `user:pass@` text. A DSN has a second legal shape. pgx accepts KEYWORD/VALUE
// connection strings (`host=h user=u password=p dbname=d`), and its errors are built from that
// shape, so a URL-shaped redactor passes them through untouched. Verified against pgx v5.9.2:
//
//   - `*pgconn.ConnectError` renders `failed to connect to `+"`"+`user=%s database=%s`+"`"+“ from
//     Config fields -- the username, verbatim, on every connection failure.
//   - `*pgconn.ParseConfigError` renders the whole connection string through pgx's own
//     `redactPW`, which masks `password=` and userinfo but KEEPS the username in every branch.
//
// Neither unwraps to a *url.Error, so the arm above never sees them, and both reached the
// URL-shaped fallback and were returned unchanged.
//
// Scrubbing those two templates is the fix this rejects. It is a denylist, and the shape that
// defeats it is already in evidence: the username also arrives from a channel that has nothing
// to do with DSN formatting at all. PostgreSQL itself quotes the role and database in its
// diagnostics -- `role "u" does not exist` (28000), `database "d" does not exist` (3D000),
// `password authentication failed for user "u"` (28P01) -- so the identifier appears in text
// the SERVER generated, which no amount of template-matching on our side can anticipate.
//
// So the test is on VALUES, not on shapes. pgconn.ParseConfig reads both DSN forms into the
// same Config, giving the user, password, database and host actually configured; if the
// rendered error contains any of them, the message is replaced WHOLESALE with a sentinel.
// This is the discipline internal/infrastructure/config's redactDatabaseURL already applies to
// the same value class, for the same stated reason -- a keyword DSN is not safely parseable as
// userinfo, so it is masked rather than picked apart.
//
// The replacement is wholesale rather than surgical because excising the matched bytes is its
// own defect: an identifier may be short or ordinary (`app`, `a`, a database literally named
// `connection`) and cutting it out of the driver's prose corrupts the diagnosis into something
// an operator cannot read -- trading a leak for an unreadable log rather than fixing either.
// A message that must be censored is one this package declines to reproduce.
//
// A message with NO configured identifier in it keeps its text in full, which is what preserves
// diagnosability: `connection refused`, `password authentication failed`, `does not exist` and
// the rest name the fault without naming the credential, and those are the errors an operator
// actually meets. redact.URLUserinfo still runs on that text as the URL-shaped backstop.
//
// When the DSN cannot be parsed at all there are no identifiers to compare against, so no
// message can be PROVEN clean and the sentinel applies unconditionally. That is the safe
// direction for the case where the value is least trustworthy, and it is unreachable in the
// ordinary run: an unparseable DSN fails at Migrate, which is the *url.Error arm above.
//
// Callers pass the error, never the DSN.
//
// It is exported so the harness in this package and the live tests in dbtest_test share ONE
// implementation. Two formatting sites disagreeing about what "redacted" means is the bug
// that produced pkg/redact in the first place.
func SafeDSNErr(err error) string {
	if err == nil {
		return "<nil>"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "the DSN does not parse as a URL (the value and the parser's message are " +
			"withheld: both can carry the credential)"
	}
	return SafeDSNErrFor(DSN(), err)
}

// SafeDSNErrFor is SafeDSNErr against an EXPLICIT DSN rather than the environment.
//
// It exists because reading the environment is a property of the CALLER's convenience, not
// of the redaction, and baking it in made the helper untestable without t.Setenv. Setenv
// mutates process-global state and Go therefore forbids it in a test with parallel
// ancestors, so every test of this behaviour had to be serial -- and, worse, a serial test
// that sets TEST_DATABASE_URL runs concurrently with the package's PARALLEL tests, which
// read the same variable through DSN(). Nothing detects that: the env var is restored by
// Cleanup, so the window is invisible in a green run and would surface as a flake.
//
// Taking the DSN as an argument removes the shared mutable state from the question
// entirely. The tests pass the value they mean and never touch the environment; the harness
// keeps its one-argument convenience.
func SafeDSNErrFor(dsn string, err error) string {
	if err == nil {
		return "<nil>"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return "the DSN does not parse as a URL (the value and the parser's message are " +
			"withheld: both can carry the credential)"
	}
	msg := redact.URLUserinfo(err.Error())
	if dsnIdentifiersPresent(dsn, msg) {
		return "the driver's message names a value from " + EnvDatabaseURL + " (it is " +
			"withheld: the user, database and host are half of the credential)"
	}
	return msg
}

// dsnIdentifiersPresent reports whether msg reproduces any identifier from the configured
// DSN.
//
// The DSN is an ARGUMENT rather than an environment read, so the comparison has no hidden
// input: see SafeDSNErrFor for why the environment version is a caller convenience only.
//
// pgconn.ParseConfig is what makes this shape-independent: it accepts BOTH the URL and the
// keyword/value forms and yields the same Config, so the comparison is against what the DSN
// MEANS rather than against how it was written.
//
// The absent and the unparseable cases are NOT the same, and the difference is deliberate:
//
//   - An UNPARSEABLE DSN reports true -- withhold. A value was configured and carries a
//     credential, but nothing can be extracted to clear a message against, so no message can
//     be proven safe. Failing open here would restore the leak in exactly the case where the
//     configured value is least well understood.
//   - An ABSENT DSN reports false -- keep the text. There is no configured value, so there is
//     no credential for a message to reproduce; withholding would suppress every diagnostic in
//     the package while protecting nothing. This is the laptop case, where every live test
//     skips anyway.
//
// So callers must NOT read this as fail-closed on absence: absence means "nothing to protect",
// not "protect everything".
//
// The empty-string guard is not decoration. A DSN with no password (the local peer-auth case)
// leaves Config.Password empty, and strings.Contains(msg, "") is true for every message, so
// omitting it would withhold every error the package ever prints.
func dsnIdentifiersPresent(dsn, msg string) bool {
	if dsn == "" {
		return false
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return true
	}
	for _, id := range []string{cfg.Password, cfg.User, cfg.Database, cfg.Host} {
		if id != "" && strings.Contains(msg, id) {
			return true
		}
	}
	return false
}
