# 2026-11-07 — #563 Microsoft sitelink and callout extensions at create

**Update** — `microsoftConfig.sitelinks` and `microsoftConfig.callouts` (gap 3 of the Microsoft
Ads gap review, #563) are created after the campaign is confirmed as campaign-level
`SitelinkAdExtension` and `CalloutAdExtension` entries. An LF event's Microsoft campaign mirrors
its Google Search campaign, which already carries sitelinks (Schedule, Register, CFP) and
callouts; on Microsoft the campaign had only the optional image extension.

- Rules live once in `microsoft.ValidateTextExtensions`, from the v13 references: sitelinks
  1–20, text 25 with a description and 35 without, both descriptions or neither (35 each), an
  absolute http/https finalUrl of at most 2,048 characters (code points), with no user
  credentials and no control character, checked before the trim; callouts 2–20 at 25 each, since one
  callout never serves. Any double-width character switches a field to the reduced limits.
  Repeats (case-insensitive) and control characters are refused.
- Synchronous 400 (`invalid campaign config: microsoftConfig.<field>: ...`) from
  `MicrosoftDispatcher.PreflightCreate`, including a wrong JSON type under a case-variant key.
  The gate and rules run on the best-effort decode, so an unrelated mistyped field cannot let a
  gated or invalid list through. Dispatch and the client re-check before any Microsoft call.
- Transport is the image extension's: a per-type `AdExtensionsAssociations/Query` (a type the
  campaign already has is skipped, so a retry never doubles it), then one `AdExtensions` add and
  one `AdExtensionsAssociations/Set` per extension, each on the existing single-item classifiers.
  The association step and the lookup were factored out of `AttachCampaignImage`
  (`associateCampaignExtension`, `findCampaignExtension`) so both attach paths classify a
  response the same way.
- A failed or UNCONFIRMED extension marks the campaign `created_degraded` with a step saying
  which; the job never fails for it. So does a campaign left with fewer than two callouts, after
  the run or as a retry found it, because Microsoft serves none. Ids land in the result as `sitelinkAdExtensionIds` /
  `calloutAdExtensionIds`. The snapshot reduces each sitelink URL like Google's.
- Default-off deployment flag `MICROSOFT_AD_EXTENSIONS_ENABLED`; the chart sets `"false"`. Not
  role-gated: the extensions are created new and name no account resource.

Not verified live: no Microsoft call was made. Turn the flag on only after a live create
confirms AddAdExtensions accepts both types with this body and SetAdExtensionsAssociations
associates them at Campaign level.
