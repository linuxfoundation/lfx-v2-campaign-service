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
// Every identifier here must be unrelated to any environment value, and the HOST is the
// one that is easy to get wrong. dsnIdentifiersPresent compares the DSN's password, user,
// database AND host, so a probe on 127.0.0.1 shares a host with a CI TEST_DATABASE_URL on
// 127.0.0.1: the refused-dial text names that host, redaction fires on the host alone, and
// the test passes with the fix reverted -- green for a reason that has nothing to do with
// what it claims to pin. Verified: with SafeDSNErr restored and TEST_DATABASE_URL pointed
// at 127.0.0.1, the 127.0.0.1 version of this test still passed.
//
// 127.0.0.2 is still loopback (nothing listens, so the connect is refused just as fast) but
// is not a host any harness DSN uses, so the assertion turns on the user/password/database
// comparison it is actually about.
func TestConnectAndMigrateWithholdsTheExplicitDSN(t *testing.T) {
	t.Parallel()

	const (
		user     = "probeuser"
		password = "probepw"
		database = "probedb"
		// Not 127.0.0.1: see the note above on sharing a host with TEST_DATABASE_URL.
		probeHost = "127.0.0.2"
	)
	// Port 1 is not listenable, so the connection fails fast and deterministically
	// without needing a database. connect_timeout keeps a firewalled runner from
	// stalling on a SYN that is dropped rather than refused.
	dsn := "postgres://" + user + ":" + password + "@" + probeHost + ":1/" + database +
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
	// Still diagnosable: a redactor that returned "" would pass the loop above. Assert on
	// the REDACTOR's own output rather than on the wrapper's format string -- the wrapper
	// always writes EnvDatabaseURL ("migrate %s: ..."), so asserting that substring is
	// vacuous and would hold even if SafeDSNErrFor returned nothing at all.
	redacted := SafeDSNErrFor(dsn, rawErr)
	if redacted == "" {
		t.Fatal("SafeDSNErrFor returned an empty rendering; withholding everything is not " +
			"redaction, and would make the leak assertions above vacuous")
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("connectAndMigrate = %q, want it to carry the redactor's rendering %q; "+
			"the operator needs the fault, not just the name of the variable", got, redacted)
	}
}
