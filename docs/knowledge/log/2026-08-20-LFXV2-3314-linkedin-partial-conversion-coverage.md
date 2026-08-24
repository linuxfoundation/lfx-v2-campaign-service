# 2026-08-20 — LFXV2-3314 LinkedIn published a partial conversion total as a complete one

**Fix** — `linkedin.GetCampaignMetrics` aggregated `externalWebsiteConversions` across only
the response elements that CARRIED the field, publishing the sum whenever at least one did:

    if elem.ExternalWebsiteConversions != nil {
        ...
        conversions += conv
        conversionsMeasured = true
    }
    ...
    if conversionsMeasured {
        total := float64(conversions)
        metrics.Conversions = &total
    }

`conversionsMeasured` is a presence-OR: any single element carrying the metric is enough to
publish. So a response where one element OMITS the field and a later one reports an explicit
`0` yields a non-nil zero — a measured-looking count built from data LinkedIn only partially
reported. The clicks of both elements still aggregate normally, so once the total clears
`minClicksForConversions` (50, `internal/service/rules/actions.go`) the `no_conversions` rule
fires HIGH against a campaign whose real conversion count is simply unknown. The rule
manufactures its own finding rather than measuring one.

The withdrawal also has to survive element ORDER. A value seen before the omission must be
retracted, not just a later one left unpublished, which is why the accumulator stays outside
`metrics.Conversions` — writing to the response field directly leaves the earlier elements'
partial sum already published when the omission is reached.

The fix adds a second flag, `conversionsIncomplete`, set on the `else` arm, and gates
publication on `conversionsMeasured && !conversionsIncomplete`. Presence and completeness are
different questions and one boolean cannot answer both.

**The class, not the line.** This PR gave Google Ads and Microsoft their conversion-absence
discipline and left the third reader behind. `microsoft/metrics.go` already had exactly this
shape — `convIncomplete`, set when `parseConversionCell` reports a BLANK cell, withdrawing the
whole `ConversionsQualified` total at the end — with a comment rejecting the partial-sum
alternative for the same reason and against the same rule. LinkedIn was simply not brought
along. When a fix establishes a discipline about absence, the question is which sibling
readers share the shape, not whether the named file is now correct.

**Why no existing test caught it.** Every conversion fixture in `metrics_test.go` was
uniformly covered: all elements carried the metric, or none did. `AbsentConversionsIsNilNotZero`
tested a single element, `ConversionsAggregateAcrossElements` two elements that both reported.
The MIXED case — the only one where presence and completeness disagree — had no fixture, so
the package was green with the bug live. A uniform fixture cannot exercise a flag whose whole
purpose is to distinguish two arms.

**Verified by mutation.** Two compiling reverts, both killed by the new tests: neutering the
guard to `conversionsMeasured && (true || !conversionsIncomplete)`, and making the flag
assignment a self-assignment so it is never set. Note the plain revert — dropping
`&& !conversionsIncomplete` outright — does NOT compile, because Go rejects the now-unused
variable; a mutation that fails to build proves nothing, so it had to be written as a
consumed-but-neutered form.

**Also corrected on this branch: two comments that argued against the fix they guard.**

`internal/dispatch/googleads.go` said a nil `Conversions` means Google did not report the
metric. This PR made `googleads.GetCampaignMetrics` never leave it nil — it materialises a
non-nil zero on the no-row path AND defaults `conv := 0.0` when the row omits the member — so
the comment described pre-fix semantics and invited exactly the nil/zero confusion the PR
removed. It now states why the POINTER is still carried (the domain nil marks platforms that
report no conversion count at all) while recording that this adapter never produces one.

`internal/platform/googleads/metrics.go` justified the non-nil zero by claiming a nil would
silence `no_conversions` "for precisely the campaign it exists to catch". That consequence
cannot occur: the rule requires `in.Clicks >= minClicksForConversions`, and a no-row window
has ZERO clicks, so it can never fire there whichever way the pointer goes. The fix is right
and the stated proof was false — a comment that would have survived any test. The real reason
is the adapter's own invariant and what consumers read from the pointer, which is what it now
says.

A right conclusion resting on a false proof is worse than no comment: the next reader either
trusts the reasoning and generalises it wrongly, or checks it, finds it false, and deletes
correct code.
