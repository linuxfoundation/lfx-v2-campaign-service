# 2026-11-06 — #552 LinkedIn ad-segment and conversion-rule pickers

**Update** — Two read-only endpoints feed the LinkedIn create inputs from the gap review (#552):
`GET /projects/{id}/linkedin-ads/audiences` lists the ad account's ad segments for
`linkedInConfig.targeting` (with the facet each goes under), and
`GET /projects/{id}/linkedin-ads/conversions` lists its conversion rules for
`linkedInConfig.conversions`.

- Both read the account a LinkedIn create for the project uses (the project's own connection, or
  the LF system account), through `LinkedInDispatcher`'s new `AudienceReader` and
  `ConversionActionReader` implementations and the orchestrator reads the Google pickers use.
- Gated on `global_marketing_ops` by the `marketing-ops-api` gateway rule; the route regex and
  the parity tests carry both paths.
- `offsetWalk` pages the finders 100 at a time and fails rather than truncating (repeat page,
  another account's element, unusable id, more than 1000 entries, a short page that still
  advertises a `nextPageToken`).

Built from the LinkedIn Marketing API docs (li-lms-2026-10). The offset paging is not yet probed
live.
