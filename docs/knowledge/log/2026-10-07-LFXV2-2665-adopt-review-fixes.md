# 2026-10-07 — Address the pre-PR review of adoption

**Fix** — pre-PR review of the Microsoft/Meta/Reddit/X adoption commit. Reddit and X ran
`identityjson.Check` over the decoded `data` only, so a duplicated `data` envelope key (or `data`
beside `Data`) was resolved last-wins and never refused; the guard now runs over the raw response
body (`apiResponse.raw`, set only by the shared decode and read only by `GetCampaign`). Microsoft's
adoption read now requests every campaign type and refuses a live non-Search campaign with the new
`domain.ErrAdoptionCampaignTypeUnsupported` (409), instead of resting on undocumented behaviour that
could have read it as absent. The API catalog, the design description and the dispatch concept now
state what an adopted row does NOT support: ACTIVATE everywhere, the bid lever on all four new
platforms, and the budget lever on Meta (it works on Microsoft, Reddit and X); those refusals and the
adapter-side budget 409 now name adoption. Meta's campaign-only fields are documented and pinned as
load-bearing, the Reddit/X bare-404 caveat is written down, and identityjson notes that converging
the Google Ads copies onto it is a follow-up for that owner. See
[internal/dispatch](../code/internal-dispatch.md) and the [API catalog](../architecture/api-catalog.md).
