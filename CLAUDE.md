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
[`specs/001-health-endpoints/`](specs/001-health-endpoints/plan.md).

## Development

See `README.md` for the `make` targets used to build, test, lint, and run
the service.
