# 2026-11-14 — #587 LinkedIn campaign name frozen on retry

**Update** — Retry safety ahead of the LinkedIn naming change (#587). A LinkedIn campaign is
reused by NAME inside its campaign group (`findCampaignByNameInGroup`), so a re-dispatch of a slot
that recomposes its name under different rules would miss the campaign an earlier attempt created
and POST a second, paid one into the same group. The name a slot has recorded is now frozen:
`model.RecordedCampaignName` reads it off the row (`campaign_name`, else `campaignName` in the
Result blob a retained partial persists), the orchestrator puts it on the dispatch context
(`model.WithDispatchCampaignName`) from the claimed row — or from the row it read at the same slot
version, when that row was cleared between the read and the claim — and the LinkedIn dispatcher
passes it as `CampaignInput.FrozenCampaignName`, which the client uses verbatim. A slot with no
recorded name composes one exactly as before, so this commit changes no name.

Today the orchestrator answers a retry of a row that recorded a partial with "reconciliation
required" and does not re-dispatch it, so the frozen name reaches the client only when a re-dispatch
does happen: the read-then-cleared race above, and any future automatic reconciliation
(LFXV2-2665), which goes through the same claim. The naming-v2 commit adds a second guard for the
case where the row itself is gone (an operator hard-deleting a stuck claim).

Tests (each proven to fail with its line of the change removed):
`TestRecordedCampaignName`, `TestDispatchCampaignName`,
`TestOrchestrator_DispatchCarriesTheClaimedRowsRecordedName`,
`TestOrchestrator_DispatchCarriesTheNameOfARowClearedBeforeTheClaim`,
`TestOrchestrator_AFirstDispatchFreezesNoName`,
`TestCreateCampaign_FrozenNameReusesTheCampaignAnEarlierAttemptCreated`,
`TestCreateCampaign_NoFrozenNameComposesTheName`,
`TestLinkedInDispatch_FrozenNameReusesTheEarlierCampaign`.
