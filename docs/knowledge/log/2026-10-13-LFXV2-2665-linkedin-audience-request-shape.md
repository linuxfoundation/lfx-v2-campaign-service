# 2026-10-13 — LFXV2-2665 LinkedIn audience uses the Analytics finder

**Fix** — The LinkedIn audience read (#314) paired CAMPAIGN with each MEMBER_* pivot on the
multi-pivot Statistics finder (`q=statistics&pivots=List(CAMPAIGN,MEMBER_…)`), but LinkedIn's
documented Statistics `pivots` enum lists no MEMBER_* value — only the single-pivot Analytics
finder's `pivot` enum does (ads-reporting-schema, "Analytics Finder Query Parameters" vs
"Statistics Finder Query Parameters") — so every non-empty read would have been rejected and
surfaced as a 503. The read now issues one `q=analytics&pivot=MEMBER_…&timeGranularity=ALL
&dateRange=(start:(…),end:(…))` request per (pivot, campaign), each with a single-campaign
`campaigns` facet and the bound account's `accounts` facet, `fields=impressions,clicks,costInUsd,
pivotValues`, and sums the per-campaign answers per value. Attribution comes from the request: an
Analytics element carries only the demographic value, so each element must carry exactly one
`pivotValue` (never a campaign URN); that LinkedIn honoured the facets cannot be checked from the
answer. The campaign cap drops from 50 to 10 (at most 50 requests per read), run 5 at a time with
the first failure cancelling the rest, inside the 20s budget. The `internal-platform-linkedin`
concept's `GetAudienceInsights` signature now includes `accountID`. Concept, api-catalog row and
design description updated.
