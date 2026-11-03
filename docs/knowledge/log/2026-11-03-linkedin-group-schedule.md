# 2026-11-03 — LinkedIn widens a reused campaign group's end date

**Update** — Every LinkedIn campaign of an event shares one campaign group (the group name
carries no slot or label token), and the group keeps the run schedule of the request that
created it. With multi-campaign, a second campaign that ends after the first reached LinkedIn
under a group that ended earlier, and LinkedIn refuses a campaign scheduled outside its group.

`CreateCampaign` now reads a reused group (`ensureCampaignGroupCoversEnd`: identity-checked
single-entity GET) and, when its end is before the campaign's, widens it with a PARTIAL_UPDATE
that re-sends the group's start unchanged. The end never moves earlier and the start is never
touched, so the group's other campaigns are unaffected. A read that cannot be trusted or a
failed write creates no campaign and returns a partial result with the group, so the claim is
kept and a retry re-reads the group. A group whose start is after the new campaign's start is
not handled and is still refused by LinkedIn.
