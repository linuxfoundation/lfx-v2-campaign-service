# 2026-11-18 — #564 X website conversions goal at create

**Update** — `twitterConfig.goal` (`WEBSITE_CONVERSIONS`) and `twitterConfig.primaryWebEventTag`
(gap 5 of the X Ads gap review, #564) are sent as `goal` and `primary_web_event_tag` on the line
item an X create makes, so X optimizes toward the conversions that web event tag records.

- The rule lives in `twitter.NormalizeConversionGoal`: the goal must be `WEBSITE_CONVERSIONS`, the
  tag 1-32 letters or digits, and each needs the other. The objective stays `WEBSITE_CLICKS`.
- New default-off flag `TWITTER_WEBSITE_CONVERSIONS_ENABLED` (constants, chart values, README).
  While off, or for a bad pair or a non-string, the create is a synchronous 400 before any job.
- A line item reused by name keeps its own goal; a warning step says so.
- Not role-gated. No web-event-tag list read was built.

Not verified live: no X call was made. A live create is owed before the flag is turned on.
