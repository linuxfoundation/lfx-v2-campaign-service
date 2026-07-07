// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import "testing"

func TestPgxURL(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@host:5432/db":    "pgx5://u:p@host:5432/db",
		"postgresql://u:p@host:5432/db":  "pgx5://u:p@host:5432/db",
		"pgx5://u:p@host:5432/db":        "pgx5://u:p@host:5432/db",
		"host=localhost user=u dbname=d": "host=localhost user=u dbname=d",
	}
	for in, want := range cases {
		if got := pgxURL(in); got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}
