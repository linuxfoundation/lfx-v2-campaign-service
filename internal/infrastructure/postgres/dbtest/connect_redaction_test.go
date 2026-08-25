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
// The probe host is therefore one that cannot be a WORKING harness host: an .invalid name
// (RFC 2606 reserves the TLD as permanently non-resolvable). But an unresolvable host only
// rules out a harness that WORKS -- it does not stop TEST_DATABASE_URL from being set to
// anything at all, and it does nothing about the other three compared fields. A harness
// legitimately using the user `probeuser`, or the database `probedb`, still makes a
// regression to SafeDSNErr withhold on that shared field and pass for the wrong reason.
// Verified: with the fix reverted and TEST_DATABASE_URL pointed at a real database owned by
// `probeuser`, this test passed; likewise for a database named `probedb`.
//
// So the environment is PINNED here rather than dodged. t.Setenv puts a value under this
// test's control that shares nothing with the probe, which removes the environment from the
// question entirely instead of arguing about which values it could plausibly hold.
//
// That costs t.Parallel, and costs nothing else: Go runs top-level parallel tests only after
// every serial test has finished, so this test's Setenv window cannot overlap the parallel
// readers of DSN() -- measured at zero overlaps, and the reason
// TestSafeDSNErrReadsTheConfiguredDSN uses the same pattern. Earlier comments in this branch
// claimed such a window was a race; it is not, and that claim has been retired everywhere.
//
// assertUnrelatedDSNKeepsIt below closes the remaining half. The loop above only shows that
// SOMETHING withheld the identifiers; it cannot show WHICH DSN was compared. Rendering the
// same error against an unrelated pinned DSN must NOT withhold it -- if both withhold, the
// first result carries no information about the redactor's argument.
//
// Do NOT remove the t.Setenv below to make this test parallel again. Two superseded
// arguments for that are on the record and both are wrong:
//
//   - "pinning the host is enough" -- it is not. An unresolvable host only rules out a
//     harness that WORKS; the user, password and database are compared too, and a harness
//     legitimately using `probeuser` or `probedb` still made a reverted fix pass.
//   - "a serial Setenv races the parallel readers of DSN()" -- it does not. Go runs
//     top-level parallel tests only after every serial test finishes, measured at zero
//     overlaps. TestSafeDSNErrReadsTheConfiguredDSN relies on the same fact.
//
// So the pin costs this one test's parallelism and buys independence from every possible
// value of TEST_DATABASE_URL. That is the trade, and it is deliberate.
func TestConnectAndMigrateWithholdsTheExplicitDSN(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it, and pinning the environment is what makes this
	// test independent of whatever TEST_DATABASE_URL happens to hold. See the note above
	// on why the serial window is safe rather than a race.
	//
	// Every field here is disjoint from the probe's, so no comparison against the
	// environment can withhold the probe's identifiers by accident.
	t.Setenv(EnvDatabaseURL,
		"postgres://harnessuser:harnesspw@harness.invalid:5432/harnessdb?sslmode=disable") // secretlint-disable-line -- synthetic pinned harness DSN

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
	const unrelatedDSN = "postgres://otheruser:otherpw@other.invalid:5432/otherdb?sslmode=disable" // secretlint-disable-line -- synthetic unrelated DSN; the wrong-DSN seam is invisible without it
	if kept := SafeDSNErrFor(unrelatedDSN, rawErr); !strings.Contains(kept, user) {
		t.Errorf("SafeDSNErrFor(unrelated dsn) = %q, want the probe's text preserved; an "+
			"unrelated DSN must not withhold this error, or the assertions above cannot "+
			"show WHICH dsn the redacting arm was handed", kept)
	}

	// What this last pair checks is that connectAndMigrate WIRES THE REDACTOR IN -- that
	// its output is the redactor's rendering rather than something the wrapper invented.
	// It is deliberately NOT a diagnosability check, and an earlier comment here wrongly
	// called it one.
	//
	// It cannot be one on this input. The control above proves rawErr names `user`, so
	// SafeDSNErrFor necessarily takes the identifier-present branch and returns the fixed
	// sentinel; the underlying fault ("no such host") is dropped with the rest of the
	// text. Rendered, to be sure rather than to assume:
	//
	//   rawErr   = failed to connect to `user=probeuser database=probedb`: hostname
	//              resolving error: lookup dbtest-probe.invalid: no such host
	//   redacted = the driver's message names a value from TEST_DATABASE_URL (it is
	//              withheld: the user, database and host are half of the credential)
	//
	// So any non-empty constant would satisfy the comparison, and asserting "the operator
	// still gets the fault" here would be false. Diagnosability on messages that name NO
	// configured identifier -- connection refused, authentication failed -- is pinned
	// where it can actually be observed: TestSafeDSNErrKeepsDriverTextForNonURLErrors and
	// TestSafeDSNErrDoesNotOverMatchEmbeddedIdentifiers.
	//
	// The empty check still earns its place: a redactor returning "" would make the leak
	// loop above vacuous, and Contains(got, "") is true for every string.
	redacted := SafeDSNErrFor(dsn, rawErr)
	if redacted == "" {
		t.Fatal("SafeDSNErrFor returned an empty rendering; withholding everything is not " +
			"redaction, and would make the leak assertions above vacuous")
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("connectAndMigrate = %q, want it to carry the redactor's rendering %q; "+
			"the arm must emit what SafeDSNErrFor returned rather than a message of its "+
			"own", got, redacted)
	}
}
