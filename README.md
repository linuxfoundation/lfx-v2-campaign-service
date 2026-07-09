# LFX V2 Campaign Service

A collection service endpoints to support Marketing Operations campaign creation and management.

## API Endpoints

- `/livez`: `GET` — checks that the service is alive (liveness probe). Returns `200` with a
  `text/plain` body of `OK`.
- `/readyz`: `GET` — checks that the service is able to take inbound requests (readiness probe).
  Returns `200` with a `text/plain` body of `OK` when ready, or `503` when not ready.

Both endpoints are unauthenticated and are excluded from the generated public API documentation.

## Development

Common workflow targets (see the `Makefile` for the full list):

```sh
make apigen        # generate API code from design/ (required before first build)
make fmt           # format Go code (gofmt + simplify)
make check-fmt     # verify formatting (used in CI)
make lint          # run golangci-lint
make test          # run tests with race detector and coverage
make build         # build a local binary
make build-release # build a static release binary for Linux
make run           # build and run locally
```

## Knowledge Base (OKF)

`docs/knowledge/` is an [Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle — plain markdown with YAML frontmatter — that gives humans and AI
agents a structured map of this repo's architecture, Kubernetes resources,
Go packages, and feature specs. Start at
[`docs/knowledge/index.md`](docs/knowledge/index.md).

**When to update it:** after merging a feature PR, changing an API
endpoint, adding or modifying a Helm resource, or changing a package's
responsibility.

**How to update it:**

1. Edit the relevant existing concept file under `docs/knowledge/**`, or add
   a new one with OKF frontmatter (`type`, `title`, `description`) if no
   existing concept covers the change. Do **not** regenerate with
   `go run ./cmd/okfgen` — that tool bootstraps new subtrees and will
   overwrite hand-edited concept files.
2. Add or update the concept's `* [Title](url) - description` bullet in the
   relevant `index.md`.
3. Append a dated entry to `docs/knowledge/log.md`:
   `## YYYY-MM-DD` followed by `**Update** — <what changed and why>.`

**Validate before pushing:**

```sh
go run ./cmd/okfvalidate ./docs/knowledge
```

This is the same check `.github/workflows/validate-okf.yml` runs in CI.

Agents are expected to do this bookkeeping automatically (see `CLAUDE.md`);
developers making manual changes should follow the same convention.
