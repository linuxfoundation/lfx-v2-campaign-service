# 2026-10-03 — Branch-wide sweep fixes to the googleads criteria paths

**Fix** — Three defects found by a file-by-file sweep of the whole
`feat/google-search-serving-readiness` branch diff, fixed together: a
non-canonical numeric language id, an unhandled exact-duplicate ad schedule, and
campaign-level negative keywords silently dropped on Demand Gen.

**1. `resolveLanguageList` accepted non-canonical numeric ids.** The language
list had the same shape-only defect a reviewer had already caught on the geo
side: `numericID` is a digits-only loop, so `"0"`, `"01000"` and a 21-digit run
all passed as ids. `"01000"` is the damaging one — it is a second spelling of
`1000`, so the dedupe below it would let TWO criteria for English through. Fixed
by putting the entry through `canonicalCampaignID`, exactly as `resolveGeoEntry`
does, which refuses all three faults and collapses every spelling to one.
Existence stays Google's to decide: a well-formed id naming no language is still
refused upstream, so this does not over-refuse. The doc comment's "as on the geo
side" claim, which the geo fix had quietly falsified, now holds again.

**2. `validateAdSchedules` had no duplicate handling.** It was the only list in
`campaign_criteria.go` without one. The deliberate decision NOT to check
interval OVERLAP is unchanged — overlap is not locally decidable against
Google's own splitting rules — but an EXACT repeat is: the same day and the same
half-open window is one criterion written twice, which Google refuses as an
overlapping ad schedule only after the campaign exists. Handled the way the rest
of the file handles repeats, and the way `DeviceBidModifiers` already did:
collapse when the two entries agree, refuse when they disagree about the bid
modifier, because one of the two values would otherwise be the one silently
dropped. The new `sameBidModifier` compares optional modifiers by value so a
repeat carrying the same adjustment through a separate pointer still collapses;
NaN cannot reach it because `validateBidModifier` runs earlier in the same loop
iteration.

**3. `NegativeKeywords` were validated on both channels but attached only on
Search.** `preflightCampaignKind` ran `validateNegativeKeywords` with no kind
gate, while `createCampaignNegativeKeywords` is called only from
`CreateCampaign`'s cascade — `CreateDemandGenCampaign` never reads
`pf.negativeKeywords`. A Demand Gen create therefore validated every term and
discarded the lot, and the operator read "campaign created" while the campaign
kept paying for exactly the queries they had named. Now REFUSED, like every
other Search-only input in that block. It is deliberately not modelled on
`Keywords`, which IS ignored on Demand Gen: that silence is defensible because
Demand Gen creates no ad and no keyword criteria at all, so a positive keyword
has nothing it could attach to and the caller is told so in the closing step. An
exclusion is the opposite case — its whole job is to stop spend. The old doc
comment recorded the silent drop as intentional and was removed rather than left
to contradict the code; `docs/api-catalog.md` and the dispatcher's
`GoogleAdsConfig.NegativeKeywords` comment were corrected in the same motion,
since `CreateCampaigns.config` is typed `Any` and the catalog IS the
consumer-facing validation contract.

Covered by five tests in `campaign_criteria_test.go` and one in
`serving_readiness_test.go`, each paired with an over-refusal guard asserting
the canonical/distinct/Search cases still pass.
