// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import "testing"

func TestPgxURL_RewritesURLSchemes(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@host:5432/db":   "pgx5://u:p@host:5432/db",
		"postgresql://u:p@host:5432/db": "pgx5://u:p@host:5432/db",
		"pgx5://u:p@host:5432/db":       "pgx5://u:p@host:5432/db",
	}
	for in, want := range cases {
		got, err := pgxURL(in)
		if err != nil {
			t.Errorf("pgxURL(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPgxURL_RejectsKeywordDSN(t *testing.T) {
	// golang-migrate cannot consume a keyword DSN, so Migrate must reject it
	// rather than pass it through and fail obscurely at driver selection.
	if _, err := pgxURL("host=localhost user=u dbname=d"); err == nil {
		t.Error("pgxURL(keyword DSN) = nil error, want a clear rejection")
	}
}
