# 2026-11-14 — #587 campaign naming v2 lists and validator moved to a shared package

**Update** — The owner decided on 2026-11-14 that the business-unit naming applies to every
paid-social platform, not only LinkedIn. Ahead of the Meta, Reddit and X commits, the
platform-independent half of naming v2 moved out of `internal/platform/linkedin` into the new
`internal/domain/campaignname`: the closed region, audience-intent, format and business-unit lists,
`Naming` (the `<platform>Config.naming` request object), `Validate` / `RequireForCreate`, the
request-side `CheckShape` / `ResolveRequest` the dispatchers run in `PreflightCreate` and again in
`Dispatch`, the unanimous-region default (`UnanimousRegion`, `CountriesRegion` over the
GEO_TO_REGION table) and the composer (`IntentSegment`, `Compose`). It also adds `Carousel` to the
format constants for the creative branches that will produce one.

LinkedIn behaviour is byte-identical: `linkedin.NameRegions`, `NameAudienceIntents`,
`NameBusinessUnits`, `CampaignNaming`, `ValidateNaming` and the `NameFormat*` constants are now this
package's view of the shared values, `ErrNamingInvalid` keeps its `linkedin-ads: invalid naming`
message (it wraps `campaignname.ErrInvalid`), and the dispatcher's `checkNamingShape` /
`linkedinNaming` delegate with the same sentences. LinkedIn's tests pass unchanged.

The business-unit rule, recorded in the package doc: an event campaign's business unit is the
foundation that owns the event (KubeCon → `Cloud Native Computing Foundation (CNCF)`), and
`LF Events` only for an LF-run event with no foundation; the caller supplies it.

Tests: `TestValidate_CanonicalisesAndWrapsThePlatformSentinel`, `TestRequireForCreate`,
`TestCheckShape`, `TestResolveRequest`, `TestUnanimousRegionAndCountriesRegion`,
`TestCountryRegions_AreInTheRegionList`, `TestIntentSegmentAndCompose`. Mutation check: making
`UnanimousRegion` ignore a second bucket fails `TestUnanimousRegionAndCountriesRegion` and
linkedin's `TestBuildCampaignNameV2`; dropping `RequireForCreate`'s business-unit check fails
`TestRequireForCreate` and `TestCreateCampaign_NamingV2RefusedBeforeAnyRequest`.
