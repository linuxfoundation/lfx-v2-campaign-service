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

**Follow-up — a fourth finding on the fix itself.** Widening the comparison to word
boundaries drew review attention to WHICH identifiers are compared, and one was missing.
`pgconn.ParseConfig` puts only the first host in `Config.Host`; a comma-separated multi-host
DSN parses the rest into `Config.Fallbacks`, and pgx names the host that failed using
`originalHostname`. Verified before fixing: against
`postgres://USER:PASS@primary.invalid:5432,secondary.invalid:5433,third.invalid:5434/multidb`,
the message `failed to connect to host "secondary.invalid": connection refused` returned
**verbatim** — `dsnIdentifiersPresent` reported false.

`dsnIdentifiersPresent` now appends every `fb.Host`. The fallbacks also carry per-host
user/password/database, but pgx copies those from the top-level config for each entry, so the
host is the only field that adds anything; it is still read per fallback rather than assumed,
since the assumption is pgx's to change.

`TestSafeDSNErrWithholdsFallbackHosts` asserts the primary and both fallbacks, because a fix
that REPLACED `Config.Host` with the fallbacks would pass a fallback-only test. Both mutations
fail it: dropping the fallback loop leaks `secondary`/`third`, and dropping `cfg.Host` from
the set leaks `primary`. A message naming no host in the DSN still keeps its text, so the
wider comparison did not buy safety with diagnosability.

**Follow-up — unbounded teardown.** A suppressed review finding flagged `freshDatabase`'s
cleanup as running both its reconnect and its `DROP DATABASE ... WITH (FORCE)` on
`context.Background()`. Verified at head: true, and the consequence is that the cleanup cannot
fail. A stalled drop or an unreachable server blocks before either `t.Errorf`, so the package
hangs to its suite-level timeout and reports it against whatever test the runner was in —
naming neither the stall nor the stray database.

Swept as a class rather than as the named line. Every `t.Cleanup` in the package was on an
unbounded context: 13 sites across `migrate_down`, `invalid_index` (including
`restoreRequiredIndex`, reached from cleanup in four tests and doing a drop, a create and a
catalog verification), `list_campaigns` (3), `project_scope`, and `audience_lease`. All now
take `dbtest.CleanupContext()`.

The root is `Background`, not the test's ctx, and that is the half worth stating: by the time
`t.Cleanup` runs the test's context is cancelled, so deriving from it would fail every
teardown statement instantly WITHOUT dropping anything — a change that reads as a fix while
leaving the rows behind. `Background` carries no cancellation to inherit; the deadline is what
this adds.

**Verification and its limit.** `TestCleanupContextIsBoundedAndUncancelled` binds the helper:
reverting it to a bare `Background()` fails on the absent deadline, and rooting it in a
cancelled parent fails on both the `Err()` and `Done()` assertions. The CALL SITES are pinned
at the source only. Reproducing the real failure needs a wedged Postgres, which no test can
arrange, and reverting the helper to an unbounded context leaves the whole suite green —
confirmed by running it, not assumed. Stated here because a source pin is weaker evidence
than a rendered one and the next maintainer should not read it as equivalent.

**Follow-up — the guard was still spelling-based.** Review pointed out that question 2
discovered explicit DSNs by looking for a parameter named `dsn`, and that `pairChecked` was
only a global nonzero check. Both halves were true, and the bypass was reproduced before
fixing: renaming `newMigrator`'s parameter to `databaseURL` and reverting its formatter to
`SafeDSNErr` removed the function from the map, `schemaObjects` alone held `pairChecked` above
zero, and the wrong-DSN regression **passed**.

This is the third time in this file that asking about a NAME rather than about a VALUE has
produced a guard that agrees with itself — after `bareErrArgs` keyed on identifiers spelled
`err`, and the window-based scans that keyed on line proximity.

The expected DSN is now read from the DSN-bearing call's own argument. Which argument that is
is stated per callee rather than guessed: every entry in `dsnCalls` takes it last, but
`withDatabase(dsn, name)` takes it first, and a "last argument" rule resolved that to the
scratch database NAME and reported a correct `SafeDSNErr(DSN())` call as mispaired. `t` is
skipped by name where helpers thread a `*testing.T`; that is honest about being a shortcut —
the alternative is `go/types` and a full package load to resolve one identifier.

A call that used the environment's `DSN()` is satisfied by either redactor, since both compare
that value; anything else requires `SafeDSNErrFor` handed the same value. `explicitPairs` is
counted separately from `pairChecked` so the environment-only sites cannot hold the counter up
while the explicit ones vanish — the exact shape of the reported bypass.

Verified: the rename-plus-revert mutation now fails (`the call used databaseURL`), the plain
revert still fails, and forcing every pairing to the environment branch trips the new
`explicitPairs == 0` self-test. The guard reports 6 pairings, 3 on an explicit DSN.

**Also — shell quoting leaked into a comment.** A `'"'"'` escape from the heredoc that wrote
the file landed verbatim in `TestCleanupContextIsBoundedAndUncancelled`'s comment. Fixed, and
swept repo-wide rather than at the reported line: the same sequence was also sitting in
`campaign_repo.go:918`, from an earlier change. Both are now plain apostrophes and
`git grep` for the sequence is empty.
