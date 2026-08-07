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

**Update** — GA-3 split into GA-3a/GA-3b (two sub-1000-line PRs); this entry covers three
fixes from the original PR's review that were re-triaged onto the recreated GA-3b branch.
(1) Moved the ad-group/ad input validation (`RegistrationURL`, ad copy, ad-group name) to run
BEFORE `CreateCampaign`'s first (budget) mutate, via a new `precomputeAdGroupAdInputs`
helper — previously it ran only inside `createAdGroupAndAd`, which executes LAST, so a bad
`RegistrationURL` or over-length ad-group name would orphan an already-committed
budget+campaign for what is purely a local input error. (2) Tightened `adGroupAdID`
(`internal/platform/googleads/adgroup_ad.go`) to require EXACTLY two numeric
tilde-separated components — it previously accepted extra tildes (e.g. `111~222~333`) and
kept the non-numeric remainder in the ad id — and added a check in `createAdGroupAndAd`
that the returned ad-group-id half matches the ad group the ad was actually created under,
reporting UNCONFIRMED on a mismatch instead of persisting an id from a response that
doesn't describe the ad this call created. (3) Added request-body assertions to
`TestCreateAdGroupAndAd_HappyPath` (ad-group and ad `:mutate` payload contents), which
previously only checked response ids.

**Update** — Follow-up fix on the GA-3b slice above, from the post-commit review cycle. A
`internal/platform/googleads/adgroup_ad_test.go` subtest for `UpdateAdGroupAndAdStatus`
wrote a captured request-path slice inside the httptest handler goroutine and read it back
from the test goroutine with no happens-before edge — a data race the repo's race detector
(`make test`) would eventually flag. Guarded with a `sync.Mutex`, matching the pattern already
used elsewhere in this package (`campaign_test.go`). `docs/api-catalog.md`'s
`GoogleAdsConfig` section was also refreshed — it still described the pre-GA-3b budget-only
contract and was missing the new `headlines`/`descriptions` fields.
