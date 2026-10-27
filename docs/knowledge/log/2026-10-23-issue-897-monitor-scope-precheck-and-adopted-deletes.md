# 2026-10-23 — account-monitor scope: precheck first, adopted deletes leave the old project

**Fix** — two follow-ups to the project-scoped account monitors (lfx-self-serve-ops#897).

**The dispatcher's 400s answer before the scope.** The scope gate ran before any dispatcher
code, so a project with no campaigns on X got 200 empty from a monitor switched off by
`TWITTER_METRICS_ENABLED`, and one with a campaign on another account got a 409, where the
catalog promises 400. `service.AccountMonitorScoper` gains `MonitorScopePrecheck`, which
`scopeAccountMonitor` runs before it reads the scope: every scoped platform's `account_id` shape
check, plus X's flag gate (`twitterMonitorPrecheck`, now shared with
`resolveTwitterMonitorClient`). The requested-account guards on Meta, Reddit and X need the stored
connection and still run after the scope; `architecture.md` and the API catalog now say so.

**A soft-deleted campaign another project adopted leaves the deleting project's monitor.** The
binding indexes cover live rows only, so after project A deleted its row project B could adopt
the same upstream campaign, and A's monitor kept showing B's budget and spend through A's deleted
row. `listProjectPlatformDeletedCampaignIDsQuery` now excludes an id that a live row in another
project holds (matched on the id alone, the conservative direction). Pinned by the SQL-shape test
and a new live-database test.

**Docs.** The four scoped monitor descriptions, the API catalog and `architecture.md` now state
that campaigns deleted in this service stay in the monitor only while they are proven to belong
to `account_id` and no other project holds them, and are otherwise omitted, never a 409.
