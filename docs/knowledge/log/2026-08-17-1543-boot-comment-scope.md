# 2026-08-17 — 1543 boot comment scope and the migrate subcommand concept

**Docs** — Two review findings, both about the same PR describing itself inaccurately.

**A claim corrected in three files survived in a fourth.** `initDatabase`'s godoc still said boot
verification means "the previous release is never migrated out from under while it may still be
serving". That is backwards, and it is the exact sentence an earlier round of this ticket
corrected in `migrateCmd`'s godoc, in this ticket's first log fragment, and in
`internal-container.md`. The PreSync Job migrates WHILE the old ReplicaSet serves — that is the
design — and expand/contract authoring is what makes the overlap safe.

The comment is now scoped to boot and says plainly what it does not cover, because the failure
mode is not a reader who learns nothing: it is a migration author who reads a safety guarantee
into it and relaxes the authoring rule the repo depends on.

Worth recording as a process note rather than a code note: three files were fixed by grepping the
distinctive phrase, and the fourth was missed because the sweep ran before the last edit rather
than after all of them. Re-running the same grep across every file type at the END is what caught
it — a correction pass is not finished when the known sites are fixed, only when the phrase
returns nothing but its own refutations.

**The `migrate` subcommand had no concept entry.** `cmd-campaign-service.md` still described the
binary's subcommand surface entirely in terms of `bootstrap-system-account`, so the load-bearing
entrypoint this ticket adds was documented nowhere a reader would look before opening source —
which is what `CLAUDE.md:17` requires and what the index exists to serve.

It now opens on there being TWO one-shot subcommands and covers `migrate`'s role as the single
writer of schema, its DSN resolution (`PG*` parts, matching the chart, because `DATABASE_URL` is
not what gets injected), and the two things the PreSync ordering does NOT buy — it is not
protection from migrating a serving release, and it is not a rollback.

The concept's frontmatter `description` is unchanged, so the `index.md` bullet is deliberately
untouched: `CLAUDE.md` conditions that step on a concept being added, renamed, or its description
changing, and editing the bullet for an in-place body update is churn the validator would then
require to match.

**Note** — A first draft of this entry linked `../resources/chart-migrate-job.md` for the Job
itself. That file does not exist and `docs/knowledge/resources/` is empty; the link was written
from an assumption about where chart concepts live. Verified before committing and removed. A
dangling reference in a knowledge bundle is worse than no reference, because the index is what a
reader consults INSTEAD of opening the tree.
