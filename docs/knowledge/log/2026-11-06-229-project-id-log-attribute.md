# 2026-11-06 — #229: project_id stays in log attributes, under one key

**Update** — issue #229 asked for one answer, applied everywhere, to whether `project_id`
belongs in log attributes (raised as a security nit on PR #228 and declined there because it
was a repo-wide question). The answer is option 1 of the four #229 weighed: **keep it**, in
clear, as `"project_id"`.

- `project_id` is a caller-supplied project UUID or slug, not a credential or PII; it is in the
  URL path of 99 routes (`/projects/{project_id}/...`) and so in every access log already; and
  it is the only field naming whose connection, brief or campaign a warning is about. Hashing
  keeps grouping but loses grep, and dropping or gating it makes alerts tenantless.
- No call site changed: all 212 distinct non-test constant log keys naming the project (as
  counted by the test below) already read `project_id`. The only other `"project"` literal outside tests is a `flag` name in
  `cmd/verify-metrics`, not a log key.
- `TestProjectIDLogKeyIsUniform` (`pkg/log/project_id_key_test.go`) pins it. It type-checks
  every non-test package under `internal/`, `cmd/` and `pkg/` against `go list -export` data,
  and fails on any constant log attribute key that names the project under another spelling
  (`project`, `projectID`, `project_id_hash`, ...).
  - It reads keys by argument type, as slog does: a string takes two slots, and a `slog.Attr`
    (inline, a variable or a helper's result) or any other value takes one.
  - It follows `[]any` slices spread into a log call by variable identity, and reads elided
    `[]slog.Attr` elements and named-constant keys.
  - It matches calls by the function they resolve to, so `http.Error` is not one.
  - `TestLogKeys_ReadsKeyPositionsOnly` pins each rule. Both tests were reworked over two PR
    #539 review rounds: messages and values were first read as keys, and then spread slices,
    shadowed variables and Attr-valued arguments were missed.
- Mutation-checked:
  - Renaming one key to `projectID` in `internal/service/connection.go`, or the spread-slice
    key in `internal/service/meta_ad_sets.go`, fails the guard.
  - Dropping `internal/` from the walk trips the positive-control floor.
  - Each of these fails `TestLogKeys_ReadsKeyPositionsOnly`: reading every argument as a key,
    giving an Attr two slots, dropping spread resolution, keying slice sources by name,
    ignoring elided `slog.Attr` elements, and matching any package's function.
- The decision is written down in `docs/knowledge/architecture/logging-conventions.md` (new)
  and under "Settled by decision" in `docs/reviews/knowledge-base/known-false-positives.md`, so
  a reviewer does not raise it again.
