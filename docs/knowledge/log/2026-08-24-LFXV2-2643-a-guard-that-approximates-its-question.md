# 2026-08-24 — LFXV2-2643 a guard that approximates its question

**Fix** — `TestNoConnectSiteRendersItsErrorRaw` is source-pinning, not behaviour: it
substitutes for the four `pgx.Connect` sites whose failure arms only run when a live
database dies mid-run, which no unit test can stage. That makes a hole in it expensive —
the sites read as pinned while nothing checks them.

Two reviewers independently reported the same hole, and it reproduced exactly: the cleanup
reconnect site puts `t.Errorf` on one line and its `dbtest.SafeDSNErr(err)` argument two
lines below, and the scanner only inspected the line containing `t.Errorf`. Replacing that
argument with raw `err` left the test green — a compiling mutation that survived.

This was the guard's SECOND miss of the same kind. The first version used a fixed 5-line
lookahead and a four-line comment pushed a `Fatalf` out of range; the fix widened the
window to the whole `if` block, which still read one line at a time. Both failures share a
cause: line proximity only APPROXIMATES the question. The guard wants to know whether an
error value reaches a formatting call without passing through the redactor, and that is a
question about expression structure, not about how the source is wrapped. A third window
would have been a third bet on the shapes nobody had written yet.

So the guard now parses itself with `go/parser` and walks the AST. Arguments are inspected
at any depth, with descent stopping at a redactor call, which is what makes
`fmt.Sprintf("%v", err)` a finding and `SafeDSNErr(err)` not one — wrapping an error in a
non-redactor renders it just as raw. Matching by identifier shape rather than by the literal
name `err` catches a renamed `connErr`.

Parsing widened the guard's REACH, and that exposed two sites the hand-maintained call list
never named: `migrate.NewWithSourceInstance`, handed `migrateURL(t, dsn)`, and
`withDatabase(dbtest.DSN(), ...)`. Both are DSN-bearing and neither was covered. The call
list was too narrow independently of how lines were matched, so the reported defect was the
smaller half of the problem.

`withDatabase` qualifies by ARGUMENT, not by name: its table-driven test feeds hardcoded
literals (`u:p@h`), whose echo leaks nothing, and flagging those would teach the reader to
ignore the guard. A guard that cries wolf on a fixture is worth less than no guard.

The walk also counts what it inspected and fails on zero. A structural guard can be emptied
silently by a rename or a refactor, and a guard that checks nothing passes for exactly the
same reason a correct one does.

What remains unpinned, stated plainly: this is still source inspection. It proves the four
sites SPELL the redactor, not that the redactor's output is credential-free at runtime —
that property is pinned separately by the behavioural `SafeDSNErr` tests. An error reaching
`t.Fatalf` through a helper function defined elsewhere, or through a variable assigned from
a redactor several statements earlier, is outside what this walk follows. It covers the
shapes this file uses, and it now fails loudly if the file stops matching it.

Ref: LFXV2-2643
