# 2026-10-06 — Describe one clock rule everywhere

**Docs** — review of #276. The code comments on `sanitizeUserinfoSnapshotRun`,
`userinfoRunIsClockShaped` and the X screen test, plus the `internal-dispatch` and
`internal-platform-twitter` concepts, still described the superseded "digits on both sides,
gives up no credential shape" rule. They now describe `redact.UsernameIsClock` — at most two
digits a side, or a real clock behind any sub-delim except `+` — and its bounded residual. No
behaviour change. See [pkg/redact](../code/pkg-redact.md).
