# 2026-11-04 — #487 review: same-slot identical bytes, canonical ids, distinct caps

**Fix** — three review threads on #487, each fixed for its class.

1. **The same bytes twice in one image slot.** Two different entries carrying identical bytes
   passed validation, because `validateImageEntries` compares the entry strings. The reachable
   cases are two URLs serving one file, or a URL and a stored asset (two stored assets cannot
   collide: the library is unique on `(brief_id, checksum)`). Google returns the existing asset
   for duplicate content, so the slot carried one asset twice, and the request (the Search
   campaign links, the Performance Max asset group, the Display or Demand Gen ad) would fail
   after the budget and campaign exist. `fetchSlotImages` now refuses it after the bounded
   fetch and before any mutate, through `refuseRepeatedSlotImage` (a SHA-256 per slot index),
   naming both positions. One shared fetcher, so Search, Display, Demand Gen image and
   Performance Max are all covered. Reuse across slots is still accepted. The Demand Gen video
   logo (one per ad) cannot repeat, and Video has no image fetch.
2. **Canonical resource-name ids.** The shared set regex accepted an id past int64.
   `validateSharedNegativeKeywordLists` now applies `canonicalCampaignID` to the set id; the
   customer id must equal the account's exactly. The sweep of resource-name regexes found one
   more: `validateConversionActions` accepted any digit run in both spellings, so it now
   applies the same check. `demandGenAudienceRE` (Demand Gen audience, Performance Max signal)
   and the Display, Search and exclusion user-list parsers already did.
3. **The caps count distinct entries.** The 20-list shared-list cap ran on the raw input. It
   now counts distinct lists after trimming and de-duplication, as the catalog and the KB
   describe, checked inside the walk so it stops at the 21st distinct list. The pre-push
   sweep found the same bug in `validateConversionActions` (a raw cap of 100, then a de-dupe
   across the two spellings); it now counts 100 distinct actions the same way.

Details, tests and mutations are in the googleads concept, under "The same bytes twice in ONE
slot" and the shared negative keyword lists bullet.
