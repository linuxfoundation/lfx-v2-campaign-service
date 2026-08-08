# 2026-08-08 — Adopting an existing Google Ads campaign instead of creating a second one

**Update** — `GoogleAdsDispatcher.Dispatch` now calls `FindCampaignByName` before creating.
When a campaign with the deterministic composed name already exists on the account, it is
adopted; only a verified absence licenses a create.

## Why a retry needed this

`ComposeName` is deterministic in the brief, so a retried dispatch asks for the same campaign
name a previous attempt may already have created. Before this, the retry created a second
PAID campaign — the duplicate-name 4xx from Google was the only thing standing between a
retry and double spend, and it only fires when the name collides exactly.

The lookup's fail-closed contract is what makes this safe to act on: `FindCampaignByName`
errors on anything it cannot verify (a transport failure, a row whose name does not match the
`WHERE` clause, a campaign in another customer), so an absence returned here is a *verified*
absence rather than a failure that looks like one. Acting destructively on `("", nil)` is only
defensible because that value cannot be produced by a lookup that merely failed.

## Two things adoption must not quietly change

**Input validation cannot depend on remote state.** Adoption returns before
`CreateCampaign` runs, and `CreateCampaign` is where the input is validated. So a NaN budget,
an over-long name or a malformed registration URL would have failed cleanly on a first
dispatch and *silently succeeded* on the retry that found a campaign to adopt — the same
request, accepted or rejected according to what happened to be sitting in the ad account.
`Client.ValidateCampaignInput` now runs the same non-mutating preflight before the lookup. It
delegates to the same `preflightCampaign` helper `CreateCampaign` uses; a second copy of the
checks would pass review once and drift on the next change to either.

**The persisted row must still record the request.** `campaignFromGoogleAdsAdoption`
initially omitted `applyCampaignConfig`, so adopted rows persisted with NULL
`budget_amount`, `budget_type` and `config_snapshot`. Those columns hold the CALLER-SUPPLIED
config for the dispatch — what was asked for — not a readback of platform state; the create
path stamps them from the same `cfg` before anything is read back. Leaving them NULL did not
express "unknown", it lost the request, and made adopted rows disagree in shape with every
sibling adapter's for no recoverable reason.

What adoption does not do is push that config upstream: the campaign keeps whatever budget
and settings it was created with, and this path creates no budget and no ad group. The row
records the request; the platform keeps its state. Reconciling the two is a metrics read's
job, not the mapper's.

## Note on the test

`TestGoogleAds_Adoption_FindsAndAdoptsExistingCampaign` uses `atomic.Bool` for its
handler-side call flags. The completed HTTP response does establish a happens-before, so
`-race` is clean with plain bools too — but that argument has to be reconstructed by every
reader, and the next test to copy the shape may not preserve the property it depends on.
