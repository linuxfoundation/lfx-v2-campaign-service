# 2026-08-18 — LFXV2-3067 budget rounding fabricated a match; unknown_count was overloaded

**Fix** — the settings readback exists to make a fabricated agreement impossible. One could still
be produced, not by a nil, but by the **formatter**.

## The rounding defect

Both sides of the budget comparison went through `formatBudgetUnits`, which renders at two
decimal places to match the row's `NUMERIC(14,2)` column. The upstream side arrives in **micros**,
so a budget of 10.004 (`10_004_000` micros) rendered as `"10.00"` — byte-identical to a recorded
`10.00` — and the field reported `match` for two budgets that genuinely differ.

Measured before the change:

```
micros=10004000  upstream_render="10.00"  recorded="10.00"  equal=true
```

The nil handling was never the only way to manufacture agreement; rounding was the other, and it
was the one nothing tested.

**Reachability is not theoretical, and it is worse than it first looks.** This service's own
create path rounds to micros from a 2dp amount, so it cannot produce sub-cent micros. Campaigns
**adopted** from outside it can — and adopted campaigns are precisely the population a settings
readback exists for, since adoption is what lets the recorded request and the live campaign
disagree at all. The defect was reachable exactly where the feature is useful.

`googleAdsUpstreamBudgetAmount` now checks `micros % 10_000 != 0` and renders those values at
full precision (`"10.004"`), so they compare unequal and the operator sees the digits that
differ. Whole-cent budgets keep the existing 2dp rendering, so ordinary campaigns do not start
reporting a divergence that does not exist — pinned in the other direction by
`TestGoogleAds_ReadSettings_WholeCentBudgetStillMatches`, because a fix that made everything
diverge would be just as useless as one that made everything match.

The remainder test uses **integer** arithmetic rather than float division: above 2^53 micros
float64 cannot represent every value, and whether two budgets compare equal must not depend on
that boundary.

## The date-prefix defect, same shape

`googleAdsDateOnly` split on the first space and kept the prefix without validating the rest, so
`"2026-08-01 garbage"` became `"2026-08-01"` and compared **equal** to a recorded `2026-08-01`.
Another agreement manufactured out of a response the code could not actually parse. It now parses
the full `yyyy-MM-dd HH:mm:ss` shape and passes anything else through whole, so an unparseable
value can only ever read as a divergence.

## `unknown_count` claimed something false

The field doc, the Goa description and the api-catalog all described `unknown_count` as how many
fields **"could not be read"**. Measured on a completely healthy readback where every upstream
field was returned and nothing failed:

```
fields=10  diverged=0  unknown=7
```

Seven of the ten are permanently `unknown` **by construction** — the five upstream-only
observations, `status` (deliberately never compared, different axes), and the two flight dates,
whose recorded side is always empty for Google Ads today. "Could not be read" is false for all
seven. An operator watching `unknown_count` for read failures sees a constant floor of 7 and
cannot distinguish it from a real decode failure, so the number they would act on is inert.

This is the `pending-claim-is-overloaded` shape: one token carrying two operator meanings.

The **response shape is unchanged** — splitting the count would be an API contract change, and
the honest fix for a number whose documentation lied about it is to correct the documentation.
All three descriptions now say "were NOT COMPARED", name both reasons, state explicitly that it
is not a read-failure count, and point at the per-field `comparison` as what distinguishes them.
The Goa `Example` moved from 2 to 7 so the generated OpenAPI stops implying a low number is
typical.

## The write-back property had no service-layer test

The dispatch-layer test proves the **adapter** does not mutate the campaign struct it is handed.
That is a different property from "the handler persists nothing": a handler can leave the struct
pristine and still write the observation through the repository. A mutation adding an
`UpsertCampaign` call to `GetCampaignSettings` **survived the entire suite**.

`TestBriefService_GetCampaignSettings_PersistsNothing` closes it, asserting on the fake repo's
recorded writes. Re-injecting the same mutation now fails it.

## Verification

Every guard confirmed by a compiling revert, not by reading it:

| Reverted | Test that failed |
| --- | --- |
| sub-cent full precision → 2dp rounding | `TestGoogleAds_ReadSettings_SubCentBudgetIsNotRoundedIntoAMatch` |
| full datetime parse → split on first space | `TestGoogleAdsDateOnly` (4 new cases) |
| service handler persists the readback | `TestBriefService_GetCampaignSettings_PersistsNothing` |
| `nil`/`nil` verdict → `match` | `TestCompareSettingsField_AbsentIsNeverAMatch` |
| mutating call added to `ReadSettings` | `TestGoogleAds_ReadSettings_IssuesNoMutatingCall` |

The last two already existed and were confirmed genuinely binding rather than assumed to be.
