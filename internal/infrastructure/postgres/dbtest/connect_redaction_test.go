// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestConnectAndMigrateWithholdsTheExplicitDSN pins the redaction on the arm of
// connectAndMigrate that a caller can actually reach, and it is a BEHAVIOURAL test: it
// renders the real error from a real failed connection rather than inspecting the source.
//
// The bug it pins is not a missing redactor call — one was already there. It is that the
// arm called SafeDSNErr, which redacts against the ENVIRONMENT's DSN(), while
// connectAndMigrate is handed an EXPLICIT dsn. On the scratch path those differ, because
// the scratch DSN is rewritten from TEST_DATABASE_URL, so the comparison was against the
// wrong string and withheld nothing.
//
// The second half of the trap is which arm of the redactor runs. SafeDSNErr discards a
// *url.Error outright, and the comment on this call claimed that arm applied. It does not:
// golang-migrate wraps pgx's *pgconn.ConnectError, which is NOT a *url.Error, so the
// string arm runs — and the string arm is exactly the one that compares against a DSN.
// Passing the wrong DSN therefore printed "user=... database=..." in full.
//
// The DSN here is deliberately unrelated to any environment value, so a regression that
// reintroduces SafeDSNErr fails this test on a developer laptop and on CI alike.
func TestConnectAndMigrateWithholdsTheExplicitDSN(t *testing.T) {
	t.Parallel()

	const (
		user     = "probeuser"
		password = "probepw"
		database = "probedb"
	)
	// Port 1 is not listenable, so the connection fails fast and deterministically
	// without needing a database. connect_timeout keeps a firewalled runner from
	// stalling on a SYN that is dropped rather than refused.
	dsn := "postgres://" + user + ":" + password + "@127.0.0.1:1/" + database +
		"?sslmode=disable&connect_timeout=1"

	_, err := connectAndMigrate(dsn)
	if err == nil {
		t.Fatal("precondition failed: connecting to a closed port succeeded, so this " +
			"test no longer exercises the failure arm it guards")
	}
	got := err.Error()

	// The control: pgx's UNREDACTED error does name the credential, so the assertion
	// below is not vacuous. Without this, a redactor that returned "" would pass.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, rawErr := pgx.Connect(ctx, dsn)
	if rawErr == nil {
		t.Fatal("precondition failed: the control connect succeeded")
	}
	if raw := rawErr.Error(); !strings.Contains(raw, user) {
		t.Fatalf("precondition failed: the driver's own error did not name %q, so this "+
			"case cannot demonstrate the leak it guards: %s", user, raw)
	}

	for _, id := range []string{user, password, database} {
		if strings.Contains(got, id) {
			t.Errorf("connectAndMigrate = %q, want %q withheld; the arm redacts against "+
				"the EXPLICIT dsn it was handed, not the environment's", got, id)
		}
	}
	// Still diagnosable: withholding everything would also pass the loop above.
	if !strings.Contains(got, EnvDatabaseURL) {
		t.Errorf("connectAndMigrate = %q, want it to still name %s so an operator knows "+
			"which value to check", got, EnvDatabaseURL)
	}
}
