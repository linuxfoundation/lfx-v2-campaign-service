# 2026-11-07 — #564 X keywords and interests at create

**Update** — `twitterConfig.keywords` and `twitterConfig.interestIds` (gap 4 of the X Ads gap
review, #564) add `BROAD_KEYWORD` and `INTEREST` criteria (EQ) to the line item an X create
makes. They go after the languages and before the included custom audiences.

- The rules live in `twitter.NormalizeKeywordInterestTargeting`. A keyword is 1-80 characters
  with no control character, and case-insensitive duplicates are dropped. An interest id is
  1-19 digits. Each list is capped at 60, and both count toward the 60-criterion per-create cap.
  `TwitterDispatcher.PreflightCreate` refuses a breach or a wrong JSON type with a synchronous
  400.
- Interest ids are checked against `GET targeting_criteria/interests`, walked to its end, before
  anything is created. The language check now shares that walker (`listTargetingValues`).
- Steps name a keyword by number. The stored config snapshot reduces a link in a keyword.
- The same default-off `TWITTER_CREATE_TARGETING_ENABLED` flag as gaps 1-2. Not role-gated.
- `followerLookalikeUserIds` is deferred (X has no user lookup to check an id before create).

Not verified live: no X call was made.
