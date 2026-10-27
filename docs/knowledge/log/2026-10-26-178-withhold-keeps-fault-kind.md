# 2026-10-26 — The boundary redaction stops censoring this package's own text (#178)

**Fix** — the local review round on the #178 branch found that the identifier comparison ran
over two kinds of message it was never written for: text this package authored itself, and
PostgreSQL's own DDL prose. Both can name a configured identifier without ever having been
handed the DSN, and both were being replaced wholesale.

The first case is self-censoring. `pgxURL` rejects a DSN golang-migrate cannot consume with
text that names `DATABASE_URL` and the two accepted schemes — a package constant with nothing
of the DSN in it — but it was a `fmt.Errorf` literal, so `redactMigrateErr` put it through
`dsnIdentifiersPresent` like driver prose. The DSN that provokes it is a keyword DSN, and
`pgconn.ParseConfig` reads a keyword DSN's user and database out perfectly well: on
`host=… user=postgres password=… dbname=postgres`, the literal `postgres://` in the message
is bounded by a space and a colon, so it matches `cfg.User` and the message became the
withhold sentinel. The error that says "your DSN is the wrong shape" was withheld exactly
when the DSN was the wrong shape. It is now the sentinel `errDSNNotURLScheme` and passes
through untouched, on exactly the argument `redactMigrateErr` already made for the index
sentinels.

The second case is on the production path. `m.Up()`'s failure is wrapped `apply migrations:
%w`, and the server's text is compared like any other, so a deployment whose database is
named after a table it migrates — `relation "campaigns" already exists` on a database named
`campaigns`, a realistic naming — logged the sentinel, the ticket, and nothing else. The
replacement stays wholesale, for the reason `SafeDSNErr` records, but `dsnFaultKind` now
appends what identifies the fault's KIND: `error_class=`, plus `sqlstate=` for a
`*pgconn.PgError`. Neither field can reproduce an identifier — a class is fixed at compile
time, a SQLSTATE is five characters from a closed set — and the server's message, which is
where the role and database names actually appear, is still dropped. `42P07` plus "withheld"
is a line that can go on a ticket. It is the same split `probeErrAttrs` already makes for the
index probe's warnings.

The classes are matched explicitly with `errors.As` against a three-entry allowlist rather
than printed with `%T`, and that was decided by rendering rather than by taste: `%T` on the
error says `*fmt.wrapError`, since everything reaching here has been wrapped for context, and
`%T` on its root cause says `*errors.joinError`, since pgx joins its per-host dial failures.
Neither names a fault. An error the allowlist does not recognise contributes nothing.

**Verification** — three tests, each mutation-tested and each mutation reverted after it was
observed. Removing `errDSNNotURLScheme` from `redactMigrateErr`'s passthrough failed only
`TestMigrateKeepsItsOwnWrongSchemeRejection`, which reported the shape diagnostic replaced by
the withhold sentinel — the reviewer's reproduction, rendered by the suite. Making
`dsnFaultKind` return the empty string failed exactly the two kind tests and nothing else:
`TestSafeDSNErrCarriesTheFaultKindWhenItWithholds`, which pins `sqlstate=42P07` surviving on
a database named `campaigns` while the word `campaigns` does not, and
`TestNewPoolWithholdCarriesTheConnectClass`, which pins the arm a pod actually meets. The
rendering quoted in `dbtest`'s `connect_redaction_test.go` was re-run against the branch and
updated to carry its new `[error_class=*pgconn.ConnectError]` tail.
