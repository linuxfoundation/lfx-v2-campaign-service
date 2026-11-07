# 2026-11-06 — #229: project_id stays in log attributes, under one key

**Update** — issue #229 asked for one answer, applied everywhere, to whether `project_id`
belongs in log attributes (raised as a security nit on PR #228 and declined there because it
was a repo-wide question). The answer is option 1 of the four #229 weighed: **keep it**, in
clear, as `"project_id"`.

- `project_id` is a caller-supplied project UUID or slug, not a credential or PII; it is in the
  URL path of 99 routes (`/projects/{project_id}/...`) and so in every access log already; and
  it is the only field naming whose connection, brief or campaign a warning is about. Hashing
  keeps grouping but loses grep, and dropping or gating it makes alerts tenantless.
- No call site changed: every non-test log key naming the project already read `project_id`.
  The only other `"project"` literal outside tests is a `flag` name in `cmd/verify-metrics`,
  not a log key.
- `TestProjectIDLogKeyIsUniform` (`pkg/log/project_id_key_test.go`) is a small guard. It parses
  every non-test Go file under `internal/`, `cmd/` and `pkg/`. It fails, with file:line, on any
  string literal passed directly to a slog logger method, or to a `slog.X` attribute
  constructor, that spells the project key other than `project_id` (`project`, `projectID`,
  `project_id_hash`, ...).
  - Known blind spots: keys built at run time, held in constants or variables, or placed in a
    slice and spread into the call are not checked. For those, the rule is enforced by review.
  - Mutation-checked: renaming the key at `internal/service/connection.go:444` to `projectID`
    fails it with that file and line.
  - A much larger, path-sensitive version was built during PR #539 review. It was replaced
    with this one by owner decision, to keep the guard small enough to review.
- The decision is written down in `docs/knowledge/architecture/logging-conventions.md` (new)
  and under "Settled by decision" in `docs/reviews/knowledge-base/known-false-positives.md`, so
  a reviewer does not raise it again.
