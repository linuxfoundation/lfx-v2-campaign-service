# 2026-08-04 — GA-4: keyword + audience-segment targeting

**Update** — Added keyword + audience-segment targeting to the Google Ads client (GA-4,
`internal/platform/googleads/targeting.go`, new file). GA-3 created an ad group with zero
criteria, which matches no query — this closes that gap: after `createAdGroupAndAd` creates the
ad, `createAdGroupTargeting` attaches caller-supplied positive Search keywords
(`Keyword{Text,MatchType}`, EXACT/PHRASE/BROAD, ≤80 runes) and/or existing audience segments
(EXISTING `.../userLists/{id}` or `.../customAudiences/{id}` resource names — this client does
not create audiences) as a single `adGroupCriteria:mutate` call, one operation per criterion, all
`ENABLED` (unlike the PAUSED ad group/ad shell — ancestor gating already blocks serving while
paused, so pre-enabling criteria means the campaign is serve-ready the instant a human activates
it). Implemented the audience "observation vs targeting" contract for real rather than just
flagging it: `CreateCampaign` (`campaign.go`) now sets `targetingSetting.targetRestrictions`
(`AUDIENCE`, `bidOnly: true`) on the campaign create WHENEVER `AudienceSegments` is non-empty, so
an audience segment added for bid/reporting doesn't silently narrow delivery to just that segment
(Google's undeclared default for a Search campaign's AUDIENCE dimension). Factored
`adGroupAdID`'s composite-resourceName split (`{parentId}~{id}`) into a generic
`compositeResourceID` helper (`adgroup_ad.go`) since AdGroupCriterion shares the identical shape.
Duplicate-criterion classification is left unverified for this resource (unlike the
budget/campaign/ad-group `DUPLICATE_NAME` family) — any 4xx here is a straightforward failure, not
reconciled by a duplicate predicate. Wired through `internal/dispatch/googleads.go`
(`googleAdsConfig.keywords`/`.audienceSegments`, mapped 1:1 into `CampaignInput`). Full unit +
integration coverage in the new `internal/platform/googleads/targeting_test.go`. Updated
`docs/api-catalog.md`'s `GoogleAdsConfig` section (the `keywords`/`audienceSegments` fields) and
`docs/knowledge/code/internal-platform-googleads.md` (new GA-4 section + updated Scope) to match.

**Update** — Follow-up fix on the GA-4 slice above, from its post-commit review cycle.
`internal/platform/googleads/targeting_test.go`'s `TestCreateAdGroupAndAd_TargetingHappyPath` and
the new `newTargetingClientCapturingCampaign` helper (used by both campaign-level
`targetingSetting` tests) captured a decoded request body inside the httptest handler goroutine
and read it back from the test goroutine with no happens-before edge — the same data-race shape
already fixed once in `adgroup_ad_test.go`'s `UpdateAdGroupAndAdStatus` tests. Guarded both with a
`sync.Mutex` (a small `capturedBody` helper for the campaign-capturing case), matching the existing
pattern.

**Update** — Fixed a Copilot-flagged PR #69 review finding: the AUDIENCE/`bidOnly: true`
observation-only `targetingSetting` was being declared on the campaign create
(`campaign.go`, `CreateCampaign`), but GA-4's audience criteria are created as
`AdGroupCriterion`s (ad-group level), not campaign-level criteria. Verified against Google's
official `UpdateAudienceTargetRestriction` sample and targeting-settings docs before fixing
(mirroring the verification discipline used for a prior Final-URL-bytes finding): Google
requires `targetingSetting` be declared at the SAME level as the criteria it restricts, and
setting it on the ad group is even blocked while the parent campaign has one set. Moved the
`targetingSetting`/`targetRestriction` application from `campaignCreate` (`campaign.go`) to
the new `TargetingSetting` field on `adGroupCreate` (`adgroup_ad.go`), set in
`createAdGroupAndAd` whenever the validated `audienceSegments` list is non-empty. Updated
`TestCreateCampaign_SetsAudienceObservationTargetingSetting`/
`TestCreateCampaign_NoAudienceSegmentsOmitsTargetingSetting` to assert on the adGroups:mutate
body instead of campaigns:mutate.
