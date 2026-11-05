# 2026-11-04 — #477 shared negative keyword lists gated behind a default-off deployment flag

**Update** — `sharedNegativeKeywordLists` names existing shared sets in the Google Ads account,
and the client checks each one against the account only. The LF account is shared by every
foundation, so a project could attach another foundation's shared list to its campaign (#468).
#459, #462 and #461 closed the same gap for their audience inputs; #477 now does the same
with `GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED`. This was the one accepted finding of the #477
pre-PR review.

`Dispatch` refuses any non-empty `sharedNegativeKeywordLists`, whatever its entries hold and on
any channel, as not-created and before the budget, unless the flag is exactly `"true"`. It
refuses through `googleAdsInputDisabled`, so the asynchronous create job reports the gate's
sentence. The chart sets the flag to `"false"`; ops enables it per environment with
`app.environment.GOOGLE_ADS_SHARED_NEGATIVE_LISTS_ENABLED.value: "true"` in the ArgoCD overlay.
The client's same-account check stays as defence in depth. The gate goes away when #468 adds a
foundation-scoped check.

Not gated: `searchAssets`. Its images, logo and business name are the caller's own content,
created as new assets in this campaign, so they name nothing another foundation owns.

Tests: `TestGoogleAds_SharedNegativeListsGate` refuses with the flag unset, `""`, `"false"`,
`"TRUE"` and `" true"`, a list holding only a blank entry, and a list on Demand Gen, all before
any request, and lets exactly `"true"` through to the attach.
`TestGoogleAds_SharedNegativeListsGateLeavesSearchAssetsAlone` runs `searchAssets` with the
flag unset. Removing the gate fails every refusal case; a case-insensitive comparison fails
`"TRUE"`, a trimmed one fails `" true"`, gating only off Demand Gen fails the Demand Gen case,
and gating `searchAssets` too fails the ungated test.
