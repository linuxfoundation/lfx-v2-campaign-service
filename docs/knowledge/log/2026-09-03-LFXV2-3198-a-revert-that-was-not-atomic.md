# 2026-09-03 — LFXV2-3198: a down migration that half-ran

**Fix** — `000030_brief_delivery_stage_key.down.sql` narrows the brief unique key back to
`(project_id, event_slug)`, which legitimately FAILS once an event holds both a paid brief and an
email series: those rows cannot be told apart under the narrow key. Refusing is correct.

What was not correct is what a refusal left behind. golang-migrate does **not** wrap a migration's
statements in a transaction — each runs on its own. So the failing `CREATE UNIQUE INDEX` aborted,
and the `ALTER TABLE ... DROP COLUMN` statements that followed ran anyway. The result was the worst
of both: the duplicate rows still there, the two columns that distinguished them gone, and neither
the old index nor the new one in place. A revert that reports failure while destroying the only
data that could have resolved it.

Fixed by wrapping the down migration in an explicit `BEGIN; ... COMMIT;`. Verified against a seeded
database by observing the failure mode first — duplicates surviving with their discriminator
dropped — and then confirming the wrapped version leaves the schema untouched on the same input.

**The general shape:** a migration runner that batches statements is not the same as one that makes
them atomic, and "the statement that failed" is not the same as "the statements that ran". Where a
down migration can legitimately refuse, the refusal has to be all-or-nothing or it is worse than no
rollback at all.
