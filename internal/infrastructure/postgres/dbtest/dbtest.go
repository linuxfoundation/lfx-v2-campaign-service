// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package dbtest gives the postgres package a LIVE database to test against.
//
// Every existing test in internal/infrastructure/postgres asserts over SQL *source
// text* — campaign_repo_test.go regexes the ON CONFLICT clauses, stuck_claims_test.go
// regexes the claim query. Those tests are worth having, but they can only check that
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
// Unset, every helper here calls t.Skip. That keeps `go test ./...` working on a laptop
// with no database, which is why the variable is opt-in rather than required — but see
// TestHarnessRunsInCI, which fails if the harness is skipped in an environment that
// promised to provide a database. A harness that silently skips everywhere is
// indistinguishable from no harness at all.
package dbtest

import (
	"context"
	"crypto/rand"
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
// The returned pool is closed by the test framework when the package finishes.
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
		return nil, fmt.Errorf("migrate %s: %w", EnvDatabaseURL, err)
	}
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

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read never returns an error on any platform this builds for, and a
		// fallback here would be a silently-colliding id — the exact defect this function
		// exists to prevent. Fail loudly instead.
		t.Fatalf("dbtest: cannot generate a unique id: %v", err)
	}
	return strings.ToLower(name+"-"+suffix) + "-" + hex.EncodeToString(b[:])
}
