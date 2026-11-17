# 2026-11-16 — #564 X gaps: pre-PR review fixes

**Fix** — from the pre-PR review of the X gaps branch (#564):

- `add-keyword-targeting` on X reads a listed `BROAD_KEYWORD` criterion with no `operator_type` as
  `EQ` (`TargetingCriterion.PositiveBroadKeyword`), the way its own echo check does, so a keyword
  already on the line item is `ALREADY_PRESENT` instead of being posted again.
- The add de-duplicates the request under the key it compares with (case folded, whitespace
  collapsed; `dedupeTwitterKeywordAdditions`): `cloud native` and `cloud  native` give one outcome,
  count once against the 60-criterion cap and are sent at most once, even after a rejected POST.
- Docs: an MP4 creative asset's consumer is `twitterConfig.videoAssetId` (the LinkedIn video
  variant is #586's, not on this branch); a website card's media is `imageAssetId` or
  `videoAssetId`; `followerLookalikeUserIds` is listed in the 60-criterion combined cap.

Not verified live: no X call was made.
