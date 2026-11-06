# 2026-11-06 — #548 refuse shared lists that are not negative keyword lists

**Update** — `sharedNegativeKeywordLists` now refuses a shared set of any type but
`NEGATIVE_KEYWORDS`. The request shape is unchanged.

- **Why.** The #536 live check showed that Google accepts a `NEGATIVE_PLACEMENTS` or `BRANDS` set
  in a `campaignSharedSets` link on a Search campaign, and the client did not check the type.
  On 2026-11-06 the owner decided to refuse such sets.
- **What.** `CreateCampaign` calls `checkSharedNegativeKeywordListTypes` right after the preflight,
  before the image fetch and the budget mutate. It sends ONE read-only `googleAds:search` on the
  connection's account:
  `SELECT shared_set.resource_name, shared_set.type, shared_set.status FROM shared_set WHERE
  shared_set.resource_name IN (...)`. A list is refused as a pre-flight refusal if its row is:
  - not `NEGATIVE_KEYWORDS`,
  - not `ENABLED`,
  - absent from the account.

  The refusal names the list and its set id (only the set id is shown in the job result) and,
  for a set of another known type, that type, taken from a fixed vocabulary and never echoed. It quotes the stored account id, as #528 does.
- **Fail closed.** A read that fails fails the create before any write, with the upstream status
  kept for the "answered HTTP n before anything was created" mapping. So do rows that do not
  decode, name a set not asked for, or repeat one. No list means no read.
- **Tests.** In `search_assets_test.go`:
  - `TestCreateCampaign_SharedListTypesAreReadInOneQuery` pins the one query and that it runs first.
  - `..._RefusesSharedListsThatAreNotNegativeKeywordLists` covers each wrong type, an
    unrecognised or missing type, `REMOVED`, `UNKNOWN` and a missing row.
  - `..._SharedListTypeReadFailureFailsClosed` covers a 503, a 400, a bare null, an undecodable
    row, an unasked set, another account's set and a repeated row.
  - `..._NoSharedListsNoTypeRead` pins that no list means no read.

  `TestGoogleAds_SharedListTypeCheckJobMessages` pins the job messages. The test fakes now answer
  the read.
- **Not live-verified.** No Google Ads call was made. The GAQL follows the documented v23
  `shared_set` fields, and the #536 read-back already returned `type` and `status` for these
  sets.
