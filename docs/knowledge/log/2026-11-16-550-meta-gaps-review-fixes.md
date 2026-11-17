# 2026-11-16 — #550 Meta gaps: numeric interest ids, city-in-region overlap, lead form gate

**Fix** — Pre-PR review fixes on the #550 Meta gaps branch.

`checkInterests` decoded each `adinterestvalid` row's `id` as a string, but Meta answers it as a
JSON number, so every create with `targeting.interests` would have failed with "returned no rows".
The row id now decodes through `graphID` (string or number), as the detailed-targeting validation
already did.

`checkGeoLocations` refused a region or city whose country is already a whole-country target but
let a city inside a region of the same request through, an overlap Meta refuses at the ad set
create, after the campaign exists. It now decodes each city's `region_id` and refuses that city
before anything is created.

`meta.Client.ActivateCampaignHoldingAdSet` had no production caller once the dispatcher moved to
`ActivateCampaignHolding`; it is removed and its test and the docs name
`ActivateCampaignHolding(…, holdAdSet=true, nil)` instead. Behaviour is unchanged.

Tests: `TestCreateCampaign_InterestValidationAcceptsNumericIDs` and
`TestCreateCampaign_CityInsideATargetedRegionCreatesNothing` and the two lead-form cases of
`TestBriefService_CreateCampaigns_SharedAccountInputsNeedMarketingOps` each fail without the fix.

`metaConfig.leadFormId` now needs `global_marketing_ops` (`service.MetaMarketingOpsInputs`, 403 or
503 before any job exists). For a project without its own Meta connection the create uses the
shared LF connection, whose Page holds every foundation's Instant Forms, and `checkLeadForm` only
proves the form is that Page's: an executive director could otherwise name another foundation's
form and collect leads into it, although the form picker read was already gated for exactly that
reason. The authorization lists in `docs/architecture.md` and `docs/api-catalog.md` now name the
Meta Instant Form list among the `global_marketing_ops` reads.
