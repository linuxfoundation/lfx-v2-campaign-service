# 2026-11-14 — #563 Microsoft structured snippets at create

**Update** — Gap 13 of the Microsoft Advertising gap review (#563) adds
`microsoftConfig.structuredSnippets`, `[{header, values}]`, created as
`StructuredSnippetAdExtension` entries on the confirmed campaign.

- `microsoft.ValidateStructuredSnippets`: at most 10 snippets; `header` one of Microsoft's 13
  English headers (any casing, sent in Microsoft's spelling, each once); 3-10 values of 1-25
  characters (12 double-width), no repeat.
- Attached on the sitelink/callout transport right after those: a campaign that already has a
  snippet gets none added; each snippet is one create and one association. Ids are recorded as
  `structuredSnippetAdExtensionIds`; any shortfall leaves the campaign `created_degraded`.
- A wrong type, a rule breach, or the list while `MICROSOFT_STRUCTURED_SNIPPETS_ENABLED` is off is a
  synchronous 400, re-checked in Dispatch. The chart sets the flag to `"false"`. Not role-gated.
- Deferred: the logo extension (`logoAssetId`). It needs Microsoft's logo image limits and its own
  media upload, separate from the campaign image extension.

Not verified live: no Microsoft call was made. The shape and header list are from the Campaign
Management v13 `StructuredSnippetAdExtension` reference.
