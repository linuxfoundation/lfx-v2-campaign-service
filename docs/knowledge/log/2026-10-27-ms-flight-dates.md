# 2026-10-27 — Microsoft Advertising campaigns take optional flight dates

**Update** — `microsoftConfig` accepts optional `startDate` and `endDate`, YYYY-MM-DD, the same
keys and format as `metaConfig`. Until now a Microsoft campaign created from LFX One had no
schedule at all: it was created PAUSED and, once enabled, ran until someone stopped it.

**Where the dates go.** Microsoft schedules delivery on the AD GROUP (`AdGroup.StartDate` /
`EndDate`, each a `{Month,Day,Year}` Date object); the campaign has no flight dates. The client
sends them on the ad-group create in `findOrCreateAdGroup`, which is what the `msDate` /
`toMSDate` helper in `campaign.go` had been reserved for.

**Validation mirrors Meta.** A real calendar date in YYYY-MM-DD, start not before today's UTC
day, end strictly after start (after today when only an end is given). One Microsoft-specific
adjustment: Microsoft reads the dates in the campaign's time zone, so when the caller supplies
`timeZone` (possibly ahead of UTC) "today" is the calendar day at UTC+14. It runs in the
`CreateCampaign` prologue, before any request, so a bad date cannot leave a campaign without an
ad group. Like Meta's, a refusal fails the platform job pre-create (claim released); it is not
a synchronous 400.

**Absent dates change nothing.** With neither date the ad-group body is byte-for-byte what it
was, and the row records no dates. When dates are given they are recorded on the campaign row
(as Meta's are), so the brief view paces the campaign over its flight — only when the client
reports `FlightApplied`, i.e. this run created the ad group with them, as `BidApplied` gates the
recorded bid. A retry that reuses the ad group leaves its schedule unchanged, records no dates
and says so in a warning step.

**Not changed.** The settings readback and the account monitor read the Microsoft campaign,
not its ad group, so they still report no flight dates for Microsoft.
