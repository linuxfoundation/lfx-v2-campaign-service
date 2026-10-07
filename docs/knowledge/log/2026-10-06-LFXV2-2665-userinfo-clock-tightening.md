# 2026-10-06 — Userinfo clock exemption tightened (PR #275)

**Fix** — three bot findings on PR #275 against the scheme-less `user:password@host` patterns
in `pkg/redact` (config_snapshot) and the X screen, kept in step.

- **Sub-delims-only usernames.** Requiring an unreserved first character let `!:pw@host` and
  `$$:pw@host` match neither pattern, so they were published and persisted verbatim. Both
  patterns gain a second username alternative of sub-delims only, held to a colon directly
  after it (RE2 has no lookahead; the required `:` does the work), so `(14:00@` still starts at
  the digit.
- **Numeric suffixes were read as clocks.** The after-last-sub-delim rule made
  `alice+2024:1234@ops.example` a "clock". `afterLastSubDelim` is replaced by `usernameIsClock`:
  an all-digit username with an all-digit password (unchanged), OR a `,` `(` `*` or `'`
  followed by a real clock — hour 1–2 digits ≤ 23, password exactly two digits ≤ 59. `+` never
  qualifies, since it is a common username character: `alice+9:30@ops.example` is refused.
- Regressions on the X screen, `pkg/redact` and the dispatch snapshot tests: the four clock
  copies (`Keynote (14:00@main.stage)`, `*9:30@main.stage*`, `'14:00@main.stage'`,
  `Mon,9:30@main.stage`) pass; `!:pw@`, `$$:pw@`, `alice+2024:1234@`, `alice+9:30@` and
  `a,2024:1234@` are caught.
