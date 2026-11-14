# 2026-11-14 — #550 Meta language targeting

**Creation** — Gap 12 of the Meta gap review (#550). A Meta ad set reached every language in its
countries. `metaConfig.targeting.locales` (Meta locale keys, positive integers, at most 50 distinct)
is sent as the ad set's `targeting.locales`, so a non-English event can reach its language's
speakers. A language is a hard constraint even with Advantage+ audience on, so it is not an
inclusion and sends no `advantage_audience` flag. Omitted, the payload is unchanged.

It has its own default-off flag, `META_LOCALE_TARGETING_ENABLED`, until a live probe:
`checkMetaTargeting` now gates each later targeting field by its own flag, and a block that sets only
`locales` no longer needs `META_AUDIENCE_TARGETING_ENABLED`, which still gates the six audience
fields (`AudienceTargeting.HasAudienceFields`) and an empty block exactly as before. Locales name no
account resource, so they are classified as not gated by `global_marketing_ops`.

Tests: `targeting_options_test.go` (the list on the ad set, no flag, three refusals with no request,
the distinct-count cap, `HasAudienceFields`) and `meta_targeting_options_test.go` (each flag gating
its own fields, a wrong type, Dispatch's wiring). Eleven hand mutations (the payload line, the
positive-key rule, the cap, the de-duplication, the normalize call, `IsZero`, `HasAudienceFields`,
the locale flag and the audience-flag condition, twice) each fail a test.
