---
type: "Go Package"
title: "internal/platform/microsoft"
description: "Microsoft Advertising (Bing Ads) Campaign Management REST v13 client: OAuth2 refresh-token auth, request layer with 429 retry + BatchErrors classification (MS-1), and PAUSED find-or-create Campaign->AdGroup->Ad creation (MS-2/MS-2.5)."
resource: "internal/platform/microsoft"
tags:
  - platform-client
  - microsoft-ads
  - bing-ads
  - oauth2
  - go-package
timestamp: "2026-07-22T00:00:00Z"
---

# internal/platform/microsoft

Package microsoft provides a Go client for the Microsoft Advertising (Bing Ads)
Campaign Management API, backing the UI's Microsoft/Bing ad channel. It speaks the
**REST** transport directly (v13) rather than the legacy SOAP Campaign Management
service, so the client matches the meta/reddit/twitter/linkedin/googleads structure
and avoids a SOAP dependency. Credentials and account configuration are injected via
`NewClient`; the client never reads the process environment.

Naming note: the platform key surfaced to callers (`CampaignResult.Platform`, every
error prefix) is `microsoft-ads`, even though the live REST host is the legacy
`campaign.api.bingads.microsoft.com` domain — "Bing Ads" and "Microsoft Advertising"
are the same platform.

## Auth

Like Google Ads, Microsoft auth is richer than a single Bearer token. `Credentials`
carries the OAuth2 `client_id`/`client_secret` + `developer_token` + `refresh_token`;
`AccountConfig` carries the `account_id` (digits only, sent as `CustomerAccountId`)
and an optional `customer_id` (digits only, sent as `CustomerId` when set). Every
call sends the bearer access token, the `DeveloperToken` header, `CustomerAccountId`,
and — when set — `CustomerId`. The refresh-token→access-token exchange runs against
the Microsoft identity platform (`login.microsoftonline.com/common/oauth2/v2.0/token`,
scope `https://ads.microsoft.com/msads.manage offline_access`) and is coalesced with a
single-flight leader/follower (the token mutex is never held across the network call,
and the refresh runs on a `WithoutCancel`-detached context so one caller's
cancellation can't tear down a shared refresh). The OAuth response body is never
echoed into errors (it can carry the `client_secret`/`refresh_token` back).

## Request layer

`doRequest` applies the repo's standard discipline: no-follow redirects (on a shallow
copy so a supplied client isn't mutated), bounded response reads (`maxResponseBytes`),
and the pre-send (`isPreSendDialError`) vs ambiguous (`transportError`) vs definite
(`apiError`, status-only `Error()`) classification. `account_id`/`customer_id` are
validated digits-only at the request choke point (a header-injection guard). The
access token is fetched INSIDE the retry loop so a long 429 backoff can't leave a
resumed retry using an expired token, and each attempt is wrapped in a per-call
`context.WithTimeout(msAdsRequestTimeout)` so a custom `WithHTTPClient` with
`Timeout==0` can't hang indefinitely.

A rate-limited (429) **idempotent** call is retried up to `retryMax` times with a
bounded backoff honoring `Retry-After`; retry eligibility is an explicit parameter
(not the HTTP method) so a mutating create (a POST) is NOT retried on 429 — the
throttled request may already have committed (double-create risk). A server-declared
`Retry-After` that exceeds `maxRetryWait` returns the `overCapRetryAfter` sentinel and
ABORTS rather than clamp-and-retry into a guaranteed second 429; the delta-seconds
form is compared in seconds BEFORE converting to a `time.Duration` (so a huge value
can't overflow the multiply and wrap to a short wait), and `parseNonNegativeInt`
rejects overflow before each multiply-add (a bare `n<0` check misses a wrap past zero).

A read failure or an oversized body is classified STATUS-AWARE (`statusAwareReadError`):
a 2xx (the mutation may have committed but the result is unreadable) is an ambiguous
`transportError`, while a known non-2xx keeps its status as an `apiError` so
definite-4xx and 429-retry classification survive; an oversized 429 still follows the
retry/over-cap path. `transportError` wraps its cause UNEXPORTED and renders it via
`safeCause` (a fixed URL-free vocabulary) so a `*url.Error`'s embedded request URL
can't leak into a persisted campaign step.

## Error-code parsing

Microsoft reports errors two ways: a non-2xx `ApiFaultDetail` body (top-level
`Code`/`ErrorCode` + nested `OperationErrors` for request-level faults and
`BatchErrors` for per-list-item faults), and — on a 200 with per-entity failures — a
`PartialErrors` array (see MS-2). `parseErrorCodes` extracts the machine-readable codes
from ALL of these (`Errors`/`OperationErrors`/`BatchErrors`/`PartialErrors`) into
`apiError.ErrorCodes` (from the FULL body before truncation), tolerating Microsoft's
string-or-number `Code` via `codeString`. Visiting `BatchErrors` matters because a
duplicate/field error on one item of a batch mutate lands there, not in
`OperationErrors`. Codes are length/count bounded; the raw body is never surfaced by
`apiError.Error()`.

## Campaign creation (MS-2)

`CreateCampaign` (in `campaign.go`) find-or-creates a PAUSED Search campaign. Two
Microsoft facts drive a contract that differs from the google-ads `:mutate` model:

**PartialErrors on 200.** `POST CampaignManagement/v13/Campaigns` returns HTTP 200 with
`{"CampaignIds":[<id-or-null>], "PartialErrors":[...]}` — a per-entity failure is a 200
with a null id slot + a PartialError, NOT a non-2xx status. `firstCampaignID` inspects
the body and distinguishes a DEFINITE rejection (null id + PartialError →
`errPartialFailure`, a clean failure, surfacing only the machine-readable codes) from a
MALFORMED 200 (no id, no PartialError → the campaign may exist → UNCONFIRMED).

**Names are case-insensitively UNIQUE.** Microsoft enforces that `Campaign.Name` is
unique among the account's active/paused campaigns, using a case-insensitive comparison
(a duplicate create is rejected with `CampaignServiceCannotCreateDuplicateCampaign`).
That uniqueness IS the idempotency key. `CreateCampaign` FIRST looks the deterministic
name up (`findCampaignByName` — a READ, idempotent, retried on 429) and returns the
existing campaign (`AlreadyExisted=true`) without creating a second; a stable `NameSuffix`
makes that reliable. The lookup POSTs `Campaigns/QueryByAccountId` with the account id +
campaign type in the body (the v13 `GetCampaignsByAccountId` REST operation is a
POST-with-body, NOT a GET), and matches case-insensitively to mirror the service's own
comparison. If the create still loses a race and returns the duplicate-name PartialError,
`isDuplicateCampaignNameErr` surfaces it as already-exists (reconcile by name), not a
clean failure. `QueryByAccountId` returns the FULL campaign set for the type in one
response (not paged), so the single-shot lookup can't miss a match; the 8 MiB read cap is
the bound.

Ambiguity classification mirrors the sibling clients: an ambiguous transport/5xx/
mutating-429 create is UNCONFIRMED with a name-only partial for reconcile-by-name; a
definite 4xx (or a definite PartialError) is a clean failure. A `context.Canceled`/
`DeadlineExceeded` from the lookup is a clean `(nil, err)` abort (the lookup creates
nothing and the create never runs) — NOT a reconcile-partial. An already-done context
before any request is likewise a clean abort.

The `AddCampaigns` operation REQUIRES a top-level `AccountId` in the request body (a
sibling to `Campaigns`, not only the `CustomerAccountId` header) — omitting it rejects
every create with `CampaignServiceInvalidAccountId`, so `createCampaignsRequest` carries
it. `Campaign.TimeZone` is SENT (defaulting to `defaultTimeZone` when the caller supplies
none): the v13 Campaign object marks the field deprecated but ALSO "Add: Required", a
genuine contradiction in Microsoft's docs — since a missing required field fails every
create while a redundant deprecated field is harmless, the client sends it.

**Budget** is `DailyBudget` — a plain decimal in the account currency, with NO micros
conversion (Google Ads uses micros; Microsoft does not). Input is validated up front:
budget finite (NaN/Inf rejected) and > 0 and <= `maxBudget`; BOTH Project and EventName
non-empty (on the SANITIZED value); the composed name (`LFX | Search Campaign | Project |
Event | Suffix`, `|`-and-control-char sanitized) is length-capped in CHARACTERS.
`toMSDate` renders Microsoft's `{Month,Day,Year}` date object, reserved for the ad-group
flight dates a later slice needs.

## Ad group + ad creation (MS-2.5)

`CreateCampaign` completes the full Campaign → AdGroup → Ad hierarchy (`adgroup_ad.go`),
all PAUSED, so the result is a usable paused campaign rather than an empty shell —
mirroring the reddit/meta clients. After the campaign is created (or found), it
find-or-creates a PAUSED ad group then a PAUSED Responsive Search Ad. Every entity's create
and read follows the same v13 REST shape as campaigns: **creates are `POST /<Entity>` with
the PARENT ID in the body** (`POST /AdGroups` with `CampaignId`, `POST /Ads` with
`AdGroupId` — NOT in the URL); **reads are `POST /<Entity>/QueryBy…`**
(`AdGroups/QueryByCampaignId`, `Ads/QueryByAdGroupId`), not GETs. Each level uses the shared
`firstEntityID` classifier (a positive-integer id via `numberID` → success; a null id slot
with an ACTUAL PartialError, gated on `partialErrorsHaveAny` so a null-only placeholder
slice does not count → `errPartialFailure`; else a malformed 200 → the `errNoID` sentinel).
Both `errNoID` and the ambiguous-transport set are treated as UNCONFIRMED at the create call
sites (the entity MAY have been created — the ad-group/ad create has no idempotency key,
so a blind retry could duplicate); only a real PartialError is a clean rejection. Each
step returns a partial carrying the ids known so far (campaign id at the ad-group step;
campaign + ad-group at the ad step) so an ambiguous failure leaves the tree reconcilable.

**Ad type — Responsive Search Ad.** v13 does NOT support adding a `TextAd`/ExpandedTextAd
(every `TextAd` field is "Add: Not supported"; a standard-text-ad add fails with
`CampaignServiceAdTypeInvalid`). The currently-addable Search text ad is the
`ResponsiveSearchAd`: **3–15 unique headline assets** and **2–4 unique description assets**,
each a `TextAsset` wrapped in an `AssetLink`, plus a required `FinalUrls`. Asset length is
WIDTH-AWARE: normal copy allows 30 (headline) / 90 (description) final characters; Microsoft
documents a reduced 15 / 45 cap "for languages with double-width characters"
(CJK/Korean/Japanese/Chinese or emoji). v13 publishes no per-character weighted formula, so
the client conservatively applies the reduced 15 / 45 cap whenever ANY double-width character
is present — never emitting an over-length asset (which would fail the ad after its parents
were created), at the cost of truncating mixed copy slightly short of the theoretical maximum.
Each asset must also contain at least one word and no newline — this word check applies
to BOTH caller-supplied copy (`checkAdCopyList`) and auto-composed assets (`boundedUniqueCopy`
drops any candidate lacking a word, so a punctuation-only `EventName` never reaches AddAds).
The composed `FinalUrls` (registration URL + `utm_*`) is length-checked against Microsoft's
2,048-char limit up front, and its host is checked against the 67-char display-domain limit
(the RSA sets no `Path1`/`Path2`, so the whole display budget is the hostname; an over-long
host passes the 2,048-char check but is rejected only at AddAds). Its ad group is created with `AdGroupType` "SearchStandard" (the
"SearchDynamic" type takes only dynamic search ads) and a `Language` (the MS-2 campaign sets
no campaign-level languages, so the ad group must carry one). The AddAds body is polymorphic
(an array of the base `Ad`), so the responsive search ad DOES send `Type: "ResponsiveSearch"`
as the wire discriminator that selects the derived subtype ("Add: Read-only" on `Ad.Type`
bars CHANGING the type, not omitting the discriminator — without it the create is rejected).
`Ad.Status` defaults to *Active* on Add, so the ad sends `Status: Paused` explicitly
(otherwise it would be eligible to serve once a human enables the campaign).
`composeAdCopy` de-duplicates (case-insensitively), width-aware-truncates, and pads the
caller's `Headlines`/`Descriptions` up to the required minimum with deterministic
placeholders; `validateAdCopy` rejects an over-count, over-long (width-aware), duplicate,
word-less, or newline-containing caller entry up front. The AddAdGroups body also carries the
docs-required `ReturnInheritedBidStrategyTypes` (reserved; sent as `false`).

Ad-group idempotency is the (case-insensitively unique) ad-group name; ads have no stable
name (and v13 ALLOWS duplicate responsive search ads in an ad group), so ad idempotency is
keyed on the destination (`findAdByFinalURL` matches an existing ad whose `FinalUrls`
contains the composed URL). NOTE: `GetAdsByAdGroupId` (`Ads/QueryByAdGroupId`) marks
`AdTypes` REQUIRED (unlike `AdGroups/QueryByCampaignId`, which needs only `CampaignId`), so
the ad lookup sends `AdTypes: ["ResponsiveSearch"]` or the lookup is rejected before the
create is reached. The ad destination and ad copy are validated UP FRONT in
`CreateCampaign`, before the campaign create (`validateAdURL`: https/http, absolute, no
userinfo, well-formed query; `redactAdURL` for errors; `validateAdCopy` for the copy), so a
bad URL/copy fails cleanly `(nil, err)` without orphaning a PAUSED campaign or ad group. The
ad's `FinalUrls` is the registration URL with LFX `utm_*` params SET (`buildAdFinalURL`
preserves every other query param). `AlreadyExisted` is true only when the campaign, ad
group, AND ad ALL pre-existed (this run created nothing); creating any level makes it false.

## Scope

MS-1 is the scaffold (auth + request layer + error classification). MS-2 adds PAUSED
find-or-create campaign creation (`campaign.go`); MS-2.5 completes the ad group + ad
(`adgroup_ad.go`). The orchestrator dispatcher (register `microsoft-ads`, use the stored
`connection-microsoft-ads` credential) follows in MS-3.
