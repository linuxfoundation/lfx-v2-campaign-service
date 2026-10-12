# 2026-10-05 — Google bidding strategies and conversion actions (LFXV2-2665)

**Creation** — a Google campaign can now be created with a chosen bidding
strategy and the conversion actions it optimizes toward. Before this the
strategy was hard-coded per channel — `manualCpc {}` on Search, `targetSpend {}`
on Demand Gen — so no campaign this service created could bid toward a
conversion at all, whatever the account had configured.

`internal/platform/googleads/bidding.go` adds `BiddingStrategy`, `TargetCPA`,
`TargetROAS` and `ConversionActions` to `CampaignInput`. The strategy names are
the Google Ads **UI's** vocabulary, lower-cased and hyphenated — `manual-cpc`,
`maximize-clicks`, `maximize-conversions`, `target-cpa`,
`maximize-conversion-value`, `target-roas` — because the operator choosing one
is reading that UI and not the proto. `target-cpa` and `target-roas` are where
the two vocabularies disagree: Google folded the standalone strategies into
MaximizeConversions and MaximizeConversionValue with a target set, and the UI
kept the old names, so both spellings resolve to the surviving strategy and the
only difference is that the target is REQUIRED under the target-bearing label.

A campaign carries **exactly one** strategy — it is a proto `oneof`, and naming
two is rejected AFTER the budget mutate. So the whole plan resolves in the pure
preflight, and `biddingFields` is one shared type embedded **anonymously** into
both channel payloads rather than a copy per channel: the oneof is a property of
the campaign resource, not of a channel, and two copies would be two places to
break it.

The defaults are byte-identical to what each channel hard-coded, pinned by a test
that asserts the marshalled strings `{"manualCpc":{}}` and `{"targetSpend":{}}`.
Every campaign this service has ever created bid by manual CPC; a default that
drifted would silently re-bid all of them. `manualCPC` and `targetSpend` are
empty structs for the same reason `cpcBidCeiling` is not offered — their
remaining proto fields are deprecated, and a deprecated field risks a rejection
landing after the budget mutate.

Demand Gen is fenced to `maximize-clicks` alone, on recorded evidence rather than
caution: a live `validateOnly` check (2026-08-14, v23) had DEMAND_GEN accept
`targetSpend` with HTTP 200 and reject `maximizeConversions` with HTTP 400
`BIDDING_STRATEGY_TYPE_INCOMPATIBLE_WITH_SHARED_BUDGET`. Widening that set is a
live-API question, not a reading of the proto.

`TargetCPA` is micros like every other currency field here. `TargetROAS` is the
one exception — `target_roas` is a proto double and Google takes the RATIO as
written, so it is not scaled, and its `0.01`..`1000.0` window is Google's own
documented one. `400` is **accepted** even though it is also how `400%` is
commonly mis-typed: refusing it would refuse a target Google accepts, which is
the over-refusal these guards exist to avoid. The ratio spelling is documented on
the field and named in the out-of-range error instead.

Conversion actions attach at CREATE time through
`campaign.selective_optimization`, not `campaignConversionGoal` — that resource
is update-only and addressed by a name containing the campaign id, so it would
need a second mutate after the campaign exists, a step that can fail and leave a
campaign bidding toward the account's goals with nothing in the result to say so.
Either spelling is accepted (bare id or full resource name), bare ids are
qualified with the campaign's own account, a name owned by a different customer
is refused, and duplicates across the two spellings are deduplicated rather than
refused.

Three things are REFUSED rather than dropped, extending the doctrine the rest of
the package already follows: a target the named strategy cannot carry; a CPC bid
under any automated strategy, checked at ad-group level as well as campaign level
because a campaign-field-only check would miss the override a caller reaches for
first; and conversion actions on Demand Gen, which does not accept
`selective_optimization`. Demand Gen's own mechanism
(`conversion_goal_campaign_config`) is a **known gap**, refused rather than
silently dropped, pending a live-API check.

`internal/platform/googleads/conversions.go` is the read half and is strictly
read-only: `ListConversionActions` is one GAQL search so a caller can build a
picker instead of hand-copying an id out of the Google Ads UI. REMOVED actions
are excluded in the WHERE clause — a removed action cannot be attached at all —
while PAUSED ones are kept, since a paused action is attachable. **Creating** a
conversion action is deliberately not offered: it is half a measurement setup,
and one created without its site tag reports as configured while recording
nothing.

`internal/dispatch/googleads.go` carries the four fields through with a mapper
that validates nothing, as every other block here does. They need no snapshot
sanitizing — a strategy label, two numbers and account-scoped ids carry no URL
and no credential — and a test pins that decision rather than leaving it
implicit.
