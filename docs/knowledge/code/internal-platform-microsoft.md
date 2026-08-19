---
type: "Go Package"
title: "internal/platform/microsoft"
description: "Microsoft Advertising (Bing Ads) Campaign Management REST v13 client: OAuth2 refresh-token + developer-token auth, request layer with 429 retry and status-aware error classification incl. BatchErrors (MS-1), and PAUSED find-or-create Campaign->AdGroup->ResponsiveSearchAd creation over the POST /<Entity> + POST /<Entity>/QueryBy… transport, idempotent by case-insensitive-unique NAME for the campaign and ad group but by DESTINATION URL for the ad (ads have no stable name and v13 permits duplicate RSAs) (MS-2/MS-2.5), keyword targeting via the DEDICATED POST /Keywords resource plus an ad-group CpcBid, without which a created Search campaign has nothing to match a query against and can never serve (MS-4/LFXV2-3279), plus ad-account discovery against the SEPARATE Customer Management v13 service on a different host, the one call that is not account-scoped (LFXV2-3064), and campaign metrics through the asynchronous Reporting v13 service — submit/poll/download folded into one bounded call, default-OFF behind MICROSOFT_METRICS_ENABLED while the contract is unverified (LFXV2-3260)."
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
call sends the bearer access token and the `DeveloperToken` header, and — when set —
`CustomerId`.

`CustomerAccountId` is **not** universal, and neither is the account id itself.
Campaign Management calls are account-scoped and send it; **Customer Management**
calls (ad-account discovery) do not, and do not validate `account_id` either. An
empty header is a claim about an account, so it is omitted entirely rather than sent
blank. This is what makes discovery reachable from a connection that holds
credentials and no account id at all — the exact state discovery exists to resolve —
and it is why "one client, one ad account" describes the Campaign Management surface
rather than a construction invariant of `Client`. The refresh-token→access-token exchange runs against
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
name up (`findCampaignByName` — a READ, idempotent, retried on 429) and REUSES the existing
campaign instead of creating a second; a stable `NameSuffix` makes that reliable. Reusing
the campaign does NOT return early — creation continues through the ad group and ad under
it (see MS-2.5 below), so the final `AlreadyExisted` reflects whether the WHOLE tree
(campaign, ad group, AND ad) pre-existed, not just the campaign. The lookup POSTs `Campaigns/QueryByAccountId` with the account id +
campaign type in the body (the v13 `GetCampaignsByAccountId` REST operation is a
POST-with-body, NOT a GET), and matches case-insensitively to mirror the service's own
comparison. If the create still loses a race and returns the duplicate-name PartialError,
`isDuplicateCampaignNameErr` surfaces it as already-exists (reconcile by name), not a
clean failure. `QueryByAccountId` returns the FULL campaign set for the type in one
response (not paged), so the single-shot lookup can't miss a match; the 8 MiB read cap is
the bound.

Ambiguity classification mirrors the sibling clients: an ambiguous transport/5xx/
mutating-429 create is UNCONFIRMED with a name-only partial for reconcile-by-name; a
definite 4xx (or a definite PartialError) is a clean failure. A lookup failure is a clean
`(nil, err)` abort ONLY when the CALLER's context is done — the gate is `ctx.Err() != nil`,
not `errors.Is(err, context.DeadlineExceeded)`. Because the client wraps each attempt in
its own `context.WithTimeout`, a per-attempt `DeadlineExceeded` can surface while the
caller's context is still live; that is a FAILED lookup (we can't confirm the campaign is
absent) and must be UNCONFIRMED with a name-only partial, NOT a clean "nothing created"
abort. An already-done context before any request is a clean abort.

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
2,048-char limit up front, and its host is checked against the display-domain limit (67
normally; Microsoft reduces it to 33 "for languages with double-width characters", so a host
containing any wide char — e.g. a CJK IDN — is conservatively held to 33, matching the copy
caps). The RSA sets no `Path1`/`Path2`, so the whole display budget is the hostname; an
over-long host passes the 2,048-char check but is rejected only at AddAds. Its ad group is created with `AdGroupType` "SearchStandard" (the
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

## Keyword targeting + ad-group bid (MS-4)

MS-2.5 produced a tree that was structurally complete and **commercially inert**: a Search ad
group with zero keywords has nothing to match a query against, so enabling the campaign in the
Bing UI would spend nothing and serve nothing. `targeting.go` (LFXV2-3279) closes that gap.

**Keywords are their own resource, NOT AdGroupCriterions.** They are created with
`POST CampaignManagement/v13/Keywords` — the historical `AddKeywords` operation, still the v13
path. This is the one place where reasoning by analogy to the google-ads sibling
(`adGroupCriteria:mutate`) produces a create that fails every time: the `AdGroupCriterions`
endpoint does exist and is spelled "Criterions", but its `AdGroupCriterionType` enum has **no
`Keyword` member** (Age, Audience, … Webpage), so keywords cannot be routed through it.
`AdGroupId` is a **sibling** of the `Keywords` array (as `CampaignId` is for AddAdGroups and
`AdGroupId` for AddAds), not a field of the Keyword object, and a Keyword is **flat**
(`{Text, MatchType, Status}`) with no `Type` discriminator and no nested `Criterion` — unlike
the polymorphic Ad body. The response is `KeywordIds` + a **flat** `PartialErrors` array (the
`NestedPartialErrors` shape belongs to the criterion endpoints this client does not use).

**Match types are `Exact`/`Phrase`/`Broad`**, PascalCase — not the google-ads SCREAMING_CASE.
`canonicalMatchType` accepts either casing and emits Microsoft's, so a caller reusing a Google
Ads payload is not refused over spelling, while an UNRECOGNISED value is refused rather than
defaulted: defaulting a typo would silently broaden or narrow what a paid campaign matches.
`validateKeywords` caps text at **100 characters** (Google Ads caps the same field at 80),
rejects control runes and empty text, bounds the list at `maxKeywords` (60, matching the
sibling — a 20-cap was observed refusing the product's own AI brief generator, which emits
~38), and de-duplicates by (matchType, **case-folded** text) while SENDING the caller's
original casing. A bad entry is a HARD error, never a silent drop: unlike ad copy there is no
defensible placeholder for a keyword, since substituting one would put an invented search term
on a paid campaign in the caller's name.

**Keywords are created PAUSED** — a deliberate divergence from googleads/targeting.go, which
creates criteria ENABLED and argues the paused ancestors are gate enough. That argument makes
the keyword list the one part of the tree no human need ever review: it would start spending
the moment the campaign and ad group are enabled, which is the documented next step. The cost
of PAUSED is one bulk-enable on a list the operator should be reading anyway; the cost of
ENABLED is unreviewed spend. The status cascade enables them, so the service-driven path is not
made harder (see Status toggle).

**Bids.** `AdGroup.CpcBid` is a `Bid` object (`{"Amount": N}`) — a plain decimal in the ACCOUNT
currency, no micros. It is a POINTER and is **omitted when unset**, which is the load-bearing
detail: Microsoft documents that an ad group with no bid is "set to the minimum depending on
your account's currency", a documented, serve-capable floor, whereas an explicit `{"Amount":0}`
is a zero bid — a different and worse thing. The client therefore invents no default bid; the
currency-correct minimum beats any constant this client could hardcode across every account
currency. A REUSED ad group keeps its existing bid: silently re-bidding a group a previous run
or a human configured would change what a live campaign pays on an idempotent retry. NOTE for
future work: Microsoft has IGNORED bid strategies on ad groups and keywords since April 2021
("the request will be ignored without error"), so an `AdGroup.BiddingScheme` would be a silent
no-op — the bid strategy is the CAMPAIGN's (v13 defaults Search to `EnhancedCpcBiddingScheme`).

**A REUSED ad group IS re-keyworded**, and the reason is a documented Microsoft behaviour
rather than a preference. `AddKeywords` has **no idempotency key** and v13 exposes **no keyword
READ** (no `GetKeywordsByAdGroupId`, no list), so when the ad group is found by name the client
cannot enumerate what already hangs off it. An earlier revision treated that blindness as a
reason to SKIP the step, believing a re-post would create a SECOND COPY of every keyword and
double the bid on those terms. **That belief is false.** Microsoft rejects a keyword that
already exists on the ad group rather than duplicating it:

* `CampaignServiceDuplicateKeyword` (**1517**) — "An attempt was made to create a duplicate of
  a keyword that already exists."
* `CampaignServiceKeywordAndMatchTypeCombinationAlreadyExists` (**1542**) — "A keyword with the
  specified match type already exists."

Comparison is by **normalized** form, not literal text: case, whitespace, accents, number/date
formatting and punctuation are folded first, so `Car`, `car` and `car.` are one keyword. A
re-post therefore duplicates nothing — each already-present keyword returns a per-entity
`PartialError` on a 200 and each genuinely NEW one is created, which is exactly the reconcile
behaviour the skip approximated, enforced by Microsoft instead of guessed at locally. Both
codes are recognized under **either** spelling (symbolic enum or numeric `Code`) by
`isDuplicateKeywordPartial`, and a batch whose only rejections are duplicates is **not** an
error: it returns the ids of whatever was newly created, with a `steps` line saying the rest
already existed.

`isDuplicateKeywordPartial` requires **every** actual error to be a duplicate, not merely one
of them. A batch can mix a duplicate with a GENUINE rejection (editorial disapproval, bad bid,
over-length term); an ANY test would report that whole batch as success and tell the operator
the editorially-rejected keyword "already existed" — a keyword that does not exist upstream and
never will. A mixed batch therefore stays on the `errPartialFailure` path, where the created
ids are still carried out and the rejection is still surfaced.

The ALL test is only as good as the error set it can see. `boundedErrorItems` retains
`maxDecodedErrorItems` (16) entries per array to bound memory against an 8 MiB fault body,
while `AddKeywords` sends up to `maxKeywords` (60) and a reuse retry re-posts the whole batch —
so a typical brief returns one duplicate per keyword and the array is routinely truncated. A
rejection past the cap is discarded during decode, leaving the retained prefix reading as
wholly-duplicate.

Refusing whenever the array was `Truncated` closed that hole but refused the ordinary
converge-on-reuse case for any brief over 16 keywords, and no count can replace it:
`PartialErrors` is sparse, and a duplicate and a genuine rejection are each exactly one error.
The classification is therefore made **during decode**, where every element is visible before
the ones past the cap are dropped. `NonDuplicateKeywords` tallies the entries carrying an actual
error code that is not an already-exists keyword code, over the whole wire array; the call site
requires it to be zero. Presence is tested on the raw bytes, so an unparseable-but-present code
counts as a rejection rather than collapsing into the null-placeholder answer.

**Why the skip had to go.** "The ad group exists" and "its keywords exist" are DIFFERENT facts,
and they come apart on the FIRST run: if run 1 creates the ad group then fails before the
keywords land (an ad failure, an UNCONFIRMED keyword step, a 429), run 2 finds the group, skips,
and returns success with no keywords. The row persists empty `KeywordIDs` and
`MicrosoftDispatcher.ToggleStatus` then refuses ACTIVATE **forever** with
`ErrCampaignNotProvisioned` — a campaign no automated path can turn on, with no keyword read
available to repair it. Posting is both the safe direction and the terminating one.

**Knock-on:** when EVERY supplied keyword was already attached, `KeywordIDs` is still empty — a
duplicate entry returns a null id slot and v13 has no read to resolve it — so the ACTIVATE guard
refuses a campaign whose keywords do exist upstream. Deliberate: the ids needed to enable those
Paused keywords are precisely what the run could not learn, so activating would claim success
while every keyword stayed Paused. Reconciliation (LFXV2-2665) is what resolves that; the
keyword-less tree, which is the case that mattered, now provisions on its own.

**Ordering and classification.** The keyword step runs LAST, after the ad, because every
earlier step is its prerequisite and keywording an ad group whose ad might still fail would
leave paid criteria on an incomplete tree. Both the keyword list and the bid are validated **up
front in `CreateCampaign`**, before the campaign create, for the same reason the URL and ad copy
are: a bad value discovered at the last step would fail after a PAUSED campaign, ad group and ad
already exist. The create is **not retried on 429** — AddKeywords has no idempotency key, and while
Microsoft refuses an exact (normalized) duplicate, an automatic in-flight retry is still not
the place to rely on that: a 429 leaves the batch's outcome unknown, and the reuse path above
is the deliberate, observable place where a re-post is made. A
definite `PartialError` is a per-entity rejection, but NOT necessarily a total one: `AddKeywords`
is a batch with an index-aligned response, so `[701, null, 703]` with one `PartialError` means two
keywords were created. Those ids travel out WITH the error — they are what the status cascade
enables on ACTIVATE, and what stops a reconciliation double-creating the entries that succeeded —
and the message carries the count (`2 of 3 keywords were created`). Only an all-rejected batch is
a clean failure with nothing to reconcile. A short/null-slot id array with no `PartialError`, or
an unparseable 2xx, is UNCONFIRMED: the keywords may exist and a blind retry would duplicate them.

`CampaignResult.KeywordIDs` carries the ids, and on a successful create the count equals the
count SENT: input is capped at `maxKeywords`, the id array decodes through `boundedKeywordIDs`
(bounded by the same cap rather than by the 16-item error-array bound), and success requires one
usable id per keyword. The step text previously hedged to a "count PARSED" because that decode
bound made a valid response come back short — LFXV2-3279 closed that, so the count is now a
confirmed figure.

**GEO TARGETING IS NOT IMPLEMENTED, and the reason is an API constraint rather than a scoping
preference.** Microsoft takes location criteria at the CAMPAIGN level
(`POST /CampaignCriterions` with a `LocationCriterion`), whose `LocationId` — a numeric
Microsoft identifier — is its ONLY Add-writable element; `DisplayName` and `LocationType` are
read-only, so a country cannot be named. Those ids come from Microsoft's geographical-locations
CSV, fetched via `POST /GeoLocationsFileUrl/Query`, which this service does not ingest. The v13
API accepts an ISO 3166 code for targeting **nowhere** — the ISO table in Microsoft's own
Geographical Location Codes guide is explicitly scoped to account business addresses, and the
locations file has no ISO column. Since the sibling dispatchers' `geoTargets` are ISO-2 strings
("US", "JP"), honouring them here would mean hardcoding an invented ISO→LocationId map on the
path that spends money, one that silently rots as locations are deprecated. `microsoftConfig`
therefore carries **no** `geoTargets` field at all: offering one the dispatcher would silently
drop is worse than not offering it. Doing this properly needs the locations file as a real,
refreshable input, plus handling for the auto-created `LocationIntentCriterion` (which can
never be deleted) and the rule that ad-group geo OVERRIDES campaign geo wholesale.

## Ad-account discovery

`ListAdAccounts` (`accounts.go`) enumerates every Microsoft Advertising account these
credentials can reach, so a connection that holds only credentials — or one being
re-pointed at a different account — can ask which accounts are available.

**A second service, on a second host.** Unlike every other call in this package,
discovery is not a Campaign Management call. It is
`POST clientcenter.api.bingads.microsoft.com/CustomerManagement/v13/AccountsInfo/Query`.
Microsoft splits its API by service across DIFFERENT hosts, so `msCustomerBaseURL` and
`WithCustomerBaseURL` sit alongside the campaign base rather than replacing it, and the
tests point the two at different servers — a call routed to the wrong service would
otherwise look correct. Reporting is a THIRD service again
(`reporting.api.bingads.microsoft.com`, reached via `msReportingBaseURL` /
`WithReportingBaseURL` / `doReportingRequest`) — see the metrics section below. It is
REST/JSON like the other two, NOT SOAP; what makes it different is that it is
ASYNCHRONOUS (submit, poll, then download a zipped CSV). The version segment is NOT a
second knob: `WithAPIVersion` sets `c.apiVersion`
for both hosts, because Microsoft versions the two services in lockstep — a caller
pinning a version is pinning the client, not one of its halves.

**The request asks about the CREDENTIALS, not about an account.** `doCustomerRequest`
deliberately does NOT call `validateAccountIDs` and does NOT send `CustomerAccountId`
— it validates only `customer_id`, and only when one is set. Requiring a valid account
id would make discovery unreachable exactly when it is needed, which is the state it
exists to resolve. The header is OMITTED rather than sent empty: an empty
`CustomerAccountId` is still a claim about an account.

Which calls omit it is decided by the **operation's scope**, not by whether the connection
happens to hold an account — discovery omits it even when `AccountID` is set. That is not
a detail: discovery exists partly to RE-POINT a connection that already has an account, so
the configured-account case is half the traffic. A client that sent the header "whenever
there is one to send" would look reasonable, pass the empty-config assertion, and scope
every re-point to the account the user is trying to move away from, returning that account
and hiding the rest. `attempt` takes an explicit `accountScoped` flag rather than inferring
this; `TestListAdAccounts_OmitsTheAccountHeaderEvenWhenOneIsConfigured` pins the case, and
a separate test pins that the campaign path still sends its account header.

**"Every account" needs one query per customer.** `AccountsInfo/Query` is scoped to ONE
customer whichever way it is called: Microsoft documents it as returning the accounts
"accessible from the specified customer", and omitting `CustomerId` does not widen it —
"if not set, the user's credentials are used to determine **the** customer", still
singular. So a user who administers several customers used to get one customer's
accounts and no sign the rest existed. `ListAdAccounts` therefore runs
`discoveryCustomerIDs` first: `POST CustomerManagement/v13/User/Query` with `UserId`
omitted returns the authenticated user's `CustomerRoles`, one entry per customer the
credentials reach, and each id gets its own `AccountsInfo/Query`. A configured
`customer_id` short-circuits that — the operator scoped the connection deliberately, and
widening it back out would undo the scoping and pay for a `User/Query` it cannot use.

`OnlyParentAccounts=false` is **not** a substitute for the loop, and the distinction is
easy to lose: a linked account is one attached to the customer BEING QUERIED, which is a
different relationship from a second customer the same user administers. Only
`User/Query` names the latter. It is still sent false, because otherwise the picker
narrows to the accounts that customer owns outright.

Only the `CustomerRoles` field of the `User/Query` envelope is decoded. The `User` object
carries a password field, a secret answer and an authentication token, and none of them
are needed here — so a malformed body is reported without ever echoing it, and a test
pins that the marker text in the response never reaches the error string.

The union is deduplicated by account id (first occurrence wins): the same account is
reachable under more than one customer whenever it is linked, which is exactly what
`OnlyParentAccounts=false` asks for, and offering it twice makes a user wonder which
entry is real. One customer erroring fails the WHOLE call — a partial union is the
false-absence bug this loop exists to remove, and it is indistinguishable from a complete
one at the boundary. `CustomerRoles` absent, `null` or `[]` is likewise an error, not zero
accounts: Microsoft documents at minimum one entry, so none of those is the answer "these
credentials reach no customers", and an unusable role id fails for the same reason a bad
account id does.

The call is marked idempotent — it creates nothing, so retrying a 429 cannot
double-create, and without the retry a transient rate limit fails a user's first attempt
to connect an account.

**Two health axes, kept apart.** `AccountLifeCycleStatus` (Active, Draft, Inactive,
Pause, Pending, Suspended) and `PauseReason` answer different questions and can
disagree — Microsoft returns a pause reason alongside a status that is not itself
"Pause", so an account can read as bindable and still not spend. `Usable()` is an
ALLOW-LIST (status exactly "Active" AND no pause reason), so an absent or unrecognized
status reads as "not confirmed usable" rather than as healthy. `StatusLabel()` is empty
for Active and for anything this package does not recognize; an empty label is not a
claim that the account is fine. `PauseLabel()` renders an undocumented flag value
verbatim ("paused (unrecognized reason 9)") rather than flattening it to "paused" —
that raw value is the only detail distinguishing a Microsoft-side change from a bug
here. Draft, suspended and paused accounts are all RETURNED with their reason, not
filtered: this feeds a picker, and dropping a user's only account answers "your
credentials reach no ad accounts" about an account sitting right there.

**Fail, do not truncate.** The endpoint is unpaginated, so there is no cursor walk to
bound; the two remaining modes both fail the whole call. `{}` (no answer) must stay
distinguishable from `{"AccountsInfo": []}` (zero accounts) — collapsing them is the
false-absence shape that sends a user looking for a permissions problem that does not
exist. The DECODER already preserves that distinction on its own: `encoding/json` leaves
a field whose key is ABSENT untouched and SETS a present `null` to nil — two different
operations, agreeing here only because the envelope is declared fresh per response — while
a present `[]` decodes to a non-nil empty slice. `AccountsInfo` is a POINTER to a slice for a reason
that is about the next reader rather than the mechanism: a plain slice invites a
`len(x) == 0` check that silently merges the two cases, while a nil pointer must be
dereferenced and the compiler makes that choice explicit. Do not "simplify" it away
without replacing the nil guard. And an id that `numberID` rejects errors rather
than skipping the row, because a response shape that far from the documented one is not
the response we think it is.

**`numberID`, not `accountIDRE`, and the distinction generalises.** `accountIDRE`
(`^[0-9]+$`) is a **transport** check: is this string safe to put in a header. It
therefore admits `0` and a forty-digit number, neither of which can name a Microsoft
entity — ids there are positive `int64`s. Discovery is an **identity** check: the id *is*
the answer, and the account it names gets bound to a connection and spends money.
`numberID` (`campaign.go`) enforces positivity and signed-64-bit range on top of the digit
shape, which is why the create path already uses it on returned ids. Everything `numberID`
accepts `accountIDRE` accepts too, so reusing the stricter one keeps the original property
— a discovered account cannot fail at bind time — while closing the gap. **A validation
borrowed from a transport concern is not automatically the right one for an identity
claim; check which question it was written to answer.**

The same rule reaches the CONFIGURED customer id, which is the easier half to miss.
`doCustomerRequest` validates it, so it looks covered — but that check is the transport one
again, and `discoveryCustomerIDs` does not use the value as a header: it returns it as the
answer to "whose accounts are these", to be enumerated under and offered as a picker.
Trusting a configured id more than a discovered one is backwards. A discovered id arrived
seconds ago from the API; a configured one has been sitting in a connection record since
whenever it was written. `discoveryCustomerIDs` therefore runs `numberID` over it and fails
the call rather than querying under an id that cannot name a customer. `Id` is decoded as a `json.Number`, not through `any`: Microsoft types
it as a `long`, and float64 silently loses precision above 2^53, producing a WRONG
account id that still looks like one (a test pins 2^53+1 round-tripping exactly).

## Scope

MS-1 is the scaffold (auth + request layer + error classification). MS-2 adds PAUSED
find-or-create campaign creation (`campaign.go`); MS-2.5 completes the ad group + ad
(`adgroup_ad.go`). MS-3 registers `microsoft-ads` and wires the stored
`connection-microsoft-ads` credential into the orchestrator dispatcher
(`internal/dispatch/microsoft.go`). MS-4 (LFXV2-3279) attaches KEYWORDS and an ad-group
CpcBid (`targeting.go`), which is what makes a created campaign able to serve at all. The
**status toggle** (LFXV2-2810) adds `UpdateCampaignAndChildrenStatus` on top: a cascade whose
ordering, child-id guard and outcome classification are described under Status toggle below. **Ad-account discovery**
(LFXV2-3064) adds `ListAdAccounts` against the separate Customer Management service, the
one call in this package that is NOT account-scoped.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` microsoft adapter (see [internal/dispatch](internal-dispatch.md))
interprets an OAuth2 app (clientId/secret) + a developer token + refreshToken;
AccountConfig comes from the connection's AccountID (the DIGITS-ONLY
`CustomerAccountId`, trimmed) plus an optional `customer_id` (the manager/`CustomerId`
header). The client builds the full Campaign → AdGroup → Ad hierarchy (all PAUSED) — so
the adapter's ad config is `microsoftConfig.budget` (the DAILY budget, in the ACCOUNT's
currency, no FX), an optional `timeZone`, the `keywords` the ad group needs in order to serve
at all, and an optional `cpcBid`. **`ToggleStatus` refuses to ACTIVATE a campaign whose
`keywordIds` are empty**, with `ErrCampaignNotProvisioned` (a 409) raised locally without
calling Microsoft — a Search campaign with no keywords cannot deliver, so enabling it would
report success for something that serves nothing. PAUSE requires no keywords: refusing it
would strand a campaign an operator is trying to stop. `NameSuffix = brief.ID` gives
deterministic retry-safe names (Microsoft enforces case-insensitive campaign-name
uniqueness, so a retry composes the SAME name and cleanly REUSES the existing campaign
rather than duplicating it — though `AlreadyExisted` stays false unless the ad group and
ad also both pre-existed). A non-nil result accompanied
by an error is a separate UNCONFIRMED partial (claim retained); (nil, err) means nothing
was created (claim released).

It has a creation dispatcher; its status-TOGGLE capability is described next.

## Status toggle

`UpdateCampaignAndChildrenStatus` cascades a status across campaign → ad group → ad → keywords,
ordered by DIRECTION, like reddit's. **Keywords are in the cascade because MS-4 creates them
PAUSED**: an activate that skipped them would enable the campaign, ad group and ad while every
keyword stayed Paused, so the campaign would serve nothing while reporting Active — precisely
the lie the cascade exists to prevent. An EMPTY keyword-id slice skips the PUT rather than
sending an empty one, and every id is validated BEFORE any mutation (a bad id found mid-cascade
would fail after the campaign and ad group had already flipped, turning a rejectable input
error into a partial cascade). Keywords are addressed THROUGH their ad group, so ids supplied
without one are refused, exactly as an orphan ad is.

- **PAUSE gates the parent FIRST** so delivery stops immediately, even if a child call then fails.
  A failure after the campaign flipped is a PARTIAL apply, reported as `Unconfirmed` rather than a
  plain error, because the parent change did land and a blind retry would misread the state.
- **ACTIVATE sends AdGroups, then Ads, then Keywords, then Campaigns last** (descendants before the parent gate) —
  NOT a strict leaf-to-root walk: Ads is deeper than AdGroups in the tree, yet AdGroups PUTs first.
  The campaign is only un-gated once its children are already serving; the reverse would briefly
  serve nothing under a live campaign.
- **Unknown children are SKIPPED, not guessed**, with direction-dependent rules. An ad can only be
  addressed when its parent ad-group id is also known. **ACTIVATE requires both child ids** — if
  either `adGroupId` or `adId` is missing, it is refused locally with `ErrCampaignNotProvisioned`
  before any upstream call, since a missing child would stay paused while the row claimed "active".
  **PAUSE only refuses the orphan-ad case** (an `adId` with no `adGroupId`): the Ads PUT is scoped
  by `AdGroupId`, so the ad cannot be addressed; sending the campaign anyway would report success
  while the ad kept serving. **PAUSE with a missing `adGroupId` also skips the ad group**: only the
  campaign PUT runs — no ad group PUT is sent. In both directions, a persisted value is refused
  rather than sent empty (which would address a different entity entirely). An ad group with no ad
  is the one asymmetric shape that IS allowed: it is addressable via its `CampaignId`.
- **Each child PUT is scoped to its OWN parent** — the ad group to the campaign, the ad to the AD
  GROUP. Passing the campaign id as `AdGroupId` would silently toggle the wrong thing.
- **The campaign must belong to the account the connection resolves to** (LFXV2-3260). Campaign
  ids are unique only within an ad account, so after `UpdateMicrosoftAds` re-points a project's
  connection the stored id can address an unrelated campaign in the NEW account — pausing or
  activating something this project does not own. The dispatcher's `verifyMicrosoftAccountMatch`
  refuses that with `domain.ErrCampaignAccountMismatch` (409) above BOTH branches, before any
  credential resolution or upstream call, and the same helper guards `ReadMetrics`. The account
  the campaign was created under is carried by `CampaignResult.AccountID` (`accountId` in the
  persisted blob), which `namePartial` stamps on every result path — success and partial alike —
  from `c.account.AccountID`. A row recording no account is treated as "unknown, proceed", so
  campaigns created before the field existed still toggle; the `microsoftAdsUrl`'s `aid=`
  parameter is read as a fallback before concluding a row is unrecorded.

Two further details belong to this layer specifically:

- **The status PUT is IDEMPOTENT, so a 429 IS retried.** Re-applying `Active`/`Paused` converges on
  the same state and cannot double-commit a paid resource, unlike the creates — which is exactly why
  retry eligibility is an explicit parameter here rather than derived from the HTTP method. Passing
  it as non-idempotent turned routine throttling into an `Unconfirmed` toggle the dispatcher then had
  to verify before retrying. Matches the sibling reddit status setter.
- **A DECODABLE success body is not an ANSWERED one.** `{"PartialErrors": null}` is Microsoft
  affirming no entity failed; `{}` or a top-level `null` never spoke to the question, yet both
  unmarshal cleanly and leave the field zero. `updateStatusResponse` therefore tracks the field's
  PRESENCE separately and reports absence as unconfirmed — otherwise a proxy error page that happens
  to parse would let the service persist a status Microsoft never confirmed. The valid empty forms
  (`null`, `[]`) are still accepted.

## Metrics read (asynchronous, default-OFF)

`GetCampaignMetrics(ctx, campaignID, window)` answers the same question as every other client
platform clients, but Microsoft is the only one whose reporting is ASYNCHRONOUS. The
pipeline is `POST Reporting/v13/GenerateReport/Submit` (returns a `ReportRequestId`) ->
`POST .../Poll` (`Pending` | `Success` | `Error`, plus a pre-signed download URL) -> `GET`
that URL for a **ZIP containing one CSV**. It is REST/JSON, not SOAP.

The `service.MetricsReader` contract is synchronous, so the submit+poll phase is bounded by
`reportPollBudget` (15s) and gives up with `ErrReportNotReady`. The binding deadline is NOT
the 60s platform ingress but `Orchestrator.ReadCampaignMetrics`'s 20s `metricsCallTimeout`;
the budget must stay under THAT or the caller's context cancels first and the sentinel
becomes dead code. The DOWNLOAD is deliberately outside that budget: once `Success` is
reported the file exists, and cutting off the transfer would discard a report already paid
for. `ErrReportNotReady` is NOT mapped to either metrics sentinel — both mean 400, and a
report still building is retryable, not unsupported.

The budget covers the SUBMIT as well as the polling, and that is a property of where the
deadline is taken: `GetCampaignMetrics` computes it before `submitReport` and threads it into
`pollReport`, which also checks it before its first poll. Taking it inside `pollReport` (the
shape this file described until LFXV2-3260) left submit unbounded, so a slow submit spent the
caller's 20s and produced `context deadline exceeded` rather than the retryable sentinel.
`TestPollBudgetCoversTheSubmitPhase` pins it; the clock it uses advances only across the
submit, because a clock that advances on every reading expires the budget under both
placements and would pass against the defect.

Several parsing properties are load-bearing and each is pinned by a test. The CSV is
**ragged** — a two-column metadata preamble, then the four-column header and data, then a
one-column copyright trailer — so `FieldsPerRecord = -1` is required; the default field-count lock
rejects the whole file at the header row, which would fail every real report. Columns are
resolved by header NAME, never by position, because Microsoft's writer chooses its own order
and a positional read would swap Clicks and Spend into plausible wrong numbers; a missing
metric column is refused rather than defaulted to zero. And the download request carries NO
bearer token: the URL is pre-signed storage, so attaching our OAuth credential would disclose
it to a host that neither needs nor expects it.

The DECOMPRESSED CSV is read into a buffer and size-checked before parsing, not streamed
through a bare `io.LimitReader`: `io.LimitReader` signals EOF at its limit rather than
erroring, so a prefix cut on a row boundary is syntactically complete and `csv.ReadAll`
accepts it — yielding a short total that reads as authoritative. The trailer is identified
POSITIVELY (blank, or a first cell starting with a copyright marker) rather than by being
narrower than the header. **Both `©` and `@` are accepted as that marker**: Microsoft's
published sample report and its `ExcludeReportFooter` description both render the footer with
an `@`, while this repo's fixtures had assumed `©` throughout, so the suite could only prove
the parser agreed with itself. An unrecognised footer is not dropped quietly — it survives the
filter, folds in as a data row and fails `parseReportInt`, turning every otherwise-successful
report into a parse error.

Width is not evidence of a trailer **at any width**, including one column. Dropping short rows
discarded real data whose metrics then vanished into a clean-looking total; a later revision
narrowed that rule to single-cell rows only, which is the same defect — a data row truncated to
its `CampaignId` has exactly that shape. Every non-blank, non-marker row therefore reaches the
column check and is REPORTED as an error rather than silently dropped. The running TOTALS are
overflow-checked as well as the per-row values, mirroring
`reddit/metrics.go` — per-row guards bound each value but say nothing about the sum the
dashboard renders.

Three properties of the SUBMIT request are load-bearing in the same way. The report is scoped
by `Scope.Campaigns` ALONE — `Scope.AccountIds` is not sent alongside it, because Microsoft
documents the scope as a union and sending both would return account-wide totals reported as
this campaign's. If Microsoft rejects the campaign-only scope (error 2027 /
`InvalidAccountThruCampaignReportScope`) the error names that tradeoff explicitly, so the first
live run diagnoses it in one read. Scope ids go out as **quoted strings**, not bare JSON
numbers — the opposite of `campaign.go`, deliberately: that is Campaign Management v13 and this
is Reporting v13. Reporting's JSON reference quotes every `long` while leaving `ReportTime`'s
`int` fields bare, and quoting also avoids the precision loss a 64-bit id would suffer past
2^53, which would scope the report to the WRONG campaign.

`ReportTimeZone` is sent explicitly because Microsoft otherwise defaults it to Pacific, which
would aggregate a different day than the UTC-computed dates the request names — a silent
off-by-one-day on every window. The value used,
`GreenwichMeanTimeDublinEdinburghLisbonLondon`, is the CLOSEST the enum offers to UTC but is
**not** UTC: it maps to `Europe/London` and observes British Summer Time, so from late March to
late October the report day boundary sits one hour before ours. No fixed-offset UTC value
exists to use instead — the published `ReportTimeZone` value set has 75 entries and contains no
UTC or Reykjavik member, and the only other UTC+0 entry (`CasablancaMonrovia`) maps to
`Africa/Casablanca`, which observes its own offset changes. The residual error is bounded at
one hour at the two ends of the window and can move an event between adjacent days; it cannot
lose one, since the range is aggregated as a whole.

There is no path in this file where a zero is synthesized. Both empty shapes — a `Success`
status naming no download URL, and a downloaded CSV whose header is followed by no data rows
— answer `ErrNoRowsInReport`, which the dispatcher maps onto `domain.ErrNoMetricsInWindow`.
The adapter cannot tell "the campaign served nothing" from "no such campaign in this
account's scope", and the two shapes carry identically little information, so neither is
rendered as a measured zero.

A report Microsoft flags as partial is refused too. `ReturnOnlyCompleteData` is sent `false`
so a window including today can build at all, which means the totals may be an under-count;
Microsoft signals that in the CSV preamble rather than the HTTP status. The parser reads
that flag and answers `ErrReportDataIncomplete` instead of returning the numbers, because
`model.CampaignMetrics` has no field for "provisional" and a partial total is
indistinguishable from a complete measurement of a smaller number.

Reads are gated behind `MICROSOFT_METRICS_ENABLED` (chart default `"false"`), mirroring
`REDDIT_METRICS_ENABLED`: the v13 Reporting contract was implemented from published
documentation and has not been exercised against a live Microsoft Advertising account, and a
guessed read returning 200 looks authoritative to every consumer.
