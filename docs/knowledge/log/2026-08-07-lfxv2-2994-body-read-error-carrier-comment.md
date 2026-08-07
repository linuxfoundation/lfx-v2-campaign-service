# 2026-08-07 — The two body-read-error paths carry the same text, not different text

**Update** — Closed dealako's remaining nit on PR #73
(`internal/platform/linkedin/metrics.go`).

The comment on the non-2xx body-read-error path claimed "the resulting STRING differs by
design" between it and the 2xx path. That stopped being true in `6e080a5`, which removed the
redundant `"read response body: "` prefix: both paths now emit `redactBodyReadError(err)`'s
message verbatim, and only the CARRIER differs — `transportError.Err` is an `error` surfaced
as a bare cause, `apiError.Body` is a free-form `string` diagnostic field.

This is the same defect class flagged the round before: a comment asserting a property the
adjacent code no longer has. A reader would take the divergence for intentional behaviour
rather than an artifact of two struct field types. The comment now states what is actually
guaranteed — identical text, different carriers — and says why the prefix must stay off BOTH
paths rather than just this one, which is the part `metrics_test.go`'s `wantBody` constant
already pins ("Matches the 2xx path's transportError.Err exactly").
