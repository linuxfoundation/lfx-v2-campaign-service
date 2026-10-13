---
type: "Architecture Doc"
title: "Meta Ad-Set Monitor and Pause/Resume"
description: "Ad-set-level monitor-and-optimize for Meta: a live read of a campaign's ad sets with status, own budget and delivery counters, and a single-ad-set pause/resume guarded by the campaign row's ETag, provenance proven before any write, ACTIVATE limited to the recorded ad set, one unretried write answered APPLIED / ALREADY_IN_STATE or a 503 when unconfirmed, and a deliberate pause of the recorded ad set kept on the campaign row so a campaign resume leaves it paused."
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
5. The service's `beforeWrite` hook, when it passed one (a PAUSE of the recorded ad set not yet
   on record — see below): run once, here, after every guard; if it fails nothing is sent.
6. ONE `POST /{ad_set_id} {"status": …}` via `do(..., retryThrottle=false)` — never repeated.

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

The ad set's STATUS is not a column of the campaign row. When a call records or clears the
deliberate pause (below) the row is written through `ReplaceCampaign` on the claim holder's
connection — version bumped, index event co-committed — and the `200`'s `ETag` is the new
version. An error response carries NO `ETag` (Goa writes it in the 200 encoder only), yet the row
may still have been bumped on some 503 paths (an UNCONFIRMED pause, or a definite refusal after the
record was written and withdrawn), so after any error a caller re-reads the campaign for its
current version before retrying. Otherwise nothing
is persisted and the `ETag` is the row's unchanged version — the precedent of pausing a
`created_degraded` campaign — after `VerifyClaimedVersion` has proved no other writer changed the
row under the claim (409 if one did). If-Match still serialises this write with every other
campaign writer through the claim.

## A deliberate ad-set pause survives a campaign pause/resume (decided 2026-10-13)

Product decision: **a deliberate pause of the campaign's recorded ad set made here survives a
campaign pause and resume.** `toggle-campaign-status` ACTIVE on Meta normally cascades to the
recorded ad set and its ads (`UpdateCampaignAndChildrenStatus`); when the row holds a pause of
that ad set it instead calls `meta.Client.ActivateCampaignHoldingAdSet`, which activates the ads
and then the campaign (last, as always) but never posts the ad set. Its ads are activated so a
later resume of the ad set alone is enough to serve; the paused ad set gates them meanwhile. A
campaign PAUSE cascades exactly as before.

**Where it lives.** In the row's existing `result` JSON (the create path's untagged
`meta.CampaignResult` blob) under the extra key `UserPausedAdSetID` — no migration, no new
column (the column is Meta-only state; `metaAdSetID` records why the ad set id itself is not a
column either). `model.WithMetaUserPausedAdSet` rewrites only that key, preserving every other;
`model.MetaAdSetHeldPaused` is true only when the key names the recorded `AdSetID`. A
re-dispatch writes a fresh `result` with a fresh ad set, so a record cannot outlive its ad set.

**What is tracked.** Only the RECORDED ad set — the only one the campaign cascade touches. A
hand-added ad set (pause allowed, activate refused) and every ad set of an ADOPTED campaign (which
records none; #288's rule — PAUSED allowed, ACTIVE refused) record nothing, and those calls
behave exactly as before.

**When it is set, kept and cleared** (service `ToggleMetaAdSetStatus`, all under the campaign's
write lock):

| Lever call on the recorded ad set | Record |
|---|---|
| PAUSED, write about to be sent | SET first, through the dispatcher's `beforeWrite` hook (after every guard and the pre-write read); if that row write fails nothing is sent — 503 "nothing was changed on Meta", or 409 if the row changed |
| PAUSED → APPLIED | stays set; `VerifyClaimedVersion` at the new version |
| PAUSED → ALREADY_IN_STATE (e.g. the campaign pause already paused it) | SET after the answer; a failed row write is 503/409 saying the ad set is already paused but the pause was not recorded |
| PAUSED → **UNCONFIRMED** | **stays set** — unknown is treated as paused, so a later campaign resume never silently re-activates an ad set the user may have paused; the usual 503 and 30 s lock cooldown |
| PAUSED → REJECTED / NOT_SENT (definite) | withdrawn; if the withdrawal cannot be established the answer is a 503 saying the ad set may be shown deliberately paused while active (resume it here to clear), never "nothing was changed" |
| ACTIVE → APPLIED or ALREADY_IN_STATE | CLEARED (the `ReplaceCampaign` compare-and-swap is the row check); a failed clear is 503/409 saying the ad set WAS resumed but a campaign resume will still leave it paused |
| ACTIVE → **UNCONFIRMED** or any failure | **stays set** — the ad set may still be paused; keeping it held never spends unasked |

**Ambiguous commits (PR #309).** `ReplaceCampaign` tells two COMMIT failures apart. If the
SERVER answers the COMMIT with an error (a `*pgconn.PgError`, such as a deferred trigger raising,
or `pgx.ErrTxCommitRollback`), the rollback is CONFIRMED: a definite failure, so nothing is on
record and the pause is not sent. If the acknowledgement is LOST (a broken connection, an I/O
error, the deadline passing while the answer is awaited), PostgreSQL may have committed; only
that case is marked `domain.ErrWriteOutcomeUnknown` (the hazard `reconcileAmbiguousAudienceCommit`
handles for audiences). Every record write
goes through `writeMetaAdSetRecord`, which never reads that as "not written". It reconciles ON THE
CLAIMED CONNECTION, never a second pooled one beside it — the claim pins its connection for the
whole request, so a fresh one would starve on a saturated pool (certain with `pool_max_conns=1`),
the trap `ReplaceCampaign`'s own connection reuse avoids. Each step is bounded by
`persistResultTimeout`:

1. The same version-guarded write is repeated on the claimed connection — which doubles as the
   usability check. Success means the first attempt rolled back and the record is now written;
   `ErrPreconditionFailed` (a guarded UPDATE waits on the row lock of a transaction still
   resolving) means it landed, accepted only when `VerifyClaimedVersion` on the claimed
   connection shows exactly the next version with the intended record.
2. Any other failure means the claimed connection is unusable: the claim is handed back
   (`ReleaseCampaignLock`, which destroys a broken connection and frees its slot) and exclusive
   ownership is TAKEN AGAIN with `ClaimCampaignVersion` — a try-lock, as every writer takes it.
   Nothing continues without a SUCCESSFUL re-claim: another writer (a campaign resume, say) may
   take the claim in the gap, so a failed re-claim FAILS CLOSED — nothing is sent to Meta and
   nothing more is written, and the 503 says the record and the ad set's status are unconfirmed
   and to re-read the ad sets. Once re-claimed the answer is definite wherever it can be:
   - At the old version, the first attempt is proven not to have landed. The write is made on
     the new claim, and only that write's own ambiguous commit counts as "unknown"; any other
     failure is a definite "not recorded", and nothing is sent.
   - At the next version, the first attempt landed if that row carries the record. If it does
     not, another writer moved the row and the first attempt did not land (had it landed, that
     writer would have moved it to +2). That is a definite 409 and nothing is sent.

   The request's deferred release, cooldown and later writes follow the new claim.

Anything else is "unknown". The state that must never arise is a pause on record over an ad set
that was never paused (`deliberately_paused` while it serves), so a pause whose record may have
landed is ALWAYS sent — pausing never spends: landed or rewritten → the normal path; unknown → the
pause is still sent, then the record is settled from a read at the old or next version on the
claim's connection (a 503 "could not be confirmed" if even that fails, with the ad set paused on
Meta, so it cannot serve despite the record). The record is settled the same way when Meta's own
answer is UNCONFIRMED or unrecognised, BEFORE the 503 and before the cooldown starts: an
unconfirmed pause counts as paused, so the record must be on the row, or a later campaign resume
would re-activate an ad set Meta may have paused. If even that settlement cannot be confirmed,
the 503 says so (re-read `deliberately_paused` and pause again before resuming the campaign).
Only a DEFINITE rollback (any error without the sentinel, including a server-rejected commit)
suppresses the pause. A withdrawal and a clear-on-resume are reconciled the same way. On a
ONE-connection pool, `TestLiveMetaPauseRecordConfirmedRollbackSendsNothing`,
`TestLiveMetaPauseRecordReconcilesWithoutASecondPooledConnection` (a real lost acknowledgement,
after which pgx closes the claimed connection, so the claim is re-taken) and
`TestLiveMetaPauseRecordRejectedOnTheReTakenClaimSendsNothing` cover these outcomes. The unit test
`TestToggleMetaAdSetStatus_ConcurrentToggleInTheReclaimGapStopsThePause` runs a real campaign
resume that takes the claim in the gap: the pause is not sent.

**Concurrency.** The record and the campaign toggle are serialised by `ClaimCampaignVersion`: a
campaign resume racing a pause in flight is refused (409, nothing sent), one on the ETag read
before the pause is 412, and the next one reads the record
(`TestToggleMetaAdSetStatus_RacesTheCampaignToggleThroughTheWriteLock`).

**Read.** `list-meta-ad-sets` reports `deliberately_paused` on the recorded ad set — this service's
record, not Meta's — so the UI can explain why an ad set stayed paused after a campaign resume and
that resuming it here is the way to clear it. Meta's `status` alone cannot say that.

## Verified from Meta's docs only (not against a live account)

The `/adsets` edge's default population (whether DELETED/ARCHIVED ad sets are omitted — the
`listed: false` path covers either answer), `level=adset` Insights carrying `adset_id` and
`account_currency`, the `campaign.id EQUAL` filter, and `POST /{ad_set_id}` answering
`{"success": true}`.
