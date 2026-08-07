# 2026-08-06 — GA-3b: cascade partial-result coverage and the weighted-character doc contradiction

**Update** — dealako flagged that `TestUpdateAdGroupAndAdStatus`'s failure sub-test drives BOTH
`adGroups:mutate` and `adGroupAds:mutate` to 5xx, so `adgroup_ad.go`'s
`&partialCascadeError{stage: "ad"}` branch was never reached. That branch is what tells the
dispatcher a toggle was PARTIALLY applied rather than not applied at all — two states that demand
different recovery — so a regression dropping the wrapping would have left the suite green. Added a
sub-test where only the ad mutate fails, asserting `IsOutcomeUnconfirmed`, the concrete
`*partialCascadeError`, its `stage`, and that the ad-group mutate really was issued (which is what
makes the outcome partial rather than a no-op).

**Update** — `TestCreateAdGroupAndAd_AdCreationFails` and the ad-ambiguous-5xx case beside it
discarded the returned `CampaignResult` with `_`. On those paths the result is the load-bearing
contract, not the error text: the campaign and ad group exist upstream, and the dispatcher needs
their ids to reconcile instead of stranding a created hierarchy. Both now assert via
`assertPartialAdGroupResult` that `CampaignID`/`AdGroupID`/`AdGroupName` are populated and `AdID` is
empty — the empty `AdID` being the signal that the cascade stopped short. Copilot had raised this on
an earlier commit and it was still open.

**Update** — Nothing verified that GA-3b's four new dispatcher field mappings
(`RegistrationURL`, `Headlines`, `Descriptions`, `EventSlug`) reached the wire. The client-level
tests build `CampaignInput` directly and so cannot catch a dispatcher that drops one, and the
cascade's fake handlers accept anything. `googleAdsServers` now records both cascade `:mutate`
bodies, and `TestGoogleAds_AdCopyMappingsReachTheWire` decodes the ad payload rather than
substring-matching it, so a HEADLINES/DESCRIPTIONS swap is detectable and not just a dropped value.
Verified binding three ways: nulling `Headlines`, redirecting `RegistrationURL`, and swapping the
two asset lists each fail it with the specific diagnostic.

**Update** — The documented headline/description limits contradicted the implementation, and the
docs were the side that was wrong. `docs/api-catalog.md` and this repo's googleads concept both
described 30/90 as plain RUNE counts, and the concept went further and asserted "there is NO
double-width-character halving rule here". `ad_copy.go`'s `googleAdsCharWeight` scores CJK and
full-width runes as 2 and `truncateWeighted` cuts to that budget, so all-wide-character copy fits
15/45 and is silently truncated at that point — exactly what the docs promised could not happen, on
a public API contract. Both now document the weighted counting and the effective 15/45 figures, and
note this MATCHES the Microsoft client's rule rather than differing from it. The behaviour itself
was already pinned by `TestComposeAdCopy_CJKHeadlineStaysUnderEffectiveWidth` and the
`truncateWeighted` table; only the prose needed correcting. Copilot had raised the same
contradiction across at least three rounds.
