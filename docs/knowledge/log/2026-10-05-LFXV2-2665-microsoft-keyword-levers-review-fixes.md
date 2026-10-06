# 2026-10-05 — Microsoft keyword levers: pre-PR review fixes

**Fix** — review findings on the Microsoft keyword levers, before the PR opened.

- **REMOVE is no longer sent after a failed PAUSE call.** A whole-call PAUSE failure, definite or
  unconfirmed, now leaves the REMOVE items `FAILED`/`NOT_SENT`, as the PAUSE-first ordering
  promised.
- **Negative-keyword entity errors.** A zero or empty `BatchErrorCollection.Code` is absent, not
  a refusal, and an entity-level code never becomes a definite whole-call error when any real id
  came back — the response falls through to per-item outcomes.
- **4335 matched strictly.** A present symbolic `ErrorCode` must be
  `CampaignServiceNegativeKeywordAlreadyExists`; the numeric code counts only when it is absent.
- **Named predicates** `model.KeywordActionApplied` / `model.NegativeKeywordPresent` replace the
  unused `KeywordOutcomeTookEffect`.
- **Documented, not fixed: ACTIVATE re-enables a keyword paused through keyword-actions.**
  Recording operator pauses would need a versioned row write on an endpoint with no If-Match,
  staling every client's ETag, and a record the Bing UI could silently contradict. Stated in the
  endpoint description, `docs/api-catalog.md` and the dispatch/platform concepts.
- `docs/api-catalog.md`'s keyword-actions row now scopes ALL-OR-NOTHING to Google Ads and says
  "before the ad platform is contacted".
- PR #261 wording nits folded in: the snapshot test's file comment and two KB sentences no longer
  say keyword values reach Microsoft "untouched"/verbatim.
