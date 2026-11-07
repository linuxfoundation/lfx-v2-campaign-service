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
When the decision was taken every non-test log site under `internal/`, `cmd/`
and `pkg/` already used it, so applying it needed no call-site change.

**Pinned:** `TestProjectIDLogKeyIsUniform` (`pkg/log/project_id_key_test.go`)
parses every non-test Go file under `internal/`, `cmd/` and `pkg/`. It fails,
with file:line, on any string literal passed directly to a slog logger method
(`Info`, `WarnContext`, `With`, ...) or to a `slog.X` attribute constructor
(`slog.String`, `slog.Group`, ...) that spells the project key other than
`project_id`.

**Known blind spots, enforced by review instead:**

- A key built at run time, or held in a constant or variable.
- A key placed in a slice (`[]any{...}`, `[]slog.Attr{...}`) and spread into
  the call.
- A literal value spelled like a project key would be reported as a key.

**For reviewers:** "`project_id` is logged" is not a finding on this repo; see
`docs/reviews/knowledge-base/known-false-positives.md` § "Settled by decision".
A different spelling of the key is a finding, and the test raises it first.

**What this does not cover:** credentials and operator-supplied provider
identifiers are judged per field. The HubSpot portal-mismatch warning logs only
the portal derived from the token, not the configured `portal_id`
(`TestHubSpotProbe_MismatchLogsOnlyTheDerivedPortal`, PR #228), and secrets go
through [`pkg/redact`](../code/pkg-redact.md).
