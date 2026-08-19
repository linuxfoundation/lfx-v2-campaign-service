# 2026-08-18 — LFXV2-3222: the retention index takes a blocking lock

**Fix** — Migration 000026 used a plain `CREATE INDEX`, justified by a comment asserting that
"golang-migrate runs each migration inside a transaction and CONCURRENTLY cannot run in one
(000018 is the exception)."

That is backwards, and the repo already records the correct fact twice. Both 000008 and 000018
state it verbatim: the pgx/v5 golang-migrate driver executes each migration with a bare
`ExecContext` and does **not** wrap it in a transaction. Enumerating the directory, **19
migration files already use CONCURRENTLY**, and `migrations/README.md` codifies
"CONCURRENTLY alone in its file" as the standing rule. 000018 is the pattern, not the exception.

The second half of the justification was self-undermining: it argued a brief write lock was
acceptable because `campaign_jobs` is small — on the very migration whose PR exists because
that table grows without bound. A plain `CREATE INDEX` takes an ACCESS EXCLUSIVE lock, so on
the deployment that most needs this prune, one with millions of accumulated rows, it would
block every job write for the duration of the build. The index for fixing unbounded growth
would have been the thing that made unbounded growth hurt.

Now `CREATE INDEX CONCURRENTLY` / `DROP INDEX CONCURRENTLY`, each alone in its file.

Verified against a live PostgreSQL 16 rather than by reading: migrations apply to version 26
with `dirty = false`, and `pg_indexes` reports the partial predicate intact. Had the original
comment been right, that apply would have failed — which is the check that settles it.
