# 2026-10-28 — The staged-body check uses this write's token only

**Fix** — the staged-body marker from earlier the same day could vouch for the wrong copy.
`verifyContentSaved` took the FIRST `lfx-staged` token it found in the written html. An operator's
edited body can still carry an earlier staging's marker, and a reverted draft carries that same
old token, so verification passed and dispatch recorded `contentStaged` for the old copy.

- `addBodySection` strips every earlier `lfx-staged` marker (comment or bare token) from the input
  before appending the new one, so a written body carries exactly one token.
- It returns that token, and `RebuildEmailContent` passes it straight to `verifyContentSaved`,
  which no longer re-derives a token from the html.

Test: `TestRebuildEmailContent_IgnoresAnEarlierMarkerInTheInputBody` (a revert to the earlier body
is `ErrContentNotPersisted`, the body as written passes, and the written body carries one marker).

See [internal/platform/hubspot](../code/internal-platform-hubspot.md).
