# 2026-11-14 — #564 X follower look-alikes at create

**Update** — `twitterConfig.followerLookalikeUserIds` (gap 4c of the X Ads gap review, #564) adds
one `SIMILAR_TO_FOLLOWERS_OF_USER` criterion per numeric X user id to the line item an X create
makes, after the platforms and before the included custom audiences.

- The rule lives in `twitter.NormalizeFollowerLookalikes` (1-19 digits, no leading zero, within
  int64, de-duplicated, at most 60; counted in the 60-criteria per-create total).
- New default-off flag `TWITTER_FOLLOWER_LOOKALIKES_ENABLED` (constants, chart values, README);
  synchronous 400 for the flag, a bad id or a non-array, re-checked in `Dispatch`.
- Accepted risk (owner decision 2026-11-14): X has no user lookup, so an id X refuses fails at its
  criterion POST. The create stops before any tweet with the PAUSED campaign and line item as a
  retained partial, the existing criterion-failure rule.
- Not role-gated: a public user id names no account resource.

Not verified live: no X call was made.
