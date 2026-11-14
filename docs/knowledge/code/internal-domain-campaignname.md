---
type: "Code Concept"
title: "internal/domain/campaignname"
description: "The platform-independent half of campaign naming v2 (#587): the closed region, audience-intent, format and business-unit lists, the validator and request-side checks every paid-social dispatcher applies to its <platform>Config.naming, the unanimous-region default, and the composer of LF's 8-segment name."
resource: "internal/domain/campaignname"
---

# internal/domain/campaignname

Campaign naming v2 (#587) names a paid-social campaign by LF's own convention,

```
Events | <event> | <Region> | <Objective> | <AudienceIntent>[ <tactic>][ #<n>] | <Format> | <BusinessUnit> | <Funnel>
```

This package holds everything about that name that does not depend on the platform, so LinkedIn,
Meta, Reddit and X compose and validate it identically:

- **Closed lists.** `Regions` (`NA`, `EMEA`, `APAC`, `LATAM`, `India`, `Global`),
  `AudienceIntents` (`Intent`, `Prospecting`, `Retargeting`), the format constants (`Static`,
  `Motion`, `Social`, `Text`, `Carousel`) and `BusinessUnits` — a copy of lf-dbt's canonical project
  names (`normalize_project_name` outputs plus the umbrella names `map_paid_ads_project_id` maps),
  pinned by linkedin's `TestNameBusinessUnits_AreFixedPointsOfTheDbtNormalizer`.
- **Business-unit rule (owner, 2026-11-14).** An event campaign's business unit is the FOUNDATION
  that owns the event (KubeCon → `Cloud Native Computing Foundation (CNCF)`); `LF Events` only for
  an event the Linux Foundation runs with no foundation. The service has no project→BU mapping, so
  the caller sends it, and it is required while a platform's naming v2 flag is on.
- **`Naming`** — the `{region, audienceIntent, businessUnit}` request object every
  `<platform>Config.naming` decodes into.
- **`Validate(n, invalid)`** canonicalises each set field case-insensitively and refuses a value
  off its list, wrapping the platform's own sentinel (which wraps `ErrInvalid`), so each
  platform's message keeps its prefix (`linkedin-ads: invalid naming: …`).
- **Request side.** `CheckShape(config, configKey)` refuses a wrong JSON shape (null, non-object,
  non-string, unknown key) and `ResolveRequest(configKey, n, invalid, enabled, flagEnv)` validates
  whatever the flag says and, with the flag on, requires a business unit; it returns nil with the
  flag off (the legacy name). Both are `ErrCreateConfigInvalid` pre-flight refusals, so
  `PreflightCreate` answers 400 and `Dispatch` re-checks them as not-created failures.
- **Client side.** `RequireForCreate` is the client's own fail-closed check before any request.
- **Region default.** `UnanimousRegion` (the one bucket every geo target falls in, else `Global`)
  and `CountriesRegion`, which buckets ISO 3166-1 alpha-2 codes through `CountryRegions` (the
  GEO_TO_REGION table Meta's and Reddit's legacy names use).
- **Composer.** `IntentSegment` appends the pipe-stripped tactic and slot tokens to segment 5 as
  the legacy names do; `Compose` joins the eight segments.

What stays in each platform client: the objective label, the format (derived from the creative it
creates), the region default from its own geo fields, the default intent, and retry safety.
Pinned by `TestValidate_CanonicalisesAndWrapsThePlatformSentinel`, `TestRequireForCreate`,
`TestCheckShape`, `TestResolveRequest`, `TestUnanimousRegionAndCountriesRegion`,
`TestCountryRegions_AreInTheRegionList` and `TestIntentSegmentAndCompose`.
