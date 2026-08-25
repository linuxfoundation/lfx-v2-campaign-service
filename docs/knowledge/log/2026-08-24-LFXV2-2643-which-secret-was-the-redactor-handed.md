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

The reviewer's proposed remedy, `t.Setenv` under a serial test, was not taken: `SafeDSNErrFor`
takes the DSN as an argument precisely to keep process-global state out of these tests, and
pinning the probe host keeps the test parallel as well as independent.

**Correction (2026-08-25).** The reason first given here — that a serial writer "still races
the package's parallel tests" — was false, and it was repeated in four other places before
review caught it. Go runs top-level parallel tests only after the serial ones finish, so the
window cannot overlap; measured at zero overlaps against three polling parallel readers. The
real cost of `Setenv` is the ordering constraint it imposes, not a race. Every copy has been
corrected.

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

**Follow-up — three suppressed findings, and a false claim of my own.**

*The `t.Setenv` race did not exist.* Five places in this branch asserted that a serial test
writing `TEST_DATABASE_URL` "races the package's PARALLEL tests" reading it through `DSN()`.
It does not: `go test` runs top-level parallel tests only after every serial test finishes, so
the `Setenv` window cannot overlap them — which is why this package's own
`TestSafeDSNErrReadsTheConfiguredDSN` is correct under exactly that pattern. Measured with a
purpose-built package: a serial `t.Setenv` test holding a 300 ms window against three parallel
readers polling for overlap produced **zero overlaps**.

The isolation rationale for `SafeDSNErrFor` survives intact; only the mechanism was wrong. The
real cost of `Setenv` here is the ORDERING CONSTRAINT — every redaction test had to be serial —
which is sufficient reason for the argument form and is what the comments now say. Corrected in
`dbtest.go`, `connect_redaction_test.go`, the concept, and both log fragments. Notably, one of
those fragments had *corrected a bot's false mechanism and then asserted its own*, which is the
same failure one layer up.

*A comment described a superseded implementation.* `TestSafeDSNErrRedactsAWrappedMigratorError`
called the `*url.Error` path "the unwrap arm". `SafeDSNErr` does not unwrap that cause and
re-render it — `errors.As` finds it through the wrapper and the whole error is DISCARDED for
`dsnUnparseableMsg`, which is the point, since a `*url.Error`'s text quotes the fragment it
choked on and that fragment can carry the credential. Now "the discard arm".

*The guard still had a name heuristic.* `liveDSNArg` asked whether an argument was literally
spelled `dsn`, so `databaseURL := dbtest.DSN()` passed to `withDatabase` made the call
invisible; a raw `%v` of its error then passed while the other sites held all three coverage
counters nonzero. Reproduced before fixing. `dsnCarriers` now computes the carrier set by data
flow — seeded from `DSN()` calls and from the string parameters of functions that perform a
DSN-bearing call, propagated through assignment — and both bypasses fail: the aliased raw
format is caught by question 1, the renamed parameter by question 2.

That is the third spelling-based heuristic this guard has shed (after `bareErrArgs` on `err`
and the line-window scans). The six mutations are now enumerated in the test's doc comment so
the next change to it can be checked against the ones that already fooled a version of it.

**Follow-up — bounding each step did not bound the teardown.** Review pointed out that the
per-cleanup deadline added above is correct per database and wrong in aggregate, and the
arithmetic holds: `TestLiveMigrationsGoDownAndUpAgain` provisions one scratch database per
migration version, there are 28 migrations, and each cleanup got its own fresh 30s budget —
~29 x 30s = 14.5 minutes serially if Postgres goes unreachable. The Makefile sets no
`-timeout`, so Go's 10-minute default fires first and the run dies at the opaque suite
timeout: exactly the outcome the deadline was added to prevent, just moved.

`scratchReaper` replaces it. Names are registered per test and dropped from ONE `t.Cleanup`
under ONE `CleanupContext`, through ONE reconnect rather than one per database — an
unreachable server now pays the connect timeout once instead of 29 times. Worst case goes
from 14.5 min to 30s, well inside the default, so the failure is reported by the test that
caused it. If the budget does expire mid-reap it says how many databases were left, rather
than reporting only the drop that hung. The reaper is keyed by `*testing.T` so a helper called
from several tests cannot merge their databases into another's teardown.

Same honesty as before: `TestScratchReaperRegistersOneCleanupForEveryDatabase` pins the
SHAPE — 29 names registered through the real `scratchDatabases` path into ONE reaper under one
deadline — which is what a unit test can reach. The stalled-server behaviour itself still
needs a wedged Postgres and is not behaviourally bound.

**Correction (2026-08-25).** The first version of that test was VACUOUS, and review caught it
where I did not. It built a local `scratchReaper`, asserted on `CleanupContext` in isolation,
and cleared `r.names` by assignment — never calling `scratchDatabases` or `reap`. Reverting the
aggregate fix to per-database cleanups left it **green**, so it pinned none of what its own
godoc and this entry claimed. It now drives the real registration path through a subtest and
fails on exactly that mutation: `scratchDatabases returned a different reaper on the second
call`. Writing the claim into the doc did not make the test true, and the entry asserted the
pin before the mutation had been run against it.

**Second correction (2026-08-25).** The REWRITE was vacuous too, in a narrower way, and review
caught that as well — a fourth instance of the same pattern on this file in one night. It
counted cleanup invocations through a counter incremented by a cleanup the TEST itself
registered, so the value was 1 by construction whether `scratchDatabases` installed one
cleanup, many, or none; removing the helper's registration entirely left the arm green.
Confirmed by running that mutation before replacing it.

That arm is gone. Nothing counts registrations now: what replaced it is a check that the
helper's reaper is no longer in `scratchReapers` after the subtest, which only the helper's own
cleanup can bring about. The two arms kept from the previous round were each re-audited by
targeted mutation rather than by re-reading — same-reaper fails when `scratchDatabases` returns
a fresh reaper per call, and the 29-name accumulation fails when `add` stops appending.

One mutation SURVIVES and is now recorded in the test's own godoc rather than left implicit:
registering the cleanup but never calling `reap()` still passes, because the test must drain
the synthetic names before the reap can dial a real server, so an empty list afterwards cannot
distinguish the reap having run from the drain having run. Whether `reap()` actually drops
anything is pinned by no unit test — it needs a live database, which is what the live
down-migration test exercises in CI.

The expiry message had the same shape of error: it reported `len(names)` — the full registered
list — so databases the loop had already dropped were counted as still present and none of the
skipped ones were named, contradicting the sentence directly above it. It now reports
`names[i:]`, the databases actually skipped, and names them.

**Also — the PR description outlived its test.** It claimed assertion (2) of
`TestLiveCredentialSurvivesTheRealByteaColumn` verified the stored column "contains no
recognizable fragment" of the plaintext. That `bytes.Contains` search was deliberately
removed in favour of exact inequality plus the deterministic AES-GCM length identity, and the
description had not caught up. Corrected to state the shipped guarantees, with the reason the
substring check was wrong in both directions kept on the record.

**Follow-up — a fresh leak, in the file that pins the redaction.** Review found that
`golang-migrate` holds the DSN inside its driver and reconnects through `database/sql`, so
`m.Up()`, `m.Steps()`, `m.Migrate()` and `m.Version()` can surface pgx's
`*pgconn.ConnectError` just as the initial connect does. Seven sites rendered those with `%v`.

Rendered before fixing, against a closed port with a known-secret DSN:

    failed to connect to `user=leakuser database=leakdb`: 127.0.0.1:1 (127.0.0.1): dial error:
    dial tcp 127.0.0.1:1: connect: connection refused

and through the redactor:

    the driver's message names a value from TEST_DATABASE_URL (it is withheld: the user,
    database and host are half of the credential)

The ratio is the lesson, and it is the same one that let the original bug survive five rounds:
the file contained **34 correct `SafeDSNErrFor` calls** alongside these **7 raw ones**. Any
grep for the redactor looked satisfied. Presence of a mitigation is not application of it.

The AST guard could not see them either, because its call set listed only functions that TAKE
a DSN as an argument — a migrator method takes none, the DSN is in the receiver. It now tracks
identifiers bound by `newMigrator` and treats their method calls as DSN-bearing; the inspected
surface went from 6 formatting calls to 15, and reverting either the `m.Up()` or the
`m.Version()` site now fails it by line number.

The rest of the class was checked rather than assumed. `Exec` failures on an already-established
connection carry no DSN: rendered, `CREATE DATABASE`/`DROP DATABASE`/syntax errors produce
`ERROR: ... (SQLSTATE ...)` and nothing more, so those sites keep `%v` deliberately. `iofs.New`
touches no DSN at all.

These sites remain pinned at the SOURCE only, and that is weaker evidence than a rendered one:
they fire when a live database dies mid-run, which no unit test can arrange. Reverting any of
the seven leaves the behavioural suite green — confirmed by running it, not assumed — and only
the AST guard goes red.

**Follow-up — the migrator fix half-landed.** Extending the guard to migrator methods made
them visible to question 1 (raw `%v`) but not to question 2 (which DSN). `dsnArgOf` reads the
expected DSN from a call's ARGUMENTS, and a migrator method has none — the DSN is in the
receiver — so `want` stayed empty and every migrator site skipped the pairing check.
Reproduced: replacing `SafeDSNErrFor(dsn, err)` with `SafeDSNErr(err)` on `m.Up()` passed.

`migratorVars` now maps each migrator variable to the DSN expression `newMigrator` was handed,
rather than to a bare bool, and `dsnArgOf` yields that expression for a method call on the
receiver. Pairings went from 6 (3 explicit) to 15 (12 explicit), and all three mutations now
fail: `SafeDSNErr` on a migrator method, `SafeDSNErrFor` handed `dbtest.DSN()`, and raw `%v`.

The shape of this miss is worth naming: the fix made the sites VISIBLE without making them
CHECKED, and the counter went up (6→15 inspected) which read as progress. A coverage number
rising is not evidence that the new coverage asks anything.

**Also — a note that contradicted its own correction.** `a-dsn-has-two-legal-shapes.md` still
carried "global env mutation racing this file's parallel readers" a few lines below the
correction retiring exactly that claim. Two contradictory statements in one entry are worse
than either alone. The note now states the real cost — the ordering constraint — and says why
the racing phrasing was withdrawn.

**Follow-up — three prose claims that outran their code.** None changed behaviour; all three
were assertions in comments and docs that the code beside them did not support.

*A "diagnosability" check that could not check diagnosability.* The last assertion in
`TestConnectAndMigrateWithholdsTheExplicitDSN` compared `connectAndMigrate`'s output against
`SafeDSNErrFor(dsn, rawErr)` under a comment claiming the operator still gets the fault. It
cannot, on that input: the control immediately above proves `rawErr` names `user`, so the
redactor necessarily takes the identifier-present branch and returns the fixed sentinel.
Rendered — `hostname resolving error: lookup dbtest-probe.invalid: no such host` becomes the
sentinel, and the fault is gone. Any non-empty constant would satisfy the comparison. The
assertion is real but narrower than advertised: it pins that the arm emits the REDACTOR's
rendering rather than a message of its own. Diagnosability is pinned where it is observable,
on messages naming no configured identifier, by
`TestSafeDSNErrKeepsDriverTextForNonURLErrors` and
`TestSafeDSNErrDoesNotOverMatchEmbeddedIdentifiers`. The comment now says which of those it is.

*A union bound called an exact probability.* Three places stated the chance of GCM ciphertext
containing `secret` as "exactly" `(54-6+1)/256^6`. `secret` having no proper self-overlap rules
out OVERLAPPING placements, but two DISJOINT occurrences can coexist (`secretsecret`), and
summing the 49 per-offset probabilities counts those twice — so the figure is an upper bound.
Verified by exhaustive enumeration on a reduced alphabet: pattern `ab` in a 4-byte blob over
`{a,b}` has exact probability 11/16 against an offset sum of 3/4, with `abab` the
double-counted string. Corrected in the test comment, the log fragment and the PR description;
the argument the number supports — that the risk is NONZERO — is unchanged, which is why the
wrong word survived three readings.

The pattern across all three is the one this branch keeps producing: the code was right and the
sentence next to it claimed more than the code did. A comment is a claim, and "exact",
"diagnosable" and "counts registrations" are each checkable.
