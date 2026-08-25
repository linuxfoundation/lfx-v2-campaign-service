# 2026-08-24 — LFXV2-2643: every migration does ship a down file

**Verification** — Re-synced this branch onto `origin/main` (merge of `3c01b1b3`)
and adjudicated a suppressed review finding against
`internal/infrastructure/postgres/dbtest/migrate_down_live_test.go:198`.

The finding is **FALSE on its load-bearing claim**. It asserts that versions
`000009` and `000023` "have no `.down.sql` files" and that `golang-migrate`
therefore treats those directions as empty migrations, so the comment's phrase
"every migration in the tree ships a `.down.sql`" overstates what the test
observes. Checked against the tree: the migrations directory holds **28 up files
and 28 down files** after the merge (main added `000028`), every up file has a
matching down file, and **no down file is zero-length**. `000009` and `000023`
each ship a down file that is a documented, deliberate no-op (`SELECT 1;` with a
comment explaining why the reversal is empty) — which is the opposite of an
absent file. A file `golang-migrate` reads and executes is not a direction it
"treats as empty"; the version is stepped through and its SQL runs.

The finding's secondary claim — that `000009` conditionally repairs an invalid
index and so is "not an exact inverse under that recovery precondition" — is
true as a statement about `000009`'s up file, which does drop and rebuild an
INVALID index inside a `DO` block. But it does not reach the comment it is
filed against. That comment does not claim any migration is an exact inverse; it
claims the down files are otherwise never executed and so can rot undetected,
and that stepping down to zero forces each one to run against the schema its own
up produced. Both remain true. No code or comment change is warranted, so none
was made.

Separately, the AST source guard in this file was mutation-tested with a
**differently-shaped violation** than the three that previously defeated it (a
5-line proximity window, a continuation-line miss, and a name heuristic). The new
shape renames the bound error to an identifier resembling nothing error-like and
nests it inside a non-redactor call, so it is invisible both to a spelling rule
and to a top-level-argument scan. The guard **caught it**, naming the correct
line and identifier — it binds the error from the assignment and descends through
wrapping calls, stopping only at a redactor, so neither renaming nor nesting
evades it.

The redaction arms were mutation-tested too: swapping the migrate arm from the
explicit-DSN redactor to the environment-DSN one — the shape that was a live
credential leak — is killed by `TestConnectAndMigrateWithholdsTheExplicitDSN`,
which observes the probe's user and database names surviving into the rendered
error. Both arms were restored and re-read at `dbtest.go:155` and `:178`.
