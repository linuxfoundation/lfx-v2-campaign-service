# 2026-11-04 — #474 keep every valid asset id when a mutate result is unusable

**Fix** — A review of #481 (after it merged) found that the Demand Gen video ad's atomic
`googleAds:mutate`, answered with the expected number of results, returned UNCONFIRMED at
the first unusable asset result and dropped every same-account asset id after it. A
foreign-account video result followed by this account's call to action 951 recorded only
the logo 900, so the call-to-action asset was left in the account with no handle on it.

The same early return sat in the Demand Gen image ad, Display, Video and Performance Max
asset loops. Each now walks the whole response, records every same-account id once
(`uniqueIDs`), and returns UNCONFIRMED after the loop, naming the FIRST bad index.
`parsedAssetIDs` and `parsedMutateAssetIDs` drop repeats too.

Tests: the `atomic wrong-account asset` case now expects `900,951`;
`TestCreateDemandGenCampaign_VideoAdKeepsEveryValidAssetID` puts the bad result in the
middle of two videos and a call to action, puts two bad results in one response, and
repeats an id; the Demand Gen image ad, Display, Video and Performance Max tests each put
a usable id after the bad one. Each test fails with its loop reverted.

Not changed: the extension assets (`createCampaignAssets`) keep their pinned
all-or-nothing contract; the Demand Gen video logo upload takes one logo; the criteria
loops return no ids from an unconfirmed response, as documented. The googleads concept's
"Unusable asset results" section gives the reasons, and the api-catalog
`demandGenVideoCreative` entry describes the full-length arm.
