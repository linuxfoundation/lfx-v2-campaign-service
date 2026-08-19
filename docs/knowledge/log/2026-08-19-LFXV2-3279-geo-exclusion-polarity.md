# 2026-08-19 — LFXV2-3279 geo criterion polarity on the reuse read

**Fix** — The reuse read introduced earlier in this branch counted a location EXCLUSION as a
satisfied positive target, because it decoded only the nested `Criterion` and dropped the outer
`CampaignCriterion` wrapper `Type`.

**A CampaignCriterion is polymorphic and the wrapper Type is the polarity.** Microsoft's v13
`CampaignCriterion` is a base type whose concrete subtype is either `BiddableCampaignCriterion`
(a POSITIVE target) or `NegativeCampaignCriterion` (an EXCLUSION). The two carry the SAME nested
`LocationCriterion` shape and therefore the same `LocationId`, so the nested type discriminator
cannot tell them apart — only the wrapper `Type` can. This client's own ADD path already sets
that field explicitly (`Type: campaignCriterionTypeBiddable`), which is what made the omission on
the read path visible: the write states the polarity, the read discarded it.

**The failure mode is worse than the duplicate the read exists to prevent.** A reused campaign
carrying a US EXCLUSION made a requested US TARGET look already-present, so the attach was
skipped, the cascade completed, and the run reported success — for a campaign that EXCLUDES the
country it was asked to serve. That is silent and the steps line actively claimed the targeting
was in place, so nothing downstream had a signal. Unlike the untargeted-campaign case this
ticket started from, the money is not spent too widely; it is spent everywhere EXCEPT where the
brief asked, which no pacing or spend guard would flag as anomalous.

**Fail closed on anything unclassifiable.** The wrapper `Type` is decoded as a POINTER so an
absent key is distinguishable from an empty string. A `NegativeCampaignCriterion` is classified
and simply excluded from the target set; an ABSENT type or an unrecognised value fails the read.
Neither guess is safe there — assuming "target" skips a needed attach, assuming "not a target"
duplicates a criterion that already exists — and both spend money, so "we could not classify it"
propagates as an error rather than collapsing into either branch. This is the same discipline the
truncation and `PartialErrors` guards on this read already apply.

**Mutation-tested.** Reverting to the pre-fix behaviour (ignore the wrapper type, admit every
`LocationId`) still compiles and fails four tests: the unit case asserting an exclusion is not a
target, the end-to-end case asserting a reused campaign carrying only an exclusion is still
attached, and the two unclassifiable-type refusals. The two pre-existing reuse fixtures were also
corrected to carry the `BiddableCampaignCriterion` wrapper the real API returns — they had
modelled a response shape Microsoft does not send.
