# 2026-10-13 — LFXV2-2665 LinkedIn settings readback, bid lever, adoption and campaign-ref

**Update** — LinkedIn gains the four capabilities every other non-Google ad platform already had
(LinkedIn moved into scope 2026-10-13). `LinkedInDispatcher` now implements `SettingsReader`
(`linkedin_settings.go`), `BidWriter` (`linkedin_bid.go`) and `CampaignAdopter`
(`linkedin_adopt.go`), all reading the campaign through one identity-checked
`linkedin.Client.GetCampaignSettings` (`campaign_settings.go`), whose 404 is absent only after a
confirming ad-account read. The bid lever writes the campaign's `unitCost` only under costType CPC
with optimizationTargetType NONE/ENHANCED_CONVERSION, sent once (`withSendOnce`), REJECTED (400)
only on LinkedIn's parsed, identity-checked error envelope naming `/Campaign/unitCost/amount`.
Finding: the create path sends no optimizationTargetType, which LinkedIn's schema defaults to
NONE (manual), so created campaigns are expected to pass — the guard still decides on the reported
value. Adopted rows carry `adopted: true`: ACTIVATE and the bid lever are refused, PAUSE flips the
campaign alone, the budget lever works. New route `resolve-linkedin-ads-campaign`
(`GET /projects/{project_id}/linkedin-ads/campaign-ref`) with HTTPRoute regex, RuleSet and parity
rows. See [internal/dispatch](../code/internal-dispatch.md) and
[internal/platform/linkedin](../code/internal-platform-linkedin.md).
