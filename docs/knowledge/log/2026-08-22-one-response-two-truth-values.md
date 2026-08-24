# 2026-08-22 — one response cannot carry two truth-values

**Fix** — LinkedIn's empty-`elements` branch returned a struct in which `Impressions`, `Clicks`
and `CostMicros` were **0 as measurements** while `Conversions` was **nil, meaning "not
measured"**. Same response, same wire read, opposite meanings about the same window.

## The three adapters held three conventions for one field

`model.CampaignMetrics.Conversions` is a `*float64` read directly by the `no_conversions` rule
and passed through to the brief response. Three adapters populate it, and each had settled a
zero-activity window differently:

- **Google** returns a **non-nil zero** on a no-rows window, and its comment says why: a
  no-activity window is a MEASUREMENT, not an absence of one.
- **LinkedIn** returned **nil** on an empty `elements` array.
- **Microsoft** withdraws to **nil** on a blank `ConversionsQualified` cell.

Three conventions for one wire field is not three decisions; it is one decision nobody made.

**Google was already right, and LinkedIn now matches it.** The deciding evidence is the
contract already written on the domain field: nil is reserved for platforms that cannot report
a campaign-level conversion count **at all** (Meta, X, Reddit, HubSpot — each for a documented
property of that vendor's API). LinkedIn is not one of them. The client always names
`externalWebsiteConversions` in the request's `fields` list, and the decoder already rejects a
null/missing `elements` as a decode error, so a well-formed empty array is LinkedIn *answering*:
the metric was requested and the campaign did nothing. Returning nil there asserted "LinkedIn
cannot measure conversions", which is false, and which no evidence on that branch supports.

## The distinction that survives, and is not the same question

LinkedIn's per-element withdrawal is **kept**, and it is not in tension with the above. An
element LinkedIn RETURNED that omitted the metric is **missing data about activity that
happened**; an empty array is **no activity to have measured**. The first is unknowable and
must stay nil — summing only the elements that carried a value hands consumers a partial count
labelled complete. The second is answered. Both branches now say what they actually know.

**Microsoft deliberately stays different**, and forcing uniformity there would have been the
error the sweep existed to prevent. Its blank cell is a row Microsoft returned *with clicks and
impressions on it* whose conversions cell is empty — activity happened and the count was not
reported. `ConversionsQualified` is only populated for accounts wired to Universal Event
Tracking. That is genuinely missing data, not an answered zero, and the existing test fixture
(`1000` impressions, `25` clicks, blank cell) is precisely the shape that must not become a
measurement. Uniformity across adapters is a property worth having only when the upstream
semantics agree; here they do not.

## Check the consumer before changing the producer

The change looked like it had a product edge — a nil suppresses `no_conversions`, a zero can
fire it HIGH. Tracing `internal/service/rules/actions.go` showed the rule is gated on
`isActive && Conversions != nil && Clicks >= minClicksForConversions && *Conversions == 0`,
with `minClicksForConversions = 50`. The empty-`elements` branch reports **zero clicks**, so
the rule **cannot fire on it under either convention**. The behavioural consequence of this
change on the rules engine is therefore **nil** — the click floor already dominates.

What the change actually buys is for the *other* consumers: `brief.go` passes the pointer
straight into the API response, where nil is omitted from the body rather than serialized. Those
readers now see "measured, and the answer was none" instead of "unmeasured", which is the true
state of a window LinkedIn answered.

**The generalisation.** A struct whose fields disagree about whether the same read happened is
reporting two facts about one event. When a pointer's nil is defined as "the platform cannot
measure this", only evidence about the PLATFORM may produce a nil — evidence about a
particular WINDOW never can.

## Mutation notes

- Reverting `zero := 0.0` to the bare struct (the original bug) was killed by both new tests.
- `zero := 0.0` → `zero := 1.0` was killed too: the assertion is deliberately two-sided
  (non-nil AND exactly zero). A nil-only assertion would have let any wrong non-nil value
  through, and a `Conversions == nil` test passes vacuously against a fixture that never
  populates the field — which is why the fixture serves a real empty array over HTTP and the
  handler asserts the metric was actually named in the query.
- Neutralising `conversionsIncomplete` (omission no longer withdraws) was killed by the two
  pre-existing partial-coverage tests, confirming the nil path for genuine missing data is
  still load-bearing after the change.
- The pre-existing `TestGetCampaignMetrics_NoElementsLeavesConversionsAbsent` pinned the OLD
  convention and had to be inverted rather than deleted. Its stated rationale — that a zero
  here is one "the rule would act on" — was already false when written, for the click-floor
  reason above. A test can encode a wrong reason and still pass.
