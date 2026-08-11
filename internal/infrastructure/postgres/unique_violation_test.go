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
// test SKIPS whenever TEST_DATABASE_URL is unset — including on a developer's machine and
// in any CI job without the Postgres service — and a skipped test prints `ok`. This one
// runs everywhere, so degrading the helper cannot reach main through a green-looking run
// that exercised nothing.
//
// The wrapped case is not decoration. Every production caller reaches this helper with an
// error that pgx has already wrapped at least once, so a version matching on the concrete
// type rather than via errors.As would pass a bare-value test and fail on every real row.
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
			name: "wrapped, which is how every caller actually sees it",
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
