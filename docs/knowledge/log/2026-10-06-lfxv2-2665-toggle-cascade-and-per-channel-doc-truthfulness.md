# 2026-10-06 — LFXV2-2665 Cascade to every child, and tell the truth per channel

**Fix** — `googleAdsToggleChildren` returned on the asset group alone whenever one was
recorded, silently discarding every ad-group target. The doc called the two mutually
exclusive "by construction", and today they are — a Performance Max campaign has no ad groups
and no other channel has an asset group — but that is an invariant of what the create paths
WRITE, while this function reads two values parsed independently out of an operator-visible
`Result` blob. A hand-repaired row, a future channel carrying both, or a change to what
adoption records would have enabled the asset group, skipped every ad group and ad, and still
flipped the campaign to ENABLED: the "reports running and mostly is not" failure
`googleAdsToggleTargets`' own cascade exists to prevent, made undiagnosable by the silence.
Both are now acted on, asset group first so no child trails an ENABLED campaign, and the
contradiction is logged rather than absorbed.
`TestToggleStatus_BothAssetGroupAndAdGroupsCascadeToBoth` pins it and fails against the
previous body.

**Docs** — `googleAdsActivationGate` refused an adopted campaign with a sentence about
provisioning. `campaignFromGoogleAdsAdoption` writes provenance and nothing else — no ad group
id, no ad id, no asset group id — so an adopted row fails whichever gate its variant selects,
permanently, however completely the campaign is provisioned upstream. The refusal is correct:
a cascade can only act on resources the blob recorded. The message was not, because it sent an
operator hunting a provisioning failure in this service's records for a campaign this service
never created. All three arms now name adoption and say un-pausing happens in the Google Ads
UI, and the adopt row in `docs/api-catalog.md` states the same consequence. PAUSE is
unaffected — the gate fires only on ACTIVATE, and pausing the campaign resource alone is
always safe.

**Docs** — Four statements this branch itself made false, found by reviewing the branch rather
than the commit: `ToggleStatus`' own doc comment still gave Search's ad-group-plus-keyword rule
as the universal ACTIVATE requirement; `CampaignInput.ProximityTargets` and
`ValidateCampaignInputKind` still said "Search only" for fields Performance Max accepts at the
campaign level (proximity, languages, ad schedules), when the per-channel split lives at
`geo.go:411` and `campaign_criteria.go:293`; and a test comment claimed keywords are
"accepted-and-ignored" off Search, which the Search-only fence retired. The test's own reason
for existing survives that fence and is now stated on its own terms.

**Docs** — Three `docs/api-catalog.md` entries counted two channels where there are now three:
`excludedGeoTargets` and the `startDate`/`endDate` flight window both apply on Performance Max,
and the custom-audience refusal justified itself with "this client creates SEARCH campaigns
only" while naming Demand Gen and Performance Max as the channels where custom audiences would
be available — the strongest argument against the refusal, sitting inside it. The refusal
stands; the reasoning now states the actual one, which is that audience criteria are attached
only on the Search ad-group cascade.
