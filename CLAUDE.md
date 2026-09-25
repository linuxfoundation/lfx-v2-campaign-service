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
   or its description changed. The bullet's description must be **verbatim**
   the concept's frontmatter `description` — `okfvalidate` fails on any
   difference, because the index is what a reader consults before deciding
   whether to open the file at all.
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

For the live schema and layout, the migrations under
`internal/infrastructure/postgres/migrations/` and `docs/architecture.md` are
authoritative; `docs/channel-connections-schema.md` and `docs/build-summary.md`
are dated design-time documents and do not override the code where they
disagree. What the formatting, MegaLinter and license-header checks cover is
defined by `GO_FILES` in the `Makefile`, `FILTER_REGEX_EXCLUDE` in
`.mega-linter.yml` and `exclude_pattern` in
`.github/workflows/license-header-check.yml`.

## Pre-PR review

> **IMPORTANT — follow this exactly.** When the implementation is complete
> and committed and you are about to open a PR:
>
> 1. **Review once.** Load `/lfx-skills:lfx-pre-pr-review` with the Skill
>    tool and follow it. It runs **one** review round of the whole branch —
>    general, security and knowledge-base reviewers in parallel — and lands
>    **all accepted findings from that round in exactly one fix commit**
>    (none if there is nothing to fix). Do not work from memory: **reload the
>    skill before each step** of the round — before launching the reviewers
>    and before the fix commit.
> 2. **Preflight.** Run the `Preflight` value below and make it pass. It is
>    deterministic checks, not a review: fix what it reports in its own
>    commit(s), as many as it takes, and rerun it — never the reviewers.
> 3. **Open the PR.** From then on there are **no local reviews of any
>    kind** — iterate only on the PR's bot and human feedback, still running
>    tests and checks.

- KB review skill: `/campaign-service-learnings-reviewer`
- Preflight: `make check-fmt && make lint && make test && go run ./cmd/okfvalidate ./docs/knowledge`

When a fix commit — the review round's or a preflight one — changes what the
code does, the knowledge-bundle rules above still apply to it.
