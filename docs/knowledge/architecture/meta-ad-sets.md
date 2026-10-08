---
type: "Architecture Doc"
title: "Meta Ad-Set Monitor and Pause/Resume"
description: "Ad-set-level monitor-and-optimize for Meta: a live read of a campaign's ad sets with status, own budget and delivery counters, and a single-ad-set pause/resume guarded by the campaign row's ETag, provenance proven before any write, ACTIVATE limited to the recorded ad set, one unretried write answered APPLIED / ALREADY_IN_STATE or a 503 when unconfirmed, and no persistence."
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
missing campaign row — or a project with no Meta connection at all (404, as on every sibling
lever), matched on `domain.ErrConnectionAbsent`, which only the connection lookup's absence
(`noOwnConnection`) carries, never on a bare `ErrNotFound`. A stored platform campaign id that is
not a canonical Meta id is refused locally with `ErrStoredPlatformIDInvalid` (409) — Meta is never
asked, so the answer never claims Meta reported anything.

## The toggle

Order, every refusal before the write:

1. Service: If-Match parsed (428), status enum and `ad_set_id` format (400), row loaded (404),
   version compared (412), capability (400), the campaign toggle's row-state rule (pending /
   orphan 409; `created_degraded` may only pause), no platform campaign id (409). Then the
   campaign's write lock is claimed at the version.
2. Dispatcher: provenance as for the read (409, zero requests); `ACTIVE` on a row that records no
   ad set — an ADOPTED campaign — refused (`ErrCampaignNotProvisioned`, 409, zero requests), the
   campaign toggle's rule; `ACTIVE` on any ad set OTHER than the recorded one refused
   (`ErrMetaAdSetNotRecorded`, 409, zero requests) — a hand-added ad set's targeting was never
   verified here; `PAUSED` allowed on any of the campaign's ad sets.
3. `GET /{ad_set_id}?fields=id,campaign_id,account_id,status`: must report this campaign under
   the connection's account (`ErrMetaAdSetNotInCampaign`, 409); `DELETED`/`ARCHIVED` is
   `ErrMetaAdSetUnwritable` (409); any other unrecognised status, or any read failure (100/33
   included), is a definite 503 — nothing was written.
4. Already at the requested status → `ALREADY_IN_STATE`, nothing sent.
5. ONE `POST /{ad_set_id} {"status": …}` via `do(..., retryThrottle=false)` — never repeated.

### Write-safety classification (`meta.ClassifyAdSetWrite`)

REJECTED is OPT-IN: only a Graph error envelope that DECODED CLEANLY (no `json.Unmarshal` error —
a type mismatch such as `"is_transient":"true"` leaves a half-read envelope) from a body
`identityjson.Check` accepted (`APIError.EnvelopeParsed`); unknown extra fields are fine, the
decoder does not disallow them; with a non-zero code other than 1
(unknown error) or 2 (service temporarily unavailable), `is_transient` false, a 4xx status other
than 408, and not a throttle, says "nothing was changed". Every other `*APIError` — including a
missing, zero or non-numeric code and a duplicated or case-folded key — is UNCONFIRMED. APPLIED
likewise requires a 2xx body that passes the identity check and carries `"success": true`; a
duplicated or case-folded `success` is UNCONFIRMED.

| What happened | Class | Answer |
|---|---|---|
| 2xx with `{"success":true}` | APPLIED | 200 `APPLIED` |
| Read showed requested status | — (not sent) | 200 `ALREADY_IN_STATE` |
| 2xx without `success:true`, or undecodable | UNCONFIRMED | 503 "unconfirmed — read the ad sets before retrying", lock held 30 s |
| 429, or a Graph rate-limit code (not retried) | UNCONFIRMED | same |
| 5xx, 3xx, 408, unreadable/HTML body, `is_transient`, code 1 or 2 | UNCONFIRMED | same |
| Transport failure / timeout after connect | UNCONFIRMED | same |
| Context already done, pre-connect dial error, request build | NOT_SENT | 503 "nothing was changed" |
| Definite 4xx (parsed envelope, not transient, not code 1/2/throttle) | REJECTED | 503 "nothing was changed" |

UNCONFIRMED is answered exactly as `toggle-campaign-status`, the budget lever and keyword actions
answer it: a 503 with fixed text and no result (so no ETag), with the write lock held for
`unconfirmedLockCooldown`. The ambiguity rule underneath is the create path's
(`IsOutcomeUnconfirmed` → `createOutcomeAmbiguous`). Client messages are fixed sentences; Meta's
text is logged.

### Other failures

| Case | Answer |
|---|---|
| Pre-write ad-set read fails or is unverifiable (100/33 included) | 503 "nothing was changed" |
| No Meta connection | 404 |
| Connection unusable / no ad account selected | 409 |
| LF system connection unusable or missing, credential decryption, service defect | 500 |
| Provenance unknown, account mismatch, invalid stored id, ad set not this campaign's/account's, not recorded (ACTIVE), adopted (ACTIVE), DELETED/ARCHIVED | 409 |
| APPLIED, but `VerifyClaimedVersion` finds the row changed or deleted | 409 "the ad set's status WAS changed on Meta, but this campaign was modified or deleted by another request meanwhile; read the ad sets before retrying" |
| APPLIED, but the row verification itself errors | 503 stating the status WAS changed on Meta |

### ETag

The ad set's status is not a column of the campaign row, so nothing is persisted and the version
is not bumped. The response's `ETag` is the row's unchanged version — the precedent of pausing a
`created_degraded` campaign — and after an `APPLIED` write `VerifyClaimedVersion` proves no other
writer changed the row under the claim (409 if one did). If-Match still serialises this write
with every other campaign writer through the claim.

## Interaction with the campaign toggle (known, product decision pending)

`toggle-campaign-status` on Meta cascades an ACTIVATE to the campaign's RECORDED ad set
(`UpdateCampaignAndChildrenStatus`). So **a campaign ACTIVATE re-activates the recorded ad set: a
deliberate ad-set pause made here does not survive a campaign pause/resume.** The campaign toggle
is deliberately unchanged; an operator who paused the recorded ad set must pause it again after
resuming the campaign.

## Verified from Meta's docs only (not against a live account)

The `/adsets` edge's default population (whether DELETED/ARCHIVED ad sets are omitted — the
`listed: false` path covers either answer), `level=adset` Insights carrying `adset_id` and
`account_currency`, the `campaign.id EQUAL` filter, and `POST /{ad_set_id}` answering
`{"success": true}`.
