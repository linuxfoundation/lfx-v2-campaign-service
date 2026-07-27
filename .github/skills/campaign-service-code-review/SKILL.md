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

Reviewer scope and the signal bar are owned by the `copilot-code-reviewer`
skill (`.github/skills/copilot-code-reviewer/SKILL.md`); this skill covers only
the line-level method. A diff alone is not enough: for each non-trivial hunk
read the **whole changed function** plus the layers around it — for a handler,
the design method and the repository call beneath; for a dispatch change, the
orchestrator contract above and the platform client below — and search for
**callers and sibling implementations** of the same pattern to confirm the
change matches how the repo already does it. Convention drift is a finding even
when the code "works", and because the per-provider packages are deliberately
parallel the sibling is usually right there: a divergence is either justified in
the code or a finding.

## The house standards

The repo defines its own standards; hold the diff to them, and name the
documented source in any standards finding. Read the parts relevant to the diff
before judging, every run, because the standards belong to the repo and move
with it. They live in:

- **`CLAUDE.md`** — the agent guide: start from the knowledge bundle, and the
  knowledge-base upkeep contract.
- **`README.md`** — the environment-variable contract with its defaults and
  required sets, the local run and Helm workflows, and the `make` targets.
- **`docs/`** — `architecture.md` (the numbered architectural decisions, the
  persistence model, the authorization model), `api-catalog.md` (the API design
  rules every endpoint follows, and the endpoint catalog), and
  `channel-connections-schema.md` (the table definitions).
- **`docs/knowledge/`** — the Open Knowledge Format bundle, with concept files
  covering the service's main packages and its Kubernetes resources: the fastest
  way to check what a package is *supposed* to do before judging the diff.

Enforcement runs in both directions: code that violates a documented standard is
a finding, and a documented standard the code has visibly outgrown is a finding
against the docs. If a documented convention is wrong for this specific change,
say so explicitly and explain the trade, rather than silently waiving or
silently enforcing it.

Read the rest of this skill as invariants, not as an inventory. This codebase
carries deliberate, reasoned exceptions to most of its own patterns, so those
sources — not this file — are the authority for the shape a given endpoint,
package, or call has today. Check them before calling a deviation a defect: an
exception the code justifies is not a finding; one that has quietly lost its
justification is.

## Quality dimensions

Run these on the changed code, scaled to the size of the change:

- **Correctness and concurrency.** A context reaches every call that can block
  and cancellation is honored — except where the code deliberately detaches, a
  real pattern here for work that must finish after its caller is gone. There,
  and for a goroutine that outlives its request, the question is whether the
  work carries its own deadline and a bounded lifetime, not whether it detached.
  Shared state that is late-bound or swapped at runtime is accessed under the
  lock that guards it; fan-out across platforms is bounded; every acquired
  resource is released on every path — `defer` where its lifetime genuinely is
  the function's, an explicit release per iteration inside a loop or a retry.
- **Error handling.** Errors wrapped with `%w` so sentinels survive, failures
  distinguished by remedy rather than collapsed. A discarded error is a finding
  when the caller needed it, not on sight — best-effort cleanup and parsing are
  deliberate here, and an error string that drops detail to avoid carrying a
  secret is doing its job. The domain package defines the sentinels and the
  service layer maps them onto the contract's typed errors; a new failure mode
  reaching the API needs that mapping, or it surfaces as an untyped 500 the
  published contract never promised.
- **Tests.** New or changed behavior needs tests that assert real behavior, not
  that a mock was called; the repo's style is table-driven Go tests beside the
  code. A test that introduces shared mutable state without synchronization is a
  defect whether or not the suite runs under a race detector. Missing tests on a
  contract-bearing, concurrency-bearing, or security-sensitive path are worth
  flagging.
- **Performance.** Unbounded result sets, a query per row where a set operation
  would do, an HTTP client or credential decryption rebuilt per request when it
  could be hoisted, and an outbound call to an ad platform with no timeout — a
  hung upstream must not become a hung handler. Retries are not universal:
  whether a call may be retried depends on the upstream operation's idempotency,
  which the clients decide per method, deliberately. Read the client's own retry
  path before calling a missing retry a defect — where a retry could
  double-create, suppressing it is correct and adding one is the finding.
- **Readability and structure.** The change reads like the surrounding code;
  names say what a thing is or does; comments explain *why*; duplicated logic
  that wants a shared helper is a finding when it traps the next editor.
- **Code truthfulness.** Comments and docs match what the code actually does.
  This repo leans hard on long explanatory comments, so a comment that has
  silently gone stale is more dangerous here than usual — one describing an
  invariant the diff just changed is a finding.

## Campaign service specifics worth a second look

- **The generated boundary.** `design/` is where the contract is authored;
  everything under `gen/` is Goa output and is not hand-edited, so editing a
  generated file is a defect — as is a `design/` change whose regenerated output
  is missing from the PR, since the build regenerates before compiling and CI
  will not catch that drift for you. Tests that exercise generated validators
  belong outside the generated tree.
- **Contract, schema, and docs move together.** A new or changed field usually
  touches the design, a paired up/down migration, the repository's column
  handling, and the documented schema; a missing down file, or a schema change
  whose docs are untouched, is worth raising. Storage and contract are
  deliberately *not* one-to-one, though — internal-only columns exist, and a
  credential column is one the API must never expose. Ask which surfaces a new
  column is *meant* to reach, from the documented schema and API design rules.
- **Optimistic concurrency.** Rows carry a version that powers ETag and
  `If-Match`, with distinct statuses for a missing precondition and a stale one.
  That contract governs the *conditional* paths only; check the documented API
  design rules for the shape an endpoint is meant to have rather than assuming
  every mutation is conditional. A conditional path that skips the precondition,
  or fails to distinguish "not found" from "version mismatch", breaks a contract
  the callers depend on.
- **Connection identity is keyed on the project.** That identifier scopes the
  rows and is the exact-match key the dispatch path joins on, so a change to how
  it is validated or normalised on any route changes which records are reachable
  — say so rather than treating it as a validation tweak. The uniqueness the
  schema enforces on it is narrower than "one row per provider, ever"; read the
  documented schema for the constraint as it stands.
- **Startup and shutdown are load-bearing.** The service supports a database
  cold start: it boots serving the contract's typed 503, retries in the
  background, and late-binds live backends without a pod restart; a migration
  failure a retry could never clear is deliberately not retried. The shutdown
  budget is arithmetic between named constants, so changing one of those
  timeouts is a behavior change even when the code compiles — ask whether it is
  stated, intentional, and bounded in blast radius.
- **The probes mean different things.** Liveness is process-only and must not
  acquire a database dependency; readiness reflects the database when one is
  configured. A database-less run is supported, not broken — a change that makes
  readiness fail in that mode, or couples liveness to the pool, breaks how
  Kubernetes treats the pod.
- **Configuration.** CLI flags win over environment variables, which win over
  defaults, and a partially supplied database configuration is rejected at
  startup rather than silently half-applied. The server and database settings
  resolve their environment-variable names through the shared constants package
  rather than string literals at the use site; that convention is not repo-wide,
  so judge a new setting against how its own package resolves configuration and
  against the documented environment-variable contract.
- **Logging.** Structured `slog`, using the context-carrying call forms wherever
  a request context is in hand, so the request ID the middleware attaches lands
  on the line, and the plain forms where there genuinely is none. An
  unstructured message where fields belong, or a log line carrying a credential,
  a key, or a DSN, is a finding. So is printing to stdout from the service; in a
  command whose output *is* stdout, it is the point.
- **Knowledge-base upkeep.** The agent guide asks a merged change to leave the
  knowledge bundle current — concept file updated, containing index bullet
  updated only when a concept was added, renamed, or re-described, dated change
  log entry appended — and expects the bundle validated rather than regenerated,
  because the generator overwrites hand-edited concept files. Read the guide for
  the contract as it stands, note a genuinely missing update, and never ask for
  a regeneration.

## Security anchors

Hold these lines on any diff that touches them:

- **Credentials are encrypted in the application, before storage.** The
  encryption key comes from the environment and is deliberately never given to
  the database. Any path that would write a credential unencrypted, return one
  in a response body, or put one in a log or an error message is a security
  defect. Responses report *whether* a connection holds credentials, not the
  credentials themselves — preserve that shape. Credential-shaped literals in
  tests are judged on whether they are real, per the repository anchor below.
- **Secrets stay out of logs and out of error strings.** The config type does
  carry secret material — the composed database DSN, password included, and the
  credential encryption key — and keeps it out of formatted output by masking
  those fields in its own formatting methods. That holds only where it is
  actually reached: a new secret-bearing field the masking misses, a formatting
  path that sidesteps the type's own, or an error or log line interpolating a
  DSN or a key, defeats it. Prefer a purpose-built safe-to-log accessor over
  formatting the config itself.
- **Secrets stay out of the repository.** No *real* credential in source,
  fixtures, values files, or tests. Unmistakably fake test data is expected and
  is never a finding on its own — the crypto round-trip and the platform client
  tests cannot be written without credential-shaped inputs. A sample key for
  local development is acceptable only while unmistakably marked non-production.
- **Authorization happens at the gateway, not in this process.** Heimdall
  authenticates and OpenFGA authorizes on a project relation before a request
  arrives, so the in-process JWT handling exists to require a token's presence
  and extract principal claims for *attribution*. Do not let a diff turn those
  claims into an authorization decision or trusted identity — they are not
  verified here, and the code says so — or make a route reachable off that path.
- **The chart's route and its authorization rules must stay in agreement.** They
  are two hand-maintained matchers over the same path set, coupled only by a
  parity test in the chart. Because the rule engine is default-deny, a path the
  route forwards but no rule authorizes becomes unreachable rather than open, and
  the inverse edit is dead config; either drift is a finding, so any new endpoint
  family needs both sides considered.
- **SQL is parameterized for values, and identifiers come from fixed internal
  allowlists.** The repositories build per-provider statements from compile-time
  column and table names while binding every value as a placeholder. New query
  code must keep that split; an identifier assembled from anything a caller can
  influence is an injection finding, and a suppressed linter warning on such a
  line needs its justification checked, not assumed.
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
- **Know your limits.** Comment only on what you can establish from the code in
  front of you; a judgment resting on a surface you cannot read does not clear
  the confidence bar in `copilot-code-reviewer`, hedged or not.
