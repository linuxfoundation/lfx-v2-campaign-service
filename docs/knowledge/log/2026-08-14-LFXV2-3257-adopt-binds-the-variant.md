# 2026-08-14 — LFXV2-3257 the adopt INSERT binds the variant

**Fix** — `adoptCampaignQuery` wrote the literal `'default'` into `variant` and never
bound `Campaign.Variant`, so the previous entry's adoption fix was INERT at the
database. The adapter read `advertising_channel_type`, the mapping was correct,
the service set the field — and the repository discarded it. An adopted Demand
Gen campaign still landed in the Search slot, still left `demand-gen` free, and
the next Demand Gen dispatch still created a second paid campaign. Found by a
review bot on the very commit that claimed to fix it.

The conflict target reads the same column, so the hardcoded literal also made
the `DO NOTHING` arm arbitrate the wrong slot: adopting a Demand Gen campaign
onto a brief already holding a Search one was refused as a duplicate. That is
what made the defect read as caution rather than as mis-slotting.

**Note** — Why the existing coverage could not catch this, which matters more
than the one-line fix. The repository's own tests assert on the query STRING,
and the service tests use a FAKE repository that stores whatever struct it is
handed. A fake cannot disagree with the SQL — and disagreement between the
struct and the SQL was the entire bug. Both layers were green throughout.

`TestLiveAdoptStoresTheCampaignsRealVariant` now goes through the real
`AdoptCampaign` against live Postgres and reads the column back, rather than
trusting the returned struct: a repository that echoed its input would satisfy
any struct assertion while having written `'default'`. Its sibling pins that
`default` and `demand-gen` are independent slots on one brief while a repeat of
either is still refused. Reverting the binding fails both, the second with the
duplicate-refusal symptom.

The general lesson is the one the KB already carries as "fixed at one layer
only": a fix that spans the adapter, the model and the service is not verified
until something exercises the WRITE. Every layer here reviewed clean
individually.
