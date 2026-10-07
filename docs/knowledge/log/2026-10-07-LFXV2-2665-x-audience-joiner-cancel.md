# 2026-10-07 — LFXV2-2665 X audience read: a joiner re-leads when the leader was cancelled

**Fix** — PR #291 review: `twitterAudienceGuard.read` handed every joiner the leader's error
verbatim, so when the LEADER's request context ended (a client disconnect, or its own deadline)
while it waited for the account slot or fetched, every concurrent identical read with a live
context got that `context.Canceled` and answered 503.

- The leader's deferred close now tags a failure that coincides with its own context ending as
  `leaderContextError`; the leader itself still gets its error untagged.
- A joiner that receives a tagged error while its own context is live loops and starts the read
  again (leading, or joining whoever did), at most `twitterAudienceMaxReLeads` (2) times. Every
  other failure — an upstream 5xx or X-side timeout while the leader's context was live, a
  malformed file — is still shared and never retried, and a joiner whose own context ends still
  gets its own error.
- Tests in `internal/dispatch/twitter_audience_guard_test.go`: leader cancelled during the fetch
  and during the slot wait (joiner re-leads and succeeds), joiner's own cancellation, upstream
  failure shared with one fetch. The re-lead and tagging tests fail with the fix reverted.
