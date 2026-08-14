# 2026-08-13 — Expand/contract written down as an enforced repo rule

**Update** — `docs/knowledge/code/internal-infrastructure-postgres.md`,
`.claude/skills/campaign-service-code-reviewer/SKILL.md` (via `.agents` symlink),
`internal/infrastructure/postgres/migrations/README.md`
(linuxfoundation/lfx-self-serve#1542). Follow-up from @bramwelt's PR #126 review — the
item he flagged as the one that prevents a repeat ("if only one of these happens, make it
(1)").

## The rule

> A migration that removes or narrows something the N-1 release's SQL depends on ships one
> release AFTER the code change that stopped depending on it.

Add the new shape and move all reads/writes onto it in release N; drop or narrow the old
shape in release N+1, once no running binary reads it. golang-migrate is one ascending
stream, so the version number is the ordering and the rule is enforceable at authoring
time — a PR that both introduces narrowing/removing DDL and the code that stops depending
on the old shape, in one release, is a finding unless a rollout ordering (e.g. `Recreate`)
covers the overlap.

## Written in three places, on purpose

The rule is only useful if a reader, a reviewer, and an author each meet it where they
look:

- **Concept** (`internal-infrastructure-postgres.md`) — the authoritative statement,
  placed with the `000013`/`000014` ledger it generalizes. Body-only edit; the frontmatter
  `description` is unchanged, so no `index.md` change is owed.
- **Reviewer surface** (`campaign-service-code-reviewer` SKILL) — added to the Postgres
  rule area so the code-review brain can quote and enforce it mechanically on migration
  diffs. A rule the reviewer cannot quote is not a finding, so the rule has to live on the
  rule surface, not only in a concept.
- **Author guide** (`migrations/README.md`) — a checklist at the point of work. Safe to add
  here because `migrations.go` embeds `*.sql` only, so a README is never seen by
  golang-migrate.

## "Stopped depending" is the subtle part

`000013`/`000014` is the case study precisely because it could NOT be split: the old full
constraint still governed soft-deleted rows the delete path had to free, so deferring the
drop shipped a delete endpoint that silently did nothing. The rule therefore says "stopped
depending" means for EVERY row the N-1 binary can still touch, soft-deleted rows included —
and names the rollout-strategy escape hatch as the exception that the PreSync-Job work
(#1543) removes the need for.

## Not a behavior change

Docs, one reviewer-skill rule, and an author README — no code or schema change, nothing to
deploy.
