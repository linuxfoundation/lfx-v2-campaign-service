# 2026-08-24 — LFXV2-2643 which secret was the redactor handed

**Fix** — three findings on the DSN-redaction PR, all on the same seam, all confirmed by
rendering rather than by reading.

`newMigrator` is handed an explicit scratch `dsn` and formatted its init failure with
`SafeDSNErr`, which compares against the environment's `DSN()`. That is the identical
wrong-DSN seam this branch already fixed in `connectAndMigrate`, recurring one function
away. The scratch DSN is `TEST_DATABASE_URL` with the database name swapped, so the
database name is precisely the identifier that diverges — and the one that leaks. Rendered:
against `envDSN=.../campaign_test` and `scratchDSN=.../down_abc123`, the message
`migration source open failed for schema "down_abc123": no such version` came back from
`SafeDSNErr` **verbatim**, and from `SafeDSNErrFor(scratchDSN, …)` as the withhold
sentinel. Now `SafeDSNErrFor(dsn, err)`.

Enumerating the class rather than the line: of the seven `SafeDSNErr` call sites, only this
one sits in a function holding an explicit DSN. The three others in `migrate_down_live_test.go`
and all six in `invalid_index_live_test.go` operate on `dbtest.DSN()`, where the environment
form is correct. One site was wrong; changing the others would have been wrong.

`dsnIdentifiersPresent` withheld on a raw `strings.Contains`. CI's user and password are both
`postgres`, a substring of `postgresql`, so ordinary prose naming no credential —
`unsupported postgresql wire protocol version 2`, `lookup postgresql.svc: no such host` —
was replaced by the sentinel. That breaks the diagnosability half of the contract in exactly
the configuration the helper exists to protect. `namesIdentifier` now matches on word
boundaries, case-insensitively; it scans rather than building a regexp because the identifier
is operator-supplied text that may carry metacharacters, and it advances one byte per attempt
so an embedded hit cannot mask a real bounded echo later in the message.

`TestConnectAndMigrateWithholdsTheExplicitDSN` could pass for the wrong reason. The host is
one of the four compared fields, so a probe sharing a host with `TEST_DATABASE_URL` makes
redaction fire on the host alone. `127.0.0.2` did not settle this — the harness contract lets
a developer point `TEST_DATABASE_URL` anywhere. **Verified**: with `SafeDSNErr` restored and
`TEST_DATABASE_URL` on `127.0.0.2`, the test still passed. The probe host is now
`dbtest-probe.invalid` (RFC 2606, permanently non-resolvable), which no working harness DSN
can share, plus a control requiring an unrelated pinned DSN to PRESERVE the same text — if
both withheld, withholding would prove nothing about the argument.

The reviewer's proposed remedy, `t.Setenv` under a serial test, was not taken and the reason
is on the record in the test: `SafeDSNErrFor` takes the DSN as an argument precisely to keep
process-global state out of these tests, and a serial test writing `TEST_DATABASE_URL` still
races the package's parallel tests reading it through `DSN()`.

**Verification** — every fix was mutation-tested, because a fix commit citing a real finding
is the most convincing form of unverified work.

Reverting `newMigrator` to `SafeDSNErr` fails the extended AST guard by name and line;
so does the subtler `SafeDSNErrFor(dbtest.DSN(), err)` — right function, wrong argument —
which the previous guard could not see, since it only checked that SOME redactor appeared.
The guard now asks a second question: inside a function holding an explicit `dsn` parameter,
is the redactor the DSN-taking form handed THAT parameter. It carries its own
`pairChecked == 0` self-test, on the same reasoning as the existing one — a renamed parameter
would empty the walk silently and leave a guard that passes because it asked nothing. It
reports 6 formatting calls and 2 pairings.

Reverting `namesIdentifier` to `strings.Contains` fails
`TestSafeDSNErrDoesNotOverMatchEmbeddedIdentifiers` on all three prose cases while the
withhold cases still pass, so the test constrains the boundary rather than agreeing with it.

The hardened `.invalid` probe now fails under an unset `TEST_DATABASE_URL`, under CI's
`127.0.0.1` value, and under the `127.0.0.2` value that previously defeated it. The limit is
stated in the test rather than glossed: an env DSN set to the probe's OWN identifiers would
still let the mutation survive. That cannot arise from a working harness — an `.invalid`
host does not resolve, so no live test could have run against it — but it is a limit of the
instrument, not something the instrument rules out.

The source guard's honesty note is unchanged and still says what it said: the four
`pgx.Connect` sites fire only when a live database dies mid-run, so it is weaker evidence
than a rendered-output assertion and is not a substitute for one.

No test was removed — `grep "^func "` across both touched test files shows one addition and
no deletions. `gofmt -s`, `go vet`, `go build`, `go test -race ./...` (34 packages) and
`golangci-lint` (0 issues) are all clean.
