# Copilot instructions — LFX V2 Campaign Service

Backend for LFX Self Serve marketing campaign operations: a **Go 1.25 / Goa**
HTTP API deployed via Helm, brokering between the LFX UI and paid advertising
platforms. Module path: `github.com/linuxfoundation/lfx-v2-campaign-service`.

Use these instructions when reviewing pull requests and answering questions
about this repository.

## Orient before reviewing

Before reasoning about a change, consult
[`docs/knowledge/index.md`](../docs/knowledge/index.md) — an
[Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
bundle that maps this repo's architecture, Kubernetes/Helm resources, Go
packages, and feature specs. It is the fastest way to understand a package's
role without loading the whole tree. Ground review comments in the concept
docs it links (e.g. `architecture/overview.md`, `code/*.md`,
`architecture/channel-connections-schema.md`).

## What to prioritize in reviews

- **Correctness & concurrency:** goroutine safety, context propagation and
  cancellation, error wrapping (`fmt.Errorf("...: %w", err)`), no swallowed
  errors, and proper resource cleanup (`defer Close()`, pool release).
- **Security & secrets:** never log or echo `PGPASSWORD`, DSNs, or the
  `CREDENTIAL_ENCRYPTION_KEY`. Ad-platform connection credentials are
  AES-256 encrypted at rest — flag any code path that would persist or emit
  them in plaintext. No secrets committed to source, fixtures, or test data.
- **Database:** repositories live under
  `internal/infrastructure/postgres`. Schema changes must ship a
  `golang-migrate` migration (paired up/down). Watch for SQL injection,
  unbounded queries, and missing tx handling.
- **Health probes:** `/livez` must stay process-only (no DB dependency);
  `/readyz` includes PostgreSQL connectivity. Don't couple liveness to the
  pool. No-DB mode (all `PG*` omitted) must keep `/readyz` process-ready.
- **Config precedence:** CLI flags > environment variables > defaults. When
  any PostgreSQL setting is supplied the full set is required. Constants for
  env var names live in `pkg/constants`.
- **Logging:** use the structured `pkg/log` helpers with context; avoid ad
  hoc `fmt.Println`/`log` and don't leak sensitive fields.

## Repo conventions to enforce

- **Generated code is off-limits by hand.** Everything under `gen/` is
  produced by Goa from the `design/` DSL. Review the `design/` change and
  confirm `make apigen` was re-run; flag edits made directly to `gen/`.
- **License headers required.** Every Go source file starts with:

  ```go
  // Copyright The Linux Foundation and each contributor to LFX.
  // SPDX-License-Identifier: MIT
  ```

  A CI job (`license-header-check`) enforces this — flag missing headers.
- **Formatting & lint:** code must pass `make check-fmt` (gofmt + simplify)
  and `make lint` (golangci-lint; see also `revive.toml`). MegaLinter and
  gitleaks/secretlint run in CI — call out likely failures.
- **Tests:** run via `make test`. New behavior needs table-driven Go tests;
  keep liveness/readiness and config-precedence paths covered.
- **Knowledge base upkeep:** when a PR changes architecture, a Helm
  manifest, a Go package's role, or fixes a notable bug, expect a matching
  update under `docs/knowledge/**`, its `index.md` bullet, and a dated entry
  in `docs/knowledge/log.md` (validated by `go run ./cmd/okfvalidate`). Note
  when this is missing. Do **not** expect `cmd/okfgen` to be re-run for edits
  — it regenerates the whole bundle and clobbers hand-edited concepts.

## Common make targets

`make apigen` (Goa codegen) · `make fmt` / `make check-fmt` · `make lint` ·
`make test` · `make build` · `make run` · `make all` (clean → apigen → fmt →
lint → test → build). Helm: `make helm-templates`, `make helm-install-local`.

## Style of review feedback

Be specific and actionable; cite the file/line and the concrete risk. Skip
nitpicks already covered by gofmt/golangci-lint. Prefer a small number of
high-signal comments over exhaustive low-value ones.
