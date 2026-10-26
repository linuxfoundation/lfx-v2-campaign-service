// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/redact"
)

// dsnSourceDescription is what this package's own redaction calls the DSN it was handed.
// Both startup boundaries use it: Migrate and NewPool.
//
// It is deliberately a DESCRIPTION rather than an environment variable's name. Both take a
// dsn string and are told nothing about where the caller got it: the service reads
// DATABASE_URL, the live-test harness reads TEST_DATABASE_URL and then REWRITES it for the
// scratch-database path, so any variable name hard-coded here would be a guess, and wrong in
// the harness. A caller that does know names it in its own wrapping — dbtest's
// connectAndMigrate prefixes "migrate TEST_DATABASE_URL: " — which is where that knowledge
// belongs.
const dsnSourceDescription = "the database DSN"

// dsnUnparseableMsg is the single rendering of the *url.Error arm. It is a constant rather
// than a literal at each site so the two cannot drift: two formatting sites disagreeing
// about what "redacted" means is the bug that produced pkg/redact in the first place.
const dsnUnparseableMsg = "the DSN does not parse as a URL (the value and the parser's " +
	"message are withheld: both can carry the credential)"

// SafeDSNErr renders an error that may carry the DSN into a form safe to log.
//
// dsnSource names where the DSN came from, for an operator who has to go and fix it: the
// name of the variable that carries it ("TEST_DATABASE_URL" in the live-test harness), or a
// description where the caller does not know the variable (dsnSourceDescription, used by
// this package's own boundary redaction). It is only ever the NAME or the description; the VALUE
// is the one thing this function exists to keep out of the log.
//
// Two error shapes defeat the report-the-name-not-the-value discipline by repeating the
// input they were given. `url.Parse` fails with a `*url.Error`, whose Error() is
// `fmt.Sprintf("%s %q: %s", Op, URL, Err)` — the whole raw URL, credentials included.
// `migrate.NewWithSourceInstance` reaches `database.Open`, which parses the URL and wraps
// that same `*url.Error` as "failed to open database: parse %q: ...". Both were verified
// against a password-bearing DSN, and both leaked it in full.
//
// For a *url.Error the cause is DISCARDED, not unwrapped. Unwrapping to `ue.Err` removes
// the URL and is most of the fix, but net/url's causes QUOTE THE FRAGMENT they choked on,
// and that fragment can be part of the credential: `postgres://u:%zz@h/db` yields
// `invalid URL escape "%zz"`, which is the entire password, and a longer password
// containing "%zz" leaks that slice of itself. A message that reproduces any part of an
// unvalidated value cannot honour the no-echo rule, so nothing derived from the input is
// carried. This is the same reasoning, and the same conclusion, as internal/platform/llm's
// proxy-URL constructor.
//
// Nothing a caller could use is lost: net/url's causes are unexported types or plain
// strings, so errors.Is/As reach nothing through them. The operator learns that the DSN
// does not parse and which variable to go and look at, which is the actionable part; the
// specific malformation is not worth a credential.
//
// Errors that are NOT a *url.Error keep their text, since they are driver and connection
// failures whose messages are diagnostic rather than echoes of the input — but only after
// they are checked against the CONFIGURED DSN's own identifiers, because that premise does
// not hold on its own.
//
// redact.URLUserinfo was the backstop here and it is not sufficient, because it understands
// only URL-shaped `user:pass@` text. A DSN has a second legal shape. pgx accepts KEYWORD/VALUE
// connection strings (`host=h user=u password=p dbname=d`), and its errors are built from that
// shape, so a URL-shaped redactor passes them through untouched. Verified against pgx v5.9.2:
//
//   - `*pgconn.ConnectError` renders a "failed to connect to ..." message embedding a
//     user=%s database=%s fragment built from Config fields — the username, verbatim, on
//     every connection failure.
//   - `*pgconn.ParseConfigError` renders the whole connection string through pgx's own
//     `redactPW`, which masks `password=` and userinfo but KEEPS the username in every branch.
//
// Neither unwraps to a *url.Error, so the arm above never sees them, and both reached the
// URL-shaped fallback and were returned unchanged.
//
// Scrubbing those two templates is the fix this rejects. It is a denylist, and the shape that
// defeats it is already in evidence: the username also arrives from a channel that has nothing
// to do with DSN formatting at all. PostgreSQL itself quotes the role and database in its
// diagnostics — `role "u" does not exist` (28000), `database "d" does not exist` (3D000),
// `password authentication failed for user "u"` (28P01) — so the identifier appears in text
// the SERVER generated, which no amount of template-matching on our side can anticipate.
//
// So the test is on VALUES, not on shapes. pgconn.ParseConfig reads both DSN forms into the
// same Config, giving the user, password, database and host actually configured; if the
// rendered error contains any of them, the message is replaced WHOLESALE with a sentinel,
// to which dsnFaultKind appends the fault's kind — the Go type and any SQLSTATE, never a
// byte of the message itself.
//
// The replacement is wholesale rather than surgical because excising the matched bytes is its
// own defect: an identifier may be short or ordinary (`app`, `a`, a database literally named
// `connection`) and cutting it out of the driver's prose corrupts the diagnosis into something
// an operator cannot read — trading a leak for an unreadable log rather than fixing either.
// A message that must be censored is one this package declines to reproduce. The kind is the
// part that survives precisely because it is not derived from the text; see dsnFaultKind.
//
// A message with NO configured identifier in it keeps its text in full, which is what preserves
// diagnosability: `connection refused`, `password authentication failed`, `does not exist` and
// the rest name the fault, and those are the errors an operator actually meets.
//
// The condition is the identifier, never the error class. Those same messages OFTEN do name a
// configured value — `password authentication failed for user "x"`, pgx's
// "failed to connect to `user=%s database=%s`" — and are withheld when they do. Nothing here
// treats a class as inherently safe; the comparison decides every message on its own text.
//
// redact.URLUserinfo still runs as the URL-shaped backstop, but the comparison is made against
// the RAW text as well as the redacted text, and that order matters. URLUserinfo is written for
// URLs, not prose: it rewrites everything up to the last pre-query `@` as `***@` and truncates at
// a `?`. An identifier containing either character therefore survives the redaction only in
// fragments, and comparing against that output alone misses it. Azure Database for PostgreSQL
// hands out usernames of the form `user@servername`, so a cfg.User of `svc@pgsrv` meeting
// `password authentication failed for user "svc@pgsrv"` (28P01) redacts to `***@pgsrv"` — the
// server half intact, the token no longer matchable. Testing the raw text closes that: the
// identifier is matched before anything has had a chance to break it up. The redacted text is
// still tested too, for the converse case where redaction leaves the identifier intact.
//
// When the DSN cannot be parsed at all there are no identifiers to compare against, so no
// message can be PROVEN clean and the sentinel applies unconditionally. That is the safe
// direction for the case where the value is least trustworthy.
//
// Callers pass the error, never the DSN.
func SafeDSNErr(dsn, dsnSource string, err error) string {
	if err == nil {
		return "<nil>"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return dsnUnparseableMsg
	}
	raw := err.Error()
	msg := redact.URLUserinfo(raw)
	if dsnIdentifiersPresent(dsn, raw) || dsnIdentifiersPresent(dsn, msg) {
		return "the driver's message names a value from " + dsnSource + " (it is " +
			"withheld: the user, database and host are half of the credential)" +
			dsnFaultKind(err)
	}
	return msg
}

// dsnFaultKind renders what identifies a withheld error's KIND, for appending to the
// sentinel above.
//
// It carries the Go type and, for a server error, the SQLSTATE — and nothing else. Neither
// can reproduce a DSN identifier: the type name is fixed at compile time, and a SQLSTATE is
// five characters from a closed set the standard defines. The server's MESSAGE, which is
// where the role and database names actually appear, is still dropped.
//
// This is the split probeErrAttrs already makes for the index probe's warnings, and it is
// here for the reason that one exists: the wholesale replacement is deliberate, but a bare
// sentinel leaves an operator with nothing to act on. A failing migration on a deployment
// whose database is named after a table it migrates — `relation "campaigns" already exists`
// on a database named `campaigns`, a realistic naming — otherwise rendered as the sentinel
// alone. `sqlstate=42P07` is a line that can go on a ticket; "withheld" is not.
//
// The classes are named EXPLICITLY, by errors.As, rather than printed with %T off err or
// off its root cause, and that is not fussiness — both of those were tried and rendered.
// Every error reaching here has been wrapped for context ("apply migrations: %w",
// "ping database: %w"), so %T on err itself says *fmt.wrapError; unwrapping to the end
// instead lands on *errors.joinError, because pgx joins its per-host dial failures. Neither
// names a fault. probeErrAttrs can use %T because it is handed the step's error unwrapped,
// at the point of failure.
//
// An unrecognised error therefore contributes NOTHING, which is the right default for an
// allowlist: this function may only add text whose provenance it can vouch for, and saying
// nothing is how it declines.
func dsnFaultKind(err error) string {
	var (
		pgErr    *pgconn.PgError
		connErr  *pgconn.ConnectError
		parseErr *pgconn.ParseConfigError
	)
	switch {
	case errors.As(err, &pgErr):
		// The server answered and named a condition; the SQLSTATE is that condition's
		// code, and the only field of a PgError that cannot quote an identifier.
		//
		// It is still a field the SERVER filled in, and this string goes into a log line,
		// so it is checked for shape before it is concatenated rather than trusted to be
		// what the standard says. A five-character alphanumeric code cannot carry a
		// newline or an `=`, and so cannot end the line early or forge a second key/value
		// pair in it. Anything else contributes the class alone — the same way an
		// unrecognised error contributes nothing.
		if isSQLState(pgErr.Code) {
			return " [error_class=*pgconn.PgError sqlstate=" + pgErr.Code + "]"
		}
		return " [error_class=*pgconn.PgError]"
	case errors.As(err, &connErr):
		return " [error_class=*pgconn.ConnectError]"
	case errors.As(err, &parseErr):
		return " [error_class=*pgconn.ParseConfigError]"
	}
	return ""
}

// isSQLState reports whether code has the shape the SQL standard gives a SQLSTATE: exactly five
// characters, each a digit or an uppercase letter.
//
// It is a shape check, not a membership check. The point is not to decide whether a code is one
// PostgreSQL defines — servers add their own, and extensions add more — but to establish that
// the value cannot do anything but name one. A string of this shape has no newline to end a log
// line early, no `=` to forge a second key/value pair in it, no space, and no quote; so a
// hostile or compromised server cannot use the field to write log content of its own choosing.
//
// It is written out rather than compiled as a regexp because it runs on an error path, where a
// package-level MustCompile is one more thing that can fail at init for a five-character check.
func isSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for i := range len(code) {
		c := code[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// dsnIdentifiersPresent reports whether msg reproduces any identifier from the configured
// DSN.
//
// pgconn.ParseConfig is what makes this shape-independent: it accepts BOTH the URL and the
// keyword/value forms and yields the same Config, so the comparison is against what the DSN
// MEANS rather than against how it was written.
//
// The absent and the unparseable cases are NOT the same, and the difference is deliberate:
//
//   - An UNPARSEABLE DSN reports true — withhold. A value was configured and carries a
//     credential, but nothing can be extracted to clear a message against, so no message can
//     be proven safe. Failing open here would restore the leak in exactly the case where the
//     configured value is least well understood.
//   - An ABSENT DSN reports false — keep the text. There is no configured value, so there is
//     no credential for a message to reproduce; withholding would suppress every diagnostic
//     while protecting nothing.
//
// So callers must NOT read this as fail-closed on absence: absence means "nothing to protect",
// not "protect everything".
//
// The empty-string guard is not decoration. A DSN with no password (the local peer-auth case)
// leaves Config.Password empty, and strings.Contains(msg, "") is true for every message, so
// omitting it would withhold every error this package ever prints.
//
// The comparison is on WORD BOUNDARIES rather than on raw substrings, and that distinction is
// load-bearing. A deployment whose user and database are both `postgres` makes "postgres" a
// substring of "postgresql" — so a raw strings.Contains matches ordinary driver prose that
// merely names the protocol ("unsupported postgresql wire protocol version 2", "lookup
// postgresql.svc failed") and replaces it with the withhold sentinel. That is a real cost,
// not a cosmetic one: it fires on generic short identifiers (postgres, test, db, root are all
// common), and it destroys the message in the one configuration most likely to be running.
//
// A credential is reproduced when the message names it as a VALUE — `user=postgres`,
// role "postgres", database "postgres" — and in every such form the identifier is bounded by
// a non-word character (=, ", space, backtick, end of string). An embedding inside a longer
// word ("postgresql") is a different token and reproduces nothing.
//
// The match is case-insensitive because Postgres folds unquoted identifiers to lower case,
// so a DSN written `user=Postgres` reaches the driver's prose as `postgres`; comparing
// case-sensitively would miss that echo.
func dsnIdentifiersPresent(dsn, msg string) bool {
	if dsn == "" {
		return false
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return true
	}
	ids := []string{cfg.Password, cfg.User, cfg.Database, cfg.Host}
	// A multi-host DSN parses its SECONDARY hosts into Fallbacks, and cfg.Host holds only
	// the first. pgx dials each in turn and names the one that failed — with
	// originalHostname, so the message carries the host as WRITTEN in the DSN. Comparing
	// against cfg.Host alone therefore cleared any error naming only a fallback, and it
	// printed in full: verified, a DSN of primary/secondary/third against
	// `failed to connect to host "secondary.invalid"` returned that text verbatim.
	//
	// Fallbacks also carry the per-host User/Password/Database, but those are copied from
	// the top-level config for every entry, so the host is the only field that adds
	// anything. It is included per fallback anyway rather than assumed: the cost is a
	// string compare and the assumption is pgx's to change.
	for _, fb := range cfg.Fallbacks {
		ids = append(ids, fb.Host)
	}
	for _, id := range ids {
		if id != "" && namesIdentifier(msg, id) {
			return true
		}
	}
	return false
}

// namesIdentifier reports whether msg reproduces id as a whole token rather than as a
// fragment of a longer word.
//
// Written as an explicit scan rather than a regexp because the identifier is arbitrary
// operator-supplied text: building a pattern from it means escaping it correctly on every
// call, and a DSN field is exactly the kind of value that contains regexp metacharacters.
// A scan has no such failure mode and does not recompile per error.
func namesIdentifier(msg, id string) bool {
	lowMsg, lowID := strings.ToLower(msg), strings.ToLower(id)
	for i := 0; ; {
		j := strings.Index(lowMsg[i:], lowID)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(lowID)
		if !isWordByte(lowMsg, start-1) && !isWordByte(lowMsg, end) {
			return true
		}
		// Advance one byte, not one match: overlapping occurrences must all be tried, or
		// a bounded echo sitting immediately after an embedded one would be missed.
		i = start + 1
		if i >= len(lowMsg) {
			return false
		}
	}
}

// isWordByte reports whether the byte at position i is part of an identifier token.
// Out-of-range positions are NOT word bytes, which is what makes a match at the very start
// or end of the message count as bounded.
func isWordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// safeDSNError is the error this package's DSN-taking boundaries return — Migrate and
// NewPool: a message that has been through SafeDSNErr, with the original retained for
// errors.Is/As and NEVER for display.
//
// Splitting those two is what lets the redaction be total without costing the caller
// anything. container.go classifies a migration failure with IsPermanentMigrationErr
// (errors.As for migrate.ErrDirty, errors.Is for the index sentinels) and then logs the
// error; the classification walks Unwrap and is unaffected, while every format verb
// reaches Error() and sees only the redacted text.
//
// cause is nil on the *url.Error arm, so there the original is not merely undisplayed but
// unreachable: SafeDSNErr discards it rather than unwrapping it (see why there), and
// leaving it retrievable through errors.As would hand the next caller the same leak this
// type exists to close.
type safeDSNError struct {
	msg   string
	cause error
}

func (e *safeDSNError) Error() string { return e.msg }
func (e *safeDSNError) Unwrap() error { return e.cause }

// redactMigrateErr renders a Migrate failure safe to log while preserving its identity.
//
// Our OWN sentinels pass through untouched. ErrInvalidIndex, ErrMissingRequiredIndex and
// ErrRequiredIndexMismatch are text this package constructs from the catalog — index names
// and the CREATE INDEX statement an operator has to run — not driver prose and not a server
// message, so the no-echo rule has nothing to say about them. Routing them through the
// identifier comparison would withhold the remediation DDL from any deployment whose
// database happens to be named after a table it indexes, which is a realistic naming and
// would trade a leak that is not there for an unrunnable fix. The split is safe because the
// connect failures that CAN carry identifiers are deliberately not wrapped in these
// sentinels: checkNoInvalidIndexes keeps them retryable and unwrapped.
//
// errDSNNotURLScheme is exempt on exactly that argument, and it was missed when the
// argument was first written: it is package-authored constant text that cannot carry a
// credential, and its own comment records the self-censoring it was losing.
//
// What every one of those exemptions actually rests on, and the thing to check before adding
// a fourth: the test is errors.Is, so an exemption covers the WHOLE CHAIN, not just the
// sentinel at its root. Wrap an exempt sentinel in text and that text leaves here unredacted
// too. For the three index sentinels that is intended and is why they exist — the wrapping
// text IS the remediation DDL this package composed. errDSNNotURLScheme is the opposite case:
// its argument is that it carries no dynamic text at all, so it is returned bare at every
// site, with no %w above it. That is a property of the call sites rather than of the sentinel,
// which is why TestDSNNotURLSchemeIsReturnedBare pins it: a future caller who wraps it with a
// driver or server message would reopen the leak here with no other test going red.
func redactMigrateErr(dsn string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrInvalidIndex) ||
		errors.Is(err, ErrMissingRequiredIndex) ||
		errors.Is(err, ErrRequiredIndexMismatch) ||
		errors.Is(err, errDSNNotURLScheme) {
		return err
	}
	return newSafeDSNError(dsn, err)
}

// newSafeDSNError wraps err so that every format verb sees only redacted text while
// errors.Is/As still reach the original.
//
// It is shared by Migrate and NewPool rather than written out at each site for the reason
// dsnUnparseableMsg is a constant: two boundaries in one package disagreeing about what
// "redacted" means is how the first leak got in. The *url.Error arm discards the cause
// instead of retaining it — leaving it reachable through errors.As would hand the next
// caller the raw URL this wrapper exists to withhold.
//
// It does NOT classify sentinels; that judgement belongs to each boundary. Migrate has
// index sentinels of its own that must pass through untouched, and NewPool has none.
func newSafeDSNError(dsn string, err error) error {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = nil
	}
	return &safeDSNError{msg: SafeDSNErr(dsn, dsnSourceDescription, err), cause: cause}
}
