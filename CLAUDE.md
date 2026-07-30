# LFX V2 Campaign Service — Agent Guide

Backend service for LFX Self Serve marketing campaign operations: a Go/Goa
HTTP API deployed via Helm, brokering between the LFX UI and paid
advertising platforms.

## Start here

Before reading source files directly, consult
[`docs/knowledge/index.md`](docs/knowledge/index.md) — an
[Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle that maps this repo's architecture docs, Kubernetes resources, Go
packages, and feature specs without requiring the whole repo in context.

## Keep the knowledge base current

Whenever you merge a PR, update a Helm manifest, or fix a bug:

1. Update the relevant concept file(s) under `docs/knowledge/**` (add a new
   one with OKF frontmatter — `type`, `title`, `description` — if no
   existing concept covers the change).
2. Update the containing `index.md` bullet if a concept was added, renamed,
   or its description changed.
3. Append a dated entry to `docs/knowledge/log.md`
   (`## YYYY-MM-DD` / `**Update** — ...`).
4. Validate locally: `go run ./cmd/okfvalidate ./docs/knowledge`.

Do not re-run `go run ./cmd/okfgen` to do this — it regenerates the entire
bundle from source and will clobber hand-edited concept files. It exists
only to bootstrap new subtrees.

## Active feature spec

The current active speckit feature spec/plan/tasks live under
[`specs/002-db-conn-check/`](specs/002-db-conn-check/plan.md).

## Development

See `README.md` for the `make` targets used to build, test, lint, and run
the service.

## Local work cycle — post-commit and pre-PR review

Run `/lfx-skills:lfx-local-review` **after every commit** while working toward a
pull request, and once more in branch mode (`branch`) as the last step before you
open one. It reviews the committed target in a temporary detached worktree, so you
can keep editing while it runs.

Three reviewers run in parallel: a general reviewer, plus this repo's own two
brains — `.claude/skills/campaign-service-code-reviewer` (audits the diff against
this repo's written rules and quotes each one) and
`.claude/skills/campaign-service-learnings-reviewer` (matches the diff against
`docs/reviews/knowledge-base/`, the patterns extracted from past review comments
on this repo). The generic `local-code-review` and `local-learnings-review` names in
`.claude/skills/` are symlinks to those two directories, and `.agents/skills/`
exposes the same two physical directories — keep exactly one copy of each brain.

The foreground summary is the evidence for that commit. It is not a report, is not
retained, and dies with the session:

- Read it in full. Its states are only `COMPLETE_WITH_FINDINGS`,
  `COMPLETE_NO_FINDINGS` and `INCOMPLETE`.
- `INCOMPLETE` is **not** a pass. Any incomplete role makes the whole run
  incomplete, and the only recovery is a complete rerun of the same harness — not
  a rerun of the failed role, and not a substitution of a different one.
- If the summary says the run used the Claude fallback because Pi was
  unavailable, that run is single-model evidence. Say so rather than presenting it
  as cross-model.

This cycle **stops at PR-open.** It never touches GitHub and never writes a label,
status, review or approval.

When a review's findings change what the code does, the knowledge-bundle rules
above still apply to the follow-up commit.
