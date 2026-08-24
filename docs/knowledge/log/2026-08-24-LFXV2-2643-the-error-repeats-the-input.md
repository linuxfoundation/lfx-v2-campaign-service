# 2026-08-24 — LFXV2-2643 the error repeats the input it was given

**Fix** — nine sites in the `dbtest` package formatted an error derived from
`TEST_DATABASE_URL` with `%v`/`%w`, and on a malformed DSN each printed the
whole URL — `user:password@` included — into the test log.

The discipline the package already followed was to report the env-var NAME and
never its value, and every one of these sites obeyed it in its format string:

    t.Fatalf("parse %s: %v", dbtest.EnvDatabaseURL, err)

The name is what appears; the value does not. What defeated it is that the
ERROR repeats the input it was given. `url.Parse` fails with a `*url.Error`,
whose `Error()` is `fmt.Sprintf("%s %q: %s", Op, URL, Err)` — so the `%v`
expands to the entire raw URL regardless of what the rest of the line says.
Verified against a password-bearing DSN in all four `net/url` failure modes
(invalid percent-escape, invalid port, unclosed IPv6 bracket, control
character): every one embedded the credential in full.

The second shape is the same defect one layer down, and it is the one a grep
for `url.Parse` does not find. `migrate.NewWithSourceInstance` reaches
`database.Open`, which parses the URL itself and returns that `*url.Error`
wrapped as `failed to open database: parse %q: ...`. So
`postgres.Migrate(dsn)` — which names no URL in any format string of ours —
returns an error carrying the whole DSN, and the six `postgres.Migrate` call
sites in `invalid_index_live_test.go` plus the harness wrap in `dbtest.go`
rendered it. That last one is the widest: `Pool` reports it with `t.Fatalf`, so
a CI runner with a malformed `TEST_DATABASE_URL` writes the credential to the
build log on every live test in the package.

The fix DISCARDS the cause rather than unwrapping it, and the reason is the
third finding, which arrived on review of the fix itself. Unwrapping removes the
URL and is most of the fix — but net/url's causes QUOTE THE FRAGMENT they choked
on, and that fragment can be part of the credential. `postgres://u:%zz@h/db`
yields `invalid URL escape "%zz"`: no URL, and still the entire password. A
longer password containing `%zz` leaks that slice of itself. So:

    var ue *url.Error
    if errors.As(err, &ue) {
        return "the DSN does not parse as a URL (the value and the parser's " +
            "message are withheld: both can carry the credential)"
    }
    return redact.URLUserinfo(err.Error())

That is the same conclusion `internal/platform/llm`'s proxy-URL constructor
already reached, in the same words: a message that reproduces any part of an
unvalidated value cannot honour a no-echo rule, so nothing derived from the
input is carried. Nothing a caller could use is lost — net/url's causes are
unexported types or plain strings, so `errors.Is/As` reach nothing through them.
The operator learns that the DSN does not parse and which variable to look at,
which is the actionable part; the specific malformation is not worth a
credential. Errors that are NOT `*url.Error` keep their text, since driver and
connection failures are diagnostic rather than echoes of the input.

The intermediate version, kept here because the reasoning is the lesson, unwrapped
instead:

    var ue *url.Error
    if errors.As(err, &ue) && ue.Err != nil {
        return redact.URLUserinfo(ue.Err.Error())
    }
    return redact.URLUserinfo(err.Error())

`ue.Err` is the diagnosis with the URL left behind, which is the part worth
printing; `redact.URLUserinfo` covers any other chain that embedded the value,
and is string-based precisely so it works where the parse FAILED and there is
no `*url.URL` to inspect.

Why not redaction alone: it is credential-safe but unreadable. Redacting the
rendered message yields

    ***@db.%zz:5432/app": invalid URL escape "%zz"

— a dangling quote, a severed host, no leading `parse`. A mutation that dropped
the `errors.As` arm and kept only `redact.URLUserinfo` SURVIVED the first
version of the test, because a credential assertion cannot tell those apart:
neither leaks.

Three mutations in a row survived-then-failed on this one function, and each
survivor marked a real gap rather than a weak test: redact-only was unreadable,
unwrap-only echoed the parser's fragment. The assertions are now "no fragment of
the input appears" plus "it still says the DSN does not parse" — a pair that
pins both directions, where each single assertion alone admits a broken fix.

The assertions are on the RENDERED STRING, not on the source of the call sites.
The leak is a property of what an error FORMATS TO, not of which verb the
format string uses: `%v` on a plain error is fine and `%v` on a `*url.Error` is
not, and only the rendered bytes distinguish them. Each case also asserts its
own precondition — that formatting the raw error DOES embed the password —
so a case that stopped exercising a parse failure fails loudly instead of
passing vacuously.

Two review findings on the fix itself, both real:

The harness first got `redact.URLUserinfo` alone rather than the unwrap, so the
WIDEST print path in the package kept precisely the shape this entry argues
against — and `TestConnectAndMigrateDoesNotEchoTheDSN`, asserting only that the
credential was absent, could not see it. `safeDSNErr` is now exported as
`dbtest.SafeDSNErr` and used by the harness too, so there is one implementation
rather than two sites disagreeing about what "redacted" means. The harness test
now also rejects the redacted-remnant form.

Separately, `freshDatabase` repointed the scratch DSN by editing `u.Path` only.
The path is not the only place a DSN names its database: pgx reads `dbname` (and
the alias `database`) from the QUERY and applies it AFTER the path, verified via
`pgxpool.ParseConfig`, which resolves `Database="fromquery"` for both spellings.
A `TEST_DATABASE_URL` written that way would leave the "scratch" DSN pointing at
the shared database, so every migration would run there and the down-to-zero
would drop its schema. The rewrite is now a pure `withDatabase` — split out so
it is testable without a server, since a live test could only exercise whichever
single DSN form the developer happens to have — and the test asserts the database
pgx RESOLVES rather than the string produced. That distinction is the finding: a
path-only rewrite LOOKS correct, so a string assertion passes in exactly the
broken case.

Note for a follow-up ticket, deliberately NOT fixed here because this is a
test-only PR: `postgres.Migrate` itself leaks the same way in PRODUCTION.
`pool.go`'s `init migrator: %w` carries the full DSN, and
`container.go` reports it via `slog.Warn` on the startup path and again on each
background retry. `ValidateMigrationDSN` gates the common config error safely,
which is exactly why the gap is easy to miss — but `Migrate` is exported and
called directly, so the path is live.
