# 2026-08-14 — LFXV2-3257 variant review fixes

**Fix** — Four defects in the variant slot work, all found in review of #130 and
all invisible to the tests that shipped with it.

`DeleteDispatchClaim`'s query gained a `variant=$3` placeholder while `Exec`
still bound two arguments. EVERY claim release would have failed at runtime and
stranded a 'pending' row — and nothing reaps those, so the slot would be blocked
forever. A query string is not type checked, and the package had no live
coverage of the release path at all, so nothing could have caught it. Both
review bots did.

Neither upsert path stamped `Campaign.Variant`, so a dispatcher's returned
campaign normalised to 'default' while the claim held 'demand-gen'. The conflict
target then missed the claimed row and INSERTed a second one.

`preflightCampaign` hardcoded `ComposeName("Search Campaign", …)`, so the Demand
Gen port composed both channels identically. Google rejects a duplicate campaign
name within an account, and two channels sharing a name are indistinguishable to
anyone reconciling by name after an ambiguous create. The kind is now a
parameter; the legacy Express path draws the same distinction.

Explicit `channel:"search"` resolved to a slot of its own while an ABSENT
channel resolved to 'default', even though both dispatch the identical Search
campaign. The updated UI names the channel explicitly, so it would have missed
the existing row on any brief created before the column and created a SECOND
paid Search campaign. Both now share 'default', which is what the backfill
means.

**Note** — 000023's guard checked only that the replacement index HAS a
predicate, not which one. An impostor such as `WHERE status = 'created'` is
unique, valid and correctly keyed, so it would have passed while enforcing a
different invariant — and 000024 then drops the real arbiter. It now compares
the predicate string exactly, as 000014 does.

A review also flagged `targetSpend` as unsupported for Demand Gen on Google Ads
v23. Kept as-is: it mirrors the legacy Express implementation's `target_spend:
{}`, which serves this channel in production today and is therefore evidence of
what the API accepts rather than a reading of the docs. The disagreement is
recorded at the call site; a live create is what settles it.
