# 2026-08-19 — LFXV2-3279 reconciling the EXACT geo set, not a subset

**Fix** — The reuse reconcile checked only that the requested LocationIds were a SUBSET of the
attached positive criteria. It now compares the exact set.

**The campaign name does not encode geo, which is what makes the subset check unsafe.**
`composeName` produces `LFX | Search Campaign | project | event | brief.ID` — `GeoTargets` is not
a component. So the same brief re-run with a NARROWER geo list composes the identical name,
`findCampaignByName` reuses the existing campaign, and every requested id is found present. The
subset check then attached nothing and reported success, for a campaign still carrying the WIDER
previous targeting. That is money spent in countries nobody approved, reported as correctly
targeted — the same class of harm as the untargeted campaign this ticket opened for, differing
only in blast radius.

**Both halves of the reconcile are needed.** `missing` answers "what must I add"; the new
`unrequestedTargets` answers "what is attached that should not be". Only both together establish
that the effective targeting equals the requested targeting; either alone leaves one direction of
drift invisible.

**Extra criteria are REFUSED, not removed**, consistent with how a conflicting exclusion is
handled. Deleting location criteria from a live paid campaign is a targeting decision that
belongs to an operator, not to a retry path that happens to be running. The error names the
unexpected locations and explains why the campaign was reused despite the different geo list, so
the remedy (remove them upstream, or create a distinct campaign) is mechanical rather than
requiring the reader to reconstruct the naming rule.

**The exact match must still succeed**, since that is the ordinary idempotent retry — refusing it
would break every re-run. That case is pinned by its own test so a future tightening cannot
collapse the two.

**Mutation-tested.** Disabling the exact-set arm still compiles and fails the extra-target test
while the exact-match test stays green, which is the pair that distinguishes a real reconcile
from a check that merely refuses everything.
