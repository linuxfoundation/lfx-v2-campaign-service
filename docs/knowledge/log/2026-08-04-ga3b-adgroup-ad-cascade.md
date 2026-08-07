# 2026-08-04 — GA-3b: full Campaign→AdGroup→Ad cascade

**Update** — Extended the Google Ads client from a create-only PAUSED campaign shell to a
full Campaign→AdGroup→Ad hierarchy (GA-3b, `internal/platform/googleads/adgroup_ad.go`,
new file). `CreateCampaign` now cascades into `createAdGroupAndAd`: a `SEARCH_STANDARD`
ad group (create-then-catch-duplicate idempotency via `AdGroupError.DUPLICATE_ADGROUP_NAME`,
mirroring the existing budget/campaign convention rather than Microsoft's find-first
pattern) followed by a PAUSED Responsive Search Ad. Ad copy is composed from optional
caller-supplied headlines/descriptions padded with deterministic eventName/project
placeholders up to Google's v23 RSA minimums (3/2, with weighted character counting where
CJK/full-width characters count as 2); the ad's final URL is the brief's registration URL UTM-tagged without
overwriting any pre-existing query params. AdGroupAd resourceNames use a composite
`{adGroupId}~{adId}` trailing segment (unlike every other Google Ads resource), handled
by a dedicated `adGroupAdID` splitter — flagged as the riskiest unverified assumption in
this slice (no live fixture to confirm the shape against). Also adds
`Client.UpdateAdGroupAndAdStatus` (ad group then ad status update, stopping on first
failure) for a future dispatcher-level cascade (GA-3c) to call.
Keyword/audience targeting is explicitly out of scope here (tracked as GA-4) — the ad group
carries no criteria, so a GA-3b-only campaign still won't serve.
