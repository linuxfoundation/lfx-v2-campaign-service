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

## Pre-PR review

Run **one** local review of the whole branch before opening the PR — never
after individual commits, and never again once the PR exists.

1. When the implementation is complete and committed, run `git fetch origin`
   and pin the range: `base_sha=$(git merge-base origin/main HEAD)`,
   `target_sha=$(git rev-parse HEAD)`.
2. Launch **two** independent background subagents **in parallel**, one per
   skill, each with `subagent_type: general-purpose`, `model: opus` (Opus 5.5),
   `run_in_background: true`. Tell each to load exactly one skill with the
   Skill tool and follow it: one loads `/lfx-skills:lfx-general-code-review`
   (general quality plus this repo's written conventions, style and rules);
   the other loads `/campaign-service-learnings-reviewer` (this repo's review knowledge base). Give each
   the full 40-character `base_sha` and `target_sha`, the instruction to review
   exactly `git diff <base_sha> <target_sha>`, and the report-only rule: they
   never edit, commit, push or write GitHub state.
3. Wait for both reports. A failed, empty or `INCOMPLETE` report is **not** a
   clean review: fix the cause and relaunch that reviewer once; if it fails
   again, stop and tell the developer.
4. Verify every finding against the code. Address every Critical and every
   reasonable Important finding in **EXACTLY ONE fix commit** (signed and
   DCO-signed-off). No fix commit if there is nothing to fix. Never one commit
   per finding.
5. Run `make check-fmt && make lint && make test && go run ./cmd/okfvalidate ./docs/knowledge`. If it fails, fold the remedy into the fix commit with
   `git commit --amend` (re-sign and re-sign-off); if review found nothing and
   there is no fix commit yet, this remedy becomes the one fix commit. Rerun
   the checks — but **do not rerun the reviewers**. The branch gains **at most one**
   commit after the implementation — the single fix commit, or none at all —
   never more.
6. Open the PR.

**Hard rules.** No local review runs after any individual commit. The
reviewers are **never** rerun on the fix commit. From the moment the PR is
open, **no local reviews of any kind**: iterate only on the PR's bot and human
review feedback, still running tests and checks, and batch each round of fixes
into as few commits as possible.

When the fix commit changes what the code does, the knowledge-bundle rules
above still apply to it.
