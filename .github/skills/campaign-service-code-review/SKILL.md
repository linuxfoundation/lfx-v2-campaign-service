---
name: campaign-service-code-review
description: >
  How to judge the implementation of an lfx-v2-campaign-service pull request:
  the Go quality dimensions (correctness, concurrency, error handling, tests,
  performance, readability, code truthfulness), how to hold the diff to the
  repo's documented standards, and the security anchors for a service that
  brokers to paid ad platforms and stores encrypted credentials. Use on every
  PR that changes code, however small; this is the reviewer's line-level lens.
---

<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Campaign Service Code Review

The `/copilot-code-reviewer` skill owns the reviewer's scope and signal
discipline; this skill owns the line-level method. Read enough surrounding code
to judge each hunk in its real context — for a handler change, the design
method, the generated interface it satisfies, and the repository call beneath
it; for a dispatch change, the orchestrator contract above and the platform
client below.

A diff alone is not enough. For each non-trivial hunk, read the **whole changed
function**, not just the diff lines, and search for **callers and sibling
implementations** of the same pattern to confirm the change matches how the repo
already does it — convention drift is a finding even when the code "works". The
per-provider packages are deliberately parallel, so the sibling is usually right
there: if the change diverges from the other providers, either the divergence is
justified in the code or it is a finding.

## The house standards

The repo defines its own standards; hold the diff to them, and name the
documented source in any standards finding. Read the parts relevant to the diff
before judging, every run, because the standards belong to the repo and move
with it. They live in:

- **`CLAUDE.md`** — the agent guide: start from the knowledge bundle, and the
  knowledge-base upkeep contract.
- **`README.md`** — the environment-variable contract with its defaults and
  required sets, the local run and Helm workflows, and the `make` targets.
- **`docs/`** — `architecture.md` (the numbered architectural decisions and
  their rationale, the persistence model, the authorization model),
  `api-catalog.md` (the API design rules every endpoint follows and the
  endpoint catalog), and `channel-connections-schema.md` (the table
  definitions).
- **`docs/knowledge/`** — the Open Knowledge Format bundle, with a concept file
  per package and per Kubernetes resource. It is the fastest way to check what a
  package is *supposed* to do before judging whether the diff belongs there.

Enforcement runs in both directions: code that violates a documented standard is
a finding, and a documented standard the code has visibly outgrown is a finding
against the docs. If a documented convention is wrong for this specific change,
say so explicitly and explain the trade, rather than silently waiving or
silently enforcing it.

## Quality dimensions

Run these on the changed code, scaled to the size of the change:

- **Correctness and concurrency.** Context propagated into every call that can
  block, and cancellation honored; goroutines with a defined lifetime and a way
  to stop; shared state that is late-bound or swapped at runtime accessed under
  the lock that guards it; bounded parallelism where the code fans out to
  several platforms; `defer` for every acquired resource. A detached goroutine
  that deliberately outlives its request context is a real pattern here — the
  question is whether its lifetime is bounded and its shutdown accounted for,
  not whether it exists.
- **Error handling.** Errors wrapped with `%w` so sentinels survive; nothing
  swallowed; failures distinguished by remedy, not collapsed. The domain package
  defines the sentinel errors and the service layer maps them onto the
  contract's typed errors — a new failure mode that reaches the API needs a
  mapping there, otherwise it surfaces as an untyped 500 that the published
  contract never promised.
- **Tests.** New or changed behavior needs tests that assert real behavior, not
  that a mock was called; the repo's style is table-driven Go tests beside the
  code. The suite runs with the race detector, so a test that introduces shared
  mutable state without synchronization is a defect. Missing tests on a
  contract-bearing, concurrency-bearing, or security-sensitive path is always
  worth flagging.
- **Performance.** Unbounded result sets, a query per row where a set operation
  would do, an HTTP client or credential decryption rebuilt per request when it
  could be hoisted, and outbound calls to an ad platform without a timeout or a
  retry budget — a hung upstream must not become a hung handler.
- **Readability and structure.** The change reads like the surrounding code;
  names say what a thing is or does; comments explain *why*, which is this
  repo's prevailing style and worth preserving; duplicated logic that wants a
  shared helper is a finding when it traps the next editor.
- **Code truthfulness.** Comments, docs, and the PR description match what the
  code actually does. This repo leans hard on long explanatory comments, so a
  comment that has silently gone stale is more dangerous here than usual — a
  comment describing an invariant the diff just changed is a finding.

## Campaign service specifics worth a second look

- **The generated boundary.** `design/` is where the contract is authored;
  everything under `gen/` is Goa output and is not hand-edited. A change that
  edits generated files directly is a defect. So is a `design/` change whose
  regenerated output is missing from the PR — the build regenerates before
  compiling, so CI will not catch that drift for you. Tests that need to
  exercise generated validators belong outside the generated tree.
- **Contract, schema, and docs move together.** A new or changed field usually
  touches the design, a paired up/down migration, the repository's column
  handling, and the documented schema. A migration without its down file, a
  column the repository writes but the contract never exposes, or a schema
  change whose documentation is untouched are each worth raising.
- **Optimistic concurrency.** Rows carry a version that powers ETag and
  `If-Match` on conditional updates, with distinct statuses for a missing
  precondition and a stale one. A new mutating endpoint that skips this, or an
  update that fails to distinguish "not found" from "version mismatch", breaks a
  contract the callers depend on.
- **Connections are singleton per provider per project**, keyed by the project
  identifier, and that identifier is the exact-match key the dispatch path joins
  on. The create routes therefore constrain it more tightly than the read and
  update routes, which stay permissive for historical rows. A change that
  loosens the create-side constraint or tightens the read-side one changes which
  records can be reached — say so.
- **Startup and shutdown are load-bearing.** The service supports a database
  cold start: it boots serving the contract's typed 503, retries in the
  background, and late-binds live backends without a pod restart; a migration
  failure a retry could never clear is deliberately not retried. The shutdown
  budget is arithmetic between named constants, guarded by a startup assertion.
  Changing one of those timeouts is a behavior change even when the code still
  compiles — ask whether it is stated and intentional, and what its blast radius
  is.
- **The probes mean different things.** Liveness is process-only and must not
  acquire a database dependency; readiness reflects the database when one is
  configured. A database-less run is a supported mode, not a broken one — a
  change that makes readiness fail in that mode, or that couples liveness to the
  pool, breaks how Kubernetes treats the pod.
- **Configuration.** CLI flags win over environment variables, which win over
  defaults; the service's configuration variable names live as constants in the
  shared constants package rather than as string literals at the use site (the
  logging package's own level switches are the standing exception); and a
  partially supplied database configuration is rejected at startup rather than
  silently half-applied. A new setting that skips any of those is a finding.
- **Logging.** Structured `slog`, using the context-carrying call forms wherever
  a request context is in hand, so the request ID the middleware attaches lands
  on the line. Ad-hoc printing, an unstructured message where fields belong, or
  a log line that carries a credential, a key, or a DSN is a finding.
- **Knowledge-base upkeep.** When a PR changes architecture, a Kubernetes
  manifest, or a package's responsibility, the repo expects a matching update
  under `docs/knowledge/`, its index entry, and a dated entry in the change log
  — and expects the bundle to be validated rather than regenerated, because the
  generator overwrites hand-edited concept files. Note a missing update; do not
  ask for a regeneration.

## Security anchors

This service brokers to paid advertising platforms and stores platform
credentials. Hold these lines on any diff that touches them:

- **Credentials are encrypted in the application, before storage.** The
  encryption key comes from the environment and is deliberately never given to
  the database. Any path that would write a credential unencrypted, return one
  in a response body, or put one in a log, an error message, or a test fixture
  is a security defect. Responses report *whether* a connection holds
  credentials, not the credentials themselves — preserve that shape.
- **Secrets stay out of logs and out of error strings.** The config type
  redacts its own secret fields when formatted, and the database password is not
  retained on it at all. A new secret-bearing field that is not covered by that
  redaction, or a new error that interpolates a DSN or a key, defeats it.
- **Secrets stay out of the repository.** No real credential in source,
  fixtures, values files, or tests. A sample key that exists for local
  development is only acceptable while it is unmistakably marked as
  non-production.
- **Authorization happens at the gateway, not in this process.** Heimdall
  authenticates and OpenFGA authorizes on a project relation before a request
  arrives, so the in-process JWT handling exists to require a token's presence
  and to extract principal claims for *attribution*. Do not let a diff turn
  those claims into an authorization decision or into trusted identity — they
  are not verified here, and the code says so. Equally, do not accept a change
  that would make a route reachable outside that gateway path.
- **The chart's route and its authorization rules must stay in agreement.**
  They are two hand-maintained matchers over the same path set, coupled only by
  a parity test in the chart. Because the rule engine is default-deny, a path the
  route forwards but no rule authorizes becomes unreachable rather than open —
  but the inverse edit is dead config, and either drift is a finding. Any new
  endpoint family needs both sides considered.
- **SQL is parameterized for values, and identifiers come from fixed internal
  allowlists.** The repositories build per-provider statements from compile-time
  column and table names while binding every value as a placeholder. New query
  code must keep that split; a table or column name assembled from anything a
  caller can influence is an injection finding, and a suppressed linter warning
  on such a line needs its justification checked, not assumed.
- **Migrations are paired and run at startup.** An irreversible or unpaired
  migration, or one that takes a lock the startup deadline cannot absorb, is a
  production risk worth raising even though it will pass CI.

## Judgment calls

- **Point at the working pattern.** When the diff violates a pattern, cite the
  sibling provider, repository, or adapter that gets it right rather than
  describing an abstract ideal.
- **Do not propose rewrites of a sound approach**, and do not suggest change for
  its own sake; working, readable code needs no improvement. The repetitive
  per-provider shape in this repo is intentional — do not ask for it to be
  collapsed.
- **Know your limits.** Distinguish "this is wrong" from "this might be a
  problem depending on context", and say which one you mean. When a judgment
  depends on something you cannot see — the platform FGA model, a deployed Helm
  value, an ad platform's live API behavior, the calling UI — note the
  dependency rather than asserting a defect you cannot confirm.
