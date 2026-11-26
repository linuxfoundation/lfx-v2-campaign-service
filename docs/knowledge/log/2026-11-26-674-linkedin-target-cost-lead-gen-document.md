# 2026-11-26 — #674 target cost refused for lead gen and document campaigns

**Fix** — after the rebase onto v1.0.44 (lead gen #664, document #665), `linkedinObjective`
refuses `bidding.targetAmount` with objective `"lead_generation"` (MAX_LEAD) or format
`"document"` (MAX_REACH by default), synchronously, before any job. A target cost is sent as
`unitCost` beside MAX_CLICK or MAX_CONVERSION only; the other automated targets were never checked
with one. Covered by `TestLinkedIn_TargetAmountRefusedForLeadGenAndDocument`.
