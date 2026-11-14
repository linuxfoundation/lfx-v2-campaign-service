# 2026-11-14 — #564 X device, age and gender targeting at create

**Update** — `twitterConfig.devices`, `ageBucket` and `gender` finish gap 9 of the X Ads gap
review (#564). They add `DEVICE`, `AGE` and `GENDER` criteria (EQ) to the line item an X create
makes, after the platforms.

- `devices`: each value must be listed by `GET targeting_criteria/devices` before anything is
  created (`resolveListedValues`). Trimmed, lower-cased, 1-32 letters or digits, at most 60.
- `ageBucket`: one of the 20 `AGE_BUCKET` values in X's Python Ads SDK. docs.x.com shows only
  `AGE_21_TO_34`. There is no `AGE_18_TO_24`. X allows one bucket per line item.
- `gender`: `MALE` or `FEMALE`, sent as X's `1` or `2` (Campaign Management guide). A numeric
  echo of the value now confirms the criterion.
- All three count toward the 60-criterion cap and the 30-criterion video cap. They use the same
  default-off `TWITTER_CREATE_TARGETING_ENABLED` flag, give synchronous 400s that are re-checked
  at dispatch, and are not role-gated.
- Placements are not built. X leaves the valid combinations to `GET line_items/placements` and
  documents only a `PROMOTED_ACCOUNT` example.

Not verified live: no X call was made.
