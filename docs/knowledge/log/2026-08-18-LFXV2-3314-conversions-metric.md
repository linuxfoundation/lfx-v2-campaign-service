# 2026-08-18 — LFXV2-3314: the conversions metric and the fifth pacing rule

**Update** — `internal/service/rules` shipped four rules. The fifth, "clicks without
conversions", could not be built: `model.CampaignMetrics` carried impressions, clicks,
cost_micros and CTR only, and no dispatcher's `ReadMetrics` populated a conversion count.

`Conversions *int64` is now on `model.CampaignMetrics`, on `rules.Input`, and on the
`campaign-metrics` wire type as an OPTIONAL attribute. The rule `no_conversions` is the fifth
entry in `Evaluate`.

## Conversions come from the platform APIs, not Snowflake

An earlier scoping note claimed this needed a Snowflake-backed source. That is false, and it is
worth recording as false: every conversions-capable platform here exposes the count through the
same reporting call the adapter already makes. Only REVENUE would need Snowflake, and no rule
in this package uses revenue.

## What was verified, and what the verification changed

Each platform was checked against its vendor's published reference before any field mapping was
written. Three findings changed the implementation from what a reasonable guess would have
produced:

- **Microsoft's `Conversions` column is deprecated as of 2022.** Microsoft's
  `CampaignPerformanceReportColumn` reference directs callers to `ConversionsQualified` and
  states the legacy column's values "may be inaccurate", because it cannot represent the decimal
  conversion values Microsoft now supports. Requesting the obvious column would have returned a
  *wrong* number rather than an older one — the failure-as-measurement class this repo refuses
  elsewhere. The request asks for `ConversionsQualified`.
- **Google's and Microsoft's counts are DOUBLES.** `metrics.conversions` is `DOUBLE` in Google's
  field reference and `ConversionsQualified` is documented as `double`; both platforms credit
  fractional conversions under attribution models that split credit. Both are rounded, not
  truncated — truncation reports a campaign holding 0.8 of a conversion as having none. Google's
  value is also a bare JSON number rather than the quoted string its int64 metrics use, so it
  needs a `*float64` field: decoding it as a string fails on every converting campaign.
- **Microsoft's conversions column is resolved but NOT required.** It is populated only for
  accounts using Universal Event Tracking, and Microsoft notes the qualified column is not yet
  available to every advertiser. Requiring it would fail the entire read — impressions, clicks
  and spend included — for any account that does not track conversions.

Confirmed additions: Google Ads `metrics.conversions` (added to the GAQL SELECT), LinkedIn
`externalWebsiteConversions` (typed `long`, and it must be named in the request's `fields` list
because LinkedIn returns only impressions and clicks by default), Microsoft
`ConversionsQualified`.

## Four platforms report nothing, and say so by being absent

Meta exposes conversions only inside the Insights `actions` array as `{action_type, value}`
objects; X splits them across per-event-type metrics (`conversion_purchases`,
`conversion_sign_ups`, …) that are JSON objects rather than counts, under metric groups this
client does not request; Reddit's v3 reporting contract has no public documentation at all; and
HubSpot's marketing-email statistics vocabulary contains no conversion counter, because an email
send has no campaign-level conversion concept.

For all four the field is left **nil**, never `0`. This is the reason `Conversions` is a pointer
rather than an `int64`: a fabricated zero is indistinguishable from a measured one to every
consumer, and it would make `no_conversions` fire on every campaign on those platforms forever —
reporting the absence of measurement as a campaign defect. Deriving one number for Meta or X
would additionally require choosing which action types count as a conversion, which is a
per-advertiser attribution policy this service is never given.

The contrast inside the HubSpot adapter is the clearest statement of the distinction:
`CostMicros: 0` stays a MEASUREMENT (HubSpot genuinely bills nothing per send, so zero is the
true cost) while `Conversions` is nil (there is no true value to report). The two absences are
different facts and the types now say so.

## The rule

`no_conversions` fires at HIGH priority on a campaign with zero measured conversions over at
least `minClicksForConversions` (50) clicks. Three gates, each preventing a fire on missing data
rather than on a finding:

- `Conversions != nil` — the platform must report conversions at all. This is the same shape of
  gate `BillsPerDelivery` gives `zero_delivery`: a rule meaningless on a channel does not fire
  there.
- The click floor — the analogue of `low_ctr`'s `minImpressionsForCTR`. It is lower (50 vs 1000)
  because clicks sit a funnel step further down and arrive one to two orders of magnitude less
  often; reusing the impression floor would mean the rule effectively never fired.
- `isActive` — per `docs/api-catalog.md`, "Every rule is gated on the campaign's status … a
  `paused` campaign raises nothing."

## Tests

Every new test was mutation-tested with a compiling revert. The binding case at each layer is
the one that distinguishes an ABSENT conversion count from a MEASURED zero: a test that passes
under both is not testing the thing the pointer exists for. Mutations that flattened a nil to
zero — in the Google Ads client, the LinkedIn aggregation, the Microsoft column resolution, the
dispatcher mapping, and the service's `rules.Input` construction — each failed a test naming
that substitution. The Meta, X and Reddit fixtures deliberately CARRY conversion-shaped payloads
in each vendor's published response shape, so a future change that starts opportunistically
reading them fails rather than silently reporting a number the adapter cannot justify.
