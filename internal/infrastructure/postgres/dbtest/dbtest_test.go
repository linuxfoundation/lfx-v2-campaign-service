// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestVerdict is the guard against a harness that quietly does nothing.
//
// Every live test in this package skips when TEST_DATABASE_URL is unset, which is what
// keeps `go test ./...` working on a laptop with no database. The same property would let
// the whole live suite skip forever in CI after a workflow edit dropped the service
// container or the env block — and a skipped test reports success, so nothing would say so.
//
// This test runs EVERYWHERE, including on the laptop, because verdict takes its two inputs
// as arguments rather than reading the environment. A live test could not check the
// "CI without a database" case at all: that combination is precisely the one in which no
// database exists to run it against.
func TestVerdict(t *testing.T) {
	cases := []struct {
		name      string
		dsn       string
		ci        string
		wantRun   bool
		wantFatal bool
	}{
		{name: "configured off CI runs", dsn: "postgres://x", wantRun: true},
		{name: "configured on CI runs", dsn: "postgres://x", ci: "true", wantRun: true},
		{name: "unconfigured off CI skips", wantRun: false, wantFatal: false},
		{name: "unconfigured on CI fails", ci: "true", wantRun: false, wantFatal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, fatal := verdict(tc.dsn, tc.ci)

			if gotRun := reason == ""; gotRun != tc.wantRun {
				t.Fatalf("run = %v (reason %q), want %v", gotRun, reason, tc.wantRun)
			}
			if fatal != tc.wantFatal {
				t.Fatalf("fatal = %v, want %v — an unconfigured CI runner must FAIL, not skip: "+
					"a skip there is a green build for a suite that never ran", fatal, tc.wantFatal)
			}
			if !tc.wantRun && !strings.Contains(reason, EnvDatabaseURL) {
				t.Errorf("reason = %q, want it to name %s so the fix is obvious from the log",
					reason, EnvDatabaseURL)
			}
		})
	}
}

// TestConnectAndMigrateDoesNotEchoTheDSN pins the harness's own leak path.
//
// Pool reports setupErr with t.Fatalf, so whatever connectAndMigrate returns is printed
// verbatim to the build log. On CI that log is visible to more people than the secret
// store, and TEST_DATABASE_URL there authenticates over TCP with a `user:password@`
// segment — so the returned error is exactly the string that must not carry it.
//
// The leak is reachable through golang-migrate: postgres.Migrate calls database.Open,
// which parses the URL and returns a *url.Error embedding it in full. That is why this
// test drives connectAndMigrate with a MALFORMED DSN — the parse failure is the branch
// that leaks, and it needs no reachable database, so this runs on a laptop with no
// Postgres at all.
func TestConnectAndMigrateDoesNotEchoTheDSN(t *testing.T) {
	const password = "hunter2-not-a-real-pw" // secretlint-disable-line
	const username = "ciuser"

	// Unclosed IPv6 bracket: golang-migrate's url.Parse rejects it, and no connection
	// is ever attempted.
	dsn := "postgres://" + username + ":" + password + "@[::1:5432/campaign_test"

	pool, err := connectAndMigrate(dsn)
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatalf("connectAndMigrate(%s) succeeded against a malformed DSN; this test no "+
			"longer exercises the parse-failure branch it guards", EnvDatabaseURL)
	}

	got := err.Error()
	if strings.Contains(got, password) {
		t.Errorf("connectAndMigrate echoed the password into its error: %q", got)
	}
	if strings.Contains(got, username) {
		t.Errorf("connectAndMigrate echoed the user half of the credential: %q", got)
	}
	// The env-var NAME must survive: an error that redacts away the identity of the
	// misconfigured variable tells an operator nothing about what to fix.
	if !strings.Contains(got, EnvDatabaseURL) {
		t.Errorf("connectAndMigrate = %q, want it to still name %s", got, EnvDatabaseURL)
	}
	// It must still say WHAT failed, so a redactor that returns an empty string or a
	// bare "error" cannot pass.
	if !strings.Contains(got, "does not parse") {
		t.Errorf("connectAndMigrate = %q, want it to still report that the DSN does not "+
			"parse", got)
	}
	// And nothing derived from the input, including the parser's own message: it quotes
	// the fragment it choked on, which can be a slice of the credential. Redacting the
	// rendering alone is credential-SAFE -- so the assertions above cannot see it -- and
	// leaves `***@[::1:5432/campaign_test": missing ']' in host`. This is the widest
	// print path in the package (Pool reports it with t.Fatalf), so it gets the same
	// treatment as every other site.
	for _, frag := range []string{"***", "campaign_test", "[::1", "missing"} {
		if strings.Contains(got, frag) {
			t.Errorf("connectAndMigrate = %q, want no fragment of the input; it echoed %q",
				got, frag)
		}
	}
}

// TestCleanupContextIsBoundedAndUncancelled pins the two properties teardown depends on.
//
// It is a UNIT test of the helper, and that is the honest limit of what can be bound here:
// the failure it guards against -- a teardown blocking forever on an unreachable server or a
// forced DROP waiting behind another session -- needs a wedged Postgres to reproduce, which
// no test can arrange. Reverting CleanupContext to a bare context.Background() leaves the
// whole suite green, verified. So the call sites are pinned at the SOURCE only (every
// t.Cleanup in this package takes its context from here), which is weaker evidence than a
// rendered failure and is not a substitute for one. What this test can do is ensure the
// helper itself keeps the two properties the call sites rely on.
func TestCleanupContextIsBoundedAndUncancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := CleanupContext()
	defer cancel()

	// BOUNDED. Without a deadline the cleanup cannot fail, so its own t.Errorf is
	// unreachable and the package hangs to the suite-level timeout instead.
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("CleanupContext returned a context with no deadline; teardown that connects " +
			"or drops on an unbounded context cannot fail, so a stalled server hangs the " +
			"package instead of reporting the database it left behind")
	}
	if d := time.Until(deadline); d <= 0 || d > CleanupTimeout {
		t.Errorf("CleanupContext deadline is %v away, want (0, %v]", d, CleanupTimeout)
	}

	// NOT ALREADY CANCELLED. Cleanup runs after the test finishes, so a context derived
	// from the test's own would already be done and every teardown statement would fail
	// instantly WITHOUT dropping anything -- the failure that looks like a fix while
	// leaving the rows behind.
	if err := ctx.Err(); err != nil {
		t.Errorf("CleanupContext returned an already-finished context (%v); teardown would "+
			"fail instantly without cleaning up anything", err)
	}
	select {
	case <-ctx.Done():
		t.Error("CleanupContext returned a context that is already Done; it must not inherit " +
			"cancellation from a test context that is over by the time Cleanup runs")
	default:
	}

	// cancel() must release it, or every cleanup leaks a timer for CleanupTimeout.
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("after cancel(), ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}
