# 2026-10-06 — LFXV2-2665 Cascade correctness findings from the local pre-PR review

**Fix** — Five findings about what the create cascades do between the preflight and the
first mutate, and about one audit step that was not true.

**Performance Max was missing the abort gate both its siblings carry.** Video and Display
each check `ctx.Err()` once the campaign exists and before the next phase; Performance Max
went straight from the criteria step into the asset group. That is the channel where the
omission costs most, because the asset-group step is THREE mutates — assets, the group,
then the links — so a cancellation landing inside it strands a group with some of its
assets linked and a link count the activation gate then has to interpret. The gate reports
the cancellation alongside the result, as the partial-result contract requires past the
campaign create.

The gate has no test, and that is stated here rather than left to be discovered: the window
it covers is between the campaign response being read and the next request being sent, so a
cancellation injected from an `httptest` handler lands inside the response read instead and
exercises the campaign-create error path. Neither Video's nor Display's gate is covered
either, for the same reason. A test that cannot tell the gate's presence from its absence
would have been worse than none.

**The Performance Max image fetch ran whether or not an asset group was asked for.** Demand
Gen and Display both short-circuit on `present`; this one walked the slot list
unconditionally, spending the caller's deadline before the budget mutate on a group that
would never be created. The deeper reason to guard it is that the fetch and the Step 5 arm
now read off the SAME condition — two places deciding independently whether a creative
exists is the shape that lets them disagree.

**An audit step said an ad was created and named no ad.** `createDemandGenAd` returned
`(nil, "", nil)` when it was handed no images, and the caller — which only calls it when the
preflight said a creative is PRESENT — then appended `"Demand Gen ad created: %s"` with an
empty id. A contradictory audit trail is worse than a refusal, because the refusal is the
only one of the two anybody acts on. It now returns an error, which is contract-correct:
nothing has been sent at that point.

**A count bound checked after the walk is not a bound on the work.** `validateCreativeText`
trimmed, display-width-measured and hashed every element into a map sized from the caller's
own length, and only then reported that the list was too long. The bound is now checked
before the loop as well. The post-loop check stays, because it is the one that reports the
SURVIVING count. The test hands it a list that is both oversized and full of elements the
walk would reject first — a count error proves the bound ran first, an "is empty" error
proves it did not.

**A deadline SHARE is not a bound when the caller has no deadline.** `fetchSlotImages`
divided the caller's remaining time in half, which does nothing at all for a call site
passing `context.Background()` — a background reconcile, a test harness. The only bound left
there was the walk's own: 25 images times the per-image timeout, over eight minutes of wall
time chosen by whoever supplied the URLs, and a host that accepts the connection and then
trickles bytes just under the per-image timeout spends all of it. There is now an absolute
ceiling, and the budget is the SMALLER of the two, so a caller with a deadline only ever
narrows it.

That arithmetic is a named function rather than four lines inline, specifically so it can be
tested. The alternative — proving the ceiling by letting a test wait out a slow host — is a
ninety-second test nobody keeps, and the three directions that matter (no deadline, a tight
one, a generous one) are each a line to assert once the computation has a name.

**The through-line.** Three of the five are a bound or a guard that was real in the case its
author had in mind and absent in the neighbouring one: a share with no deadline to divide, a
count check after the work it was meant to prevent, a `present` guard on one call and not
its twin. A conditional safeguard is only a safeguard in the conditions it covers, and the
useful question is always which call site reaches it with the condition unmet.
