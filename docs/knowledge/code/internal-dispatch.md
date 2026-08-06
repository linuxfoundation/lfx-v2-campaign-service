---
type: "Go Package"
title: "internal/dispatch"
description: "Per-platform PlatformDispatcher adapters bridging the orchestrator to the channel API clients (six paid ad platforms plus the hubspot email channel), plus the HubSpot audience builder."
resource: "internal/dispatch"
---

# internal/dispatch

Package dispatch holds the per-platform `PlatformDispatcher` adapters that connect the
orchestrator (`internal/service`) to the ad-platform API clients
(`internal/platform/*`). The orchestrator is agnostic to the platforms — it calls
`Dispatch(ctx, brief, provider, config)` on a registered adapter and expects back a
`*model.Campaign` with `PlatformCampaignID`/`Status`/`Result` populated. This package
is the only place that knows both the orchestrator's contract and the concrete
clients, which is why it lives outside `service` (keeping `service` free of platform
imports) and outside each `platform/*` package (avoiding an import cycle).

## Also here: the audience builder

`AudienceBuilder` (`audience_builder.go`) is NOT a `PlatformDispatcher` — it implements
`service.AudienceBuilder` for the audience build (LFXV2-2774). It lives in this package for the
same reason the adapters do: HubSpot credentials are stored per project as encrypted
connections, so it needs the same `credsSource` resolution, and putting it in `service` would
drag platform clients back into the orchestrator's package.

It resolves its HubSpot client once per BUILD, cached on a context scope created by
`BeginBuild` — not on the builder. The distinction matters in both directions: one build creates
several lists which must all land in the same portal (or the master references ids that do not
exist together), but the builder is a container SINGLETON, so caching on it would pin a
credential for the life of the process and keep using a connection that had since been rotated,
revoked or deleted.

## What each adapter does

1. **Resolve credentials** (shared) — `credsSource.resolve(projectID, provider)` does
   the ONE mechanical step common to every platform: `ConnectionReader.Get` then
   `Encryptor.Decrypt`, returning the raw plaintext blob plus the connection's
   non-secret fields (`AccountID`, `ProviderConfig`, `Status`). It does NOT interpret
   the plaintext — credential shapes differ per platform (OAuth2 refresh tokens,
   OAuth1 4-tuples, static bearer tokens), so each adapter unmarshals the blob into
   its own credential struct.
2. **Map inputs** (per-platform) — the adapter reads the brief's event destination
   from its top-level `URL` field (with a nested `registrationUrl` in the opaque JSON
   only as a fallback) and `eventName` from the opaque JSON blobs, plus the
   per-platform config (its OWN nested key — `redditConfig`/`linkedInConfig`/… — out
   of the single `CreateCampaigns` `Input.Config` envelope, via
   `unmarshalPlatformConfig`) onto the client's `CampaignInput`. The **Project** name
   segment is stamped from the authenticated `brief.ProjectID`, NOT from caller JSON
   (it's the data pipeline's attribution join key — see docs/api-catalog.md).
3. **Call the client** and map the result → `model.Campaign` (upstream id, name, the
   provider result blob in `Result`, and a status, since the orchestrator does not set
   a status on success and `UpsertCampaign` writes it verbatim). The success status is
   `created` normally, or **`created_degraded`** when the campaign was created upstream
   but a non-fatal sub-step is incomplete — a promoted-post/ad that failed or is
   unconfirmed, or fewer ads created than requested. The adapter returns a NIL error in
   the degraded case (the paid campaign exists, so failing the job would mislead and be
   unrecoverable by retry — idempotency short-circuits a re-dispatch), and instead makes
   the shortfall VISIBLE via the distinct status (details are in `Result`/Steps) for a
   human or monitor to reconcile. The orchestrator fills project/brief/job/platform
   (and, for a retained ambiguous orphan, a `pending` status).

## The claim contract (release vs retain)

The claim is PERMANENT until released — deliberately NOT auto-reclaimed on a timer. `pending`
is overloaded: it marks both a claim merely in flight AND an AMBIGUOUS dispatch outcome, which
the orchestrator persists as `pending` precisely because the provider MAY already have created
a paid campaign. No column distinguishes the two, so a time-based reclaim would eventually
authorize a DUPLICATE paid create against a campaign that already exists upstream — the exact
failure the claim exists to prevent. Safe automatic recovery needs provider idempotency keys or
an authoritative reconcile first (LFXV2-2665).

The cost is that a pod crashing between claim and release strands a `pending` row that blocks
every future dispatch for that pair, recoverable only by a human. `StuckDispatchClaims` makes
those rows VISIBLE (read-only, `stuckClaimReportAge` = 4m, bounded by `providerCallTimeout` so a
healthy in-flight claim is never reported) instead of leaving them silently invisible until
someone notices a campaign will not dispatch.

**Every stuck claim requires an upstream-platform check before deletion — including a bare
`version = 1` row with no platform id and no result blob.** That shape is NOT evidence the
provider was never called: `dispatchOne` retains the claim WITHOUT upserting when a dispatcher
returns `(nil, nil)`, when it returns an empty upstream id, and when it returns a non-pre-create
`(nil, err)`. In each case a paid campaign may already exist upstream while the row remains
byte-for-byte identical to an abandoned pre-create claim. Confirming that no worker is running
is therefore NOT sufficient to delete: the schema cannot distinguish the two, so the only safe
floor is to check the platform. The `remediation` field on each logged claim states this, and
`version`/`platform_campaign_id`/`has_result` only sharpen WHY the check is owed, never waive it.

The orchestrator single-flight-claims a `(brief, platform)` pair before dispatch and
decides, from the returned error, whether to RELEASE the claim (retry-safe) or RETAIN
it (a blind retry could double-create). Adapters drive that decision:

- A failure that happened BEFORE any upstream create — missing/invalid/undecryptable
  connection, config/validation errors, incomplete credentials, or a client `(nil,
  err)` — is wrapped as a `preCreateError` (via `notCreated`), which implements
  `NoUpstreamCreate() bool`. The orchestrator detects it with `errors.As` and RELEASES
  the claim.
- Any NON-NIL client result returned alongside an error means something may have
  landed upstream, so the adapter hands the campaign back with the error and the
  orchestrator RETAINS the claim. The decision keys on `result == nil` ALONE — NOT on
  whether the campaign id is populated: an ambiguous first-create (or a 2xx with no
  id) returns a non-nil, name-only partial whose `PlatformCampaignID` is EMPTY, and
  that still must retain the claim (LinkedIn even returns a non-nil result carrying a
  `CampaignGroupID` on a definite campaign failure, because the group is permanent).
  The retained row is recorded as a recoverable orphan; its upstream id may be empty
  until reconciled.

## Registration

Adapters are registered in `internal/container` (`registerDispatchers`), called from
BOTH the fast path and the cold-start retry path so the set is identical regardless
of how the DB comes up. A provider without a registered adapter records jobs that
report "no dispatcher registered" (logged as a startup warning via
`logMissingDispatchers`); adapters land incrementally per platform.

Registered so far (`registerDispatchers`): **reddit**, **linkedin**, **meta**,
**twitter** (the OAuth1 4-tuple adapter, LFXV2-2642), **googleads** (LFXV2-2636),
**microsoft** (Bing Ads, LFXV2-2805), **hubspot** (the email channel, LFXV2-2777) — every
provider CreateCampaigns accepts now has a dispatcher.

Each adapter interprets its own credential + config shape:
- **reddit** — OAuth2 (clientId/secret/refreshToken); AccountID from the connection.
- **linkedin** — a single OAuth2 accessToken; builds RuntimeConfig from the
  connection's AccountID + `org_id` (must be the NUMERIC org id) plus caller-supplied
  targeting profiles from config.
- **meta** — a single OAuth2 accessToken; AccountConfig from AccountID (`act_...`) +
  `page_id` (REQUIRED by the connection design — the dispatcher needs it to attach the
  promoted-object page, so requiring it at connection time surfaces a 4xx instead of a
  silent dispatch failure); Budget is in the ACCOUNT's currency (no FX), optional
  CurrencyOffset.
- **twitter** — OAuth1 4-tuple (consumer key/secret + access token/secret); AccountConfig
  from AccountID + `funding_instrument_id`. Budget (`budgetAmount`) is in the ACCOUNT's
  currency (no FX). Surfaces a `Reused` reuse/config-drift flag and classifies an
  exhausted mutating 429 as UNCONFIRMED; validates the destination URL (https/http, no
  embedded userinfo) up front. `validateTwitterConnection` holds the credential rules
  shared by `Dispatch` and `ToggleStatus`, with ONE intentional asymmetry:
  `funding_instrument_id` is required only by `Dispatch`. It is a create-time field that
  `UpdateCampaignAndChildrenStatus` never puts on the wire, so requiring it in the shared
  validator would refuse an otherwise-valid pause. Do not fold that check into
  `validateTwitterConnection` — both halves are pinned by tests.
- **googleads** — OAuth2 application (clientId/secret + refreshToken) PLUS a Google Ads
  API developer token; AccountConfig from AccountID (the customer id) + an OPTIONAL
  `login_customer_id` (the manager/MCC account, from the connection's ProviderConfig).
  Budget (`googleAdsConfig.budget`) is in the ACCOUNT's currency (no FX). The client
  today creates a PAUSED search-campaign shell (budget → campaign); its two-step
  hierarchy means a PRE-attachment (budget-stage) orphan is reconciled by its deterministic
  `CampaignBudgetName`, but once the campaign attaches a non-shared budget's name synchronizes
  to the campaign name, so a campaign-stage partial reconciles the budget by `CampaignBudgetID`
  instead (the partial carries both). Either way the dispatcher returns a non-nil result
  (retaining the claim) on an ambiguous/duplicate-name create rather than releasing on an empty id.
- **microsoft** — OAuth2 app (clientId/secret) + a developer token + refreshToken;
  AccountConfig from the connection's AccountID (the DIGITS-ONLY `CustomerAccountId`, trimmed)
  plus an optional `customer_id` (the manager/`CustomerId` header). The client builds the
  full Campaign → AdGroup → Ad hierarchy (all PAUSED) — so the adapter needs no ad config
  beyond `microsoftConfig.budget` (the DAILY budget, in the ACCOUNT's currency, no FX) and an
  optional `timeZone`. `NameSuffix = brief.ID` gives deterministic retry-safe names (Microsoft
  enforces case-insensitive campaign-name uniqueness, so a retry composes the SAME name and
  cleanly REUSES the existing campaign (`AlreadyExisted=true`, no error) rather than
  duplicating). A non-nil result accompanied by an error is a separate UNCONFIRMED partial
  (claim retained); (nil, err) means nothing was created (claim released).
- **hubspot** — the EMAIL channel (not an ad platform), single private-app token. Unlike the ad
  adapters (which CREATE a campaign) it STAGES a marketing email: it CLONES a caller-specified
  template (`hubspotConfig.sourceEmailId`) and points the clone's send list at the brief's BUILT
  audience — resolved from the `campaign_audiences` resource (LFXV2-2773) via an injected
  `audienceReader`, taking the newest hubspot audience and refusing if it is not yet `built`
  (`PlatformMasterListID` → the send list, `SuppressionListIDs` → exclusions). The cloned email's
  HubSpot id is the campaign's `PlatformCampaignID`; the clone is a DRAFT (a human sends it). AI
  body content (LFXV2-2775) and audience building (LFXV2-2774) are separate steps. Claim
  contract: an UNCONFIRMED clone (2xx-no-id / transport) retains the claim with a name-only
  partial; a post-clone send-list failure is a partial (the email exists — retain + reconcile);
  a definite pre-clone failure releases the claim.

## Status toggle (optional capability)

`StatusToggler` is an OPTIONAL dispatcher interface (separate from `PlatformDispatcher`) —
`ToggleStatus(ctx, projectID, platform, campaign *model.Campaign, status)` — for
pausing/resuming a live campaign on the platform (ACTIVE↔PAUSED). It receives the full
persisted `*model.Campaign` (not just the id) so an adapter can reach any child ids it stored
at creation. The orchestrator's `ToggleCampaignStatus` type-asserts it (returning
`ErrToggleUnsupported` when a platform hasn't wired it), so it can be added platform-by-platform
without touching every adapter. **reddit** implements it: `resolveRedditClient` (shared with
`Dispatch`, so a create and a toggle accept exactly the same connections) builds the client,
then `client.UpdateCampaignAndChildrenStatus` PATCHes `configured_status` on the campaign AND
its child ad group + ad (read from the persisted `CampaignResult`) — because the create path
PAUSES all three, so toggling only the campaign would not serve. **meta** implements it too and
CASCADES: its create PAUSES the campaign, ad set, and ads, so `UpdateCampaignAndChildrenStatus`
POSTs the status to the campaign, the persisted ad set id, and each ad DISCOVERED via
`GET /{adSetID}/ads` (Meta persists the ad set id but not the individual ad ids). It needs only
the access token, not the page id. **linkedin** implements it and also
CASCADES: its create leaves the campaign PAUSED and its creatives DRAFT, so a full ACTIVATE
must lift the creatives too (a DRAFT creative never serves, and a creative's EFFECTIVE status
is gated by its campaign). `UpdateCampaignAndCreativesStatus` PARTIAL_UPDATEs the campaign
status, DISCOVERS the creatives via the creatives FINDER (LinkedIn persists only a creative
count, not ids), and PARTIAL_UPDATEs each creative's `intendedStatus`. On a PAUSE, a definite
400 on an in-review creative is tolerated (LinkedIn forbids pausing an in-review creative) —
the campaign is already the effective gate. An UNCONFIRMED client outcome (via `<platform>.IsOutcomeUnconfirmed`)
is wrapped in `unconfirmedToggleError` whose `Unconfirmed()` the service detects across the
package boundary (same behavioral-interface pattern as `NoUpstreamCreate`). 

**X/Twitter** implements it too, with a DIFFERENT cascade shape: scope is the campaign + line
item ONLY. `CreateCampaign` creates both PAUSED but the promoted-tweet association is created
ACTIVE by the API (that endpoint does not accept `entity_status`), and the LINE ITEM is X's
delivery gate — so pausing the line item stops serving and re-activating it resumes serving
without the association ever changing. Toggling the promoted tweet would be unnecessary and,
on activate, unable to make an otherwise-paused tree serve. `UpdateCampaignAndChildrenStatus`
PUTs `entity_status` (query params, not a JSON body, per the X Ads v12 contract), ordering
child-first on ACTIVATE and campaign-gate-first on PAUSE. An ACTIVATE with an unknown
line-item id is refused as `ErrCampaignNotProvisioned` (a 409) before any call.

**Google Ads** implements PAUSE only; **ACTIVATE is refused** with `ErrCampaignNotProvisioned`
(→409, raised locally without calling Google). The create path provisions only a PAUSED search
campaign SHELL (budget → campaign) with no ad group, ad, or keywords, so flipping the campaign
to ENABLED would report success while nothing can serve — the exact lie that sentinel exists to
prevent. There is no cascade for the same reason: there are no children to cascade to.
`UpdateCampaignStatus` sends a single `campaigns:mutate` UPDATE with `updateMask: "status"`.
Note the vocabulary: Google spells the serving state **ENABLED**, not ACTIVE. When GA-3+ adds
ad groups/ads/keywords, this must grow BOTH a cascade and a real child-id-based activate guard,
matching the reddit shape.

**Microsoft Ads** now implements the toggle as well (`UpdateCampaignAndChildrenStatus`). It is a
full three-level cascade — campaign, ad group, ad — ordered by DIRECTION, like reddit's:

- **PAUSE gates the parent FIRST** so delivery stops immediately, even if a child call then fails.
  A failure after the campaign flipped is a PARTIAL apply, reported as `Unconfirmed` rather than a
  plain error, because the parent change did land and a blind retry would misread the state.
- **ACTIVATE works upward, children first**, so the campaign is only un-gated once its children are
  already serving — the reverse would briefly serve nothing under a live campaign.
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
- **Outcome classification** folds Microsoft's 200-with-`PartialErrors` contract into an error, and
  treats an ABSENT `PartialErrors` (a `{}` or top-level `null` body) as unconfirmed rather than
  success — see the microsoft concept for why decodable is not the same as answered.

## Channel kinds: paid ads vs email

`model.ChannelKind` classifies each provider as **`paid-ads`** or **`email`** (`Provider.Kind()`,
with `Provider.IsPaidAds()` as the common shorthand). The distinction is BEHAVIOURAL, not
cosmetic: a paid ad channel CREATES a campaign that spends budget and can be paused/resumed
mid-flight, whereas the email channel STAGES a draft a human sends — no budget, no delivery
this service controls, nothing to pause.

HubSpot is the only email provider today. Branch on `Kind()` rather than comparing against
`ProviderHubSpot`, so a second email provider does not require hunting down every hardcoded
check. `Kind()` enumerates providers explicitly and returns `""` for an unclassified one, so a
newly added provider surfaces the omission instead of silently inheriting paid-ads behaviour.

Two places this shows up today:

- `dispatchableProviders` (container) spans BOTH kinds — email is dispatchable even though it
  is not an ad platform — which is why it is named for dispatch rather than "ad platforms".
  `logMissingDispatchers` logs each missing provider's kind so an operator can tell a missing
  paid platform (budget unspent) from a missing email channel (no drafts staged).
- The `ErrToggleUnsupported` 400 distinguishes the two reasons: email has no run state BY
  DESIGN, while an ad platform's toggle may simply not be wired yet.

See [internal/dispatch](../../../internal/dispatch).
