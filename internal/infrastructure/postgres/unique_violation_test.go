// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUniqueViolationOn pins the helper itself, without a database.
//
// The live witness for this behaviour is TestAudienceLeaseMappingIgnoresOtherUniqueIndexes
// in the dbtest package, and it is the stronger test: it proves a REAL Postgres 23505 from
// a second unique index carries that index's name in ConstraintName, which is the fact the
// whole narrowing rests on and which no synthetic *pgconn.PgError can establish. But that
// test does not always run: dbtest.Pool skips when TEST_DATABASE_URL is unset, and a
// skipped test prints `ok`.
//
// Be exact about where that gap is, because dbtest already closed half of it. Pool's
// verdict (dbtest.go:100-121) splits on $CI: on a runner an unset database URL is a
// t.Fatal, deliberately, so a misconfigured workflow cannot report success. The skip is
// the LOCAL case — `go test ./...` on a laptop, a pre-commit hook, a container without
// the Postgres service and without $CI set. That is where a degraded helper would go
// unnoticed, and it is where most edits to it are first run. This test covers it.
//
// The wrapped case does NOT reproduce the current production path, and saying so is the
// point of stating it. Measured by printing the chain at the call site against a real
// 23505: the helper receives a bare *pgconn.PgError, one layer, unwrapped — all three
// callers hand it the error scanAudience returns, and scanAudience returns row.Scan's
// error unchanged. A version matching with a type assertion instead of errors.As would
// therefore pass today.
//
// It is here because that is a property of today's call sites, not of the helper's
// contract. The helper takes an `error`, and the first caller to add context to one — a
// retry wrapper, a %w on the way out of a transaction — would silently stop matching. The
// case costs one line and pins errors.As as part of the contract rather than an
// implementation detail that happens to be unobservable.
func TestIsUniqueViolationOn(t *testing.T) {
	const want = "uq_campaign_audiences_brief_platform_building"

	pgErr := func(code, constraint string) error {
		return &pgconn.PgError{Code: code, ConstraintName: constraint}
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "the constraint we mean",
			err:  pgErr("23505", want),
			want: true,
		},
		{
			name: "wrapped, which no caller does today but the contract allows",
			err:  fmt.Errorf("create audience: %w", pgErr("23505", want)),
			want: true,
		},
		{
			// The whole point of the narrowing: a future unique index on the same table
			// must not inherit ErrAudienceBuildInFlight.
			name: "a different unique index on the same table",
			err:  pgErr("23505", "uq_some_index_a_later_migration_adds"),
			want: false,
		},
		{
			// Postgres leaves ConstraintName empty for violations it cannot attribute;
			// that must not match a named constraint.
			name: "a 23505 with no constraint name",
			err:  pgErr("23505", ""),
			want: false,
		},
		{
			// A check violation naming the same string is still not a unique violation.
			name: "the right name under the wrong SQLSTATE",
			err:  pgErr("23514", want),
			want: false,
		},
		{
			name: "foreign key violation",
			err:  pgErr("23503", "campaign_audiences_brief_id_project_id_fkey"),
			want: false,
		},
		{
			name: "not a PgError at all",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUniqueViolationOn(tc.err, want); got != tc.want {
				t.Errorf("isUniqueViolationOn(%v, %q) = %v, want %v", tc.err, want, got, tc.want)
			}
		})
	}
}

// TestIsUniqueViolation covers the SIBLING helper, which the narrowing left in place.
//
// `isUniqueViolationOn` is the narrow one — it matches a named constraint. `isUniqueViolation`
// stays load-bearing for the callers that legitimately want ANY 23505, and it detects through a
// different mechanism: an `interface{ SQLState() string }` rather than a `*pgconn.PgError` type
// assertion. Two mechanisms means two ways to regress, and only one of them had a test.
//
// The interface form is the interesting half. It matches anything exposing SQLState, so a driver
// wrapper or a test double satisfies it without being a *pgconn.PgError — which is why the
// non-pg case below uses a bare struct rather than a wrapped nil.
func TestIsUniqueViolation(t *testing.T) {
	tests := map[string]struct {
		err  error
		want bool
	}{
		"a unique violation": {err: &pgconn.PgError{Code: "23505"}, want: true},
		// Any 23505, regardless of which constraint raised it — that breadth is the whole
		// difference from isUniqueViolationOn, and it is deliberate for callers that only need
		// to know a uniqueness rule fired.
		"a unique violation on some other constraint": {
			err:  &pgconn.PgError{Code: "23505", ConstraintName: "uq_something_else"},
			want: true,
		},
		"a different SQLSTATE": {err: &pgconn.PgError{Code: "23503"}, want: false},
		"wrapped": {err: fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505"}), want: true},
		"an error that is not a pg error at all": {err: errors.New("connection refused"), want: false},
		"nil": {err: nil, want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isUniqueViolation(tc.err); got != tc.want {
				t.Errorf("isUniqueViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
