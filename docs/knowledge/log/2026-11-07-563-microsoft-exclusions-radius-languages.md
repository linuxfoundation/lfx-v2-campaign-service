# 2026-11-07 — #563 Microsoft excluded countries, radius targets and campaign languages at create

**Update** — Gaps 5 and 6 of the Microsoft Advertising gap review (#563) add three fields:
`microsoftConfig.excludedGeoTargets` (countries to exclude), `proximityTargets` (radius targets)
and `languages` (the campaign's ad languages, so KubeCon Japan can target Japanese).

- The rules live once in `microsoft.ValidateCreateTargeting`. `MicrosoftDispatcher.PreflightCreate`
  refuses a breach or a wrong JSON type with a synchronous 400
  (`invalid campaign config: microsoftConfig.<field>: ...`).
- The excluded codes resolve to LocationIds before anything is created. On a campaign this run
  created, the exclusions and then the radii are attached right after the positive geoTargets
  and before the ad group, under the same fail-closed rules: a failed, partial or unconfirmed
  attach stops the create with the PAUSED campaign reported and retained. The three criterion
  calls now share `postCampaignCriterions`.
- `languages` is set as the new campaign's `Languages`, and the ad group is created with no
  `Language`, because an ad group language overrides the campaign's. Without it the ad group
  stays English.
- A create that finds its campaign by name stops before the ad group when any of the three is
  set, because they apply only to a campaign this run creates.
- Default-off deployment flag `MICROSOFT_CREATE_TARGETING_ENABLED`; the chart sets `"false"`.
  Not role-gated.

Not verified live: no Microsoft call was made. The shapes are from the Campaign Management v13
reference.
