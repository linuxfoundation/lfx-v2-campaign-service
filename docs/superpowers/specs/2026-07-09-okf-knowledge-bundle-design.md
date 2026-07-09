# OKF Knowledge Bundle + Validator — Design

Date: 2026-07-09

## Goal

Establish a baseline [Open Knowledge Format (OKF)](https://github.com/GoogleCloudPlatform/knowledge-catalog/tree/main/okf)
knowledge bundle for this repository so AI agents have a fast, structured map of the
codebase (docs, Kubernetes resources, Go packages, feature specs) without loading the
entire repo into context. Add a CI check that validates the bundle stays OKF-conformant.
Update `CLAUDE.md` (and a symlinked `AGENTS.md`) to point agents at the bundle and
establish the ongoing maintenance convention.

## OKF conformance rules (v0.1, relevant subset)

- Every non-reserved `.md` file in the bundle must contain a parseable YAML frontmatter
  block with a non-empty `type` field.
- Reserved filenames: `index.md` (directory listing, no frontmatter except optional
  `okf_version` at the bundle root) and `log.md` (chronological history, no frontmatter).
- `index.md` bullet format: `* [Title](url) - description`.
- `log.md` entries: `##`-level ISO 8601 (`YYYY-MM-DD`) date headings, newest first,
  optionally leading with a bold action word (`**Update**`, `**Creation**`, etc.).
- Recommended frontmatter fields beyond `type`: `title`, `description`, `resource`,
  `tags`, `timestamp`.
- Consumers must tolerate unknown types/fields, missing optional fields, and broken
  links — the format favors permissive consumption over strict schemas.

## Directory layout

```
docs/knowledge/
├── index.md
├── log.md
├── architecture/
│   ├── index.md
│   ├── overview.md                   # wraps docs/architecture.md
│   ├── api-catalog.md                # wraps docs/api-catalog.md
│   ├── channel-connections-schema.md # wraps docs/channel-connections-schema.md
│   └── build-summary.md              # wraps docs/build-summary.md
├── kubernetes/
│   ├── index.md
│   └── <one .md per template in charts/lfx-v2-campaign-service/templates/*.yaml>
├── code/
│   ├── index.md
│   └── <one .md per Go package dir: cmd/, internal/*, pkg/*, design/>
└── specs/
    ├── index.md
    └── 001-health-endpoints/
        ├── index.md
        └── <one .md per spec/plan/tasks/data-model/quickstart/research file>
```

Each concept file is a **thin wrapper**: OKF frontmatter + a short summary + a
relative markdown link back to the real source file. Original files
(`docs/*.md`, Helm templates, Go source, speckit docs) remain canonical and
untouched — the knowledge bundle links to them rather than duplicating their
content, avoiding drift on files that already exist and change independently.

## Generator: `cmd/okfgen`

A one-time-use Go program (`go run ./cmd/okfgen`) that builds the initial bundle:

- **Docs wrapper** — for each `docs/*.md`: extract the first `#` heading as `title`,
  first sentence of body as `description`, `type: "Architecture Doc"`; write
  `docs/knowledge/architecture/<name>.md` with frontmatter + short summary + link
  back to the original file.
- **Kubernetes wrapper** — for each `charts/lfx-v2-campaign-service/templates/*.yaml`:
  regex-extract `kind:` (best-effort — these are Helm templates with `{{ }}`
  interpolation and are not valid standalone YAML) plus any leading comment;
  `type: "Kubernetes Resource"`; one concept file per template, linking back to
  the chart file.
- **Go package wrapper** — for each package directory (`cmd/campaign-service`,
  `internal/container`, `internal/infrastructure`, `internal/middleware`,
  `internal/service`, `pkg/constants`, `pkg/log`, `pkg/utils`, `design`): use
  `go/parser` to extract the package doc comment (if present) and list of exported
  declarations; `type: "Go Package"`; link back to the directory.
- **Spec wrapper** — for each file under `specs/001-health-endpoints/`
  (`spec.md`, `plan.md`, `tasks.md`, `data-model.md`, `quickstart.md`,
  `research.md`): `type: "Feature Spec"`.
- **Index generation** — after concepts are written, generate `index.md` in every
  directory (including bundle root) per OKF §6: a heading plus
  `* [Title](relative-link) - description` bullets sourced from each concept's
  frontmatter.
- **log.md seed** — one initial entry:
  `## 2026-07-09` / `**Creation** — initial OKF knowledge bundle generated from
  existing docs, Helm charts, Go packages, and speckit specs.`

This tool is meant to be run once (or deliberately re-run to bootstrap a new
subfolder). Re-running will overwrite hand-edited concept files — this tradeoff is
documented in the tool's doc comment / `--help` output. It is **not** invoked by CI.

## Validator: `cmd/okfvalidate` + CI

`go run ./cmd/okfvalidate <bundle-dir>` checks OKF §9 conformance:

1. Every non-reserved `.md` under the bundle has a parseable YAML frontmatter block.
2. That frontmatter has a non-empty `type` field.
3. `index.md` files have no frontmatter, except the bundle-root `index.md`, which
   may declare `okf_version`.
4. `index.md` bullet lines follow `* [Title](url) - description`.
5. `log.md` entries are `##`-level ISO 8601 date headings, newest first.

Exits non-zero with descriptive errors (file + rule violated) on any failure.

`.github/workflows/validate-okf.yml`: triggers on pull requests touching
`docs/knowledge/**`; sets up Go at the version pinned in `go.mod`; runs
`go run ./cmd/okfvalidate ./docs/knowledge`.

## `CLAUDE.md` / `AGENTS.md`

Replace the current one-line speckit pointer in `CLAUDE.md` with:

- A short repo/stack summary (Go service, Goa-generated API, Helm chart deployment).
- **Primary instruction:** consult `docs/knowledge/index.md` first to find relevant
  context before reading source files directly.
- **Maintenance instruction:** after merging a PR, updating a Helm manifest, or
  fixing a bug, update the relevant `docs/knowledge/**` concept file and append an
  entry to `docs/knowledge/log.md`.
- A retained reference to `specs/001-health-endpoints/plan.md` as the current
  active feature spec (no longer the sole content of the file).

`AGENTS.md` becomes a symlink to `CLAUDE.md` (`ln -s CLAUDE.md AGENTS.md`).

## Out of scope

- Rewriting or migrating existing `docs/*.md` content — they stay canonical.
- Parsing `gen/` (generated Goa code) into concepts — it's derived, not source of
  truth.
- Any automation that auto-runs `okfgen` on merge; ongoing updates are agent-driven
  edits per the `CLAUDE.md` maintenance instruction, not regeneration.
