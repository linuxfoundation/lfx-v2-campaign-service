---
type: "Go Package"
title: "internal/platform/googleads"
description: "Google Ads API REST client: OAuth2 refresh-token auth, request layer, GAQL search (GA-1), PAUSED campaign creation (GA-2), ad group + responsive search ad creation (GA-3), and keyword/audience-segment targeting on that ad group (GA-4)."
resource: "internal/platform/googleads"
tags:
  - platform-client
  - google-ads
  - oauth2
  - gaql
  - go-package
timestamp: "2026-07-18T00:00:00Z"
---

# internal/platform/googleads

Package googleads provides a Go client for the Google Ads API, ported from the
upstream TypeScript `google-ads-api` gRPC usage (`campaign-proxy.service.ts` /
`campaign-metrics.service.ts`) to a **REST** client that speaks the Google Ads
REST transport directly (`googleAds:search` / `:mutate`). REST — rather than the
official gRPC SDK — is deliberate so the client matches the meta/reddit/twitter/
linkedin clients' structure and avoids a large generated gRPC dependency.
Credentials and account configuration are injected via `NewClient`; the client
never reads the process environment.

## Auth

Google Ads auth is richer than a single Bearer token. `Credentials` carries the
OAuth2 `client_id`/`client_secret` + `developer_token` + `refresh_token`;
`AccountConfig` carries the `customer_id` (digits only) and an optional
`login_customer_id` (manager/MCC access). Every call sends the bearer access
token, the `developer-token` header, and — when set — the `login-customer-id`
header. The refresh-token→access-token exchange is coalesced with a single-flight
leader/follower (the token mutex is never held across the network call, and the
refresh runs on a `WithoutCancel`-detached context so one caller's cancellation
can't tear down a shared refresh). The OAuth response body is never echoed into
errors (it can carry the `client_secret`/`refresh_token` back).

## Request layer

`doRequest` applies the repo's standard discipline: no-follow redirects
(unconditional, on a shallow copy so a supplied client isn't mutated), bounded
response reads (`maxResponseBytes+1`), and the pre-send (`isPreSendDialError`) vs
ambiguous (`transportError`) vs definite (`apiError`, status-only `Error()`)
classification. A rate-limited (429) **idempotent** call is retried up to
`retryMax` times with a bounded backoff honoring `Retry-After`; retry eligibility
is an explicit parameter (not the HTTP method) because GAQL `:search` is
POST-but-read-only — a `:mutate` create must NOT be retried (double-create risk).
`customer_id`/`login_customer_id` are validated as digits-only before any request.

## GAQL search

`gaqlSearch` runs a cursor-paginated GAQL query (POST) with a repeated-token guard,
a page cap, and BOTH an aggregate-row cap and an aggregate-byte cap so a many-page
result can't OOM the service. The byte cap counts each page's FULL raw payload
(not just result rows), so it also bounds the retained nextPageToken strings — a
row cap alone doesn't bound memory, so the byte cap is the real memory guard.

**GAQL gotcha:** in API v23, `campaign.start_date` / `campaign.end_date` were
replaced by `campaign.start_date_time` / `campaign.end_date_time` — the old fields
are rejected as unrecognized.

## Campaign creation (GA-2)

`CreateCampaign` (in `campaign.go`) creates a PAUSED search campaign as two
sequential `:mutate` calls: `campaignBudgets:mutate` (a non-shared `STANDARD`
budget, `amountMicros` = budget × 1,000,000) then `campaigns:mutate` (status
`PAUSED`, `advertisingChannelType` `SEARCH`, referencing the budget's
`resourceName`, with a dependency-free `manualCpc {}` bidding strategy — a broker
can't assume conversion tracking, which `maximizeConversions` requires). The
`CampaignInput.Budget` field is denominated in the ad ACCOUNT's currency, NOT USD —
Google interprets the resulting `amountMicros` in the account's own currency and
the client does no FX conversion, so a value of 50 is 50 of whatever the account is
denominated in. (The field was renamed from `BudgetUSD`, which implied a conversion
this client does not perform; mirrors the meta client's `Budget`.) The
campaign create also sets `containsEuPoliticalAdvertising:
DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` — v23 REQUIRES this on every create
(omitting it fails with `FieldError.REQUIRED`, and since 2026-04-01 an account with
any undeclared campaign has ALL mutate calls rejected). It also sets
`networkSettings` with `targetGoogleSearch: true` (search/content networks
explicitly false): a SEARCH campaign that targets NO network — which is what an
omitted `networkSettings` resolves to (proto3 bools default false) — is rejected with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK` AFTER the budget mutate has
committed, an avoidable orphan. Google Search only is the conservative choice for a
PAUSED broker shell; `targetSearchNetwork` stays false because true would opt into
Search Partners (and requires `targetGoogleSearch`), which a generic broker shouldn't
assume. Both resource ids are
surfaced (`campaignBudgetId` + `campaignId`) via `firstResourceName`, which decodes
`results[0].resourceName` and returns both the resource name and its trailing-id
segment. It errors when the body is malformed, carries no result/resourceName, OR
the resourceName is present but MALFORMED (e.g. `customers/1/campaigns/` or
`noslash`) such that no id can be extracted — accepting a present-but-malformed
name would let creation continue with an empty, unreconcilable id (or report
success with a blank id), so it is treated as UNCONFIRMED like the no-resourceName
case. Between the two calls, if the
caller's context is already done, the campaign `:mutate` is skipped and the created
budget is returned as a reconcilable partial rather than fired on a dead context.

Input is validated up front, before any paid `:mutate` call: the budget must be
finite (NaN/Inf rejected — NaN passes every ordered comparison, so it would
otherwise slip through and create a zero-unit budget) and must round to a positive
`amountMicros` (a sub-micro value like 0.0000001 is > 0 but converts to 0 micros);
and BOTH Project AND EventName must be non-empty (independently — mirrors the
meta/twitter/reddit clients). `CampaignInput.EventSlug` (plus the newer
`RegistrationURL`/`Headlines`/`Descriptions` fields) feeds the GA-3 ad-group/ad
step below — the campaign `:mutate` itself still ignores them. Project is the canonical attribution key the data
pipeline parses out of the campaign name, so a campaign with only one segment is
mis-attributed, not just "less descriptive". Caller-supplied name segments are
sanitized (`sanitizeNamePart`) to strip the `|` delimiter AND any control character
(incl. NUL — v23 forbids NUL/LF/CR in `Campaign.name`; `strings.Fields` only folds
whitespace control chars, so NUL is mapped to a space explicitly) before composing,
so a raw `|` can't inject extra pipe-fields (breaking name-based
attribution/reconciliation that splits on `|`) and a control char can't reach a paid
`:mutate` as a guaranteed-invalid name.

The composed name is length-validated against **per-entity** limits IN THEIR OWN
UNITS, not a single shared number (verified against the v23 System Limits table +
RPC field references): `CampaignBudget.name` is 1..255 UTF-8 BYTES (`len`), and
`Campaign.name` is up to 256 CHARACTERS (`utf8.RuneCountInString`,
`StringLengthError.TOO_LONG`). The unit difference is load-bearing — a multibyte name
hits the budget's byte ceiling sooner than 256 characters — so `validateEntityName`
is told the measured length and unit per name. Both are validated before either
`:mutate`; the budget name is composed+checked first, so its 255-byte cap is the
binding preflight guard for an ASCII name, and an oversized name never wastes a paid
call or orphans a budget.

Because `:mutate` has NO idempotency key, a blind retry double-creates. So every
create outcome is classified: an ambiguous failure (a mutating 3xx/5xx `apiError`,
a mutating **429** — `doRequest` deliberately does NOT retry a non-idempotent 429
because the throttled request may already have committed — a `transportError`, or a
2xx with no `resourceName`) is reported UNCONFIRMED (verify before retrying) with a
partial result carrying whatever exists so an orphan is reconcilable. The partial
carries BOTH deterministic names (`CampaignName` and `CampaignBudgetName`) alongside
any known ids. Before attachment the two names DIFFER (`LFX | Budget | …` vs `LFX |
Search Campaign | …`), so a caller reconciling a possibly-orphaned BUDGET **at the
budget stage** (no id yet) uses `CampaignBudgetName`. IMPORTANT: once the campaign
attaches, a non-shared (`explicitlyShared=false`) budget's name SYNCHRONIZES to the
campaign name — so at a CAMPAIGN-stage ambiguous failure the budget's current name is
unknown (it may already be `campaignName`). That is exactly why the budget-stage
partial also carries `CampaignBudgetID`: past attachment, reconcile the budget by ID,
not by its original name. A definite 4xx means only THAT `:mutate` was rejected — for a
campaign-create 4xx the budget from the first mutate still exists, so the partial
carries `campaignBudgetId` for reconcile/cleanup (it is NOT "nothing was created"
overall). Before the FIRST mutate, an already-cancelled context is caught explicitly
(`ctx.Err()`) and returns a clean `(nil, err)` — otherwise, with a cached OAuth token
the cancellation would only surface inside `httpClient.Do` as a `transportError` and
be mis-reported as UNCONFIRMED, implying a budget might exist when nothing was sent.
`createOutcomeAmbiguous` (5xx/429/transport always; 3xx only on a mutating method),
`isDuplicateBudgetNameErr` (a 4xx `CampaignBudgetError.DUPLICATE_NAME`), and
`isDuplicateCampaignNameErr` (a 4xx `CampaignError.DUPLICATE_CAMPAIGN_NAME` — a
DIFFERENT code from the budget's) drive this: a retry with a stable `NameSuffix`
that collides on the family-appropriate duplicate code is surfaced as
already-exists (reconcile by name). Error codes are parsed from
`error.details[GoogleAdsFailure].errors[].errorCode` (a single-key category→enum
object) from the FULL error body in `doRequest` and retained on
`apiError.ErrorCodes` — NOT re-parsed from the truncated `apiError.Body` (a real
Google error JSON exceeds `maxErrorBodyChars`, so parsing the truncated snapshot
would silently drop the codes). Codes are length/count bounded; the raw body is
never surfaced by `apiError.Error()`. Composed names are deterministic (`LFX |
<kind> | Project | Event | NameSuffix`) so a retry with the same `NameSuffix`
collides on the duplicate code rather than double-creating. NOTE: a non-shared
budget's name syncs to the campaign name once attached; Google does NOT document
that the original budget name is then freed for reuse, so for at-most-once retries
callers should pass a stable-per-logical-campaign `NameSuffix` (which makes the
retry collide on `DUPLICATE_NAME`) rather than relying on name reuse.

## Responsive Search Ad copy + final URL (GA-3a)

`ad_copy.go` holds the pure, side-effect-free helpers GA-3's ad-group/ad
creation cascade consumes: `composeAdCopy` resolves caller-supplied
headlines/descriptions into a valid RSA content set (trimmed, weight-capped —
CJK/full-width runes count double, see `truncateWeighted` — deduped, padded
with deterministic `eventName`/`project`-derived placeholders
up to the minimum, capped at the maximum — never silently dropping down to
zero, but empties/duplicates ARE dropped and the count IS capped, so a caller
should not assume its exact input list survives verbatim), and
`buildAdFinalURL` builds the ad's destination URL from the brief's
registration URL, UTM-tagging it without overwriting a `utm_*` key the
registration URL already carries.

`buildAdFinalURL` rejects a registration URL that: fails to parse; uses a
scheme other than http/https; has no host; carries embedded userinfo
(`user[:password]@host` — an ad destination never needs URL credentials, and
forwarding them downstream would leak a basic-auth secret); or has a
malformed query string (an unparsable percent-escape in `RawQuery` is
silently dropped by `u.Query()`, which would otherwise alter the actual
destination the ad points to). Every validation error redacts the raw
registration URL via `redactURLForError` (scheme+host+path only) before
including it in the message — the URL may carry a token in its query string
or userinfo, and this error can be logged or persisted in a result
step/snapshot. Mirrors the twitter client's `redactURLForError` and the
reddit/meta clients' equivalent `redactURL` (userinfo/credentials-in-caller-
URL pattern, see `docs/reviews/knowledge-base/credentials-and-untrusted-text.md`).

## Ad group + responsive search ad creation (GA-3b)

`CreateCampaign` extends the GA-2 campaign shell with a real
Campaign→AdGroup→Ad hierarchy, in `adgroup_ad.go`: after the campaign
`:mutate` commits, `createAdGroupAndAd` sends `adGroups:mutate` (a single
`SEARCH_STANDARD` ad group referencing the campaign's resourceName, status
`PAUSED`) then `adGroupAds:mutate` (a `PAUSED` Responsive Search Ad on that
ad group). After the ad is created, if the caller supplied any
`Keywords`/`AudienceSegments`, `createAdGroupTargeting` (GA-4, below) attaches
them to the same ad group before `CreateCampaign` returns — without it, the ad
group carries no targeting and the campaign can never serve, even once
enabled. Both the ad-group/ad steps run unconditionally after every
successful campaign create; there is no way to create a campaign shell with
no children through this path today.

**All ad-group/ad input validation runs BEFORE the first (budget) mutate.**
`CreateCampaign` calls `precomputeAdGroupAdInputs` — which validates the
destination URL (`buildAdFinalURL`), ad copy (`composeAdCopy`), and ad-group
name (`validateEntityName`) with no network calls — immediately after the
budget/campaign name validation and before `campaignBudgets:mutate` is sent.
`createAdGroupAndAd` then takes the precomputed `finalURL`/`headlines`/
`descriptions`/`adGroupName` as arguments and performs no local validation of
its own. This ordering matters: `createAdGroupAndAd` runs LAST, after the
budget and campaign already committed, so validating its inputs only at that
point would let a bad `RegistrationURL` or an over-length ad-group name orphan
a real, already-created budget+campaign for what is purely a local input
error — every other precondition in this file (budget/campaign name length,
attribution fields) is likewise checked before its first upstream call for
the same reason.

**Idempotency** mirrors the budget/campaign convention (create-then-catch,
not Microsoft's find-by-name-first): the ad group has a deterministic
composed name (`composeName("Ad Group", in)`), so a retry with the same
`NameSuffix` collides on Google's `AdGroupError.DUPLICATE_ADGROUP_NAME` and is
reported "already exists, reconcile by name" (`isDuplicateAdGroupNameErr`) —
exactly like the budget/campaign duplicate branches. Unlike those, a
duplicate/ambiguous ad-group outcome is NOT looked up by name to recover its
id, so a retry after that outcome cannot re-attempt the ad create either (no
ad-group id to attach it to) — a documented, intentional limitation requiring
manual reconciliation, consistent with the existing budget/campaign-duplicate
precedent. The ad itself has no unique name (Google permits duplicate ad
content within an ad group) and gets no find-first safety net: a duplicate or
ambiguous ad-group-create always bails out BEFORE the ad-create step runs, so
within a single `CreateCampaign` call — or across retries, which re-hit the
same ad-group duplicate check and bail at the same point — the ad `:mutate`
can never be attempted twice against one ad group.

**AdGroupAd resource names are a composite key**: unlike every other Google
Ads resource (a single trailing numeric id split by `resourceID`), an
AdGroupAd's resourceName trailing segment is `{adGroupId}~{adId}` (tilde-
separated, e.g. `customers/1/adGroupAds/111~222`). `adGroupAdID` splits on
`~` after `resourceID`'s slash-split and requires EXACTLY two components, both
a non-empty run of ASCII digits (`numericID`) — a third tilde-separated
component or a non-numeric half is rejected as malformed (returns `("", "")`)
rather than silently accepted, since the extra/non-numeric text would
otherwise be carried into `res.AdGroupID`/`AdID` and later interpolated into a
`resourceName` path by `UpdateAdGroupAndAdStatus`. `createAdGroupAndAd` also
verifies the returned ad-group-id HALF of the adGroupAd resourceName matches
the ad-group id the ad was actually created under — a mismatch means the
response doesn't describe the ad this call just created, so it is reported
UNCONFIRMED rather than persisted. This composite shape is the single
highest-risk unverified assumption in this slice (no live fixture in-repo to
confirm it against) — if wrong, both `UpdateAdGroupAndAdStatus`'s resourceName
construction and `firstResourceName`'s extraction on ad create would silently
misbehave.

**Ad copy** (`composeAdCopy`) accepts caller-supplied `Headlines`/
`Descriptions`, trims/rune-truncates/dedupes them (`boundedUniqueCopy`), then
pads with deterministic eventName/project-derived placeholders
(`defaultHeadlines`/`defaultDescriptions` via `padUnique`) up to Google's v23
RSA minimums (3 headlines ≤30 runes, 2 descriptions ≤90 runes). Caller-supplied
entries are accepted up to the maximum (15 headlines, 4 descriptions), with
later entries beyond those limits silently dropped. Unlike the Microsoft client,
Google's limits are plain rune counts — there is NO double-width-character
halving rule here. A caller with zero usable copy AND an empty EventName is a
hard error (there is nothing to advertise).

**The ad's final URL** (`buildAdFinalURL`) is the brief's `RegistrationURL`
tagged with `utm_source=google`, `utm_medium=cpc`, `utm_campaign` (from
EventSlug, falling back to EventName then NameSuffix), and `utm_content`
(from Project) — any query param the registration URL already carries,
INCLUDING a pre-existing `utm_*` key, is preserved rather than overwritten
(`setIfAbsent`). An empty/non-http(s)/no-host registration URL is rejected
before any mutate runs, so a bad input never orphans an ad group with no ad.

`Client.UpdateAdGroupAndAdStatus` sends the ad group status update then the ad
status update, stopping on the first failure without attempting the second —
the caller owns the all-or-nothing cascade semantics, not this method.

## Status toggling (GA-3c)

`GoogleAdsDispatcher.ToggleStatus` (`internal/dispatch/googleads.go`) cascades
the campaign-level PAUSE/ACTIVATE that GA-2 introduced down to the ad group +
ad GA-3b creates, mirroring the reddit adapter's child-cascade contract:

- **PAUSE flips the campaign first**, then the ad group/ad. Pausing the
  parent stops delivery immediately regardless of whether the child update
  that follows succeeds — if that child update fails or is UNCONFIRMED, the
  error still surfaces to the caller (the campaign is left paused, the child
  status is unresolved) rather than being swallowed. If either child id is
  absent (e.g. a campaign shell with no fully-created ad group/ad — see
  GA-3b's duplicate/ambiguous-outcome limitations), there is nothing to pause
  downstream and only the campaign is toggled.
- **ACTIVATE flips the children first, campaign last**, so a campaign never
  reports ENABLED before its ad group/ad already do — the reverse order could
  leave the campaign live for a moment with paused children. ACTIVATE is
  refused up front (`domain.ErrCampaignNotProvisioned`, mapped to a 409
  without calling Google) when either child id is unknown, since enabling
  just the campaign in that state would report success while nothing can
  serve.

`googleAdsChildIDs` recovers the ad-group/ad ids from the campaign's
persisted `Result` blob (the JSON GA-3b's create path stores) — a
missing/unparseable blob yields empty ids, which is what drives the "nothing
to cascade to" branches above.

## Keyword + audience targeting (GA-4)

`createAdGroupTargeting` (in `targeting.go`) attaches positive Search
keywords and/or existing audience segments to the ad group `createAdGroupAndAd`
just built, as a SINGLE `adGroupCriteria:mutate` call carrying one operation
per criterion — batched (not one call per criterion) so the whole set shares
one atomic outcome, the same "no partial state" reasoning as every other
mutate here, just extended to N operations. Input is validated up front
(`validateKeywords`/`validateAudienceSegments`), before any mutate call in
`createAdGroupAndAd` runs, alongside the existing URL/copy/name checks — a bad
keyword or audience value must not leave a created ad group + ad with a
failed targeting step the caller has to puzzle out separately.

**Keywords** (`Keyword{Text, MatchType}`) are positive Search criteria only;
`MatchType` is one of `EXACT`/`PHRASE`/`BROAD`. `Text` is capped at v23's
80-rune `KeywordInfo.text` limit. Up to 20 keywords per call (a sanity cap on
this broker's input, not a Google Ads platform limit); duplicates (same
matchType+text) are deduped rather than rejected.

**Audience segments** are EXISTING Google Ads audience resource names — this
client does not create audiences. `audienceCriterionField` infers the mutate
oneof field from the resource-name's collection segment: `.../userLists/{id}`
(Customer Match / remarketing list) maps to `userList`, `.../customAudiences/{id}`
maps to `customAudience`; any other shape (`userInterest`, `combinedAudience`,
`detailedDemographic`, …) is rejected — deliberately narrow scope matching what
"a built campaign audience" (the `campaign_audiences` resource,
`docs/api-catalog.md`) represents. Also capped at 20 per call, deduped by
resource name.

**Observation vs targeting — the audience-restriction gotcha**: a Search ad
group's audience criteria default to TARGETING (restrictive — narrows
delivery to ONLY that audience) unless a `targetingSetting.targetRestrictions`
with `targetingDimension: "AUDIENCE", bidOnly: true` is declared at the SAME
level the criteria are attached. GA-4's audience criteria are created as
`AdGroupCriterion`s, so this must be set on the AD GROUP `:mutate` create, not
the campaign create — Google requires `targetingSetting` live at the same
level as the criterion, and even rejects setting it on an `AdGroup` while the
parent `Campaign` has one (per Google's `UpdateAudienceTargetRestriction`
sample, which reads/writes `ad_group.targeting_setting`, not
`campaign.targeting_setting`). `createAdGroupAndAd` (`adgroup_ad.go`) sets it
on the ad group create WHENEVER the validated `audienceSegments` list is
non-empty (and omits it entirely otherwise) — so an audience segment added for
bid/reporting purposes doesn't silently narrow delivery to that segment alone.
(An earlier version of this code set it on the campaign create instead, which
Google's docs confirm has no effect on ad-group-level criteria.)

Every criterion is created `ENABLED` (not `PAUSED` like the ad group/ad
shell): a criterion's own status is one more gate on top of its ancestors (ad
group, ad, campaign) already being enabled — Google won't serve it while any
ancestor is PAUSED, so creating it ENABLED now means the campaign is
immediately serve-ready the moment a human flips the ad group/ad/campaign to
ENABLED, with no separate targeting-activation step.

**AdGroupCriterion resource names share AdGroupAd's composite shape**
(`{adGroupId}~{criterionId}`) — `compositeResourceID` (factored out of GA-3's
`adGroupAdID` in `adgroup_ad.go`) is reused here to extract the criterion id.

**Duplicate-criterion classification is unverified** for this resource
(unlike the budget/campaign/ad-group `DUPLICATE_NAME` family): any 4xx on
`adGroupCriteria:mutate` — including a possible duplicate on retry — is
reported as a straightforward failure, not reconciled by a duplicate
predicate. An ambiguous outcome (5xx/429/transport, or a 2xx with a
malformed/short mutate response) is reported UNCONFIRMED, same convention as
GA-2/GA-3.

`CampaignResult.KeywordCriteriaIDs`/`AudienceCriteriaIDs` are populated from
the mutate response only when targeting was attempted and succeeded; both are
empty if targeting was never attempted (no `Keywords`/`AudienceSegments`
supplied) or failed before any criterion resource name could be parsed. The
per-platform dispatcher config (`internal/dispatch/googleads.go`,
`googleAdsConfig.Keywords`/`.AudienceSegments`) maps the wire JSON shape 1:1
into `CampaignInput.Keywords`/`.AudienceSegments`.

## Scope

GA-1 is the scaffold (auth + request layer + GAQL search); GA-2 is campaign
creation (`:mutate`); GA-3a is ad-copy generation and final-URL building
(`ad_copy.go`, this file's previous section); GA-3b is the ad-group/ad
creation cascade that consumes it; GA-3c is the dispatcher-level
status-toggle cascade over that ad group/ad; GA-4 is keyword/audience-segment
targeting on that same ad group. The orchestrator dispatcher (registering
`google-ads` so briefs dispatch upstream) is wired in
`internal/dispatch/googleads.go` (LFXV2-2636). Metrics reads and keyword
actions (pause/adjust an individual keyword post-creation) follow in later GA
slices.
