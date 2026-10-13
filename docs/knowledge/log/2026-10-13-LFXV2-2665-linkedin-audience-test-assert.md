# 2026-10-13 — LFXV2-2665 LinkedIn audience cancellation test keeps the cause

**Fix** — Review of #324: the LinkedIn audience cancellation test accepted any non-nil error that
was not `context.Canceled`. It now requires, via `errors.As`, the failing request's `*apiError`
with status 500, so a read that replaced the cause with a generic error, dropped the error chain
or reported another status fails it. The `PivotValues` decoder comment in
`internal/platform/linkedin/monitor.go` no longer says the plural field "cannot be" requested via
`fields`; only the legacy singular `pivotValue` is unprojectable, matching the RawQuery comment
narrowed in #324. No other stale claim remains in the package or the knowledge base. Concept
updated.
