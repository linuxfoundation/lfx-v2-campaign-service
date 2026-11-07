# 2026-11-06 — Reddit conversion event (#549)

**Update** — `redditConfig.conversionEvent` lets a conversions campaign optimize toward a pixel
event other than `PURCHASE`. The choices are `SIGN_UP`, `LEAD`, `PAGE_VISIT`, `VIEW_CONTENT`,
`ADD_TO_CART`, `ADD_TO_WISHLIST` and `SEARCH`, the web-pixel members of Reddit's
`components_enums_optimization_goal`. Registration on most LF events fires `SIGN_UP` or `LEAD`
rather than `PURCHASE`. The value is sent as `optimization_goal` on both the campaign and the ad
group, because campaign budget optimization requires the two to match. Omitted, the create is
unchanged.

`RedditDispatcher.PreflightCreate` refuses an unknown value, another objective, `null`, an
empty or non-string value, or a case variant of the key with a synchronous 400.
`Dispatch` repeats the check before any Reddit request, and the client checks the
value against the objective again before account verification. The field sits behind
`REDDIT_CONVERSION_EVENT_ENABLED`, which is off by default: the values are from the published
enum, but no live create has confirmed a non-PURCHASE goal on a CONVERSIONS CBO campaign.
Gap review: #549.
