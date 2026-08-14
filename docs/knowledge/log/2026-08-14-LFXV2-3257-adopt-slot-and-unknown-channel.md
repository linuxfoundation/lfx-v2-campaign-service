# 2026-08-14 — LFXV2-3257 adopt slots and unsupported channels

**Fix** — Two defects with one shape: a value that is not a real slot being
filed under one that is.

Adoption refused a Demand Gen campaign on any brief that already had a Search
campaign. The occupancy pre-check ran BEFORE the platform lookup, so it could not
know which slot the campaign would occupy — and it guessed `VariantDefault`,
returning 409 whenever that slot was filled. Those are different slots and the
insert would have accepted the pair; worse, the variant-aware re-check written
for exactly this case sat unreachable below it.

The pre-check now fires only when EVERY slot the platform can adopt into is
occupied, which needs no guess (`model.AdoptableVariants`). That keeps the
property the earlier check existed for — an occupied brief answers a
deterministic 409 rather than a 503 a platform outage could produce — while
letting the variant-aware check after the lookup decide the rest.

`variantForDispatch` passed an UNSUPPORTED channel straight through
`NormalizeVariant`, which is a pass-through for any non-empty value. So
`channel:"default"` — valid JSON, not an accepted Google channel — resolved to
the real SEARCH slot, the idempotency fast path found that brief's existing
Search campaign, and its id was returned as a SUCCESS. The dispatcher never ran,
so its "unsupported channel" error never reached the caller, who was told a
campaign it had never validly asked for had been created. Unsupported channels
now resolve to `VariantInvalid`, the same answer the undecodable-config case
takes. An ABSENT channel stays supported and means Search — that is what every
pre-channel brief sends.

**Note** — The fake campaign repository ignored the variant parameter entirely,
keying on `(brief, platform)` where the real partial unique index is
`(brief, platform, variant)`. It therefore enforced a constraint STRICTER than
production and reported a legitimate second slot as a conflict — hiding the
service-side bug above rather than catching it. Both its read and its adopt path
now model the real key, and the bare `(brief, platform)` form is still accepted
as meaning the default slot so existing single-variant fixtures keep working.

A fake that does not model the constraint hides exactly the bug the constraint
exists to catch.
