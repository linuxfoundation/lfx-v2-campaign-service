---
type: "Architecture Doc"
title: "Logging conventions"
description: "Structured-log attribute conventions for the service, starting with the settled #229 decision: the project a line is about is logged in clear as project_id, under that one key everywhere, pinned by TestProjectIDLogKeyIsUniform."
resource: "pkg/log"
---

# Logging conventions

The service logs through `log/slog` with the JSON handler that
[`pkg/log`](../../../pkg/log) installs (`InitStructureLogConfig`), so every
attribute key is a field a log query can filter or group on. A key is
therefore part of the operational contract: two spellings of the same thing
split every query in half.

## `project_id` is logged, in clear, under one key

**Decision (issue #229, 2026-11-06):** a log line about a project carries the
project as the attribute `"project_id"`, with the value exactly as the request
supplied it. It is not hashed, dropped, or gated behind a debug level.

Why this option and not the other three #229 weighed:

- **It is not a secret or PII.** `project_id` is a caller-supplied project UUID
  or slug (`design/connection.go`, the `project_id` attribute: "Project UUID or
  slug that scopes the connection"). It names a foundation's project, not a
  person, and grants nothing.
- **It is already in every access log.** 99 routes carry it in the URL path
  (`/projects/{project_id}/...`), so the gateway and ingress logs hold it for
  every request. Removing it from application logs would hide nothing.
- **It is the triage key.** On a shared-account deployment it is the only
  field saying *whose* connection, brief or campaign failed. A probe or
  dispatch warning without it is unactionable, and an alert fires with no
  tenant.
- **Hashing would be decorative.** A `project_id_hash` keeps grouping but
  loses direct grep; nobody hashes a slug to search logs, so in practice it
  is option "drop it" with extra code.

**One key, everywhere.** The spelling is `project_id`: not `project`,
`projectID`, `projectId`, `project_uid`, `project_slug`, nor a hashed variant.
When the decision was taken every non-test log site already used it (212
distinct constant log keys under `internal/`, `cmd/` and `pkg/`, as counted by
`TestProjectIDLogKeyIsUniform`), so applying it
needed no call-site change.

**Pinned:** `TestProjectIDLogKeyIsUniform` (`pkg/log/project_id_key_test.go`)
type-checks every non-test package under `internal/`, `cmd/` and `pkg/` (against
`go list -export` data) and fails on any constant log attribute key that names
the project under another spelling. It reads keys the way slog does, by argument
type:

- In a log call's arguments after the message, a string is a key and takes two
  slots. A `slog.Attr`, whether built inline, held in a variable or returned by a
  helper, takes one slot, and so does any other value.
- It also reads the first argument of `slog.String`/`Int`/`Any`/`Group`/
  `GroupAttrs` and the like, and the group name passed to `Logger.WithGroup`.
- It reads the `Key` of a `slog.Attr` literal, including one inside
  `[]slog.Attr{{...}}`.
- It follows a `[]any` slice spread into a log call (`fields...`,
  `append(fields, ...)...`), as described next. A shadowing variable counts as
  a different variable.

A spread is resolved by interpreting the enclosing function's statements up to
the log call. The interpreter tracks whole paths: each path holds one concrete
value for every `[]any` variable, so slices assigned together on one branch
stay together, and an alias is followed.

- Statements run in order.
- An `if`, `switch` or `select` runs each branch in sequence from the paths on
  entry, and keeps the union of the paths that fall through. A `return` or
  `panic` ends its path.
- A loop body runs zero or one times, after a `for` loop's initializer. A
  `break` leaves the loop or switch without running the loop's post statement,
  and a `continue` goes on to it.
- A labelled `break` or `continue` targets the loop or switch it names.
- A `fallthrough` carries a clause's paths into the next clause.
- Paths are bounded at 1024. A spread with more fails the guard rather than
  checking a subset. So does any slice the interpreter does not model, rather
  than being read as empty: a spread in a function that contains a `goto` or
  writes `[]any` elements in place (`fields[i] = v`, `copy`, `clear`), a
  non-constant reslice, a multi-value assignment, or a call result from outside
  the package.
- A same-package function's `[]any` result (for example `probeErrAttrs` in
  `internal/infrastructure/postgres`) is resolved by interpreting its body up to
  each `return`. `make([]any, 0, n)` is empty, a constant reslice
  (`fields[:]`, `fields[2:]`) keeps its window, and an indexed literal
  (`[]any{1: v, 0: k}`) is read in index order. One with gaps is untraceable.
- A method expression such as `(*slog.Logger).Info(logger, ...)` has its
  explicit receiver dropped before keys are read.
- A log call inside a closure sees the paths where the closure sits.

Each resulting sequence is resolved in full before it is paired, so a pair split
across a slice and its `append` is paired as slog pairs it, and every
alternative is checked.

An explicit `any("k")` conversion is looked through, because slog classifies
the dynamic value. Calls are matched by the function they resolve to, so `http.Error` is
not a log call. A message or value that reads "project" is not a key and
passes. A key held in a named constant is checked.

**What it does not see:**

- A key built at runtime.
- A key that reaches slog across a function boundary. For example, a wrapper
  `func warn(msg string, args ...any)` that spreads `args...` into slog, or a
  `[]any` passed in as a parameter, or a slice passed to a function that
  rewrites its elements. The guard traces a spread only within the
  enclosing function, where a parameter has no known contents.

The repo has no such logging wrapper today. The one slice that is handed to a
helper (`logFields` in `internal/service/meta_ad_sets.go`) is checked where it
is built, because it is also spread into slog there. Add a wrapper only with a
matching extension to the guard.
`TestLogKeys_ReadsKeyPositionsOnly` pins each rule. A floor on the number of
`project_id` keys seen keeps the walk from passing vacuously.

**For reviewers:** "`project_id` is logged" is not a finding on this repo; see
`docs/reviews/knowledge-base/known-false-positives.md` § "Settled by decision".
A different spelling of the key is a finding, and the test raises it first.

**What this does not cover:** credentials and operator-supplied provider
identifiers are judged per field. The HubSpot portal-mismatch warning logs only
the portal derived from the token, not the configured `portal_id`
(`TestHubSpotProbe_MismatchLogsOnlyTheDerivedPortal`, PR #228), and secrets go
through [`pkg/redact`](../code/pkg-redact.md).
