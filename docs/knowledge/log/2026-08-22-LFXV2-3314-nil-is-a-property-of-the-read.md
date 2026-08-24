# 2026-08-22 — a nil conversions total is a property of the READ, not the platform

**Docs** — the one-line contract on `model.CampaignMetrics.Conversions` said nil means "the
channel does not report a campaign-level conversion count at all". The detailed prose directly
below it already described two conversion-CAPABLE channels returning nil, so the summary
contradicted the arms it introduced.

## What the code actually does

Both cases are in this file's own adapters, and both were verified by reading them rather than
from the summary:

- LinkedIn (`internal/platform/linkedin/metrics.go`) tracks `conversionsMeasured` and
  `conversionsIncomplete` separately. One returned element that omits
  `externalWebsiteConversions` sets `conversionsIncomplete` and withdraws the WHOLE total to
  nil, including the part earlier elements already contributed.
- Microsoft (`internal/platform/microsoft/metrics.go`) returns nil for a blank
  `ConversionsQualified` cell, which is blank whenever the account has no Universal Event
  Tracking.

Neither is "this platform cannot measure conversions". Both are "this read did not produce a
complete measurement". The contract is now stated over the read, and enumerates the two groups
that reach nil so a consumer cannot infer platform incapability from a nil.

## The second copy of the same claim

`internal/service/rules/actions.go` justified its `in.Conversions != nil` gate as gating "on the
platform being able to measure conversions AT ALL" — the same too-narrow framing, in the one
place where acting on it costs something. The GATE is correct and unchanged; only its stated
reason was wrong, and a reader trusting that reason might have concluded the gate was
unnecessary for LinkedIn and Microsoft. It now names the incomplete-response case too.

Fixing only the struct comment would have left the misleading sentence in the consumer that
matters most, so both were swept together.

## Why the gate is load-bearing

Checked with a COMPILING revert rather than by reading: replacing the nil check with a
`convForRule := 0.0` default that treats nil as a measured zero builds cleanly and is killed by
`TestEvaluate_NoConversionsDistinguishesAbsentFromMeasuredZero`, which reports "an unmeasured
campaign was reported as a failing one". A revert that merely deleted `in.Conversions != nil`
would have nil-panicked at `*in.Conversions` and "failed" without testing the property at all —
the failure message is what confirms the mutation died for the right reason.

## The rule

When a doc comment summarises cases that follow, the summary is a claim the cases can falsify.
Here the arms were right and the sentence above them was wrong, which is the harder direction to
notice: every individual behaviour was correct and only the contract stated over them was not.
