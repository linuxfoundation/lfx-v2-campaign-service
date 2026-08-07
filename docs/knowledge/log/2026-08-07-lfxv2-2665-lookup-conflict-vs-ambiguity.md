# 2026-08-07 — LFXV2-2665: a confirmed conflict is not an unconfirmed absence

**Update** — `errLookupConflict` now marks the three definite-conflict returns in
`findCampaignByName` / `findAdSetByName` (a match that is not `PAUSED`, or whose objective
differs). `CreateCampaign` checks it before anything else and returns a clean failure with no
partial; every OTHER lookup failure, on both the campaign and the ad-set path, is UNCONFIRMED.

**Fix** — The preceding pass made every failed campaign lookup UNCONFIRMED by joining
`errLookupAmbiguous` unconditionally. That swept up the two errors `findCampaignByName`
deliberately leaves un-marked. Those branches are not failures to establish absence: the lookup
enumerated the name and READ the match, so presence is confirmed with a stated reason. Reporting
them UNCONFIRMED retains a partial for a create that provably never happened and sends an
operator to Ads Manager to verify a fact the error already states — on every retry, forever,
because the conflict is stable until someone renames or deletes the live campaign. The conflict
check runs BEFORE the cancelled-context check, too: a fact the lookup already established does not
become uncertain because the caller went away afterwards.

**Fix** — The ad-set lookup was still on the old side of the rule, gated on
`!createOutcomeAmbiguous(adSetLookupErr)` with the rationale that a pre-send or 4xx failure means
"the ad set was definitely not looked up". True, and beside the point. That lookup runs ONLY when
the campaign was adopted from a prior attempt, which is precisely the case where a prior attempt
may already have parented an ad set under it. Reported as a clean failure, the next dispatch POSTs
the same deterministic ad-set name under the same campaign and duplicates real spend. It now
splits the same two ways as the campaign path.

Four tests: a status conflict and an objective conflict assert `errLookupConflict`, a false
`createOutcomeAmbiguous`, a nil result, and that no mutating call is reached; an ad-set 4xx and an
ad-set cancellation assert the partial is retained and marked UNCONFIRMED. Reverting either half
fails its own tests with the intended diagnostic.
