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
3. Add a new file `docs/knowledge/log/YYYY-MM-DD-<slug>.md` (slug = ticket +
   short description) with a first H1 dated to match the filename, then a
   bold kind marker and an em dash: `**Update** — ...`. `**Update**` is the
   default and covers most entries; the bundle also uses `**Fix**`,
   `**Creation**`, `**Note**`, `**Verification**` and `**Docs**` where one of
   those describes the entry better. Pick the accurate one — the marker is a
   label, not a fixed literal. One file per entry — never edit another
   entry's file.
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

Run, from this repo, **after every normal signed commit** while working toward a
pull request:

```text
/lfx-skills:lfx-local-review
```

It reviews the **newest commit** — by default the range `HEAD^..HEAD`, the diff
that commit introduces against its first parent — so you can keep editing while it
runs. A caller that needs a wider range may supply a direct base parameter; the
review never derives one, never fetches, and never consults a remote.

Three reviewers run in parallel and each returns an ordinary Markdown review: a
general reviewer, plus this repo's own two brains —
`.claude/skills/campaign-service-code-reviewer` (audits the change against this
repo's written rules and quotes each one) and
`.claude/skills/campaign-service-learnings-reviewer` (matches it against
`docs/reviews/knowledge-base/`, the patterns extracted from past review comments
on this repo). The generic `local-code-review` and `local-learnings-review` names in
`.claude/skills/` are symlinks to those two directories, and `.agents/skills/`
exposes the same two physical directories — keep exactly one copy of each brain.

When the host reports that Pi is unavailable it runs the trio as Claude subagents
instead, following `.claude/skills/local-review-fallback` — this repo's launch
table for exactly those three reviewers, aliased at
`.agents/skills/local-review-fallback`. It carries no review criteria of its own.

Read the reports in full. **This session — not the reviewers — fixes what they
find.** Reviewer children never edit source, commit, push, or touch GitHub beyond
read-only inspection. Land the fixes as normal signed conventional commits with a
`fix(<scope>): ...` or `fix: ...` prefix, then **rerun the complete trio**.

A review whose first line is `INCOMPLETE — <reason>`, or a role the host reports
as failed or empty, makes the **whole cycle incomplete**. Successful siblings do
not rescue it: two clean reports next to one incomplete one is not a pass. Resolve
the cause and rerun the complete trio under one harness — never just the failed
role, and never a mix of Pi and Claude evidence in the same cycle.

Before opening a PR: drain the reviews, then run the repo's normal readiness and
preflight checks.

This cycle **stops at PR-open.** Pushing and opening the PR happen under the
coordinator's release instruction, not from the review cycle, which never writes a
label, status, review or approval.

When a review's findings change what the code does, the knowledge-bundle rules
above still apply to the follow-up commit.
