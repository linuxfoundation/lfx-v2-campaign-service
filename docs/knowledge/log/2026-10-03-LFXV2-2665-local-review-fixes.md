# 2026-10-03 — LFXV2-2665 local review fixes (geo id canonicalisation, campaign-stage UNCONFIRMED, catalog channel drift)

**Fix** — four findings from the local pre-PR review round on `7e1b0101`, each
verified against the code before being acted on.

## Raw numeric geo ids were checked on SHAPE alone

`resolveGeoEntry` (`internal/platform/googleads/geo.go`) accepted any digits-only
entry whose digits were not all zero. Three faults got through that are decidable
locally, with no lookup: `"0"` names nothing, `"02840"` is a non-canonical
spelling of `2840` (two criteria for one place, which Google rejects as a
conflict), and a 21-digit run overflows the int64 Google exposes these ids as.
All three would be refused only at `campaignCriteria:mutate` — AFTER the budget
and campaign are committed — so a typo stranded a paid campaign.

`canonicalCampaignID` is REUSED rather than reimplemented. It is already this
package's answer for exactly this class of value (`ValidateKeywordActions` uses it
on ad group and criterion ids, naming the leading-zero spelling in its comment),
it does `ParseInt` + `v <= 0` + a round-trip equality check, and collapsing every
spelling to one is the whole point. This is NOT the lookup the surrounding comment
declines to do: whether `1014044` names a real place still needs Google, and a
well-formed id naming nothing is still refused upstream. Only the
locally-decidable faults moved earlier.

`TestValidateGeoTargets_RejectsNonCanonicalNumericIDs` covers all five spellings on
both the target and the exclusion entry point.
`TestValidateGeoTargets_StillAcceptsCanonicalNumericIDs` is the over-refusal guard
— the tightening must never refuse an id the previous code accepted and Google
would have taken.

## `UpdateCampaignStatus` discarded its mutate response

The ad group and ad stages were already held to the short-2xx standard
(`checkStatusMutateResults`, `adgroup_ad.go`), but the campaign stage was
`if _, err := c.doRequest(...)` — a 2xx acknowledging no operation read as a
confirmed flip. That is the worst of the three stages to get wrong: on PAUSE the
campaign mutate runs FIRST, and its success is what gates the child cascade, so an
unacknowledged flip taken as confirmed sends the children on from a state nobody
verified.

The refusal is wrapped in a new `unconfirmedCampaignStatusError`, NOT in
`partialCascadeError`. The latter's `Error()` reads "status cascade partially
applied — the X stage failed after the preceding ones succeeded", which is FALSE
on PAUSE, where the campaign stage has no preceding stages. Reusing it would have
been a helper with a mismatched contract. The new type follows the package's
one-small-typed-error-per-surface convention (`unconfirmedBudgetMutateError`,
`partialCascadeError`, `unconfirmedKeywordError`) and satisfies the same
`Unconfirmed() bool` behavioural interface `IsOutcomeUnconfirmed` honours, so the
dispatcher carries it through as "verify upstream before retrying" and the claim
stays RETAINED. A retry re-applies the same idempotent status.

`TestUpdateCampaignStatus_ShortMutateResponseIsUnconfirmed` covers the empty-results,
absent-results and malformed-body cases;
`TestUpdateCampaignStatus_ExtraResultsDoNotFailACorrectFlip` is the over-refusal
guard, since extra results are ACCEPTED by the same standard elsewhere.

## `DeviceBidModifiers` doc comment under-reported the vocabulary

The dispatch config comment listed MOBILE/DESKTOP/TABLET; `validateDeviceBidModifiers`
and `docs/api-catalog.md` both list four, including `CONNECTED_TV`. The comment now
matches, and additionally states why `bidModifier` is pointer-typed and REQUIRED
here when the same field is optional on `adSchedules`: an omitted modifier decoded
as 0 is the -100% opt-out, which would switch a device off while reporting a
successful create.

## The catalog permitted five fields the client refuses on Demand Gen

`validateCriteriaPlan` refuses `languages`, `adSchedules`, `deviceBidModifiers`,
`excludedAgeRanges` and `excludedGenders` outright when `campaignType` is
`demand-gen`, but none of the five `docs/api-catalog.md` entries said so — while
every OTHER Search-only field added in the same change (`proximityTargets`,
`cpcBid`, the three extension fields, `adGroups`) did carry the restriction. A
consumer following the catalog got a 202 and a dead job. The section preamble's
"every one of them is additive" compounded it by reading as channel-independent.

Since `CreateCampaigns.config` is typed `Any` in `design/`, the catalog IS the
consumer-facing validation contract, so this was a real contract defect rather than
a documentation nicety. All five entries now carry the restriction, the preamble is
qualified, and the Go struct comment's "all four" is corrected to name all FIVE
slices the one guard refuses together.
