# 2026-11-06 — #229: project_id stays in log attributes, under one key

**Update** — issue #229 asked for one answer, applied everywhere, to whether `project_id`
belongs in log attributes (raised as a security nit on PR #228 and declined there because it
was a repo-wide question). The answer is option 1 of the four #229 weighed: **keep it**, in
clear, as `"project_id"`.

- `project_id` is a caller-supplied project UUID or slug, not a credential or PII; it is in the
  URL path of 99 routes (`/projects/{project_id}/...`) and so in every access log already; and
  it is the only field naming whose connection, brief or campaign a warning is about. Hashing
  keeps grouping but loses grep, and dropping or gating it makes alerts tenantless.
- No call site changed: all 204 non-test string-literal log keys naming the project (as counted by
  the test below) already read `project_id`. The only other `"project"` literal outside tests is a `flag` name in
  `cmd/verify-metrics`, not a log key.
- `TestProjectIDLogKeyIsUniform` (`pkg/log/project_id_key_test.go`) pins it: it parses every
  non-test file under `internal/`, `cmd/` and `pkg/` and fails on any slog call or `slog.Attr`
  key that names the project under another spelling (`project`, `projectID`,
  `project_id_hash`, ...). Mutation-checked: renaming one key in
  `internal/service/connection.go` to `projectID` fails it, and so does dropping `internal/`
  from the walk (the positive-control floor).
- The decision is written down in `docs/knowledge/architecture/logging-conventions.md` (new)
  and under "Settled by decision" in `docs/reviews/knowledge-base/known-false-positives.md`, so
  a reviewer does not raise it again.
