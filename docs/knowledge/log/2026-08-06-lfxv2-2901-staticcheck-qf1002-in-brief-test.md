# 2026-08-06 — LFXV2-2901: staticcheck QF1002 failed Build and Test

**Update** — Replaced a `switch { case err == nil: ... }` in
`internal/service/brief_test.go` with a plain `if`/`continue`; CI's staticcheck
flagged it as QF1002 (could use tagged switch), which failed Build and Test.
