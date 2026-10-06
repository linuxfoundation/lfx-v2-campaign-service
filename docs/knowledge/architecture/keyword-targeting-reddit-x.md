---
type: "Architecture Doc"
title: "Keyword Targeting on Reddit and X"
description: "The keyword levers for Reddit and X, where a keyword is an entry in the ad group's or line item's targeting rather than a pausable criterion: a live read and a reduce-only removal, with ownership proven before any write, a compare-and-set revision on Reddit's whole-object write, and per-criterion outcomes on X; no per-keyword metrics."
resource: "internal/service/brief_keyword_targeting.go"
---

# Keyword Targeting on Reddit and X

`GET /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/keyword-targeting`
(`get-keyword-targeting`) and
`POST …/campaigns/{campaign_id}/keyword-targeting/removals` (`remove-keyword-targeting`),
both `campaign_manager`, LFXV2-2665. They close the keyword-lever gap for the two platforms whose
keywords are not criteria: Google Ads and Microsoft Advertising keep `apply-keyword-actions`,
which is untouched (its Google and Microsoft responses are byte-identical to before).

## Research (2026-10-06)

| Question | Reddit Ads API v3 | X Ads API v12 |
|---|---|---|
| Do our creates set keywords? | **Yes.** `CreateCampaign` puts `redditConfig.keywords` into the ad group's `targeting.keywords` (`internal/platform/reddit/client.go`, `baseTargeting`), kept on the communities/interests retry. | **No.** `CreateCampaign` creates campaign → line item → promoted tweet and never calls `targeting_criteria`; there is no keyword field in `twitterConfig`. Keywords exist only where an operator added them in X Ads Manager. |
| Read | `GET /ad_accounts/{account}/ad_groups/{id}` → `targeting.keywords` (the read the bid lever already makes). | `GET accounts/:account_id/targeting_criteria?line_item_ids=…&with_deleted=false`, cursor-paged; fields `id`, `line_item_id`, `targeting_type`, `targeting_value`, `operator_type`, `deleted`. |
| Write | No per-keyword endpoint. `PATCH` the ad group's `targeting`, which is **replaced as a whole object**. | `DELETE accounts/:account_id/targeting_criteria/:id` per criterion. A batch endpoint exists; its size limit and atomicity could not be established (sources disagree), so it is not used. |
| Pause? | No — a keyword has no status. | No — a criterion has no status. |
| Per-keyword metrics | A `KEYWORD` report breakdown is listed by secondary sources, but Reddit reporting is itself default-off (`REDDIT_METRICS_ENABLED`) pending a live read. **Not built.** | Segmentation is async-job only, and the current analytics page lists `AGE, GENDER, METROS, REGIONS, PLATFORMS, CONVERSION_TAGS` — no keyword segmentation found. **Not built.** |

Sources: X Ads API Campaign Management reference
(https://docs.x.com/x-ads-api/campaign-management/reference — too long to fetch whole; its
targeting section was confirmed through the page's search index), X's official Python SDK
(https://github.com/xdevplatform/twitter-python-ads-sdk, `twitter_ads/campaign.py`:
`TargetingCriteria` resource paths and properties, `LineItem.targeting_criteria` listing with
`line_item_ids`), the X analytics page (https://docs.x.com/x-ads-api/analytics.md). Reddit's
v3 OpenAPI document (https://ads-api.reddit.com/api/v3/openapi.json) refuses automated fetches,
so its facts come from **secondary** references to it: the `reddit_ads_update_ad_group` tool
("Targeting is replaced as a whole object"; fields `keywords`, `excluded_keywords`, …) and
`reddit_ads_get_report` (breakdowns incl. `KEYWORD`) at
https://glama.ai/mcp/servers/filippofinke/reddit-ads-mcp. **Unverified:** none of this has been
exercised against a live Reddit or X ad account; the X DELETE echo (`deleted: true`) and the
exact `targeting_criteria` list envelope are taken from the SDK and docs, not observed.

## Design

- **Own methods, not new kinds on `apply-keyword-actions`.** That method's ids are digits-only
  (X ids are base-36; a Reddit keyword has no id, only its text) and its `PAUSE` has no meaning
  here. Separate `KeywordTargetingReader` / `KeywordTargetingRemover` capabilities, type-asserted
  in the orchestrator like `NegativeKeywordAdder`, so neither shape can reach the other's adapter.
- **Reduce-only, and never the last keyword.** There is no add (supported upstream on both, not
  built: every keyword lever here only reduces what serves). A removal that would leave no
  keyword is refused (409): with none left the ad group / line item stops being keyword-targeted
  and serves to its other targeting alone, which widens delivery.
- **Ownership is proven before any mutation.** The row must record the ad group (Reddit
  `adGroupId`) or line item (X `LineItemID`); the platform must report it live and under THIS
  campaign (an unreported owner is refused like a different one); on X every named criterion
  must be a live, positive keyword criterion of that line item, read before the first DELETE.
- **Provenance and account identity before any call.** The read uses the toggle's check (an
  unrecorded creating account proceeds); the removal fails closed on it, as every mutation does.
- **Reddit concurrency.** The read returns `revision`, `sha256:` of the whole targeting object's
  canonical JSON (keys sorted, numbers kept exact). The removal re-reads, refuses (409
  `ErrKeywordTargetingChanged`) unless the fingerprint still matches, and writes back exactly
  that read with the named keywords taken out — every other member byte-for-byte. Reddit
  documents no conditional write, so the milliseconds between that read and the PATCH remain a
  window; a re-read afterwards must show exactly the written keywords and an unchanged
  fingerprint of every other member, or the outcome is UNCONFIRMED (and logged at ERROR when a
  non-keyword dimension moved).
- **X concurrency.** The guard's list is not enough on its own: two removals of DIFFERENT
  criteria (two requests, or a request and an operator in X Ads Manager) would each pass it and
  together empty the line item. So the targeting is re-listed immediately before EACH DELETE: an
  item whose criterion has meanwhile gone is FAILED/`NOT_FOUND`, and one that is now the last
  positive keyword is FAILED/`WOULD_EMPTY` — neither is sent. X offers no conditional delete, so
  the moment between that re-list and the DELETE remains a window, as Reddit's does; a concurrent
  removal landing exactly there can still leave the line item without keywords.
- **Keywords are matched exactly.** A Reddit removal names the keyword as the read reported it
  and is compared byte for byte — no trimming, no case folding — and echoed back unchanged; an
  all-whitespace keyword is a 400. X criterion ids are never trimmed either.
- **The Reddit write keeps what it read.** Every other targeting member, and every keyword element
  it does not remove (a `null` element included), is sent back exactly as read: compacted, never
  re-escaped (`json.Marshal` would rewrite `<`, `>` and `&` inside strings, so the body is encoded
  with HTML escaping off and sent pre-encoded), numbers keeping their spelling (a 20-digit integer,
  `1.10`), an explicit `null` staying `null`, an absent member staying absent.
- **Reddit writes are default-off** (`REDDIT_KEYWORD_TARGETING_WRITES_ENABLED`, exact `"true"`):
  a read representation the write interprets differently would change geo, community or
  interest targeting on a live, spending ad group. The read is not gated.
- **Outcomes.** Reddit: one PATCH, so every item shares its outcome (all APPLIED, or the call
  errors). X: one DELETE per item in request order, never retried on a 429; APPLIED, FAILED
  (`NOT_FOUND`, `REJECTED`, `NOT_SENT`) or UNCONFIRMED (transport, 3xx, 5xx, 429, or a 2xx not
  reporting this criterion deleted). The call errors only when no item got a definite answer.
  A short outcome slice from an adapter is UNCONFIRMED (`unconfirmedOutcomeCountError`).
- **Errors** never carry platform text: the service maps the domain sentinels
  (`ErrKeywordTargeting{Unsupported,Invalid,Unaddressable,Changed,WouldEmpty}` plus the shared
  connection/provenance ones) to fixed messages and logs the adapter text through
  `safeErrSummary`.

## Code

- `internal/platform/reddit/keyword_targeting.go` — `GetAdGroupTargeting`,
  `RemoveAdGroupKeywords`, `SameKeywords`.
- `internal/platform/twitter/keyword_targeting.go` — `ListLineItemTargetingCriteria`,
  `DeleteTargetingCriterion`.
- `internal/dispatch/reddit_keyword_targeting.go`, `internal/dispatch/twitter_keyword_targeting.go`.
- `internal/service/brief_keyword_targeting.go`; orchestrator `ReadKeywordTargeting` /
  `RemoveKeywordTargeting` (`opReadKeywordTargeting`, `opRemoveKeywordTargeting`).
- Routes inherit the `briefs(/.*)?` HTTPRoute match and `/briefs/**` RuleSet rule; pinned by
  `charts/lfx-v2-campaign-service/parity_test.go`.
