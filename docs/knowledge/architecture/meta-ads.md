---
type: "Architecture Doc"
title: "Meta Ad Read Under an Ad Set"
description: "A read-only live listing of the ads under one Meta ad set of a campaign this service owns, with status, creative and delivery counters: provenance and the ad set's parent campaign and account proven before its ads are read, every ad's parent ids and every raw body checked, the listing capped with a truncated flag and delivery all or nothing; no per-ad writes."
resource: "internal/dispatch/meta_ads.go"
---

# Meta Ad Read Under an Ad Set

`GET /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/meta-ad-sets/{ad_set_id}/ads?window=…`
(`list-meta-ads`), `campaign_manager`. Decision D7, first step: the ads under ONE ad set, so an
operator can see which ad is spending. **Strictly read-only** — a per-ad pause/resume is a later
decision (D7) and is not implemented. It mirrors the ad-set read in
[Meta Ad-Set Monitor and Pause/Resume](meta-ad-sets.md) at every step; only the differences are
spelled out here.

It is a capability only `MetaDispatcher` implements (`service.MetaAdReader`); every other
platform is `400` before anything is contacted. No rule or route change (chart comments only): the route nests under
`/briefs/**` (RuleSet) and `briefs(/.*)?` (HTTPRoute); `parity_test.go` pins the path.

## Order

1. Service: `window` (400), `ad_set_id` format (400, before any connection work — the design's
   Pattern and `meta.ValidateAdSetID` agree, pinned by
   `TestMetaAdsAdSetIDPattern_MatchesPlatformValidator`), row loaded (the only campaign 404),
   capability (400), no platform campaign id (409).
2. Dispatcher: a non-canonical stored campaign id is `ErrStoredPlatformIDInvalid` (409); then the
   ad-set read's provenance (`metaAdSetScope`): unknown provenance or an account mismatch is 409
   with zero requests.
3. `GET /{ad_set_id}?fields=id,campaign_id,account_id,status` (`GetAdSetState`) must report THIS
   campaign under the connection's account, else `ErrMetaAdSetNotInCampaign` (409) and nothing
   else is read — an empty list is never answered for another campaign's ad set. Any ad set of
   the campaign is readable, recorded or hand-added, adopted row or not.
4. `meta.Client.ListAdSetAds` (`internal/platform/meta/ads.go`):

| Request | Bound | Checks |
|---|---|---|
| `GET /{ad_set_id}/ads?fields=id,name,status,effective_status,creative{id,name},adset_id,campaign_id,account_id&limit=100` | 5 pages; a `paging.next` on the last one sets `truncated` | canonical unique id; `adset_id` = this ad set; `campaign_id` = this campaign; `account_id` = the connection's (else `ErrAdAccountMismatch` → `ErrCampaignUpstreamIdentityMismatch`, 409); status tokens `^[A-Z][A-Z0-9_]{0,63}$`; a creative, when present, has a canonical id; names at most 1024 bytes |
| `GET /act_{id}?fields=currency` | one | ISO 4217 code |
| `GET /act_{id}/insights?level=ad&fields=ad_id,adset_id,campaign_id,impressions,clicks,spend,account_currency&filtering=[adset.id EQUAL id]&date_preset=…&limit=500` | 20 pages, then the read FAILS | `adset_id` = this ad set; `campaign_id` = this campaign; canonical `ad_id` at most once; `account_currency` = the account's; counter ABSENT = 0, explicit `null` or non-string refused |

Every page passes `identityjson.Check` on its RAW bytes before decoding (`getChecked`, shared
with the ad-set read), so a duplicated or case-folded `adset_id` cannot smuggle another parent.
Paging follows the opaque `after` cursor, never Meta's token-bearing `paging.next` URL.

## Differences from the ad-set read

- **Truncation instead of refusal on the listing.** The ad listing stops at its cap and answers
  `truncated: true` with the ads read so far. Meta caps an ad set's ads far below 500, so this is
  not expected in practice. The DELIVERY read stays all or nothing: an ad that delivered but was
  not listed (deleted/archived since, or past the cap) is reported with `listed: false` and its
  counters, so the ad set's spend is never under-reported.
- **No budgets.** Ads carry none; the response has `creative_id` / `creative_name` instead, from
  the same request's field expansion.
- **No row record.** Nothing like `recorded` / `deliberately_paused`: the row records no ad ids.

## Failures

`classifyMetaAdSetError` with verb `read_ads` (log op `read_meta_ads`, upstream metric op
`read_meta_ads`): the ad-set read's table, with its own 503 text ("the ad set's ads could not be
read from Meta"). Foreign ad set, provenance unknown, account mismatch, upstream identity mismatch,
invalid stored id, unusable connection or no ad account: 409. The identity-mismatch 409 and the
unsupported-window 400 carry the ad read's own wording (an ad of this ad set; Meta ads). No Meta connection or no campaign
row: 404. LF system connection, decryption, service defect: 500. Anything unverifiable — Graph
100/33 on the ad set or its ads included — 503, never 404.

## Verified from Meta's docs only (not against a live account)

The `/{ad_set_id}/ads` edge's default population, the `creative{id,name}` expansion,
`level=ad` Insights carrying `ad_id`/`adset_id`, and the `adset.id EQUAL` filter.
