# 2026-10-23 — account-monitor scope: later takeovers, unrecorded provenance, scope 409 body

**Fix** — PR #391 review follow-ups to the project-scoped account monitors
(lfx-self-serve-ops#897).

**A campaign another project took over stays out after that project deletes it too.**
`listProjectPlatformDeletedCampaignIDsQuery` hid this project's deleted row only while another
project held the same upstream id LIVE, so A deletes → B adopts → B deletes handed the campaign,
with B's budget and spend, back to A's monitor. Another project's row now takes the campaign over
when it is live OR was recorded later (`l.created_at > c.created_at`), whatever its status. Pinned
by the SQL-shape test and the live-database test, which now covers that sequence and its reverse.

**Google Ads and X monitors refuse a campaign that records no creating account.** Their ids are
unique only within an account and the monitor may read an account a legacy row was never created
under, so `requireRecordedMonitorProvenance` (`internal/dispatch/account_monitor_scope.go`)
applies Reddit's strictness on the monitor only: a live unrecorded row is a 409
(`ErrCampaignProvenanceUnknown`; beside a mismatched row, `ErrCampaignProvenanceMixed`), and a
deleted one is omitted. The insight reads keep their permissive legacy-row policy.

**Scope 409s carry their own body.** Google Ads, Meta and Reddit declared the shared
`ConflictError`, publishing a connection-collision example (`already_exists`) they never return.
They now declare `AccountMonitorScopeConflictError` (required `reason`:
`campaign_account_mismatch` or `campaign_scope_invalid`), and `classifyMonitorError` sets the
reason on every platform.

**Docs.** X's `account_too_many_active_campaigns` text, in the design, the 409 message and the
API catalog, now says the 200-campaign cap counts the whole shared account, not only the
project's campaigns.
