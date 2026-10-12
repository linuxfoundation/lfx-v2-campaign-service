# 2026-10-06 — Keep the clock exemption with sub-delim usernames

**Fix** — pre-PR review of the post-merge review-thread fixes. Widening the scheme-less
`user:password@host` username class to RFC 3986 sub-delims (in `pkg/redact` and the X screen)
let the leftmost match start on prose punctuation, so `Keynote (14:00@main.stage)` read as
username `(14`, failed the digits-both-sides clock test, and was refused by the X screen and
blanked in `config_snapshot`.

- Both patterns now require an unreserved FIRST username character, so `(14:00@…`,
  `*9:30@…*` and `'14:00@…'` are matched from the first digit.
- Both clock tests (`userinfoRunIsClockShaped`, `sanitizeUserinfoSnapshotRun`) judge the
  username's segment after its last sub-delim (`afterLastSubDelim`), for `Mon,9:30@main.stage`,
  whose leftmost match starts on the letter.
- `admin!:pw@host` and `a(b:pw@host` are still caught; regressions on both sides.
- `classifyDiscoveryError` documents that only a method declaring
  `AccountMonitorConflictError` may receive the two X-monitor sentinels.
