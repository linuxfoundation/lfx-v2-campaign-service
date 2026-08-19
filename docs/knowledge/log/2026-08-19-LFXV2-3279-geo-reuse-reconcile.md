# 2026-08-19 — LFXV2-3279 geo reuse reconcile

**Fix** — The bot reviewers found that the previous fix in this branch created a new defect of
the same family it was meant to close, plus three genuine gaps. All are addressed by READING the
campaign's existing location criteria instead of guessing about them.

**The previous fix was wrong in the other direction.** Skipping the geo attach whenever
`alreadyExisted` was true stopped the duplicate-on-retry problem, but assumed a reused campaign
was already targeted. It is not always: a run whose attach was REJECTED leaves a campaign with
NO criteria, and the retry then skipped the attach, finished the cascade and reported SUCCESS
for a campaign that serves everywhere — the exact harm this ticket exists to prevent,
reintroduced through the back door. The step text also asserted that prior targeting was present
in precisely the case where it was not. Confirmed with a two-run probe before changing anything:
run 1 failed the attach, run 2 returned `err=nil`.

**Neither blind guess is safe, so the criteria are now read.**
`POST /CampaignCriterions/QueryByIds` (verified against the v13 reference) enumerates them, and
only the locations genuinely missing are attached. Two wire details differ from the ADD path:
`CriterionType` is `Location`, not `Targets` — "The *Targets* value is not allowed for this
operation" — and `CampaignCriterionIds` is sent as **null** to mean all of them, which is the
only way to enumerate criteria whose ids this run never learned. Its `PartialErrors` is the FLAT
`BatchError` array, not the nested shape the ADD path returns. A read FAILURE is propagated, not
treated as "no criteria": "we could not check" must never collapse into the re-attach path,
which would duplicate every location.

**This also removes the earlier known limitation.** `GeoCriterionIDs` being empty on a reused
campaign was previously unavoidable because the client called no criterion read. It now does, so
the reuse path reports what is actually attached rather than a documented blind spot.

**A truncated error array was classifiable as success.** `boundedNestedErrorItems` retains
`maxDecodedErrorItems` entries while the call sends up to `maxGeoTargets`, so a rejection past
the cap was DISCARDED during decode and the surviving prefix read clean — reporting an
untargeted campaign as targeted. Both the outer flag and each nested `BatchErrors.Truncated` are
now checked, matching the invariant the keyword path already applies.

**Duplicate display names are now ambiguous rather than first-wins.** Microsoft warns that
"Multiple location IDs can have the same display name" AND that row order is not guaranteed, so
first-wins is arbitrary ACROSS refreshes, not merely within one file: the same brief could
silently resolve to a different `LocationId` after a 24h refresh. A name carried by two or more
DISTINCT Active `Country` rows is dropped, so resolving it refuses. A repeated identical row is
not ambiguous — it names one place — and still resolves.

**A partial attach no longer reports "NOT geo-targeted".** When some criteria attached and
others were rejected, the `default` arm claimed the campaign had no targeting at all, which
would send an operator to fix targeting that partly exists. That case now reports the attached
count against the requested count.

**The cache's real scope is now documented rather than overstated.** `MicrosoftDispatcher`
builds a new client per `Dispatch`, so the 24h TTL and single-flight coalesce fetches within one
create and any concurrent callers sharing that client — not across jobs. A cross-job cache needs
a longer-lived owner injected into the dispatcher; that is a separate change, and claiming it
here would have been false.

**Mutation results.** Four further mutations, all killed: reverting the reuse path to a blind
skip (caught by the attach-the-missing test), treating a read failure as "no criteria", ignoring
the truncation flags, and restoring first-wins on an ambiguous name. Twenty-seven mutations have
now been run across this branch with none surviving; the two that survived earlier rounds were
reported and fixed rather than weakened.

**Follow-up: the read itself had two defects, both found by Cursor on the reconcile commit.**
`existingLocationIDs` sent `CriterionType` as the JSON type DISCRIMINATOR (`LocationCriterion`)
where the read wants the bare request ENUM (`Location`). Three vocabularies meet on this path —
the add body's discriminator, the add request's `Targets`, and the read request's `Location` —
and collapsing any two produces a call that is silently wrong rather than rejected: the read
returned no criteria, which the reuse path would then read as "nothing attached" and re-attach
every location, duplicating exactly what the reconcile exists to prevent. They are now three
separate constants whose comments say which vocabulary each belongs to. The read also skipped
the truncation invariant the add path had just gained, so a discarded error past the decode cap
could make an incomplete read look clean and under-report the existing criteria. Both fixes are
mutation-verified (29 mutations across the branch, none surviving).
