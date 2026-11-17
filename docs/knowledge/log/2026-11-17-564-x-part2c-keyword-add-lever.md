# 2026-11-17 — #564 X gaps, part 2c: the positive keyword add lever returns

**Update** — the third and last follow-up to PR #614, stacked on part 2b, brings back the
positive keyword add on an existing X campaign (gap 12), with its tests:

- `POST /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keyword-targeting/additions`
  (`add-keyword-targeting`, `campaign_manager` through the `/briefs/**` rule; `parity_test.go` pins
  the path). Each of 1-20 distinct keywords becomes one `BROAD_KEYWORD` criterion with
  `operator_type` `EQ` on the line item this service created (`twitter.Client.CreateKeywordCriterion`,
  sharing the negative add's POST). A keyword already targeted is `ALREADY_PRESENT` and not sent;
  the line item stays capped at 60 criteria (409 `ErrKeywordTargetingLimit`).
- `TwitterDispatcher` gets a per-line-item `targetingLock`, bound to the Postgres advisory lock by
  `bindKeywordTargetingWriteLock` like Reddit's, held from the criteria read through the last POST.
- `TargetingCriterion.PositiveKeyword` reads a keyword criterion with no `operator_type` as `EQ`,
  as the write path's echo check does, so the read, the removal and the add see one set.
- Off unless `TWITTER_KEYWORD_ADD_ENABLED` is exactly `"true"` (chart `"false"`); off, the endpoint
  answers 400 before X is contacted.

With this part every feature moved out of #614 is back. No long create-campaigns or `config`
description is edited here.

Not verified live: no X call was made.
