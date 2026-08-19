# 2026-08-18 — LFXV2-3314 conversions: keep the fraction, and Google's absent-is-zero

**Fix** — two defects in the conversions metric added earlier on this branch, both raised in
review and both confirmed against the vendors' published field references before being changed.

This entry SUPERSEDES `2026-08-18-LFXV2-3314-conversions-metric.md` on two points, which are
left standing there because the log is append-only and an entry describes the revision that
wrote it: that entry's `Conversions *int64` is now `*float64` on `model.CampaignMetrics`,
`rules.Input` and the `campaign-metrics` wire type alike, and its "Both are rounded, not
truncated" no longer describes any adapter — none rounds. Read that entry for why the metric
exists and this one for the types it actually ships with.

**Two comments outlived the rounding they described.** Removing the rounding left prose behind
at sites the type change did not touch, so the code and its own commentary disagreed. The
Microsoft column block still said `foldReportRows` "parses it as a float and rounds" while the
same function's body twenty lines down said the fraction is kept — and it is; the file's only
`math.Round` scales spend. A `rules` test helper's godoc still opened by naming `int64p`, a
symbol the refactor renamed to `convp` and which no longer exists anywhere. Both are the same
class as the superseded fragment above: a sentence describing a revision that a later commit on
the SAME branch replaced. When a type change removes an operation, the comments asserting that
operation are part of the change, not adjacent to it.

**The contract could not represent a fractional conversion.** `conversions` was typed `Int64`
through the design, the domain model and the rules input, and both the Google Ads and Microsoft
adapters ROUNDED their platform value to reach it. Google types `metrics.conversions` as DOUBLE
and Microsoft types `ConversionsQualified` as double, and both credit fractional conversions
under data-driven, position-based and offline attribution — so a campaign genuinely credited 0.4
of a conversion was reported as having produced ZERO. The `no_conversions` rule reads exactly
that number, so the rounding did not merely lose precision: it MANUFACTURED the finding the rule
exists to report honestly, raising a HIGH-priority "no conversions" item against a converting
campaign. This is the same fabricated-measurement failure as substituting 0 for an unmeasured
count, in the opposite direction. The field is now `Float64` end to end and no adapter rounds.

Microsoft's case was worse than a single rounding, because it rounded per ROW and then summed:
twenty report rows of 0.4 became twenty zeroes and a reported total of 0 for a campaign that
converted eight times. It now accumulates the float and checks the total for overflow.
LinkedIn's `externalWebsiteConversions` is typed `long` and carries no fraction, so its exact
int64 overflow guard is kept and the value is widened only at the boundary.

**An absent `metrics.conversions` from Google Ads is a measured zero, not "unmeasured".** The
adapter left the pointer nil whenever the member was missing from the row. But that field is
ALWAYS in this query's SELECT list, and Google Ads REST is proto3 JSON, which OMITS fields
holding the default value — so an absent member on a selected field is the encoding of 0.0. The
same file already documents this for the other metrics: `parseMetricInt` treats an omitted
impressions/clicks value as a measured 0 for precisely this reason. Leaving conversions nil there
meant `no_conversions` could NEVER fire for a Google campaign that genuinely converted nobody,
which is the rule's entire purpose. Google now always reports a non-nil count.

**nil still means what it meant, and that distinction is unchanged.** nil is reserved for
platforms whose API cannot report a campaign-level conversion count at all — Meta and X expose
conversions only as per-action-type structures with no scalar to read, Reddit's reporting
contract is undocumented, and an email send has no conversion concept. On those four the field
stays ABSENT on the wire and the rule never fires. What changed is that "Google returned no
conversions" is no longer misclassified as "Google cannot measure conversions"; a zero default
for the four platforms that cannot measure would still be the fabricated measurement this epic
has been removing, and remains refused.

**Tests.** Every fix is pinned by a test checked against a compiling revert: restoring the
rounding fails `FractionalConversionsArePreserved` (0.4 -> 0), restoring absent-is-nil fails
`AbsentConversionsIsMeasuredZero`, restoring Microsoft's per-row rounding fails
`FractionalConversionsSumWithoutPerRowRounding` (five rows of 0.4 -> 0 instead of 2), and a
zero-default on the campaign-scoped response mapping fails the new
`GetCampaignMetrics_ConversionsAbsentZeroAndFractionAreDistinct`.

One mutant SURVIVED the first pass and is worth recording: loosening the rule's comparison from
`*in.Conversions == 0` to `< 1` broke nothing, because every fixture used a whole number. That is
the exact defect the fractional change exists to prevent, invisible to the suite that was
supposed to cover it. `NoConversionsSilentOnAFractionalConversion` now walks 0.1/0.4/0.5/0.9 and
kills it. A rule whose tests only use integers cannot detect an integer assumption.
