# 2026-10-07 — LFXV2-2665 Meta ad sets: PR #288 review fixes

**Fix** — Only a well-formed refusal is REJECTED, and only a trustworthy confirmation is APPLIED:

- `ClassifyAdSetWrite` now also requires a non-zero Graph `code`: an error object with the code
  missing, `0`, or non-numeric is UNCONFIRMED, not "nothing was changed".
- `do()` marks an `APIError` `EnvelopeParsed` only when the raw error body passes
  `identityjson.Check`, so a duplicated or case-folded `code` / `is_transient` (e.g. a throttle code
  followed by a duplicate `code: 100`) is UNCONFIRMED. Every other decoded field is copied as
  before; only the ad-set write classifier reads `EnvelopeParsed` / `IsTransient`.
- `UpdateAdSetStatusOnce` checks the raw 2xx body with `identityjson.Check` before reading
  `success`, so `{"success":false,"Success":true}` is UNCONFIRMED, not APPLIED.
- The `list-meta-ad-sets` description now names both 404s (missing row, no Meta connection) and
  says no upstream answer is ever a 404.
