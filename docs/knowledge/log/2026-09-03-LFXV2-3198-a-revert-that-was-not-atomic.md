# 2026-09-03 — LFXV2-3198: an explicit transaction on a down migration, and a claim that was wrong

**Fix** — `000030_brief_delivery_stage_key.down.sql` narrows the brief unique key back to
`(project_id, event_slug)`, which legitimately FAILS once an event holds both a paid brief and an
email series: those rows cannot be told apart under the narrow key. Refusing is correct. The file
wraps its statements in an explicit `BEGIN; ... COMMIT;` so a refusal leaves the schema untouched.

**The correction, and it matters more than the fix.** An earlier version of this entry claimed the
explicit transaction was the remedy for an observed incident: that golang-migrate ran the statements
one at a time, so a failing `CREATE UNIQUE INDEX` aborted while the `ALTER TABLE ... DROP COLUMN`
statements after it ran anyway, leaving duplicates with their discriminator gone.

**That cannot happen in this repository's configuration.** `pgxURL` (`pool.go:794`) only rewrites
the URL scheme to `pgx5://`; it does not set `x-multi-statement`, and nothing else does. With that
flag off, the pgx5 driver submits the whole file in a single `ExecContext`, and PostgreSQL wraps a
multi-statement simple query in an IMPLICIT transaction — so the batch already rolls back as a unit.
The migrations README says exactly this at line 59, in the course of explaining why
`CREATE INDEX CONCURRENTLY` must live alone in its own file.

Verified directly rather than reasoned about: two statements in one `psql -c` call, the second
dividing by zero, left the first statement's row absent. Batch rolled back whole.

The explicit `BEGIN/COMMIT` stays. It is a defensive guarantee that survives someone enabling
`x-multi-statement` later — the flag exists precisely so a file CAN be run statement-by-statement —
and it states the intent locally rather than depending on a driver default two layers away. But it
is belt-and-braces, not the fix for an incident, and the file's comment now says so.

**The general shape:** "I wrapped it in a transaction and the failure went away" is not evidence
that the absence of a transaction caused the failure. Before writing a mechanism into the knowledge
log, check the layer that would have to be misconfigured for it to be real — here a single grep for
`x-multi-statement` would have settled it, and the repo's own README already stated the answer.
