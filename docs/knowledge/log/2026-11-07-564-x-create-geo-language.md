# 2026-11-07 — #564 X create-time location and language targeting

**Update** — `twitterConfig.geoTargets` and `twitterConfig.languages` (gaps 1 and 2 of the X Ads
gap review, #564) add `LOCATION` and `LANGUAGE` targeting criteria to the line item an X create
makes. Before this, the line item carried no criteria and served worldwide once enabled, which
the Implement tab warned about.

- The rules live once in `twitter.NormalizeCreateTargeting`: country codes are trimmed,
  uppercased and must be two letters, with at most 50; language values are trimmed, lowercased
  and must be 2–8 letters or hyphens, with at most 20. Duplicates are dropped.
- Synchronous 400 (`invalid campaign config: twitterConfig.<field>: ...`) from
  `TwitterDispatcher.PreflightCreate` for a rule breach or a value that is not an array of
  strings (case-variant keys included). It runs on the best-effort decode, like the bid plan.
  Dispatch and the client re-check before any X call.
- Each country is resolved through `GET targeting_criteria/locations` (`COUNTRIES`, exactly one
  entry) and the languages are checked against `GET targeting_criteria/languages`, before the
  campaign is created. A miss or an unreadable lookup creates nothing.
- The criteria are POSTed one at a time after the line item and before the promoted tweet. A
  failure stops the create before any tweet, with the PAUSED campaign and line item reported and
  the count applied. That partial create is retained (a repeated request answers "reconciliation
  required"). A create that finds its line item by name re-targets it only if X reports it PAUSED,
  adding just the missing criteria before the tweet.
- Default-off deployment flag `TWITTER_CREATE_TARGETING_ENABLED`; the chart sets `"false"`. It
  names no account resource and is not role-gated.

Not verified live: no X call was made. The shapes are from X Ads API v12's published
reference. Turn the flag on only after a live create confirms the country lookup's
`location_type` spelling and the criterion echo.
