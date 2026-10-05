# 2026-10-02 — Google Ads Search serving readiness: CPC bid, flight window, negative keywords

**Update** — Closed the three Search serving-readiness gaps in the Google Ads
create cascade. A campaign this service creates is still PAUSED, but it is now
something a human can un-pause without first fixing it by hand in the Google
Ads UI.

**1. CPC bid on the ad group** (`internal/platform/googleads/adgroup_ad.go`).
`CampaignInput.CPCBid` is a manual CPC bid in whole units of the ad ACCOUNT's
currency, converted to `adGroup.cpcBidMicros` by the new `validateCPCBid`. `0`
means UNSET and omits the field — an explicit `"cpcBidMicros": 0` is a zero
bid, a different request. The OMISSION is done by `omitempty` on the payload
field, not by a flag the validator returns; `minCPCBid` is what keeps the two
meanings apart, since an accepted bid never rounds below 10000 micros. (The
Microsoft adapter's `validateCpcBid` does return such a flag, because its
payload is not `omitempty`-driven.) Accepted window `0.01`..`1000.0`, matching
`internal/platform/microsoft/targeting.go`, with NaN/Inf rejected explicitly
because they pass every ordered comparison. The window is not a Google platform
limit; Google documents no account-currency minimum, so unlike the Microsoft
client there is no fallback clause. It is loose on purpose — it exists to catch
the micros-vs-units mistake, and refusing a bid Google would have accepted is
the worse failure. `createAdGroupAndAd` gained exactly one parameter;
`precomputeAdGroupAdInputs` was left untouched at seven returns so the
positional destructuring in `adgroup_ad_test.go` still compiles. The ad-group
step string now reports the bid or its absence, and deliberately claims no
serving consequence — that has not been verified live.

**2. Campaign flight window** (`campaign.go`, `demandgen.go`).
`CampaignInput.StartDate`/`EndDate` are `YYYY-MM-DD` and render into the **v23**
`startDateTime`/`endDateTime` request fields as `"<date> 00:00:00"` /
`"<date> 23:59:59"`. These are the v23 names the settings readback already
documented on the read side: `startDate`/`endDate` were REPLACED in v23 and are
rejected as unrecognized, so the request side was written against the new names
from the start. A flight window is a property of the campaign rather than of the
channel, so the Demand Gen create carries it too. Each date is independently
optional.

Validation is a format regex THEN `time.Parse`, because
`time.Parse("2006-01-02", …)` accepts single-digit months and days — the meta
client pairs them for the same reason. The only cross-check is
`!end.Before(start)` when both are present: a SAME-DAY window is accepted, since
with the boundaries above it is a well-defined 24-hour flight and a one-day event
promo is an ordinary ad buy. Rejecting it would have contradicted the reason
those boundaries are explicit at all. There is deliberately **no past-start-date
check**, diverging from the meta client: Google interprets these in the ad
account's timezone, which this client does not know, so a UTC "today" would
refuse creates Google accepts.

**3. Negative keywords on the write path** (`targeting.go`, `campaign.go`).
`CampaignInput.NegativeKeywords` become CAMPAIGN-level `campaignCriteria` with
`negative: true`, batched into one atomic mutate by the new
`createCampaignNegativeKeywords`, inserted between the geo criteria and the ad
group. Campaign level keeps the exclusions applying to any ad group added later.
It is deliberately NOT folded into the geo `campaignCriteria:mutate`: a shared
mutate would make either list's failure discard the other, and geo's failure
sentence ("it has NO location criteria and would serve worldwide if enabled")
would be false for a dropped negative. `negative` carries no `omitempty` — the
field's whole purpose is to be `true`, and dropping it on a `false` would create
a POSITIVE keyword, buying the exact traffic the caller asked to exclude; a test
pins both values on the wire.

`validateKeywordList(noun, keywords, max)` was extracted so the positive path's
four error strings stay byte-identical while every negative-path message says
"negative keyword" — a test pins the original string. `maxNegativeKeywords = 60`
matches the positive cap rather than being set tighter; the 2026-08-13 incident
where a 20-keyword cap blocked every default create is why a cap here is sized
for real inputs. Dedupe is **per list**, because a term may legitimately be both
a positive and a negative keyword.

**Shared properties.** All three are optional and a no-op when absent, so every
caller predating them is unaffected. All three are validated inside
`preflightCampaignKind`, BEFORE the first budget `:mutate`, so a bad local input
cannot orphan a paid resource — and the same validation runs in
`ValidateCampaignInput`, so the adoption path cannot accept an input the create
path would refuse. The negatives mutate runs after the campaign exists and
follows the usual partial-result contract: the error comes back alongside the
non-nil `*CampaignResult`, never as `(nil, err)`.
`CampaignResult.NegativeKeywordCriteriaIDs` records what was created, with the
three-way presence convention `GeoCriterionIDs` documents.

**Dispatch** (`internal/dispatch/googleads.go`). `googleAdsConfig` gained
`negativeKeywords`, `cpcBid`, `startDate` and `endDate`, none with `omitempty`
— the channel-recognisability rule depends on every key always being written.
`startDate`/`endDate` are spelled as the meta and reddit configs spell them, and
both `applyCampaignConfig` call sites (create and adoption) now pass them, so
the recorded flight-window side of the settings comparison is populated whenever
the caller supplied a date; the comment claiming it is always nil was corrected.
All four are documented field by field in `docs/api-catalog.md` under
`GoogleAdsConfig` — `CreateCampaigns.config` is `Any` in the Goa design, so Goa
validates nothing and the catalog IS the consumer-facing contract. Creation is
async, so an undocumented rule surfaces as a `202` and a job that dies later,
with no synchronous error for the caller to debug against.

No Goa design change: these fields live inside the free-form `googleAdsConfig`
blob, which the design does not model.

**Tests.** `internal/platform/googleads/serving_readiness_test.go` covers the
three validators, the three JSON payload shapes (including that the pre-v23
date spellings never appear), and two end-to-end cascade tests — call order plus
payload contents, and seven bad inputs that must fail before any mutate with
`ValidateCampaignInput` refusing the same input.
`internal/dispatch/googleads_serving_readiness_test.go` proves the four config
fields reach the outbound requests, that omitting them reproduces the
pre-feature request byte-shape with no `campaignCriteria:mutate` at all, that
the flight window reaches the Demand Gen payload without leaking the Search-only
fields into it, and that a bad value fails pre-create.

The negatives mutate's four POST-CAMPAIGN failure branches each have their own
test, mirroring what the geo path already covers: a definite failure, a short
mutate response, a criterion resource name that is malformed/wrong-kind/another
campaign's, and a 429 that must not be retried. Every one asserts the
partial-result contract directly — the campaign is already paid for by then, and
with nothing pinning it a later `return nil, negErr` would have passed the whole
suite. Verified by making exactly that change locally: all four fail, then pass
again once reverted. Captures inside the `httptest` handlers are mutex-guarded
and decoded with the package's handler-safe `decodeRequest`, never `t.Fatalf`,
which calls `FailNow` and is valid only on the test goroutine; the dispatch test
gained `criteriaResultsOrErr` as the handler-safe form of `criteriaResults` for
the same reason.
