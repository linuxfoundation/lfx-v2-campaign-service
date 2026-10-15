# 2026-10-16 — LFXV2-2665: Google Ads bid write and negative keywords

**Update** — Google Ads gained the two post-create levers it was missing: `BidWriter`
(`WriteBid`) and `NegativeKeywordAdder` (`AddNegativeKeywords`). It is now the sixth
`BidWriter` and the fourth negative-keyword platform; the hubspot email channel remains the
only dispatcher that is not a `BidWriter`.

**Client (`internal/platform/googleads`).**

- `bid_update.go` — `ValidateMaxCPCBid` lifts the create path's own bounds (`minCPCBid` 0.01,
  `maxCPCBid` 100,000, NaN/Inf refused, `math.Round` to micros) so a bid the create path would
  refuse cannot be reached by editing, and `UpdateAdGroupCPCBid` writes `cpc_bid_micros` with
  one `adGroups:mutate` under an update mask naming that field alone. It is `UpdateCampaignBudget`
  line for line: guards before the request is built, the mutate sent idempotent because
  re-applying the same bid converges, and the acknowledgement verified with
  `validateResourceKind("adGroups", …)` BEFORE the id comparison — `firstResourceName` returns
  only the trailing segment, so another account's ad group or this account's campaign budget
  would otherwise pass an id-only check. Anything unverifiable is `unconfirmedBidMutateError`
  (`Unconfirmed()`), never a definite "not modified": the request was accepted.
  `adGroupCPCBidUpdate.CpcBidMicros` carries NO `omitempty` — under an update mask, a dropped
  field clears the bid rather than setting it.
- `negative_keywords.go` — `ListCampaignNegativeKeywords` reads the campaign's live negatives
  (no status predicate: GAQL already excludes REMOVED from `campaign_criterion`, which is exactly
  the colliding set) and re-applies every WHERE predicate in code, plus a UTF-8 check on the
  text. `AddCampaignNegativeKeywords` re-validates at the package boundary and delegates to the
  new shared `mutateCampaignNegativeKeywords`, which `createCampaignNegativeKeywords` in
  `targeting.go` is now a thin wrapper over — so the create path and the edit lever cannot drift
  on `negative`, on match types, or on how a mutate response is verified. The one thing that
  differs is parameterised: the definite-failure note, because the create path's sentence (a
  brand-new campaign left with no exclusions) is false on an edit.

**Dispatcher (`internal/dispatch`).**

- `googleads_bid.go` — provenance fails closed, then bid type, then the recorded ad group set.
  **Google is the first platform whose row can record more than one ad group**, and more than one
  is REFUSED rather than resolved: siblings carry independent bids, so the request names no single
  bid. `sortedKeys` keeps both the pick and the refusal message stable across identical requests.
  The strategy read admits only a reported `MANUAL_CPC`; an unreported one is refused rather than
  assumed, and because Google offers no portfolio `MANUAL_CPC` the same guard covers portfolios
  with no second query. Amount refusals map to 400 through `googleAdsBidAmountRejected` — needed
  here (unlike `WriteBudget`) because the service layer's own 1,000,000 ceiling and half-micro
  cutoff accept a wide band this client refuses, each of which would otherwise 503 and invite a
  retry that can never succeed.
- `googleads_negative_keywords.go` — campaign-level negative KEYWORD criteria, the same level the
  create path's exclusion list uses. A pre-read is REQUIRED, not an optimisation:
  `campaignCriteria:mutate` is atomic, so one already-present keyword fails the whole batch.
  Comparison is on `matchType + NUL + textfold.Key(text)`. Every keyword actually sent shares one
  outcome, and a 2xx returning a mismatched id count is UNCONFIRMED rather than paired
  positionally. The 503 arm fires only when no keyword reached a definite outcome, so
  `ALREADY_PRESENT` facts are not lost to a blanket failure.

**Service layer.** `negativeKeywordPlatform` (`brief_negative_keywords.go`) admits
`model.ProviderGoogleAds`, and the three caller-facing 400 messages (platform list, revision,
match types) name Google Ads. The bid path needed no allowlist change: `BidWriter` is reached by
type assertion only.

**Note — the one platform whose own default passes the guard.** `defaultBiddingStrategy`
(`internal/platform/googleads/bidding.go`) returns `MANUAL_CPC` for SEARCH, so a Search campaign
this service creates satisfies the manual-bid guard with no operator change — unlike Reddit
(`BIDLESS`), Meta (`LOWEST_COST_WITHOUT_CAP`) and X (`AUTO`), where the docs' "only after an
operator switches it" caveat is true. Demand Gen, Performance Max, Video and Display default to
automated strategies and are refused. The guard still decides on what Google REPORTS, never on
that default.

**Sibling-parity sweep.** Diffing `WriteBid` against `microsoft_bid.go` branch by branch turned
up one real gap: the Microsoft path checks the ad group id it read from the row against
`microsoftCampaignIDRE`, and this path did not. `UpdateAdGroupCPCBid` does refuse a non-numeric id
— it interpolates it into a resource name — but with a plain error, which the service answers
"the platform could not be reached; the campaign was not modified": false twice over, since
nothing was sent and no retry can fix a malformed id in the row. `WriteBid` now runs
`googleads.ValidateCampaignID` on the recorded ad group id (the canonical positive-int64 check;
Google ad group ids have the same spelling) and returns `ErrBidUnwritable` before a credential is
decrypted. Two user-facing sentences had to move with it: `brief_bid.go`'s `ErrBidUnwritable` 409
now names all three refusal kinds rather than only the adopted-campaign one, and
`docs/api-catalog.md` dropped the claim that the several-ad-groups 409 names the ad groups to the
caller — the ids go to the service log.

The allowlist's 400 sentence is now the named constant `negativeKeywordUnsupportedMessage`, so
the switch and the sentence can be checked against each other — adding a platform to one and not
the other tells every other platform's caller that the new one is unsupported, which is exactly
what had to be fixed here. `TestAddNegativeKeywords_UnsupportedPlatformIsBadRequest` no longer
lists Google among the refused platforms (hubspot took its place in the table), and
`TestBidWriter_GoogleAndHubSpotDoNotImplementIt` became
`TestBidWriter_HubSpotDoesNotImplementIt`, with `GoogleAdsDispatcher` moved into the compile-time
`service.BidWriter` assertions. `design/brief.go` and `docs/api-catalog.md` were updated for both
endpoints and the API regenerated with `make apigen`.
