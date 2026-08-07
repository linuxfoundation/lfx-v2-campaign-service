# 2026-08-05 — LFXV2-2023 GA-3b: review fixes re-triaged onto the split branch

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
