# 2026-10-26 — `postgres.Migrate` redacts its own errors (#178)

**Fix** — `postgres.Migrate` now returns only redacted errors. Two sites inside it leaked
the DSN, not one: a malformed URL reaches `migrate.NewWithSourceInstance` → `database.Open`
and renders the whole DSN, password included, through `*url.Error`; and every failure to
CONNECT renders pgx's ``failed to connect to `user=%s database=%s` `` — from `Up()` and from
`checkNoInvalidIndexes` alike, which wrapped its `pgxv5.Connect` failure bare.

The redaction is at the exported boundary rather than at each failing line. `Migrate` is
split into a wrapper and `runMigrations` (the original body, unchanged), and the wrapper
passes every error through `redactMigrateErr`. A site-by-site fix leaves the next site
unguarded, and the second site had already proved that happens.

The implementation moved out of `dbtest` into `internal/infrastructure/postgres/dsnredact.go`
— the reasoning was never test-only — and `dbtest`'s `SafeDSNErr`/`SafeDSNErrFor` are now
delegations. It did not go into `pkg/redact`, whose one-implementation rule it is otherwise
obeying: it needs `pgconn.ParseConfig` to learn the configured identifiers, and `pkg/redact`
imports only `net/url` and `strings`.

Three properties the fix had to keep, each pinned by a test:

- `errors.Is`/`As` still see through the wrapper (`safeDSNError.Unwrap`), because
  `IsPermanentMigrationErr` depends on `errors.As(err, &migrate.ErrDirty)`.
- The three index sentinels pass through unredacted, so the remediation text naming an index
  survives. Safe because the connect failures that CAN carry identifiers are deliberately
  not wrapped in those sentinels.
- For a `*url.Error` the cause is DISCARDED, not unwrapped: `net/url`'s causes quote the
  fragment they choked on, which can be a slice of the password.

**Note** — two corrections on the record. #178's reachability claim is wrong for the password
arm: `ValidateMigrationDSN` (`container.go:477`) rejects every parse-failure shape before
boot reaches `Migrate`, and a 14-shape probe found nothing that clears it and then hits a
`*url.Error`. The production-reachable leak is the username-and-database echo, at startup and
on every background retry. And boundary redaction double-redacted in `dbtest`'s
`connectAndMigrate`, which was already redacting — destructively, since the first pass has
discarded the `*url.Error`. That caller now passes the error through; the `NewPool` arm below
it keeps its own redaction, which nothing stands behind.
