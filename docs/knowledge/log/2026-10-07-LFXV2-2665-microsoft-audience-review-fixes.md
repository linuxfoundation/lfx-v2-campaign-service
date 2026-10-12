# 2026-10-07 — LFXV2-2665 Microsoft audience: pre-PR review fixes

**Fix** — Pre-PR review of `get-microsoft-ads-audience`:

- The api-catalog `/meta-ads/audience` row no longer lists Microsoft among the platforms with no
  audience route; Microsoft serves age/gender only, through `/microsoft-ads/audience`.
- `ErrAudienceScopeTooLarge` / `ErrAudienceScopeInvalid` are documented as shared by the Meta and
  Microsoft audience reads, with both ceilings (`meta.MaxAudienceCampaigns`;
  `microsoft.MaxKeywordReportCampaigns`, 300).
- `microsoftReportScopeRules` gained a per-kind `validateID`, so the audience read validates stored
  ids with `ValidateAudienceReportCampaignID` and its error chain names the audience scope
  (`microsoft.ErrAudienceReportScope`), not the keyword report's.
- AgeGroup/Gender labels now refuse Unicode format characters (Cf, e.g. the bidi override U+202E
  or a zero-width joiner) as well as control characters.

See [Microsoft keyword insights](../architecture/microsoft-keyword-insights.md).
