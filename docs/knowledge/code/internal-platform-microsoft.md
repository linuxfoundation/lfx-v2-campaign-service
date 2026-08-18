---
type: "Go Package"
title: "internal/platform/microsoft"
description: "Microsoft Advertising (Bing Ads) Campaign Management REST v13 client: OAuth2 refresh-token + developer-token auth, request layer with 429 retry and status-aware error classification incl. BatchErrors (MS-1), and PAUSED find-or-create Campaign->AdGroup->ResponsiveSearchAd creation over the POST /<Entity> + POST /<Entity>/QueryBy… transport, idempotent by case-insensitive-unique NAME for the campaign and ad group but by DESTINATION URL for the ad (ads have no stable name and v13 permits duplicate RSAs) (MS-2/MS-2.5), plus ad-account discovery against the SEPARATE Customer Management v13 service on a different host, the one call that is not account-scoped (LFXV2-3064)."
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
(`internal/dispatch/microsoft.go`). The **status toggle** (LFXV2-2810) adds
`UpdateCampaignAndChildrenStatus` on top: a three-level cascade whose ordering, child-id guard and
outcome classification are described under Status toggle below. **Ad-account discovery**
(LFXV2-3064) adds `ListAdAccounts` against the separate Customer Management service, the
one call in this package that is NOT account-scoped.

## Dispatch adapter (internal/dispatch)

The `internal/dispatch` microsoft adapter (see [internal/dispatch](internal-dispatch.md))
interprets an OAuth2 app (clientId/secret) + a developer token + refreshToken;
AccountConfig comes from the connection's AccountID (the DIGITS-ONLY
`CustomerAccountId`, trimmed) plus an optional `customer_id` (the manager/`CustomerId`
header). The client builds the full Campaign → AdGroup → Ad hierarchy (all PAUSED) — so
the adapter needs no ad config beyond `microsoftConfig.budget` (the DAILY budget, in the
ACCOUNT's currency, no FX) and an optional `timeZone`. `NameSuffix = brief.ID` gives
deterministic retry-safe names (Microsoft enforces case-insensitive campaign-name
uniqueness, so a retry composes the SAME name and cleanly REUSES the existing campaign
rather than duplicating it — though `AlreadyExisted` stays false unless the ad group and
ad also both pre-existed). A non-nil result accompanied
by an error is a separate UNCONFIRMED partial (claim retained); (nil, err) means nothing
was created (claim released).

It has a creation dispatcher; its status-TOGGLE capability is described next.

## Status toggle

`UpdateCampaignAndChildrenStatus` cascades a status across campaign → ad group → ad, ordered by
DIRECTION, like reddit's:

- **PAUSE gates the parent FIRST** so delivery stops immediately, even if a child call then fails.
  A failure after the campaign flipped is a PARTIAL apply, reported as `Unconfirmed` rather than a
  plain error, because the parent change did land and a blind retry would misread the state.
- **ACTIVATE sends AdGroups, then Ads, then Campaigns last** (children before the parent gate) —
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

The `service.MetricsReader` contract is synchronous and the platform ingress times out at
60s, so the submit+poll phase is bounded by `reportPollBudget` (20s) and gives up with
`ErrReportNotReady`. The DOWNLOAD is deliberately outside that budget: once `Success` is
reported the file exists, and cutting off the transfer would discard a report already paid
for. `ErrReportNotReady` is NOT mapped to either metrics sentinel — both mean 400, and a
report still building is retryable, not unsupported.

Three properties are load-bearing and each is pinned by a test. The CSV is **ragged** —
a two-column metadata preamble, then the four-column header and data, then a one-column `©`
trailer — so `FieldsPerRecord = -1` is required; the default field-count lock rejects the
whole file at the header row, which would fail every real report. Columns are resolved by
header NAME, never by position, because Microsoft's writer chooses its own order and a
positional read would swap Clicks and Spend into plausible wrong numbers; a missing metric
column is refused rather than defaulted to zero. And the download request carries NO bearer
token: the URL is pre-signed storage, so attaching our OAuth credential would disclose it to
a host that neither needs nor expects it.

The one path where zeroes are truthful is a `Success` status with no download URL —
Microsoft's "report built, no rows". Every other empty outcome in this file is an error.

Reads are gated behind `MICROSOFT_METRICS_ENABLED` (chart default `"false"`), mirroring
`REDDIT_METRICS_ENABLED`: the v13 Reporting contract was implemented from published
documentation and has not been exercised against a live Microsoft Advertising account, and a
guessed read returning 200 looks authoritative to every consumer.
