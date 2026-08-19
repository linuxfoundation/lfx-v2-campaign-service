# 2026-08-19 — LFXV2-3282 Reddit reporting review follow-ups

Four review findings against the reporting-contract change, all verified real before
being acted on.

## The window check ran AFTER credential resolution

`reddit.ValidateMetricsWindow` was added and documented as the way a caller rejects an
unsupported window before credentials are touched — matching LinkedIn and X — but
`RedditDispatcher.ReadMetrics` still resolved the client first and only reached the
window check inside `GetCampaignMetrics`. A broken connection therefore masked an
unsupported window, answering a connection error where the accurate answer is a
permanent 400 naming the one thing the caller can change.

The check now runs before `resolveRedditClient`, wrapped in
`domain.ErrMetricsWindowUnsupported`, exactly as `linkedin.go` does — which is the
whole reason `ValidateMetricsWindow` is package-level and clock-free.

The pre-existing `TestReddit_ReadMetrics_UnsupportedWindowIs400` could not catch this:
it used a HEALTHY connection, so it passed under either ordering. The new
`TestReddit_ReadMetrics_UnsupportedWindowBeatsAResolutionFailure` breaks both at once
and asserts the window wins and the resolution error does not surface.

## Report errors leaked the ad account id

`reportDecodeError` deliberately pins the literal `"reports"` path because the account
id is interpolated into the real one. But that covers only the 2xx-decode arm. A 4xx/5xx
builds an `apiError`, and a transport failure a `transportError`, both carrying the real
path — and both stringify into the service's warning log. Those are the arms that fire
during an actual outage, so the invariant did not hold where it mattered.

`redactReportPath` now removes the account id from the error at the call site.

**It took two attempts, and the first one is the more instructive.** The initial fix was a
TYPE SWITCH: rewrite `Path` on `*apiError` and `*transportError`, and peel one `*url.Error`
layer for the wrapped cause. A reviewer pointed out that this enumerates types rather than
covering the surface, and they were right on both counts:

- `request()` also returns FIVE plain `fmt.Errorf` values that interpolate the path directly
  — body encode, request build, pre-send dial, rate-limit abort, retries exhausted. No type
  to switch on, so all five still leaked. Confirmed with a probe:
  `reddit API POST /ad_accounts/t2_test/reports -> 429: rate-limit reset ... aborting`
- peeling one `*url.Error` layer misses a NESTED one, whose inner `Error()` prints the full
  request URL regardless of any outer wrapper.

The redactor is now a substitution over the RENDERED message: replace the account-bearing
path segment (and any bare occurrence of the id) wherever it appears. That covers every arm
including ones added later, which is the property that matters for a log sink — an
enumeration of types silently stops covering new arms, and this one already had seven. The
original chain is preserved behind `redactedError.Unwrap`, so `errors.Is`/`As` classification
(timeouts, context cancellation, `createOutcomeAmbiguous`) is unchanged; verified that
`errors.As` still reaches a `*url.Error` nested two wrappers deep.

This is the same lesson as the sweep-the-surface rule: the finding was one leaking arm, but
the fix had to be the class of arm, not the named one.

## "Four of the five guesses were wrong" — it was five of six

Both tables list six elements, one of which (path + method) held. The prose said four of
five in three places: `metrics.go`, the contract log entry, and
`internal-platform-reddit.md`. Corrected to five of six at all three.

## Five operator-facing docs still called the contract a guess

The change updated `internal-platform-reddit.md` to cite Reddit's published spec while
`README.md`, `docs/api-catalog.md` (two sites), `internal-dispatch.md`,
`kubernetes/deployment.md`, and a dispatcher test comment still described the contract
as undocumented and inferred. That is contradictory published guidance, and the stale
half is the one an operator hits first.

All are updated to the accurate current position, which is narrower than either
extreme: the shapes come from the official public OpenAPI document, and what remains
unverified is that no request has been made against a live ad account — so behaviour a
schema cannot express (zero-activity rows, the account's attribution window) is still
unconfirmed. That is what the gate now says it is for. Historical log entries are left
intact.

## Test that could not detect the constant it claimed to pin

`TestValidateMetricsWindowMatchesDateRangeForWindow` claimed to pin agreement for every
domain window but listed `"yesterday"` as a string literal and omitted
`MetricsWindowLast14Days` entirely. Adding only that constant to
`supportedMetricsWindows` would have evaded it. Every domain constant is now listed by
name.

**Verification** — mutations, each compiling and reverted:

- resolving credentials before the window check → the new ordering test fails with
  `the connection error masked the window rejection`; the OLD test still passes, which
  is why it was not sufficient.
- dropping `redactReportPath` → all three sub-cases fail.
- reverting to the type-switch shape (redact only `*apiError`/`*transportError`) → the
  rate-limit sub-case fails, printing the leaked path. This is the mutation that reproduces
  the reviewer's finding, and the reason the test now drives a plain-`fmt.Errorf` arm rather
  than only the two typed ones.
- adding `MetricsWindowLast14Days` to `supportedMetricsWindows` only → the sync test now
  fails (`ValidateMetricsWindow err=<nil> but dateRangeForWindow err=unsupported`); it
  did not before.
