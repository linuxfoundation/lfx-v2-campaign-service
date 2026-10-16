# 2026-10-16 — Google Ads could bid toward a ROAS it could not report

**Update** — `GetCampaignMetrics` selected four columns: `metrics.impressions`, `metrics.clicks`,
`metrics.cost_micros`, `metrics.conversions`. `bidding.go` has been able to set `TargetROAS` since
the bidding work landed, so the service could instruct Google to optimise toward a return on ad
spend and then had no way to tell anyone whether it was achieved. `metrics.conversions_value` is
now selected, and ROAS is derived from it.

## The unit asymmetry is real and is why ROAS is derived here

`metrics.conversions_value` is typed DOUBLE in Google's field reference and is reported in WHOLE
units of the account's currency. `cost_micros` is micros. Scaling the value to micros to match
would have invented six decimal places the upstream figure does not carry, so the two now sit
beside each other in different units — which is a footgun unless something removes the need to
reconcile them. `Roas` does: it is computed once in the adapter, `conversions_value` over
`cost_micros/1e6`, which cancels the currency and leaves the one dimensionless figure in
`CampaignMetrics` that is comparable across accounts in different currencies.

Computing it in the adapter rather than at each consumer is the same call `Ctr` already makes, for
the reason this repo keeps relearning: two call sites dividing one pair of fields eventually
disagree about an edge case, and the disagreement surfaces as two dashboard numbers for one
campaign with no way to tell which is right.

## Three rows where ROAS must be WITHHELD

Each has an obvious wrong answer that renders as a real result, which is why `Roas` is a pointer:

- **No conversion value.** An unmeasured numerator.
- **Zero spend.** The ratio is a division by zero. A campaign that earned value without spending
  has an UNDEFINED return on ad spend — not an infinite one, and emphatically not 0, which states
  the opposite of what happened. Google omits zero-valued metrics entirely, so off a live account
  this case arrives as an ABSENT `costMicros`, not a `"0"`.
- **Negative spend.** `parseMetricInt` wraps `strconv.ParseInt`, which accepts a leading minus, so
  a corrupt cost reaches the division intact and would produce a negative ROAS indistinguishable
  from a campaign that genuinely lost money. Found by writing the guard as `costMicros > 0` and
  then asking what the `<0` half of that test was actually excluding — the first draft's comment
  claimed `parseMetricInt` already guaranteed non-negative, which it does not.

`TestGetCampaignMetrics_RoasWithheldWhenNotAMeasurement` covers all three as named subtests,
and the `roas` attribute description in `design/brief.go` enumerates all three — a user-facing
string that lists its cases has to list the last one too.

## The one assumption a test cannot check

The REST member name `conversionsValue` is assumed to be the lowerCamelCase of the GAQL column,
exactly as `cost_micros` → `costMicros` already is in the same struct. It is flagged in the struct
with the file's existing UNVERIFIED ASSUMPTION banner rather than left implicit, because the
failure is silent in the worst way: an absent member is deliberately decoded as a MEASURED 0.0, so
a wrong key would not error — it would report `conversionValue` 0 and `roas` 0 on every campaign.
Fixtures are written against the same assumption they would be testing, so only a live response
settles it.

The magnitude guard the two doubles share is now the single predicate `isUsableMetricDouble`
rather than the same four-condition test written twice in one function, so they are held to one
standard by construction; each call site keeps its own error sentence.

## The value's nil is weaker than the count's nil, and says so

`model.CampaignMetrics.Conversions` carries an AUDITED contract: which of the seven adapters
populate it was verified vendor reference by vendor reference, so a nil there is a statement about
the platform. `ConversionValue` claims less. Google Ads is the only adapter that populates it
today, and the other six were NOT audited for a value field — so its doc comment states that nil
means only "not measured on this read", and that adding another platform later is adapter work
which changes no contract. Reusing the stronger wording would have let a consumer infer, from this
change alone, that LinkedIn and Microsoft cannot report conversion value. Neither was checked.

The same distinction is pinned in the `campaign-metrics` design type, where `conversion_value` and
`roas` are both OPTIONAL and excluded from `Required`. The type's pinned object example is the
EMAIL shape and already omits `conversions`; it omits both new fields for the same reason, since
the email channel reports neither.

## Deliberately NOT extended

- **The account-monitor surface** (`design/connection.go`, `googleads/monitor.go`). That type is
  shared across four platforms, and no rule or consumer reads a return on ad spend. Adding an
  unread field there would widen a shared four-platform contract to carry something only one
  platform populates and nothing consumes.
- **`internal/service/rules`.** No rule evaluates ROAS, so `rules.Input` is unchanged and nothing
  fires or fails to fire differently. Both fields are reporting surface only.

## Files

`internal/platform/googleads/metrics.go` (SELECT, both struct fields, the magnitude guard, the
derivation), `internal/domain/model/campaign.go`, `internal/dispatch/googleads.go`,
`design/brief.go` + `make apigen`, `internal/service/brief.go` (both `briefs.CampaignMetrics`
mapping sites).
