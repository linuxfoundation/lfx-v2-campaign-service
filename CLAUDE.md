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

## Review lifecycle configuration

Load and follow `/lfx-skills:lfx-local-review` as the sole owner of the review
lifecycle. The values below configure that skill and do not replace or override
its instructions.

- repo code reviewer: `/campaign-service-code-reviewer`
- repo learnings reviewer: `/campaign-service-learnings-reviewer`
- readiness action: `go run ./cmd/okfvalidate ./docs/knowledge`
- preflight action: `make check-fmt lint test build`
- post-PR extension: `none`
