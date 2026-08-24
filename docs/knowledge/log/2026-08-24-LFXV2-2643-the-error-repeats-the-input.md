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

The fix unwraps rather than redacts:

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
neither leaks. The test now also asserts the host fragment is absent, which
pins the cause being unwrapped OUT of the message rather than the message being
redacted in place, and that mutation now fails.

The assertions are on the RENDERED STRING, not on the source of the call sites.
The leak is a property of what an error FORMATS TO, not of which verb the
format string uses: `%v` on a plain error is fine and `%v` on a `*url.Error` is
not, and only the rendered bytes distinguish them. Each case also asserts its
own precondition — that formatting the raw error DOES embed the password —
so a case that stopped exercising a parse failure fails loudly instead of
passing vacuously.

Note for a follow-up ticket, deliberately NOT fixed here because this is a
test-only PR: `postgres.Migrate` itself leaks the same way in PRODUCTION.
`pool.go`'s `init migrator: %w` carries the full DSN, and
`container.go` reports it via `slog.Warn` on the startup path and again on each
background retry. `ValidateMigrationDSN` gates the common config error safely,
which is exactly why the gap is easy to miss — but `Migrate` is exported and
called directly, so the path is live.
