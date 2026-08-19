# 2026-08-19 — LFXV2-3279 refusing a target that is already excluded

**Fix** — The polarity fix earlier today was incomplete, and the bot review caught the gap one
step past where the first fix stopped.

**Classifying an exclusion was not enough; it had to be REPORTED.** `existingLocationIDs` began
recognising a `NegativeCampaignCriterion` and omitting it from the positive-target set, which is
correct as far as it goes. But the reuse path then saw the location as MISSING, attached a
positive criterion, and reported success — while Microsoft applies exclusions AFTER inclusions,
so the country stayed excluded. That is the same silent wrong-geo outcome the polarity check was
added to close, arriving one layer later: the campaign looks targeted, the run says so, and
delivery excludes the very country the brief asked for.

**Not targeted and actively excluded are different answers.** The read now returns them
separately — the positive-target set and the excluded LocationIds — because collapsing them is
exactly what made an exclusion read as a satisfied target in the first version. A caller that
cannot tell "absent" from "excluded" has no way to act correctly on either.

**The create REFUSES on a collision rather than resolving it.** Removing the exclusion would make
the attach effective, but that is a deliberate targeting decision somebody made on a live
campaign, and a broker silently deleting it would be overriding an operator — a worse failure
than refusing. The error names the colliding locations and says what to do (remove the exclusions
upstream, or target different countries). The refusal is SCOPED to a genuine collision: an
exclusion on a country nobody requested is a coherent targeting shape and does not block the
create.

**Ad-group-level geo is a documented bound, not a claim.** Microsoft lets an ad group carry its
own location criteria, which OVERRIDE the campaign's for that group. This client never creates
them, so a tree it builds end to end decides geo in exactly one place; the residual gap is a
reused ad group that a human added ad-group geo to outside this service, since
`findOrCreateAdGroup` matches by name without reading criteria. Closing that needs an
`/AdGroupCriterions` read in the reuse path — a separate change, now stated in the code as a
scope bound rather than left as an unexamined assumption.

**Mutation-tested.** Dropping the conflict refusal still compiles and fails the collision test;
dropping the exclusion reporting fails both that test and the read-level one asserting exclusions
come back separately. The unrelated-exclusion test pins the scope so a future tightening cannot
turn the refusal into a blanket block on any campaign that excludes anything.
