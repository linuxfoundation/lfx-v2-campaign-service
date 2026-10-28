# 2026-10-23 — account-monitor deleted scope: order takeovers by binding, not claim

**Fix** — PR #391 review follow-up to the project-scoped account monitors
(lfx-self-serve-ops#897).

**A dispatch that binds after a takeover is now the latest owner.**
`listProjectPlatformDeletedCampaignIDsQuery` decided which project last held a campaign every
project has deleted by `created_at`, but a dispatch's `created_at` is its claim, inserted before
the platform call. A dispatch that binds an EXISTING upstream campaign (Google `adoptExisting`,
Meta same-name reuse) could claim, wait out another project's adopt-then-delete, then bind and
delete it — and the former owner's monitor got the campaign, with this owner's budget and spend,
back. Migration `000048` adds `campaigns.platform_bound_at`, stamped when a write binds a new
upstream id (upsert INSERT arm, upsert conflict arm on an id change, adoption), and the
`NOT EXISTS` now compares `COALESCE(platform_bound_at, created_at)`. Nullable with no default and
no backfill, so N-1 writes and older rows fall back to `created_at`. Pinned by the SQL-shape tests,
the deleted-scope live test (claim and binding order disagreeing, both ways) and
`TestLiveClaimThenUpsertStampsPlatformBoundAt`.
