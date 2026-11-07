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
  - It follows `[]any` slices spread into a log call by variable identity. It resolves the
    whole spread sequence before pairing, so a pair split across a slice and its `append` is
    read correctly. The slice is traced by interpreting the enclosing function's statements
    up to the call, tracking whole paths, so slices assigned together stay together.
    Branches run in sequence and the paths that fall through are joined (a `return` ends a
    path, as does a `panic`), loops run zero or one times after a `for` initializer, a
    `break` skips the post statement, a labelled `break`/`continue` targets its named loop,
    `fallthrough` carries paths into the next clause,
    and aliases are followed. More than 1024 paths fails the guard (pinned by
    `TestLogKeys_PathOverflowIsReported`) rather than checking a subset, and so does a
    spread in a function with a `goto` or an in-place `[]any` element write
    (`TestLogKeys_GotoIsReportedUntraceable`, `TestLogKeys_UnmodelledSlicesAreReportedUntraceable`),
    as does any other slice change it does not model (a non-constant reslice, a multi-value or
    out-of-package call result).
  - A same-package helper's `[]any` result is resolved through its body, so the keys
    `probeErrAttrs` builds are now checked. `make([]any, 0, n)` is empty, constant reslices
    keep their window, and a method expression's explicit receiver is dropped.
    Every resulting path is checked.
  - It looks through `any("k")` conversions, and reads elided `[]slog.Attr` elements,
    named-constant keys, and group names (`slog.Group`, `slog.GroupAttrs`,
    `Logger.WithGroup`).
  - It does not see keys that reach slog through another function's parameter (a `...any`
    wrapper, a slice parameter). The repo has no such wrapper, and the logging-conventions
    page states the limit.
  - It matches calls by the function they resolve to, so `http.Error` is not one.
  - `TestLogKeys_ReadsKeyPositionsOnly` pins each rule. Both tests were reworked over the PR
    #539 review rounds. Messages and values were first read as keys. Then spread slices,
    shadowed variables and Attr-valued arguments were missed. Then pairs split across an
    append, and interface conversions, were mis-paired. Then conditional reassignments
    were dropped, and appends were attached to a replaced slice. Then an alias was lost, and
    appends within one branch were treated as independent. Then a `for` initializer was
    skipped, per-variable joins mixed exclusive branches, and returned paths were merged.
    Then a `break` still ran the loop's post statement,
    `fallthrough` was ignored, the path bound dropped branches silently, labelled
    branches targeted the innermost loop, a `goto` was taken to fall through,
    element writes were ignored, and a reslice or a method expression's receiver hid keys.
- Mutation-checked:
  - Renaming one key to `projectID` in `internal/service/connection.go`, or the spread-slice
    key in `internal/service/meta_ad_sets.go`, fails the guard.
  - Dropping `internal/` from the walk trips the positive-control floor.
  - Each of these fails `TestLogKeys_ReadsKeyPositionsOnly`: reading every argument as a key,
    giving an Attr two slots, dropping spread resolution, keying slice sources by name,
    ignoring elided `slog.Attr` elements, matching any package's function, pairing a slice and
    its appends separately, dropping the conversion unwrap, running a branch's statements as
    independent choices, not tracking aliases, dropping the not-taken path, keeping only the
    first switch clause or the first alternative, not descending into a closure, skipping a
    `for` initializer, treating `return` or `panic` as a no-op, letting
    `break` fall through or run the post statement, and mixing variables across paths.
- The decision is written down in `docs/knowledge/architecture/logging-conventions.md` (new)
  and under "Settled by decision" in `docs/reviews/knowledge-base/known-false-positives.md`, so
  a reviewer does not raise it again.
