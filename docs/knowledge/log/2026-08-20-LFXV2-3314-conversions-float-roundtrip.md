# 2026-08-20 — LFXV2-3314 the conversions accumulator round-tripped through float64 every element

**Fix** — `linkedin.GetCampaignMetrics` aggregated `externalWebsiteConversions` using the
domain model's `*float64` field as the accumulator, so every iteration read the running
total back out of a float64 and converted it to an int64 again:

    var running int64
    if metrics.Conversions != nil {
        running = int64(*metrics.Conversions)
    }
    if conv > math.MaxInt64-running {
        return nil, fmt.Errorf("...aggregate externalWebsiteConversions overflows int64")
    }
    total := float64(running + conv)
    metrics.Conversions = &total

The comment directly above it already claimed the opposite — "the running total is
accumulated as an int64 and only widened at the end" — and so did the concept doc. The
claim was the correct design; the code did not implement it.

Above 2^53 float64 stops being able to represent every consecutive integer, so the
`int64 -> float64 -> int64` round trip is not lossless. Two things follow. Increments are
silently swallowed: once the running total reaches 2^53, adding 1 produces a value float64
cannot hold and it rounds straight back down, so the element contributes nothing. And the
overflow guard on the NEXT iteration is computed against `running`, a value that is no
longer the true sum — near the top of the range that can reject a valid aggregate or admit
one it should have refused.

The fix keeps a local `int64` accumulator plus a `bool` presence flag across the whole loop
and widens once after aggregation, matching what `CostMicros` in the same loop already does.
The presence flag matters: the previous code used "pointer is non-nil" as both the running
value and the has-been-measured signal, and the nil-versus-measured-zero distinction the
field exists to protect has to survive the accumulator no longer living in the field.

The overflow check now runs against the exact int64 accumulator, which is where it was
always meant to run — `externalWebsiteConversions` is typed `long` in LinkedIn's Ads
Reporting schema, so unlike Google's and Microsoft's doubles there is no fraction to
preserve and the exact integer guard is the right one.

**Verification** — a fixture of three elements, `2^53`, `1`, `1`. The exact total,
`2^53 + 2`, IS representable as a float64, so a correct implementation reports it exactly
and the single final widen loses nothing; only the per-iteration round trip corrupts it.
Before the fix:

    --- FAIL: TestGetCampaignMetrics_ConversionsAggregateAboveFloat64IntegerPrecision (0.00s)
        metrics_test.go:1606: Conversions = 9007199254740992, want 9007199254740994: the
        running total round-tripped through float64 mid-loop and lost 2 conversions above 2^53

Both increments were swallowed, not one — after element one the total sits exactly on 2^53,
and each subsequent `+1` rounds back down to it. Reverting the accumulator to the pointer
field while leaving the new final widen in place compiles, and the test fails again with the
same figures, while `TestGetCampaignMetrics_ConversionsAggregateAcrossElements` (4+3=7) keeps
passing — the small-value path was never wrong, which is why no shipped test caught this.

**Docs** — `CampaignActionItem`'s pinned example moved from `budget_constrained` to
`zero_delivery`. The pin was added on 2026-08-19 to stop Goa auto-selecting
`no_conversions` beside `platform: reddit-ads`, a finding Reddit cannot produce; that
constraint still holds and `zero_delivery` satisfies it too. But `budget_constrained` fires
on a campaign spending AHEAD of plan, and the sibling examples pinned beside it read
`issue: No impressions or spend recorded` and `action: Check targeting and creative approval
status` — the zero_delivery symptom and remedy. The composed example described two opposite
findings at once, and since `BriefMetrics.action_items` has no example of its own, the
incoherent composition propagated into every array example. `priority` is now pinned to
`HIGH` for the same reason: zero_delivery is raised at HIGH, so an auto-selected `MED` would
contradict the rule beside it. Regenerating shuffled unrelated placeholder strings across
the spec — Goa's example generator is seeded by the whole design, which is the drift the
original pin was written to guard against.

`docs/api-catalog.md` claimed conversions are "fractional" wherever present, naming Google
Ads, LinkedIn and Microsoft, then justified only the two doubles. LinkedIn's is an integer
count widened onto the shared `float64` wire type and can never be fractional, so the
"fractional" framing and the "do not treat a value below 1 as zero" guidance are now scoped
to Google Ads and Microsoft, with LinkedIn's integer nature stated explicitly. Two markdown
defects in the same row were also carried in from #150: an unbalanced `**` that left a
closing marker with no partner (breaking bold pairing for the rest of the row) and the
run-together "byevery". A sweep of the file for both classes found no others — the remaining
odd `**` counts are bold spans legitimately wrapped across two lines, plus one inside the
code span `/projects/:projectId/briefs/**`.
