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

The registered set is `registerDispatchers` in `internal/container` — check there for which
providers currently have an adapter rather than duplicating the list here, since it grows
per platform PR and the two drift.

Each adapter interprets its own credential + config shape; see the "Dispatch adapter
(internal/dispatch)" section of the matching platform concept for the per-platform detail:
[reddit](internal-platform-reddit.md), [linkedin](internal-platform-linkedin.md),
[meta](internal-platform-meta.md), [twitter](internal-platform-twitter.md),
[googleads](internal-platform-googleads.md), [microsoft](internal-platform-microsoft.md),
[hubspot](internal-platform-hubspot.md).

## Status toggle (optional capability)

`StatusToggler` is an OPTIONAL dispatcher interface (separate from `PlatformDispatcher`) —
`ToggleStatus(ctx, projectID, platform, campaign *model.Campaign, status)` — for
pausing/resuming a live campaign on the platform (ACTIVE↔PAUSED). It receives the full
persisted `*model.Campaign` (not just the id) so an adapter can reach any child ids it stored
at creation. The orchestrator's `ToggleCampaignStatus` type-asserts it (returning
`ErrToggleUnsupported` when a platform hasn't wired it), so it can be added platform-by-platform
without touching every adapter.

An UNCONFIRMED client outcome (via `<platform>.IsOutcomeUnconfirmed`) is wrapped in
`unconfirmedToggleError`, whose `Unconfirmed()` the service detects across the package
boundary (same behavioral-interface pattern as `NoUpstreamCreate`) — every adapter that
implements `StatusToggler` follows this contract; see the linked platform concepts below
for which do and their implementation details.

Which children a toggle must reach, any asymmetric ACTIVATE/PAUSE handling, and
whether a platform has wired `StatusToggler` at all, is per-platform and
documented in that platform's own "Dispatch adapter (internal/dispatch)"
section: [reddit](internal-platform-reddit.md), [linkedin](internal-platform-linkedin.md),
[meta](internal-platform-meta.md), [twitter](internal-platform-twitter.md),
[googleads](internal-platform-googleads.md), [microsoft](internal-platform-microsoft.md),
[hubspot](internal-platform-hubspot.md) — not summarized here, since a
platform wiring or changing its toggle behavior would otherwise mean editing
this shared file too.

## Metrics read (optional capability)

`MetricsReader` is a second OPTIONAL dispatcher interface, alongside `StatusToggler` —
`ReadMetrics(ctx, projectID, platform, campaign *model.Campaign, window model.MetricsWindow)
(*model.CampaignMetrics, error)` — for a live, read-only performance snapshot of one
campaign (impressions, clicks, cost, CTR) over a caller-supplied window. Same pattern as the
toggle: the orchestrator's `ReadCampaignMetrics` type-asserts it (returning
`ErrMetricsUnsupported` when a platform's dispatcher doesn't implement it, without ever
contacting the platform), so it is added platform-by-platform without touching every adapter,
and it receives the full persisted `*model.Campaign` so an adapter can reach any child ids it
stored at creation (e.g. an ad group/ad set id, if a platform's reporting API needs it).
Unlike `ToggleStatus`, a `ReadMetrics` call has no ambiguous mutation to protect — there is
nothing to leave in an unknown state — so adapter errors propagate to the service verbatim; there
is no UNCONFIRMED wrapping equivalent to `unconfirmedToggleError`.

`window` arrives as a closed, platform-agnostic `model.MetricsWindow` value (`today`,
`yesterday`, `last_7_days`, `last_14_days`, `last_30_days`, `this_month`, `last_month`) — never
a platform's own literal. Each platform adapter owns the mapping from this vocabulary to its
platform's actual query syntax (e.g. Google Ads' GAQL `DURING` literals, Meta's Insights
`date_preset`), and any platform-specific validation of the mapped value (e.g. an allow-list
guard against GAQL injection) belongs in that platform's client package, not in the adapter or
the orchestrator.

**Microsoft Ads is NOT a `MetricsReader` and is not expected to become one under this
contract.** Its Campaign Management API v13 (REST/JSON, synchronous — what the existing
create dispatcher and status toggle use) has no metrics surface. Metrics live in a wholly
separate service, the Reporting API v13: SOAP, and asynchronous
(`SubmitGenerateReport` → poll `PollGenerateReport` until the status leaves `Pending` →
download a zipped CSV via a `ReportDownloadUrl`). There is no synchronous "impressions for
this campaign" call, so it cannot satisfy `ReadMetrics`'s one-bounded-call contract within
`metricsCallTimeout` (20s). Closing this gap needs a design decision (e.g. a bounded
submit-and-poll with a hard ceiling, or a persisted/sweeper-refreshed snapshot instead of a
live read) — deferred, not attempted here.

**LinkedIn** implements it as `LinkedInDispatcher.ReadMetrics`, which resolves the account
credentials and then delegates to the platform client helper
`linkedin.Client.GetCampaignMetrics(ctx, accountID, campaignID, window)` — the helper is not
itself the `MetricsReader`, its signature differs from the interface. The helper maps the
shared `model.MetricsWindow` to a Rest.li 2.0 nested date-range literal via
`dateRangeForWindow`, then queries the Ad Analytics `adAnalytics` finder
(`q=analytics`) scoped to the campaign/account URNs built from the persisted bare numeric
`PlatformCampaignID`. Five of the shared vocabulary's seven windows map to a concrete date
range — `today`, `last_7_days`, `last_30_days`, `this_month`, `last_month`. `yesterday` and
`last_14_days` are NOT mapped, and `ErrUnsupportedWindow` for them is raised by the clock-free
`linkedin.ValidateMetricsWindow`, which the dispatcher calls BEFORE `creds.resolve` and
translates to `ErrMetricsWindowUnsupported`. That order is load-bearing rather than stylistic:
an unsupported window is a permanent 400 whatever state the connection is in, but resolving
credentials first makes a project with an inactive or incomplete connection fail with a
connection error that `BriefService` maps to 503 — telling the caller to retry a request that
can never succeed. `dateRangeForWindow` calls the same validator first, so the two cannot drift.
Spend (`costInUsd`, decimal USD) is converted to
micro-currency (×1e6, rounded rather than truncated) after a `maxCostDecimalLen` (40-byte) bound
— the 10 MiB response cap does not bound a single decimal, and `big.Rat` parsing/scaling is
super-linear in digit count and does not observe the request context. `Ctr` is computed as
clicks/impressions, 0 when impressions is 0. A finder response with an empty (non-nil)
`elements` array is zero-activity, not an error; a nil/missing `elements` field on a 2xx is
rejected as a decode error rather than silently reported as zero. An element reporting clicks
with zero impressions is likewise rejected: a click implies the impression that carried it, so
that shape means the element is incomplete and publishing it would report `Ctr=0` beside a
non-zero click count. Per-metric presence tracking is deliberately not used — an
omitted-because-zero key is indistinguishable from an omitted-because-malformed one, so
requiring every key would reject responses that are genuinely fine.

**Reddit implements it, but the capability is GATED OFF BY DEFAULT because the entire
request/response contract is an UNVERIFIED, BEST-EFFORT GUESS**
(`internal/platform/reddit/metrics.go`). `RedditDispatcher.ReadMetrics` returns
`domain.ErrMetricsUnsupported` — the same 400 a platform with no metrics support at all
produces — unless `REDDIT_METRICS_ENABLED` is exactly `"true"`; any other value, including
unset, fails closed. The gate exists because DECLARING the method is itself the capability
switch: the orchestrator discovers `MetricsReader` by type assertion and the published
endpoint invokes it immediately, so an ungated wiring would return 200 from a guessed shape
and currency unit, with none of the caveats visible in the response. The gate is read per
call rather than at construction, so a deployment flips it without a rebuild.
`REDDIT_METRICS_ENABLED` is declared in `pkg/constants` and wired in the chart's
`values.yaml`. Once the shape is verified against a live ad account, the gate is deleted. Unlike this client's create/toggle endpoints
(ported from a working upstream client) and unlike Meta/LinkedIn/X's metrics clients (built
against each platform's public API docs), Reddit's v3 reporting/metrics endpoint has no public
documentation — it is gated behind Reddit's developer portal and a private Postman collection.
The implementation is inferred only from this package's own proven v3 conventions (resource
nesting, OAuth2 bearer + retry/backoff, the `{"data": ...}` envelope): a `POST
/ad_accounts/{account_id}/reports` with a guessed `{"data": {starts_at, ends_at, campaign_ids,
breakdowns, fields}}` body, decimal-string spend (converted to micros ×1e6, rounded), and an
empty result rows array treated as zero-activity. This was investigated and recorded as BLOCKED
on LFXV2-2995 before the file was written — treat every field name and the request/response
shape as a placeholder to be corrected once official Reddit Ads API access confirms the real
contract, not a confirmed integration.

**X/Twitter** implements it: `twitterMetricsWindow` maps the shared `model.MetricsWindow`
vocabulary to X Ads' own `MetricsWindow` literals, then `GetCampaignMetrics(ctx, campaignID,
window)` queries the X Ads `/stats` endpoint. **CRITICAL: X's stats endpoint caps queryable
date ranges at 7 days per request.** Only `yesterday`, `today`, and `last_7_days` map to a
supported X window (`YESTERDAY`/`TODAY`/`LAST_7_DAYS`); every other foundation window
(`last_14_days`, `last_30_days`, `this_month`, `last_month`) returns `twitter.ErrUnsupportedWindow` explaining
the platform's API limitation (NOT a reduced range, average, or extrapolation). This is a
permanent X API constraint documented in the knowledge base. Spend is returned by X as
`billed_charge_local_micro`, already in micro-currency units (no USD parsing or conversion).

## Account discovery (optional capability)

`GoogleAdsDispatcher.ListAccounts(ctx, projectID, platform) ([]model.AccessibleAccount, error)`
enumerates the ad accounts reachable **upstream at the provider** with the connection's stored
credential. It exists so an operator configuring a connection can pick the right account instead
of pasting a customer ID by hand.

**Now fully wired.** The adapter landed one PR ahead of its caller; both halves are present as of
this change. `internal/service/orchestrator.go` declares `AccountLister` alongside `StatusToggler`
and `MetricsReader`, and `Orchestrator.ReadAccounts` reaches it through the same optional-capability
type assertion `MetricsReader` uses — a platform whose dispatcher does not implement the interface
yields `ErrAccountsUnsupported`, which the service layer maps to 400 rather than to a 503 that
invites a pointless retry.

Note what it is NOT: this does not list anything this service stores. A project holds at most one
connection per provider, and that singleton is read via `GET .../connection-{provider}`.
`AccessibleAccount` (`ID`, `Label`) is a live projection of the provider's own account list, never
persisted — the same live-read-only discipline as `ReadMetrics`. Errors from the provider CALL
propagate verbatim: a read has no ambiguous mutation to protect, and the adapter does not classify
those, leaving the HTTP status mapping to the service layer.

**Errors that arise BEFORE any request are classified here, and must be.** The service layer has
exactly one default arm for an unrecognized error, and it answers 503 — "the provider call failed,
retry later". Three conditions would land there wrongly: an inactive connection, a credential blob
that is incomplete or structurally malformed, and a `login_customer_id` stored with dashes. (A blob
that fails AUTHENTICATION is not one of them — see the decrypt split below.) None of them
improve with time; all of them need a human to edit the connection. So
`resolveGoogleAdsDiscoveryClient` wraps each with `domain.ErrConnectionNotUsable`. The 400 mapping
for that sentinel lands with the endpoint in the follow-up PR; the wrap has to exist here, in the
layer that knows the failure was pre-send, because nothing downstream can recover the distinction.

The manager-id check is duplicated on purpose. `Client.validateLoginCustomerID` still validates it
(the backstop for every other caller), but it does so inside the same call that talks to Google, so
by the time it fires the error is indistinguishable at this boundary from a genuine upstream
failure. `storedCustomerIDRE` in `internal/dispatch/googleads.go` therefore checks the STORED value
where it is read — the check has to happen where the answer is still classifiable. The two regexps
must stay in step.

`creds.resolve` classifies each of its failure branches, and the splits are deliberate. A connection
row with an EMPTY credential blob is permanently unusable as it stands, so it carries
`domain.ErrConnectionNotUsable` and a read-only caller answers 400. Two branches do not:
`domain.ErrNotFound` means there is no connection at all (→ 404, and the caller should create one,
not edit one), and a repository failure is a genuine "try again later" (→ 503). Flattening either
into "not usable" would lose a distinction the service layer depends on.

**A decrypt failure is not one condition, and it splits again.** Only a blob the encryptor could not
even ATTEMPT to authenticate — `domain.ErrCredentialsMalformed`, for AES-GCM a ciphertext shorter
than a nonce — is proven bad ROW data, and only that branch earns `ErrConnectionNotUsable` → 400. A
GCM AUTHENTICATION failure carries `domain.ErrCredentialDecryptionFailed` instead: it means a wrong
or rotated APPLICATION key, or tampering (`internal/infrastructure/crypto/aesgcm.go` states both),
and that key is deployment-wide, so the same failure hits every project's connection at once.
Reported as "not usable as configured" it would answer 400 to all of them — every operator told to
go fix a connection that is fine — and would erase the 500 that says the deployment is broken. An
unrecognized decrypt error takes the authentication path on purpose: an `Encryptor` that proves
nothing about the row must not be read as accusing it.

**Which defect it was is carried by a second sentinel, and the log line is why.** Alongside
`ErrConnectionNotUsable`, each stored-connection defect wraps one of
`domain.ErrConnectionInactive`, `ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`, or
`ErrProviderConfigInvalid`. The status is still decided by the one sentinel; these only name the
reason. They have to be sentinels rather than message text because the service layer cannot log the
error at all: `validateGoogleAdsCredentials` detects the undecodable case by decoding the DECRYPTED
blob, and `encoding/json` quotes its input — a `*json.SyntaxError` names the offending character, a
`*json.UnmarshalTypeError` names the field being read. So that unmarshal error is **dropped, not
wrapped**: nothing a reader could act on is lost (the remedy is "re-save the credential", not "fix
byte 41"), and `errors.Is` over a fixed vocabulary carries the diagnosis with no payload attached to
carry secrets in.

The two decrypt sentinels are declared in `internal/domain` rather than in `crypto` because callers depend
on the `domain.Encryptor` PORT and never import the implementation; the port's doc states the
wrapping obligation, and `crypto`'s `ErrCiphertextTooShort` / `ErrDecryptionFailed` each wrap their
domain sentinel so `errors.Is` carries the classification across the layer without inverting the
dependency. Note the decrypt branches wrap BOTH a sentinel and the decrypt error (`%w: %w`); the
service layer logs that cause rather than returning it, since a decrypt failure can carry ciphertext
detail.

Google Ads is the only implementation today, via
`Client.ListAccessibleCustomers` → `customers:listAccessibleCustomers`. That endpoint is
**account-agnostic** — it has no `customers/{id}` path segment, unlike every other Google Ads call
— and it is sent with a nil body (so no `Content-Type`) and `idempotent=true` (a pure read, so
retrying a 429 cannot double-apply anything).

**Discovery runs without an account id, deliberately, at both layers.** A connection is created
with credentials first and an account chosen afterwards — from the list this call produces — so
requiring a customer id before discovery means the caller must already know the answer to the
question they are asking. Two preconditions used to enforce exactly that, and both were relaxed
in a targeted way rather than removed:

- The dispatcher's `validateGoogleAdsConnection` demands a non-empty `accountID`. Discovery now
  routes through `validateGoogleAdsCredentials` (via `resolveGoogleAdsDiscoveryClient`) instead,
  which keeps every other check — active status, decodable blob, all four OAuth fields — so a
  discovery call against a stale or half-configured connection still fails as a *connection*
  problem rather than as an opaque error from Google.
- `Client.doRequest` validates `c.account.CustomerID` as digits-only. The account-agnostic paths
  call `doRequestValidated` instead, which is `doRequest` with the id precondition discharged by
  the caller. It exists ONLY so those paths can share one copy of the URL construction, header
  set, body bounding, retry gating, and `apiError`/`transportError` classification. The
  `login-customer-id` header is still attached and still validated (`validateLoginCustomerID`).

**A manager credential needs the hierarchy walked, because the flat list does not do it.**
`customers:listAccessibleCustomers` returns the accounts the authenticated user can act on
DIRECTLY; a `login-customer-id` header does not make it enumerate that manager's children. On an
MCC connection — the normal shape for agency-managed accounts — the flat list is therefore often
just the manager itself, and every child ad account the caller actually wants to pick is missing.
So when a manager id is configured, `listManagerClients` expands it with a `customer_client` GAQL
query scoped to the manager (`gaqlSearchForCustomer`, which takes an explicit customer id rather
than the client's empty one). Manager rows are filtered out of the result: a manager account
cannot hold campaigns, so offering one would let a caller select an account that fails at the
first create. Only `status = 'ENABLED'` clients are requested. The expansion also supplies
`descriptive_name`, which the flat endpoint does not return at all — so labels appear only for
accounts reached this way. Without a manager id there is no hierarchy root to walk and the direct
list is the whole answer.

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
