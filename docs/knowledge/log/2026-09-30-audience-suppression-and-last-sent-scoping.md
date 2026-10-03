# 2026-09-30 — audience suppression matching, last-sent edition scoping, legacy list dedupe

**Fix** — three defects in what the Audience Builder showed an operator for one event
(reproduced on `agntcon-mcpcon-japan`):

- **Standard suppressions resolved as "not found in this portal" while existing.**
  `MatchesStandardSuppression` compared raw names, so `"Unsubscribed - Lists"` or
  `"GDPR_Suppression 26Q1"` never equalled the hygiene term. Names are now normalised on both
  sides (case, quarter tokens, years, separators, a trailing `list`/`lists`) and compared for
  equality — still not containment, which would admit an event's own lists.
- **"Recent sends for this event" listed every Japan event.** `japan` was a distinctive token, so
  one location word admitted any send that named the country. Place names joined
  `genericEventWords`, and `MatchLastSent` now drops a row that names only OTHER editions' regions
  (`editionRegions`). A row naming no region is kept.
- **Every list of an older send appeared twice.** A marketing email carries the same list under
  `contactIlsLists` and the legacy `contactLists`; `listBriefs` read them as two. Legacy ids are
  now mapped through `ListIDForLegacy` (`/crm/v3/lists/idmapping`) and dropped when the id or
  name is already listed.

The dispatch/send path is unchanged. Tests: `TestMatchLastSent_ALocationAloneIsNotEvidence`,
`TestMatchLastSent_AnotherEditionOfTheSameSeriesIsRejected`,
`TestMatchesStandardSuppression_NormalisesSeparatorsQuartersAndSuffix`,
`TestLastSent_ALegacyIDForAListAlreadyListedIsNotRepeated`.
