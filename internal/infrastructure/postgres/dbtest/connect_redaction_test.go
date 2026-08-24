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
// database AND host, so if the probe shares a host with TEST_DATABASE_URL the refused-dial
// text names that host, redaction fires on the host ALONE, and the test passes with the fix
// reverted -- green for a reason that has nothing to do with what it claims to pin.
//
// Choosing an unusual loopback address does not settle that, and an earlier version of this
// comment wrongly claimed it did: 127.0.0.2 is only a host no harness is EXPECTED to use.
// The harness contract constrains TEST_DATABASE_URL no further than "a database this package
// may freely modify", so a developer may legitimately point it at that address and the
// wrong-reason pass returns silently.
//
// The probe host is therefore one that CANNOT also be the harness's. A DSN whose host is an
// .invalid name (RFC 2606 reserves the TLD as permanently non-resolvable) can never be a
// working TEST_DATABASE_URL, because a database no one can resolve is not a database any
// live test could have run against. So the host comparison cannot fire on a shared value --
// no valid harness DSN shares this host -- and the assertion necessarily turns on the
// user/password/database comparison it is about.
//
// assertUnrelatedDSNKeepsIt below closes the remaining half. The loop above only shows that
// SOMETHING withheld the identifiers; it cannot show WHICH DSN was compared. Rendering the
// same error against an unrelated pinned DSN must NOT withhold it -- if both withhold, the
// first result carries no information about the redactor's argument.
//
// The residual case, stated rather than glossed: if TEST_DATABASE_URL were set to this
// probe's OWN identifiers, the environment comparison would withhold and the mutation would
// survive again. That value cannot arise from a working harness -- an .invalid host does not
// resolve, so no live test in this package could ever have run against it -- but it is a
// limit of the instrument, not something the instrument rules out. Verified by mutation:
// with SafeDSNErr restored, this test fails under an unset TEST_DATABASE_URL, under CI's
// 127.0.0.1 value, and under a 127.0.0.2 value; it passes only under that self-referential
// DSN.
//
// Serializing the test and calling t.Setenv is the wrong remedy for the same reason
// SafeDSNErrFor exists: taking the DSN as an argument is what let these tests stop depending
// on process-global state, and a serial test that writes TEST_DATABASE_URL still races the
// package's PARALLEL tests, which read it through DSN().
func TestConnectAndMigrateWithholdsTheExplicitDSN(t *testing.T) {
	t.Parallel()

	const (
		user     = "probeuser"
		password = "probepw"
		database = "probedb"
		// An .invalid host, not a loopback address: see the note above. RFC 2606
		// reserves this TLD as permanently non-resolvable, so no working
		// TEST_DATABASE_URL can share it and the host comparison cannot fire on a
		// value the environment also holds.
		probeHost = "dbtest-probe.invalid"
	)
	// The name does not resolve, so the connect fails fast and deterministically without
	// needing a database or a listenable port. connect_timeout bounds a resolver that
	// stalls rather than answering NXDOMAIN.
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
	// The control must show the UNREDACTED error really does name the credential,
	// otherwise the withhold assertions below are vacuous. pgx builds
	// *pgconn.ConnectError out of the parsed Config, so it names user and database even
	// when the failure was DNS rather than a refused dial.
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

	// Which DSN did the redactor compare against? The loop above cannot say: it shows
	// only that the identifiers are absent. Rendering the SAME error against an
	// unrelated pinned DSN must PRESERVE it -- if an unrelated DSN withheld it too, then
	// withholding says nothing about the argument, and the wrong-DSN seam this test
	// exists to pin would still be invisible.
	const unrelatedDSN = "postgres://otheruser:otherpw@other.invalid:5432/otherdb?sslmode=disable"
	if kept := SafeDSNErrFor(unrelatedDSN, rawErr); !strings.Contains(kept, user) {
		t.Errorf("SafeDSNErrFor(unrelated dsn) = %q, want the probe's text preserved; an "+
			"unrelated DSN must not withhold this error, or the assertions above cannot "+
			"show WHICH dsn the redacting arm was handed", kept)
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
