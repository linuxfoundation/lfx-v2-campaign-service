// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgconn"
)

// The three leaks these tests pin were each verified against origin/main before the fix:
// Migrate's error rendered the whole password-bearing DSN on an unparseable URL, and
// user=/database= verbatim on every failure to connect — the second one at startup AND on
// every background retry, which is the one a running pod actually meets.
//
// None of them needs a database: both arms fail before any schema work, the first in the URL
// parser and the second on a refused connection to a closed local port.

// TestMigrateDoesNotLeakTheDSNOnAnUnparseableURL covers the arm where the error repeats the
// input it was given.
//
// The password is chosen so that the malformation IS part of the password: net/url reports
// `invalid URL escape "%zz"`, and "%zz" is a slice of the secret. That is what makes this
// test reject the half-fix as well as the absence of a fix. Unwrapping the *url.Error to its
// cause removes the URL field — the obvious repair, and the one a later refactor is most
// likely to "simplify" back to — but keeps the parser's quoted fragment, so the surviving
// message still carries part of the password and this assertion still fails.
func TestMigrateDoesNotLeakTheDSNOnAnUnparseableURL(t *testing.T) {
	t.Parallel()

	const (
		secret   = "s3cret%zzpw"
		leakUser = "leakuser"
		leakDB   = "leakdb"
	)
	dsn := "postgres://" + leakUser + ":" + secret + "@db.invalid:5432/" + leakDB

	err := Migrate(dsn)
	if err == nil {
		t.Fatal("Migrate accepted an unparseable DSN")
	}
	msg := err.Error()
	for _, forbidden := range []string{secret, "%zz", dsn, leakUser, leakDB, "db.invalid"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("Migrate error reproduces %q: %s", forbidden, msg)
		}
	}
	if !strings.Contains(msg, "does not parse as a URL") {
		t.Errorf("Migrate error does not say the DSN is unparseable: %s", msg)
	}

	// The *url.Error is discarded, not merely undisplayed. Leaving it reachable through
	// errors.As would hand the next caller back the same leak — ue.URL is the raw DSN —
	// and a caller that logs a cause it fished out of the chain is exactly the shape this
	// defect had in the first place.
	var ue *url.Error
	if errors.As(err, &ue) {
		t.Error("the *url.Error is still reachable through errors.As; it carries the raw DSN")
	}
}

// TestMigrateDoesNotLeakIdentifiersWhenTheConnectionFails covers the arm a running pod
// actually meets. The password never appears here — pgx redacts that itself — but the
// username and database name do, in pgx's own "failed to connect to `user=… database=…`"
// template, and by this repo's redaction contract those are half a credential
// (pkg/redact's package doc).
//
// Port 1 on the loopback interface is closed, so this fails fast and reaches no network.
func TestMigrateDoesNotLeakIdentifiersWhenTheConnectionFails(t *testing.T) {
	t.Parallel()

	const (
		secret   = "s3cretpw"
		leakUser = "leakuser"
		leakDB   = "leakdb"
	)
	dsn := "postgres://" + leakUser + ":" + secret + "@127.0.0.1:1/" + leakDB + "?sslmode=disable"

	err := Migrate(dsn)
	if err == nil {
		t.Fatal("Migrate reported success against a closed port")
	}
	msg := err.Error()
	for _, forbidden := range []string{secret, leakUser, leakDB} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("Migrate error reproduces %q: %s", forbidden, msg)
		}
	}
}

// TestRedactedMigrateErrorKeepsItsIdentity pins the half of the contract that is not about
// text. container.go classifies a migration failure with IsPermanentMigrationErr before it
// logs it; if the wrapper broke the chain, a permanent failure would be read as transient and
// the pod would 503-loop on a schema no retry can fix.
func TestRedactedMigrateErrorKeepsItsIdentity(t *testing.T) {
	t.Parallel()

	dsn := "postgres://leakuser:s3cretpw@127.0.0.1:1/leakdb"
	dirty := fmt.Errorf("apply migrations: %w", migrate.ErrDirty{Version: 7})

	got := redactMigrateErr(dsn, dirty)
	if !IsPermanentMigrationErr(got) {
		t.Fatalf("redaction broke the errors.As chain: %v would boot 503 instead of failing fast", got)
	}
}

// TestRedactedMigrateErrorKeepsTheIndexSentinelsReadable pins the deliberate exception. The
// index sentinels carry the remediation an operator has to run — the index names and the
// CREATE INDEX statement — and that text is this package's own, built from the catalog
// rather than from the DSN or the server. Routing it through the identifier comparison would
// withhold the fix from any deployment whose database is named after a table it indexes.
func TestRedactedMigrateErrorKeepsTheIndexSentinelsReadable(t *testing.T) {
	t.Parallel()

	// A DSN whose database name is also a word in the remediation text: the exact shape
	// that would be censored if the sentinels went through the comparison.
	dsn := "postgres://campaigns:s3cretpw@127.0.0.1:1/campaigns"
	const remediation = "REINDEX INDEX CONCURRENTLY campaigns_brief_id_idx"
	sentinel := fmt.Errorf("%w: %s", ErrInvalidIndex, remediation)

	got := redactMigrateErr(dsn, sentinel)
	if !strings.Contains(got.Error(), remediation) {
		t.Errorf("the remediation an operator needs was withheld: %s", got)
	}
	if !errors.Is(got, ErrInvalidIndex) {
		t.Errorf("the sentinel no longer matches errors.Is: %v", got)
	}
}

// TestSafeDSNErrKeepsAMessageThatNamesNothing is the diagnosability half: the redaction must
// not be a blanket. An error that reproduces no configured identifier keeps its text in
// full — "connection refused" and its kin are what an operator actually works from.
func TestSafeDSNErrKeepsAMessageThatNamesNothing(t *testing.T) {
	t.Parallel()

	dsn := "postgres://leakuser:s3cretpw@db.example:5432/leakdb"
	const msg = "server closed the connection unexpectedly"

	if got := SafeDSNErr(dsn, "DATABASE_URL", errors.New(msg)); got != msg {
		t.Errorf("a message naming no configured value was withheld: got %q, want %q", got, msg)
	}
}

// NewPool is Migrate's nearest sibling: same package, same dsn string, and container.go
// calls the two of them seventeen lines apart on the same startup path, wrapping and
// propagating both to a logging caller. Redacting one and not the other would have left the
// leak open on the arm a pod meets MORE often — a database that is not up yet fails the ping
// on every restart, whereas migrations run once.
//
// Like the Migrate tests above, these reach no database: port 1 on the loopback interface is
// closed, so the connection is refused immediately.

// TestNewPoolDoesNotLeakIdentifiersWhenThePingFails pins the arm the probe caught. Before the
// fix the error read "ping database: failed to connect to `user=leakuser database=leakdb`",
// which is the same pgx template, and the same half-credential, that #178 closed in Migrate.
func TestNewPoolDoesNotLeakIdentifiersWhenThePingFails(t *testing.T) {
	t.Parallel()

	const (
		secret   = "s3cretpw"
		leakUser = "leakuser"
		leakDB   = "leakdb"
	)
	dsn := "postgres://" + leakUser + ":" + secret + "@127.0.0.1:1/" + leakDB + "?sslmode=disable"

	pool, err := NewPool(context.Background(), dsn)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool reported success against a closed port")
	}
	msg := err.Error()
	for _, forbidden := range []string{secret, leakUser, leakDB} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("NewPool error reproduces %q: %s", forbidden, msg)
		}
	}
	// The operator still has to be able to tell the two failing arms apart, so the arm
	// label survives the redaction — only the driver's prose is replaced.
	if !strings.Contains(msg, "ping database") {
		t.Errorf("NewPool error no longer says which arm failed: %s", msg)
	}
}

// TestNewPoolKeepsTheConnectErrorReachable is the identity half, matching
// TestRedactedMigrateErrorKeepsItsIdentity. Nothing in the tree classifies this error today,
// but the wrapper is the kind of thing a later caller reaches for, and a redaction that
// silently severs the chain would make that classification wrong rather than impossible.
func TestNewPoolKeepsTheConnectErrorReachable(t *testing.T) {
	t.Parallel()

	dsn := "postgres://leakuser:s3cretpw@127.0.0.1:1/leakdb?sslmode=disable"

	pool, err := NewPool(context.Background(), dsn)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool reported success against a closed port")
	}
	var ce *pgconn.ConnectError
	if !errors.As(err, &ce) {
		t.Errorf("redaction broke the errors.As chain: %v", err)
	}
}

// TestNewPoolDoesNotDoubleRedactTheParseArm pins the arm deliberately left alone. ParseConfig
// failures already return dsnParseError, whose message is static and names DATABASE_URL;
// putting that through the DSN redaction as well would swap a precise diagnostic for the
// generic "does not parse as a URL" and tell the operator less, for no gain — both are
// already DSN-free. This is the regression guard for exactly the tidying-up a later reader is
// most likely to attempt.
func TestNewPoolDoesNotDoubleRedactTheParseArm(t *testing.T) {
	t.Parallel()

	const secret = "s3cret%zzpw"
	dsn := "postgres://leakuser:" + secret + "@db.invalid:5432/leakdb"

	pool, err := NewPool(context.Background(), dsn)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool accepted an unparseable DSN")
	}
	msg := err.Error()
	for _, forbidden := range []string{secret, "%zz", dsn, "leakuser", "leakdb", "db.invalid"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("NewPool error reproduces %q: %s", forbidden, msg)
		}
	}
	if !strings.Contains(msg, "invalid DATABASE_URL") {
		t.Errorf("the parse arm lost its own diagnostic to a second redaction: %s", msg)
	}
}

// The two tests below close the review finding that the boundary redaction also ran over
// text this package authored itself and over the server's own DDL prose — both of which can
// name an identifier without ever having been handed the DSN.

// TestMigrateKeepsItsOwnWrongSchemeRejection pins the self-censoring case. pgxURL's rejection
// is a package constant naming DATABASE_URL and the two schemes it accepts, but the DSN that
// provokes it is a keyword DSN — and on the commonest one of all, user and database are both
// `postgres`, which the literal "postgres://" in that very message matches on a word
// boundary. The error that says "your DSN is the wrong shape" was therefore withheld exactly
// when the DSN was the wrong shape. It reaches no database: pgxURL rejects on the prefix.
func TestMigrateKeepsItsOwnWrongSchemeRejection(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic keyword DSN; no real credential. secretlint-disable-line
	const dsn = "host=db.invalid user=postgres password=s3cretpw dbname=postgres sslmode=disable"

	err := Migrate(dsn)
	if err == nil {
		t.Fatal("Migrate accepted a keyword DSN")
	}
	if !errors.Is(err, errDSNNotURLScheme) {
		t.Fatalf("Migrate no longer reports the wrong-scheme rejection: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "must be a postgres:// or postgresql:// URL") {
		t.Errorf("the shape diagnostic was replaced by the withhold sentinel: %s", msg)
	}
	// The exemption must not become a hole: the text is a constant, so it carries no part
	// of the DSN it was handed, and the password above is the proof.
	if strings.Contains(msg, "s3cretpw") {
		t.Errorf("the exempt rejection reproduces the password: %s", msg)
	}
}

// TestSafeDSNErrCarriesTheFaultKindWhenItWithholds pins the other half: when the comparison
// does withhold, the operator still gets what identifies the fault. A server error naming a
// table that shares the database's name is the realistic production case — the message is
// dropped, but `42P07` is what makes the sentinel actionable, and it cannot carry an
// identifier.
func TestSafeDSNErrCarriesTheFaultKindWhenItWithholds(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic DSN; no real credential. secretlint-disable-line
	const dsn = "postgres://campaigns:s3cretpw@db.invalid:5432/campaigns?sslmode=disable"
	pgErr := &pgconn.PgError{Code: "42P07", Message: `relation "campaigns" already exists`}

	got := SafeDSNErr(dsn, dsnSourceDescription, fmt.Errorf("apply migrations: %w", pgErr))
	if strings.Contains(got, "campaigns") {
		t.Fatalf("the withheld message still names the database: %s", got)
	}
	if !strings.Contains(got, "sqlstate=42P07") {
		t.Errorf("the withhold dropped the SQLSTATE, leaving nothing to diagnose: %s", got)
	}
	if !strings.Contains(got, "error_class=*pgconn.PgError") {
		t.Errorf("the withhold dropped the error class: %s", got)
	}
	// An error this package cannot name contributes nothing — the allowlist declines by
	// staying silent rather than printing a wrapper's type as though it were a fault.
	plain := SafeDSNErr(dsn, dsnSourceDescription, errors.New("dial campaigns: refused"))
	if strings.Contains(plain, "error_class=") || strings.Contains(plain, "sqlstate=") {
		t.Errorf("an unrecognised error was given a kind: %s", plain)
	}
}

// TestNewPoolWithholdCarriesTheConnectClass is the production-path half of the test above:
// the arm a pod actually meets on restart is a refused connection, and its rendering has to
// reach the operator as something more than "withheld". pgx joins its per-host dial failures
// and the ping arm wraps the result, so neither %T on the error nor %T on its root cause
// names the fault — this pins that the explicit class survives both.
func TestNewPoolWithholdCarriesTheConnectClass(t *testing.T) {
	t.Parallel()

	dsn := "postgres://leakuser:s3cretpw@127.0.0.1:1/leakdb?sslmode=disable"
	pool, err := NewPool(context.Background(), dsn)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool reported success against a closed port")
	}
	msg := err.Error()
	if !strings.Contains(msg, "error_class=*pgconn.ConnectError") {
		t.Errorf("the withheld ping failure names no fault class: %s", msg)
	}
	for _, forbidden := range []string{"s3cretpw", "leakuser", "leakdb"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the kind suffix reintroduced %q: %s", forbidden, msg)
		}
	}
}

// TestSafeDSNErrWithholdsAnAtShapedUsername pins the leak the security round found, which the
// two tests above do not reach: the identifier comparison used to run over the REDACTED text
// only, and the redaction itself is what destroys the token.
//
// redact.URLUserinfo is written for URLs. It rewrites everything up to the last pre-query `@`
// as `***@`, so on prose that happens to contain an `@` it eats the start of the line and
// leaves the tail. Azure Database for PostgreSQL issues usernames of the form `user@servername`
// — the single commonest way a legitimate username contains an `@` — so `svc@pgsrv` meeting the
// commonest auth failure of all, 28P01, redacted to `***@pgsrv"`: the SERVER half of the
// username, emitted and logged, with the comparison finding nothing because the token it was
// looking for no longer existed as a token.
//
// The fix compares against the raw text too. Both halves are asserted, because withholding on
// the raw text while still RETURNING the redacted fragment would be the same leak.
func TestSafeDSNErrWithholdsAnAtShapedUsername(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic DSN; no real credential. secretlint-disable-line
	const dsn = "postgres://svc@pgsrv:s3cretpw@db.invalid:5432/appdb?sslmode=disable"
	pgErr := &pgconn.PgError{
		Code:    "28P01",
		Message: `password authentication failed for user "svc@pgsrv"`,
	}

	got := SafeDSNErr(dsn, dsnSourceDescription, fmt.Errorf("ping database: %w", pgErr))
	for _, forbidden := range []string{"svc@pgsrv", "pgsrv", "s3cretpw"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the message reproduces %q: %s", forbidden, got)
		}
	}
	// It must be the WITHHOLD, not merely a message that happens to omit the token: the
	// failing behaviour returned a redacted fragment, which also omits the whole username.
	if !strings.Contains(got, "sqlstate=28P01") {
		t.Errorf("the withhold dropped the SQLSTATE, leaving nothing to diagnose: %s", got)
	}
	if !strings.Contains(got, "withheld") {
		t.Errorf("the message was returned rather than withheld: %s", got)
	}
}

// TestDSNFaultKindRejectsAHostileSQLState pins that the one server-controlled field this
// package does print cannot write log content of its own.
//
// pgErr.Code is filled in by whatever answers on the other end of the socket. It reaches a log
// line through string concatenation, so a value carrying a newline could end the line early and
// forge a second one, and an `=` could forge a key/value pair inside this one. The shape check
// is what stops that; an unrecognisable code degrades to the class alone rather than to silence,
// so an operator still learns the server answered.
func TestDSNFaultKindRejectsAHostileSQLState(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic DSN; no real credential. secretlint-disable-line
	const dsn = "postgres://appuser:s3cretpw@db.invalid:5432/appdb?sslmode=disable"

	for _, code := range []string{
		"42P07\nlevel=info msg=\"all good\"",
		"42P0",
		"42P077",
		"",
		"42p07",
		"4 P07",
	} {
		pgErr := &pgconn.PgError{Code: code, Message: `relation "appdb" already exists`}
		got := SafeDSNErr(dsn, dsnSourceDescription, fmt.Errorf("apply migrations: %w", pgErr))
		if strings.Contains(got, "sqlstate=") {
			t.Errorf("code %q was printed as a SQLSTATE: %s", code, got)
		}
		if !strings.Contains(got, "error_class=*pgconn.PgError") {
			t.Errorf("code %q cost the operator the error class too: %s", code, got)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("code %q put a newline into the rendered line: %q", code, got)
		}
	}

	// The well-formed case still carries, so the check is a shape gate and not a removal.
	ok := SafeDSNErr(dsn, dsnSourceDescription, fmt.Errorf("apply migrations: %w",
		&pgconn.PgError{Code: "42P07", Message: `relation "appdb" already exists`}))
	if !strings.Contains(ok, "sqlstate=42P07") {
		t.Errorf("a valid SQLSTATE was rejected: %s", ok)
	}
}

// TestDSNNotURLSchemeIsReturnedBare pins the property redactMigrateErr's exemption rests on.
//
// The exemption is an errors.Is test, so it covers the whole CHAIN: anything wrapped ABOVE this
// sentinel leaves redactMigrateErr unredacted along with it. For the three index sentinels that
// is deliberate — their wrapping text is the remediation DDL this package composed. For
// errDSNNotURLScheme the argument is the opposite one, that it carries no dynamic text at all,
// and that is a property of its call sites rather than of the sentinel. A future caller who
// wrapped it with a driver or server message would reopen the leak with no other test going
// red, so this asserts the bare return directly.
func TestDSNNotURLSchemeIsReturnedBare(t *testing.T) {
	t.Parallel()

	//nolint:gosec // synthetic keyword DSN; no real credential. secretlint-disable-line
	const dsn = "host=db.invalid user=appuser password=s3cretpw dbname=appdb sslmode=disable"

	err := Migrate(dsn)
	if !errors.Is(err, errDSNNotURLScheme) {
		t.Fatalf("Migrate no longer reports the wrong-scheme rejection: %v", err)
	}
	if err.Error() != errDSNNotURLScheme.Error() {
		t.Fatalf("the sentinel reached redactMigrateErr wrapped with caller text, which the "+
			"errors.Is exemption passes through unredacted:\n got: %s\nwant: %s",
			err.Error(), errDSNNotURLScheme.Error())
	}
}
