# 2026-10-13 — LFXV2-2665 LinkedIn settings readback, bid lever, adoption and campaign-ref

**Update** — LinkedIn gains the capabilities every other non-Google ad platform already had
(LinkedIn moved into scope 2026-10-13). `LinkedInDispatcher` now implements `SettingsReader`
(`linkedin_settings.go`) and `CampaignAdopter` (`linkedin_adopt.go`), both reading the campaign
through one identity-checked `linkedin.Client.GetCampaignSettings` (`campaign_settings.go`), whose
404 is absent only after a confirming ad-account read. Adopted rows carry `adopted: true`:
ACTIVATE is refused, PAUSE flips the campaign alone (never its creatives), the budget lever works.
New route `resolve-linkedin-ads-campaign` (`GET /projects/{project_id}/linkedin-ads/campaign-ref`)
with HTTPRoute regex, RuleSet and parity rows. See [internal/dispatch](../code/internal-dispatch.md)
and [internal/platform/linkedin](../code/internal-platform-linkedin.md).
