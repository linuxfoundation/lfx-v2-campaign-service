# 2026-11-06 — #552 LinkedIn conversion association on create

**Update** — `linkedInConfig.conversions` (`CampaignInput.Conversions`) associates LinkedIn
conversion rules with the campaign once it exists, gap 3 of the LinkedIn Ads gap review (#552).
Before, a manual-bid `WEBSITE_CONVERSION` campaign was created with no conversion attached.

- One `PUT campaignConversions/(campaign:…,conversion:…)` per distinct id, after the campaign is
  created or reused and before the dark posts. The PUT is idempotent.
- A failed association does not stop the create; the row is `created_degraded`, the result lists
  `conversionsAssociated` / `conversionsFailed`, and a step names each failed id with its HTTP
  status only.
- `linkedin.ValidateConversions` refuses a URN, a non-id, an empty list and more than 20 ids
  before any job or request (`PreflightCreate` answers a synchronous 400).
- Behind `LINKEDIN_ADS_CONVERSIONS_ENABLED`, off by default; the ids also require
  `global_marketing_ops` (`service.LinkedInMarketingOpsInputs`, one check with the Google inputs).

Built from the LinkedIn Marketing API docs (li-lms-2026-10). Not yet probed live.
