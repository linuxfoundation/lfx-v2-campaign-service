# 2026-10-28 — ROAS contract tightened on review, and pinned at the service boundary

**Fix** — Review of #364 after merging `main` into it. No behaviour change; three gaps
between what the code does and what its contract and tests said.

## The adapter's own comment listed two withheld cases, not three

`googleads.CampaignMetrics.Roas` said "two distinct cases reach nil" (no value, no spend)
while the code's `costMicros > 0` guard, `model.CampaignMetrics.Roas` and the `roas` API
attribute all name three — negative cost included. The adapter comment is the one the
derivation is written against, so it now names all three, and says why the no-value case
cannot arise there (this adapter always populates `ConversionValue`).

## The API did not say what scale ROAS is on, or what its absence means

`roas` is a ratio (2.41 = 241%), the same scale as the target ROAS `bidding.go` sends.
Nothing said so, and a percentage reading is the natural mistake. The attribute now says it,
and also says that its absence carries no meaning of its own: which case withheld it is
recoverable from `conversion_value` and `cost_micros`, so it does not add a second overloaded
nil of the kind #175 records for `conversions`. A present 0 is always a measurement.

`cost_micros` now states that Google Ads reports it in the ad account's own currency — the
same one `conversion_value` is in — so the "same currency" premise of `roas` is written down
on both fields rather than only on one.

## Neither service mapping site was tested

`GetCampaignMetrics` and `renderBriefMetrics` each copy the two pointers into the response,
and no test caught either copy being dropped or flattened. Added
`TestBriefService_GetCampaignMetrics_ConversionValueAndRoasPassThrough` and
`TestGetBriefMetrics_RowCarriesConversionValueAndRoas`, both covering the zero-spend row
where a measured-zero value sits beside a withheld ROAS.

## Merge with main

Only the five single-line OpenAPI JSON specs conflicted, resolved by `make apigen`. #364
adds no create or dispatch path, so the google-ads create-outcome contract #405 wrote into
`PlatformResult.error` needed no extension; the new read failure ("conversions value is not
a usable amount") surfaces through the existing metrics-read error handling.
