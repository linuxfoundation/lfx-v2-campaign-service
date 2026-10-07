---
type: "Architecture Doc"
title: "Meta Ad-Set Monitor and Pause/Resume"
description: "Ad-set-level monitor-and-optimize for Meta: a live read of a campaign's ad sets with status, own budget and delivery counters, and a single-ad-set pause/resume guarded by the campaign row's ETag, provenance proven before any write, one unretried write classified APPLIED / UNCONFIRMED / ALREADY_IN_STATE, and no persistence."
resource: "internal/service/meta_ad_sets.go"
---

# Meta Ad-Set Monitor and Pause/Resume

`GET /projects/{project_id}/briefs/{brief_id}/campaigns/{campaign_id}/meta-ad-sets?window=…`
(`list-meta-ad-sets`) and
`POST …/campaigns/{campaign_id}/meta-ad-sets/{ad_set_id}/status` (`toggle-meta-ad-set-status`),
both `campaign_manager`, LFXV2-2665. On Meta the ad set — not the campaign — holds the budget,
the schedule and the targeting, so a campaign-level read hides which ad set is spending and a
campaign-level pause stops all of them. These two endpoints work one level down.

Both are capabilities only `MetaDispatcher` implements (`service.MetaAdSetReader`,
`service.MetaAdSetStatusToggler`); every other platform is `400` before anything is contacted.
Neither route needed a chart edit: both nest under `/briefs/**` (RuleSet) and `briefs(/.*)?`
(HTTPRoute); `parity_test.go` pins both paths.

## The read

Three kinds of GET, all or nothing (`meta.Client.ListCampaignAdSets`):

| Request | Bound | Checks |
|---|---|---|
| `GET /{campaign_id}/adsets?fields=id,name,status,effective_status,daily_budget,lifetime_budget,bid_strategy,campaign_id,account_id&limit=100` | 10 pages (`after` cursor, never Meta's `paging.next` URL, which carries the token) | canonical unique id; `campaign_id` = this campaign; `account_id` = the connection's (else `ErrAdSetAccountMismatch` → `ErrCampaignUpstreamIdentityMismatch`, 409); status tokens `^[A-Z][A-Z0-9_]{0,63}$`; integer budgets, not both |
| `GET /act_{id}?fields=currency` | one | ISO 4217 code |
| `GET /act_{id}/insights?level=adset&fields=adset_id,campaign_id,impressions,clicks,spend,account_currency&filtering=[campaign.id EQUAL id]&date_preset=…&limit=500` | 20 pages | `campaign_id` = this campaign; canonical `adset_id` at most once; `account_currency` = the account's; counter ABSENT = 0, explicit `null` or non-string refused |

Every page passes `identityjson.Check` on its RAW bytes before decoding. A `paging.next` on the
last allowed page, a missing or repeated cursor, or a missing `data` field fails the read rather
than truncating it. The window maps through the metrics read's `date_preset` allow-list.

An Insights row for an ad set the listing did not return (deleted or archived since) is reported
with `listed: false` and its counters only, so the campaign's spend is never under-reported.
Budgets are rendered by `formatMinorUnits` with the account currency's offset — the settings
readback's rendering — and are absent (never guessed) for an unmapped currency or a CBO ad set.

Provenance is the settings readback's: unknown provenance (`ErrCampaignProvenanceUnknown`) is 409
before a credential is resolved; a recorded account that differs from the connection is 409 with
zero requests. Graph 100/33 on the campaign is unverifiable (503), never a 404: the only 404 is a
missing campaign row. A project with no Meta connection is 409 here, not 404, for that reason.

## The toggle

Order, every refusal before the write:

1. Service: If-Match parsed (428), status enum and `ad_set_id` format (400), row loaded (404),
   version compared (412), capability (400), the campaign toggle's row-state rule (pending /
   orphan 409; `created_degraded` may only pause), no platform campaign id (409). Then the
   campaign's write lock is claimed at the version.
2. Dispatcher: provenance as for the read (409, zero requests); `ACTIVE` on a row that records no
   ad set — an ADOPTED campaign — refused (`ErrCampaignNotProvisioned`, 409, zero requests), the
   campaign toggle's rule; `PAUSED` always allowed.
3. `GET /{ad_set_id}?fields=id,campaign_id,account_id,status`: must report this campaign under
   the connection's account (`ErrMetaAdSetNotInCampaign`, 409); `DELETED`/`ARCHIVED` is
   `ErrMetaAdSetUnwritable` (409); any other unrecognised status, or any read failure (100/33
   included), is a definite 503 — nothing was written.
4. Already at the requested status → `ALREADY_IN_STATE`, nothing sent.
5. ONE `POST /{ad_set_id} {"status": …}` via `do(..., retryThrottle=false)` — never repeated.

### Write-safety classification (`meta.ClassifyAdSetWrite`)

| What happened | Class | Answer |
|---|---|---|
| 2xx with `{"success":true}` | APPLIED | 200 `APPLIED` |
| Read showed requested status | — (not sent) | 200 `ALREADY_IN_STATE` |
| 2xx without `success:true`, or undecodable | UNCONFIRMED | 200 `UNCONFIRMED`, lock held 30 s |
| 429, or 4xx with a Graph rate-limit code | UNCONFIRMED (not retried) | 200 `UNCONFIRMED`, lock held |
| 5xx, 3xx, unreadable error envelope | UNCONFIRMED | 200 `UNCONFIRMED`, lock held |
| Transport failure / timeout after connect | UNCONFIRMED | 200 `UNCONFIRMED`, lock held |
| Context already done, pre-connect dial error, request build | NOT_SENT | 503 "nothing was changed" |
| Definite 4xx (not a throttle) | REJECTED | 503 "nothing was changed" |

The ambiguity rule is the create path's (`IsOutcomeUnconfirmed` → `createOutcomeAmbiguous`), so a
write and a create never disagree. Client messages are fixed sentences; Meta's text is logged.

### ETag

The ad set's status is not a column of the campaign row, so nothing is persisted and the version
is not bumped. The response's `ETag` is the row's unchanged version — the precedent of pausing a
`created_degraded` campaign — and after an `APPLIED` write `VerifyClaimedVersion` proves no other
writer changed the row under the claim (409 if one did). If-Match still serialises this write
with every other campaign writer through the claim.

## Verified from Meta's docs only (not against a live account)

The `/adsets` edge's default population (whether DELETED/ARCHIVED ad sets are omitted — the
`listed: false` path covers either answer), `level=adset` Insights carrying `adset_id` and
`account_currency`, the `campaign.id EQUAL` filter, and `POST /{ad_set_id}` answering
`{"success": true}`.
