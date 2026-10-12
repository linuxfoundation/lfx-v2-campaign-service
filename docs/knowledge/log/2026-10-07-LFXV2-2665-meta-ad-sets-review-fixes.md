# 2026-10-07 — LFXV2-2665 Meta ad sets: pre-PR review fixes

**Fix** — Findings from the pre-PR review of the Meta ad-set read and pause/resume:

- **UNCONFIRMED is a 503**, not a 200 outcome: fixed text "the ad set status change is
  unconfirmed — it may or may not have been applied on Meta; read the ad sets before retrying",
  no ETag, write lock held for the cooldown — as the campaign toggle, budget and keyword levers
  answer it. The 200 outcomes are APPLIED and ALREADY_IN_STATE.
- **REJECTED is opt-in** (`meta.ClassifyAdSetWrite`): only a parsed Graph envelope that is not
  `is_transient`, not code 1/2, not 408 and not a throttle says "nothing was changed"; every other
  `*APIError` (HTML body, unread envelope, transient) is UNCONFIRMED. `APIError` gained
  `IsTransient` and `EnvelopeParsed`, set only by `copyEnvelope`.
- **ACTIVATE only the recorded ad set** (`ErrMetaAdSetNotRecorded`, 409, zero requests); PAUSE is
  allowed on any of the campaign's ad sets.
- **Invalid stored campaign id** has its own sentinel (`ErrStoredPlatformIDInvalid`, 409, fixed
  text) instead of claiming Meta reported another account.
- **No Meta connection is 404**, like every sibling lever, matched on the new
  `domain.ErrConnectionAbsent` that `noOwnConnection` now carries beside `ErrNotFound`; a bare
  `ErrNotFound` from elsewhere is no longer given the connection answer.
- **Documented** the full failure table (incl. the post-APPLIED verification 409, which states the
  change WAS applied) and that a campaign ACTIVATE re-activates the recorded ad set, so a
  deliberate ad-set pause does not survive a campaign pause/resume (campaign toggle unchanged;
  product decision pending).
- The send-timeout test's channel receives are bounded.
