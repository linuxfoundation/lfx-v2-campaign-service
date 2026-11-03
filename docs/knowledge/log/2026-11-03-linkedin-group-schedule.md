# 2026-11-03 — LinkedIn widens a reused campaign group's end date

**Update** — Every LinkedIn campaign of an event shares one campaign group (the group name
carries no slot or label token), and the group keeps the run schedule of the request that
created it. With multi-campaign, a second campaign that ends after the first reached LinkedIn
under a group that ended earlier, and LinkedIn refuses a campaign scheduled outside its group.

`CreateCampaign` now reads a reused group (`ensureCampaignGroupCoversEnd`: identity-checked
single-entity GET) and, when its end is before the campaign's, writes `runSchedule.end` alone with
a nested PARTIAL_UPDATE `$set`; the start is never sent and the end never moves earlier through
this call. The read-write-verify runs under a process-wide lock per group, and every write is
re-read and repeated (up to 3 writes) if another replica put back a shorter end. Any failure
returns no result, because the group pre-existed and nothing was created, so the claim is
released and a retry re-reads the group instead of stopping at "reconciliation required". A
group whose start is after the new campaign's start is not handled and is still refused.
