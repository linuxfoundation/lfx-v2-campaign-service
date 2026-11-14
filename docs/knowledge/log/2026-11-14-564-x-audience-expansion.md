# 2026-11-14 — #564 X audience expansion at create

**Update** — `twitterConfig.audienceExpansion` (gap 11 of the X Ads gap review, #564) is sent as
`audience_expansion` (`BROAD`, `DEFINED` or `EXPANDED`) on the line item an X create makes.

- The rule lives in `twitter.NormalizeAudienceExpansion` (trimmed, upper-cased, closed set; blank
  means unset). A bad value, a non-string, or any value while the flag is off is a synchronous 400.
- New default-off flag `TWITTER_AUDIENCE_EXPANSION_ENABLED` (constants, chart values, README).
- A line item reused by name keeps its own expansion; a warning step says so.
- Not role-gated: the value names no account resource.

Not verified live: no X call was made.
