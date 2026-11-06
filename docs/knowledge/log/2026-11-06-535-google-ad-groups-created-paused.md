# 2026-11-06 — Google Ads: every ad group is created PAUSED, like Search (#535)

**Fix** — #519's live run read Display ad group 202450120472 back ENABLED under its PAUSED
campaign 24438695954. Search created its ad group PAUSED; Display, Demand Gen and the retained
Video cascade created theirs ENABLED. All of them now send `status: PAUSED` (`StatusPaused`).

| Builder | Before | After |
|---|---|---|
| Search (`adgroup_ad.go`) | PAUSED | PAUSED (unchanged) |
| Display (`display.go`, single group and every `displayAdGroups[]` entry) | ENABLED | PAUSED |
| Demand Gen (`demandgen.go`, single group and every `demandGenAdGroups[]` entry) | ENABLED | PAUSED |
| Video (`video.go`, unreachable: creation is refused) | ENABLED | PAUSED |
| Performance Max asset group (`pmax_creative.go`) | PAUSED | PAUSED (unchanged) |

- **Why ENABLED was not kept:** no code comment or doc gave a functional reason. Each comment
  said "as on Demand Gen", and Demand Gen's own create had no comment at all. The reconciler
  argument in the Video docs ("an ENABLED ad group under a PAUSED campaign is correctly created")
  described the old state; it did not justify it.
- **The enable path already handles it:** `GoogleAdsDispatcher.ToggleStatus` ACTIVATE enables
  every recorded ad group and ad (or the PMax asset group) BEFORE the campaign, on every
  channel, and `googleAdsActivationGate` refuses a campaign with no recorded ad group. No code
  reads an ad group's status or filters GAQL on `ad_group.status`. So a campaign activated
  through this service serves as before.
- **What it changes outside the service:** the ADS were already created PAUSED on every channel,
  so enabling only the campaign never started a new campaign serving. The PAUSED ad group is an
  additional safeguard: before, once someone had enabled the ad in the Google Ads UI, enabling
  the campaign alone was enough to spend; now every level must be enabled, as on Search.
- **Tests:** the per-channel create tests now expect PAUSED, and
  `TestToggleStatus_ActivateEnablesTheAdGroupsTheCreatePathPaused` pins
  adGroups → adGroupAds → campaigns with ENABLED on Demand Gen, Display and Video. Mutation-checked:
  restoring ENABLED on each builder fails its tests, and making the cascade skip ad groups on
  ACTIVATE fails the new dispatch test.
- Docs updated: `docs/api-catalog.md` (Video and Display creative fields), `code/internal-platform-googleads.md`
  (status-toggling decision, Video and Display sections, description), `code/index.md`.
