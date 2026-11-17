# 2026-11-14 — #563 Microsoft logo extension at create

**Update** — The logo half of gap 13 of the Microsoft Advertising gap review (#563), deferred when
the structured snippets landed, adds `microsoftConfig.logoAssetId` and `logoBusinessName`: a stored
creative asset of the brief attached to the confirmed campaign as a `LogoAdExtension`.

- `microsoft.ValidateLogo`: PNG or JPEG only (`video/mp4` and every other type refused), square,
  128-2048 px a side, at most 76,800 bytes (the `Image.Data` base64 ceiling of the upload path);
  `ValidateLogoBusinessName`: 1-25 characters, no control character.
- `AttachCampaignLogo`: an association read for `LogoAdExtension` (a campaign that has one gets
  nothing uploaded or added), then `AddMedia` as an `Image1x1`, `AddAdExtensions` with
  `{BusinessLogo, BusinessName}` and a Campaign association. Ids are recorded as `logoMediaId` and
  `logoAdExtensionId`; any shortfall leaves the campaign `created_degraded` with a step.
- A non-string value, a non-UUID id, one field without the other, a bad business name, or either
  field while `MICROSOFT_LOGO_EXTENSION_ENABLED` is off is a synchronous 400, re-checked in
  Dispatch. A missing or unusable asset fails the job before any Microsoft call. The chart sets the
  flag to `"false"`. Not role-gated.

Not verified live: no Microsoft call was made. The shapes and limits are from the Campaign
Management v13 `LogoAdExtension`, `Image`, `AddMedia` and `AdExtensionsTypeFilter` references and
the ad extensions guide.
