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
   its own credential struct. When the project has NO connection, resolution falls
   back to the reserved `model.SystemProjectID` scope (see below).
2. **Map inputs** (per-platform) — the adapter reads the brief's event destination
   from its top-level `URL` field (with a nested `registrationUrl` in the opaque JSON
   only as a fallback) and the event name from the opaque JSON blobs — read as
   `eventName`, falling back to `name`, which is the spelling the UI writes
   (LFXV2-3259) — plus the
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

## Stored-connection defects are tagged where they are detected

This section is about the six PAID-ADS adapters. HubSpot is a registered dispatcher too and it
is deliberately outside the scope; the table below says why.

Every paid-ads adapter runs the same pre-flight before it contacts a platform: the connection row
must be `active`, its decrypted blob must be valid JSON, the decoded credentials must have every
required field, and (for the paths that need one) an ad account must have been selected. All four
are STORED-STATE defects — a human has to edit the connection, and no amount of retrying helps.

The service layer has exactly one default arm for an error it does not recognize, and it answers
**503**. So an untagged defect is not merely mislabelled: it tells the caller a platform did not
respond, about a platform that was never contacted, and prescribes a remedy (wait, retry) that
cannot ever succeed. Each defect is therefore wrapped with `domain.ErrConnectionNotUsable` — which
selects the status — PLUS a reason sentinel from the fixed vocabulary (`ErrConnectionInactive`,
`ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`, `ErrAccountNotSelected`), which is what
the handler logs. Both are required: the status marker alone logs `reason=unclassified`.

Two properties of this pattern are easy to lose and worth stating outright:

- **The unmarshal error is DROPPED, not wrapped.** It is produced by decoding the DECRYPTED
  credential blob, and `encoding/json` quotes its input — `*json.SyntaxError` names the offending
  character, `*json.UnmarshalTypeError` names the field. Keeping it in the chain puts
  credential-derived bytes within reach of anything that renders or `errors.As`-walks the error.
  Nothing actionable is lost: the remedy is "re-save the credential", not "fix byte 41".
- **The tagging belongs in the SHARED resolve/validate helper, not at each call site.** Where an
  adapter HAS one — Google Ads, Reddit, X/Twitter and Microsoft Ads each route Dispatch,
  `ToggleStatus` and (where wired) `ReadMetrics` through a single helper — tagging it once covers
  every path. Meta gained one in LFXV2-3061 (`resolveMetaCredentials`) and
  LinkedIn in LFXV2-3196 (`resolveLinkedInCredentials`), so every ad adapter now has one. Tagging per-path is how Google Ads ended up correct on discovery while its other
  callers were still bare, before the helper absorbed it.

Which adapters honour it today:

| Adapter | Tagged | Paths covered |
|---|---|---|
| Google Ads | yes | `validateGoogleAdsCredentials` — dispatch, toggle, metrics, discovery |
| Reddit | yes | `resolveRedditClient` — dispatch, toggle, metrics |
| X/Twitter | yes | `validateTwitterConnection` — dispatch, toggle, metrics |
| Microsoft Ads | yes | `validateMicrosoftConnection` — dispatch, toggle, metrics (async Reporting API; default-OFF) |
| Meta | yes | `resolveMetaCredentials` — dispatch, toggle, metrics (LFXV2-3061) |
| LinkedIn | yes | `resolveLinkedInCredentials` — toggle, metrics (LFXV2-3196); Dispatch keeps its own inline checks, which wrap in `notCreated()` to release the claim |
| HubSpot (email) | **n/a** | out of scope — see below |

HubSpot is listed for completeness, not as a gap. Its checks in `internal/dispatch/hubspot.go`
are bare — inactive row, JSON decode, empty `privateAppToken`, and no ad-account check at all,
because an email connection has no ad account to select. But they are bare on a DIFFERENT axis:
they are wrapped in `notCreated(...)`, the pre-create classification the orchestrator uses to
release the dispatch claim, and campaign create is asynchronous, so none of them is choosing
between a 409 and a 503 the way the six above are. Tagging them would change what a polled job
result says, which is worth doing, but it is a separate question from the status mapping this
section is about — do not read the empty cell as work queued behind LFXV2-3069 part 2.

LinkedIn was the last adapter still inlining `d.creds.resolve(...)` at more than one call site,
so tagging it was an extraction rather than an annotation — Meta was in the same state until
LFXV2-3061 performed exactly that extraction. LFXV2-3196 did the same for LinkedIn's toggle and
metrics paths.

Its `Dispatch` deliberately keeps its own inline checks: they wrap in `notCreated()` to release
the dispatch claim, which is a different contract from returning a classified error to a
synchronous handler, and folding the two would mean the helper had to know which caller it had.

One asymmetry worth carrying: LinkedIn emits `account_not_selected` on toggle and metrics where
Meta does not. LinkedIn's client is constructed with a `RuntimeConfig` naming the account, so an
empty account id cannot reach the platform at all; Meta targets the campaign node by platform id
and never reads the account, so an account cleared after creation must not block pausing.

The full rationale for the classification — including why a decrypt failure splits into two
sentinels, and why an inactive row is refused rather than treated as "pending" — lives in the
Google Ads discussion under *Account discovery* below, since that is where the pattern was first
worked out.

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

**Microsoft Ads implements `MetricsReader` as of LFXV2-3260**, but the read is gated OFF
by default. Its Campaign Management API v13 (REST/JSON, synchronous — what the create
dispatcher and status toggle use) has no metrics surface; metrics live in a separate
service, the Reporting API v13. That API is **REST/JSON, not SOAP** — an earlier revision of
this document said SOAP — and it is asynchronous (`SubmitGenerateReport` → poll
`PollGenerateReport` until the status leaves `Pending` → download a zipped CSV via a
`ReportDownloadUrl`).

The adapter absorbs the asynchrony behind one bounded call: `reportPollBudget` (15s) caps
the whole submit+poll phase and must stay strictly under `metricsCallTimeout` (20s), or the
caller's context cancels first and `ErrReportNotReady` becomes unreachable —
`TestReportPollBudgetStaysUnderTheMetricsCallTimeout` pins that relationship. The download
sits outside the budget deliberately: once `Success` is reported the file exists, and
cutting off the transfer would discard a report already paid for.

"The whole submit+poll phase" is a claim about WHERE the deadline is taken, and it only
became true in LFXV2-3260. `GetCampaignMetrics` computes `deadline` BEFORE calling
`submitReport` and passes it into `pollReport`, so submit time is charged against the same
budget the polling spends. An earlier revision created the deadline INSIDE `pollReport`,
after submit had already returned: submit was effectively free, and a slow submit surfaced
the caller's `context deadline exceeded` instead of `ErrReportNotReady`, with no download
headroom left. `pollReport` also checks the budget BEFORE its first poll, so a submit that
already consumed it answers `ErrReportNotReady` without issuing a poll there is no time to
act on. `TestPollBudgetCoversTheSubmitPhase` pins this with a clock that advances only
during the submit — an every-reading clock expires the budget either way and cannot tell the
two placements apart.

Because the request/response shapes follow Microsoft's published documentation but have
never been exercised against a live Bing account, `ReadMetrics` answers
`domain.ErrMetricsUnsupported` unless `MICROSOFT_METRICS_ENABLED` is exactly `"true"` —
the same fail-closed gate Reddit uses. Delete the gate once the shape is confirmed live.

**Microsoft enforces the same account-identity invariant Google Ads does, on BOTH its read
and its toggle** (LFXV2-3260). A Microsoft campaign id is unique only WITHIN an ad account,
while `resolveMicrosoftClient` returns the project's CURRENT connection, which
`UpdateMicrosoftAds` can re-point between create and a later call. Unguarded, the stored
`PlatformCampaignID` is addressed against the NEW account, where it matches nothing (a false
"no metrics") or collides with an unrelated campaign — whose numbers would be rendered as
this campaign's measurement on the read path, and whose delivery would be changed on the
toggle path. `verifyMicrosoftAccountMatch` is shared by both callers so they cannot drift,
and returns `domain.ErrCampaignAccountMismatch` (409) before any request is issued.

The creating account is stamped into the result blob as `microsoft.CampaignResult.AccountID`.
`microsoftCreationAccountID` prefers that field and falls back to the `aid=` parameter of the
`microsoftAdsUrl` the blob has always carried, so rows written before the field existed stay
checkable rather than silently unguarded. It mirrors `googleAdsCreationCustomerID`'s
`customerId`/`ocid` pair exactly, INCLUDING the contract that an ABSENT id means "unknown,
proceed": only a present-and-different id is a mismatch. Absence must not become a new
failure signal, or every pre-LFXV2-3260 row would be stranded — unlike HubSpot, which fails
closed on absent provenance because a bare HubSpot email id carries no recoverable fallback
at all. The comparison is against `Client.AccountID()` (the ad account), NOT the MCC parent
`customer_id`.

**Meta** also implements it: `MetaDispatcher.ReadMetrics` resolves the connection the same way
`ToggleStatus`/`Dispatch` do, then calls `meta.Client.GetCampaignMetrics`, which issues a single
`GET /{campaign-id}/insights` Graph API request for `impressions`, `clicks`, `spend` over a
`date_preset` mapped from `window` (validated against a platform-side allow-list — an invalid
caller value fails inside the client). The campaign id is validated as digits-only (the same
`numericIDRE` sibling Meta client methods use) BEFORE string concatenation into the request path,
since the Graph API has no parameterized path segments. `spend` arrives as a JSON string in
whole units of the ad ACCOUNT's currency (not micros, unlike Google Ads, and not necessarily
USD — the service does no FX conversion) and is `strconv.ParseFloat`'d then scaled ×1,000,000 and
rounded (not truncated) to `CostMicros`, guarding against non-finite (`NaN`/`Inf`) results. CTR is
computed client-side the same way as Google Ads.

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
connection error, reported as something other than the window defect the caller actually has.
(Before LFXV2-3196 that error was untagged and `BriefService` mapped it to 503 — telling the
caller to retry a request that can never succeed. `resolveLinkedInCredentials` now tags it, so
the same states answer 409 for a project-owned row and 500 for an unusable LF system fallback;
the ordering argument is unchanged, since neither answer is the window's 400.) `dateRangeForWindow` calls the same validator first, so the two cannot drift.
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

**HubSpot (the email channel)** implements it too, and it is the one `MetricsReader` whose
subject is not an ad campaign. `HubSpotDispatcher.ReadMetrics` calls
`hubspot.ValidateMetricsWindow` BEFORE `resolveHubSpotClient` — the same load-bearing order as
LinkedIn and for the same reason — then reads the staged email's statistics and restates
`CampaignID` as the SERVICE's campaign UUID, which the platform client cannot know (it keyed
its result by the HubSpot email id it queried).

Four things about it differ from every ad adapter, and a consumer that assumes otherwise
reports false numbers:

- **Identity is bound to the PORTAL, and it is resolved from the token, not from config.** A
  HubSpot email id is a bare numeric that is unique only inside the portal that minted it, so
  `Dispatch` records the portal in the campaign's `Result` blob and `ReadMetrics` refuses
  (`domain.ErrCampaignAccountMismatch`, 409) unless it still matches. Both sides come from
  `Client.AuthenticatedPortalID`, which calls `POST /oauth/v2/private-apps/get/access-token-info` — deliberately NOT
  `providerConfig["portal_id"]`, an optional operator-supplied string used only for app URLs
  that `SetCredentialHubspot` leaves untouched when it swaps the token. A config-on-config
  comparison would fire only on the DECLARED change and stay silent on the undeclared token
  swap, which is the actual risk; that version was written, found unsound and reverted before
  this one. The asymmetry between the two callers is intentional: the `Dispatch` lookup is
  BEST-EFFORT (the call can still fail on network or upstream error, and a provenance read must
  not block a send that is otherwise ready), while `ReadMetrics` FAILS CLOSED — including when
  the row records no portal, which is every campaign staged before this landed. Those rows are
  unreadable until re-dispatched, and that is the honest outcome: nothing about such a row
  establishes which portal its id means.

  A row with no recorded portal is an ABSENCE, not a MISMATCH, and the two need different
  operator-facing remedy text: "reconnect the original account" (the mismatch case) tells the
  operator to point the connection back at a tenant this row never named, which is not
  something they can do. `ReadMetrics` returns `errors.Join(domain.ErrCampaignProvenanceUnknown,
  domain.ErrCampaignAccountMismatch)` for the no-portal case — joined, not returned alone, so
  every pre-existing `errors.Is(err, ErrCampaignAccountMismatch)` caller still matches — and
  `brief.go`'s status mapping checks `ErrCampaignProvenanceUnknown` FIRST (case order matters
  in that switch) to give the correct "must be re-dispatched" 409 message instead.

  That absence is checked BEFORE `AuthenticatedPortalID` is called, and the order is
  load-bearing rather than incidental. Absent provenance is a purely LOCAL fact — no value the
  lookup could return would change the answer — so asking first inverted the outcome for exactly
  the rows the guard exists for: a legacy row read while token-info was throttled or down
  returned the transient "cannot establish which portal this token authenticates against" 503
  instead of the deterministic `ErrCampaignProvenanceUnknown` 409, hiding the one remedy that
  fixes it (re-dispatch, which writes the provenance) behind an upstream failure that no amount
  of retrying the read will clear, and spending up to `portalLookupTimeout` of the 20s metrics
  budget on a call whose result was already irrelevant. Pinned by
  `TestHubSpot_ReadMetricsRefusesUnrecordedProvenanceBeforeContactingHubSpot`, which asserts the
  lookup is never CONTACTED — asserting only on the sentinel would keep passing against an
  implementation that asks first and happens to get an answer.

  The best-effort portal lookup in `Dispatch` is bounded by its OWN `portalLookupTimeout` (10s),
  not the caller's context: the HubSpot client's retry policy alone can wait up to
  `retryMax*maxRetryWait` (180s) under sustained throttling, which exceeds the entire 2-minute
  `providerCallTimeout` (`internal/service/orchestrator.go`) and would otherwise hand the
  mutating `CloneEmail`/`SetSendList` calls that follow an already-cancelled context. A
  provenance read whose failure is only ever logged must not be able to spend the budget those
  calls need.

- **The window does not scope the counters.** HubSpot's statistics span selects WHICH EMAILS
  are in scope by SEND date; the counters returned are that email's totals to date. `today`
  and `last_30_days` on an email sent this morning return the same numbers. `Window` records
  what was asked, not a period the counters cover. Genuine event-time windowing needs the
  email-events API and is deliberately not attempted.
- **An empty match is a successful read of nothing, not a failure.** The adapter marks
  `hubspot.ErrNoSentEmailInWindow` with `domain.ErrNoMetricsInWindow` so the service answers
  409. Unmarked it would take the 503 default — and this is the ORDINARY state, because
  `Dispatch` stages the cloned email as a DRAFT for a human to send. Note what the sentinel
  does NOT claim: sent-outside-the-window, never-sent and no-such-id arrive in one
  indistinguishable shape, so it names all three.
- **`CostMicros` is always 0**, and the extra counters ride in `CampaignMetrics.Email`
  (`sent`, `delivered`, `opens`, `clicks`, `bounces`, `unsubscribes`), a nil-for-ad-platforms
  pointer. `Impressions`/`Clicks` mirror `opens`/`clicks`. The zero cost means "not billed
  per send", not "free", and must not be blended into a cross-channel CPA.

`resolveHubSpotClient` was extracted from `Dispatch` once `ReadMetrics` became a second
caller, rather than inlining the credential sequence a third time. Its two error axes are owned
by different places:

- **CREATE axis — the mutating caller's.** The helper adds `NoUpstreamCreate` to nothing;
  `creds.resolve`'s error already carries it, and a read has no create to disown. `Dispatch`
  therefore passes an already-marked error through and wraps everything else in `notCreated`,
  the same shape the reddit adapter uses.
- **AUDIENCE axis — the helper's, at the point of detection.** Each of the three
  stored-connection defects carries `domain.ErrConnectionNotUsable` plus a reason sentinel:
  `ErrConnectionInactive`, `ErrCredentialsUndecodable`, `ErrCredentialsIncomplete`. Returned
  bare they fall to `GetCampaignMetrics`' default arm and answer 503 for a platform that was
  never contacted. Same template as `validateGoogleAdsCredentials`, including the named return
  with `defer func() { err = res.systemScoped(err) }()` so a later return site cannot forget to
  re-attribute the error to the LF system row. The `json.Unmarshal` cause is DROPPED, not
  wrapped — it is derived from the DECRYPTED blob and `encoding/json` quotes its input.

The token is `TrimSpace`d ONCE inside the helper and the trimmed value is
what reaches `hubspot.NewClient`, so the incomplete-credential check is made against the value
the client will actually use.

## Account discovery (optional capability)

`GoogleAdsDispatcher.ListAccounts(ctx, projectID, platform) ([]model.AccessibleAccount, error)`
enumerates the ad accounts reachable **upstream at the provider** with the connection's stored
credential. It exists so an operator configuring a connection can pick the right account instead
of pasting a customer ID by hand. `MetaDispatcher.ListAccounts` is the second implementation.

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

### Google Ads

**Errors that arise BEFORE any request are classified here, and must be.** The service layer has
exactly one default arm for an unrecognized error, and it answers 503 — "the provider call failed,
retry later". Three conditions would land there wrongly: an inactive connection, a credential blob
that is incomplete or structurally malformed, and a `login_customer_id` stored with dashes. (A blob
that fails AUTHENTICATION is not one of them — see the decrypt split below.) None of them
improve with time; all of them need a human to edit the connection. So each is wrapped with
`domain.ErrConnectionNotUsable`, in the layer that knows the failure was pre-send, because nothing
downstream can recover the distinction.

Ownership of that wrap is SPLIT, and the split follows which function is in a position to know.
`validateGoogleAdsCredentials` tags the three CREDENTIAL-STATE failures — a non-`active` status,
a blob that is not valid JSON, a blob missing a required field. `validatedLoginCustomerID` tags
the one that is not about the credential at all: a `login_customer_id` stored with dashes.

**Both are called by EVERY path that reads the column, and that is the whole point of the second
one being a function.** The manager-id check used to sit INLINE in
`resolveGoogleAdsDiscoveryClient`, which meant only the discovery endpoint got it. The other paths
read the same stored column, handed it to the same client, and classified the same defect
differently: the value reached the client uninspected, failed there at `validateLoginCustomerID`,
and arrived at the orchestrator indistinguishable from an upstream failure — same call, same error
type. The default arm answered `503`, promising a retry would help, for a stored value only a
human can repair. LFXV2-3052 hoisted it into a helper.

There are **three** readers, not two, and the third is easy to miss: `resolveGoogleAdsClient`
(toggle, metrics), `resolveGoogleAdsDiscoveryClient` (account discovery), and `Dispatch` — which
builds its own client INLINE rather than through a resolver, because it predates both of them.
Enumerating callers by the abstraction ("which resolvers call this?") does not find it;
enumerating by the STORED KEY does — and, now that this PR has hoisted the read into one
helper, so does enumerating by the helper. Both commands need `-F`, because the useful search
strings contain regex metacharacters (`[`, `"`), and quoting, because an unquoted `[...]` is a
shell glob:

```bash
grep -rn -F 'login_customer_id' internal/ | grep -v '_test\.go'   # the key, every reader
grep -rn -F 'validatedLoginCustomerID' internal/                  # the helper: 3 call sites
```

Note that `grep -rn -F 'providerConfig["login_customer_id"]'` is now the WRONG enumeration even
though it runs: after the hoist there is exactly one such expression, inside the helper itself.
An enumeration keyed on an expression the refactor was designed to centralise reports one
reader and reads as reassurance. `Dispatch` is also the
path where the consequences are worst: it is the one that spends money, and the client's own
validator renders the offending value with `%q`, which the orchestrator then writes to its
dispatch-failure log line — so leaving it uninspected leaked account-identifying configuration
into logs on top of misclassifying the failure. Its wrap is `notCreated`, preserving create-only
claim semantics: nothing was sent, so the claim must be released rather than retained for
reconciliation.

A check that lives on one of several paths through the same column is not a check; it is a coin
flip on which endpoint the caller happened to use.

An empty `login_customer_id` is legal and means "no manager", so only a non-empty malformed value
fails. The error names the field and the rule but never echoes the VALUE: a manager id is
account-identifying configuration, this error reaches a log, and the rest of this path keeps
error text to a fixed sentinel vocabulary with no payload attached.

### Meta

`MetaDispatcher.ListAccounts` follows the same contract, through
`resolveMetaDiscoveryClient`, and the differences are the interesting part.

**The account id is not consulted, and `AccountConfig` is left zero.** Graph
`GET /me/adaccounts` asks what the TOKEN reaches, so scoping the client to one of the answers
would narrow the response to a subset of the question. Requiring an account id would also make
the endpoint reachable only by connections that no longer need it.

**Both lifecycles are reachable as of LFXV2-3061.** The resolver was already correct for
first-time bootstrap — credentials stored, account chosen afterwards, the way Google Ads
works — and `MetaAdsConnectionConfig` no longer declares `Required("account_id")`. Closing it
was not a one-line loosening: an empty account id had to be tagged first, or a Meta connection
parked mid-bootstrap would fail `Dispatch` with an error nothing classifies — the caller would
learn that the campaign did not launch, not that the reason is a choice they have not made.
`requireMetaAccountID` supplies that tagging. Note the shape of that answer: campaign create is ASYNCHRONOUS (`design/brief.go`
answers `StatusAccepted`), so no error of any kind surfaces as a 409 — `docs/api-catalog.md`
records the same split for Google Ads. Nor does the reason reach the polled job result:
`dispatchPlatform` collapses every dispatcher error into the fixed string
`"platform campaign creation failed"` (`internal/service/orchestrator.go`), so the job says the
create failed and never says why. The synchronous 409 that
names the missing account belongs to `ToggleStatus` and `ReadMetrics`, and neither needs
anything here: both target the campaign node by id and document that they need no account id
(`internal/dispatch/meta.go`), so they already work on an account-less row. `Dispatch` is
therefore the only exit tagged, and what the tagging improves is the dispatch-failure LOG LINE —
the orchestrator splits on the reason before collapsing the result, so `account_not_selected`
reaches an operator reading logs and no caller-facing surface at all. `internal/bootstrap` records
the same for the system row. Delivered by LFXV2-3061.

**The unmarshal cause on the decrypted blob is DROPPED, not wrapped.** It is the only value in
the resolver derived from decrypted plaintext, and this error is logged and, on the not-usable
arm, described to the caller. Today's `encoding/json` happens not to quote the offending bytes
for a struct of string fields, but that is a behaviour rather than a documented guarantee and
it does not hold for every field type. `TestMeta_ListAccounts_UndecodableBlobDropsTheUnmarshalCause`
asserts the error text EXACTLY, against the two sentinels plus the resolver's own project context
(`": meta credentials for project proj are not valid JSON"`) — that suffix is built from the
project id, never from plaintext. Equality is the binding form: asserting merely that the text
does not contain the secret would pass with the cause appended.

**Known-bad accounts are returned, not filtered.** Disabled, unsettled, pending-review,
pending-settlement, grace-period and closed accounts come back with the reason in the label
(`"LF Events (disabled)"`). This is a picker: dropping them answers "your token reaches no ad
accounts" about an account sitting right there. The label reuses
`inactiveAccountStatusLabels`, the same map `CreateCampaign`'s preflight refuses on, so the
picker and the create path cannot disagree about which accounts are known-bad. `account_status`
0 means the field was absent, which is not a claim of disabled, and gets no label.

### Google Ads: manager mode, discovery and the bootstrap lifecycle

This section is Google-Ads-specific, and the two after it alternate again — `### Meta: the same
bootstrap shape`, then `### Google Ads: the manager hierarchy`. The material was written as one
narrative when Google Ads was the only implementation, so provider changes fall MID-SECTION and
each one needs its own heading rather than a single split at the top.

That is what made this hard to get right: the first attempt added `### Meta` above and assumed
everything after it was Meta's; the second reopened Google Ads here and assumed everything after
THAT was Google's. Both are wrong in the same way — the boundary is wherever the subject changes,
not wherever a heading was last placed. Reading the rendered heading sequence catches it; a diff
of the inserted lines never will, because every inserted line is correct in isolation.

The manager-id check is duplicated on purpose. `Client.validateLoginCustomerID` still validates it
(the backstop for every other caller), but it does so inside the same call that talks to Google, so
by the time it fires the error is indistinguishable at this boundary from a genuine upstream
failure. `storedCustomerIDRE` in `internal/dispatch/googleads.go` therefore checks the STORED value
where it is READ, not where it is used — the check has to happen while the answer is still
classifiable. The two regexps must stay in step.

The Google Ads implementation goes via `Client.ListAccessibleCustomers`, which branches on mode
before it issues any request. In **direct mode** (no `login_customer_id`) it reaches
`customers:listAccessibleCustomers`. That endpoint is **account-agnostic** — it has no
`customers/{id}` path segment, unlike every other Google Ads call — and it is sent with a nil body
(so no `Content-Type`) and `idempotent=true` (a pure read, so retrying a 429 cannot double-apply
anything). In **manager mode** (`login_customer_id` configured) the flat endpoint is skipped
entirely: the call returns through `expandManagerHierarchy` first
(`internal/platform/googleads/client.go:1025-1027`), because a flat read whose rows are never
consumed could only add a way for discovery to fail.

**Discovery runs without an account id, deliberately, at both layers.** The call is
account-agnostic: it asks which customer ids the CREDENTIAL reaches, so an account id is not
a narrower version of the question, it is a different one.

Both lifecycles are now SUPPORTED. `GoogleAdsConnectionConfig` no longer declares
`Required("account_id")` (Meta joined it in LFXV2-3061, which added the `account_not_selected`
tagging its campaign create was missing — a credentials-first row needs both the discovery
endpoint and account-needing paths that name the missing choice), so
this endpoint serves BOTH re-pointing an existing connection ("which other customer ids does this
credential reach?") and first-time bootstrap:

```
POST   /projects/{id}/connection-google-ads          (credentials, no account_id)
GET    /projects/{id}/connection-google-ads/accounts (discovery)
PUT    /projects/{id}/connection-google-ads          (set the chosen account_id)
```

A connection in the intermediate state stays `status=active` and stores `account_id` as `""`.
That is not a loose end: `validateGoogleAdsCredentials` REFUSES a non-active connection, so any
"pending"-style status would make step two unreachable and dead-end the bootstrap it exists to
serve. `active` says the connection is ENABLED for credential-based operations, NOT that the
credentials were verified — nothing verifies them, so an active row can hold material the
platform will reject. Readiness to run a campaign is `account_id` being non-empty, and the paths
that need it say so with `ErrAccountNotSelected`.

The two preconditions below were relaxed for the endpoint's own semantics rather than for the
stored value, which is why the design change above was all that bootstrap additionally needed:

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

### Meta: the same bootstrap shape

**Meta shares the same bootstrap shape as of LFXV2-3061**, now that its own account-picker
endpoint (`GET .../connection-meta-ads/accounts`, LFXV2-3062) exists to complete it:
`MetaAdsConnectionConfig` no longer declares `Required("account_id")` either (`page_id` stays
required — it names a Facebook page the operator already controls, not something discovery
resolves). The credential-state checks that used to be inlined at each of `Dispatch`,
`ToggleStatus`, and `ReadMetrics` are now `resolveMetaCredentials`, a single helper in the shape
`validateGoogleAdsCredentials` established and `resolveRedditClient` adopted — active status,
decodable blob, non-empty access token, each
tagged with `domain.ErrConnectionNotUsable` plus its reason sentinel, and a `defer` that runs
every return path through `systemScoped`. That `defer` closes over a plain local bound once
(`conn := res`), NOT over the named return: every not-usable path returns a nil connection, and
`systemScoped` is a no-op on a nil receiver, so reading the named return there would silently
drop the system-row attribution from exactly the errors that need it. It deliberately does not check
`account_id`: that is `requireMetaAccountID`, called ONLY by `Dispatch`, right after credential
resolution, because campaign creation builds Graph paths as `/{accountID}/campaigns` and needs a
real account id (Meta has no discovery for `page_id`, the other prerequisite `Dispatch` checks).
`ToggleStatus` and `ReadMetrics` do NOT call it: both target an existing campaign by platform id
(`POST /{campaignID}`, `GET /{campaignID}/insights`) and never read `AccountConfig.AccountID` at
all, so requiring a selected account for either would refuse a perfectly servable pause/resume
or metrics-read on a connection whose account selection was later cleared via `PUT`.
`requireMetaAccountID` wraps an empty account the same way Google Ads does: `ErrConnectionNotUsable`
selects the status, `domain.ErrAccountNotSelected` supplies the `account_not_selected` reason
token, and it is matched before the general unusable-connection arm.

**A CONFIRMED default is the reason to reject this without discovery, not just the goal to
build toward it.** The Google Ads log entry documenting its own bootstrap states the general
rule as "an optional `account_id` and a discovery endpoint ship together or not at all" — the
worry being a connection an operator cannot finish from inside this API. For Meta specifically
that risk did not materialize: LFXV2-3062 (the discovery endpoint) was built as the deliberate
first half of this same two-PR sequence, so by the time `Required("account_id")` was dropped
here, the completion path already existed in review, and a generic `PUT
/connection-meta-ads` (every provider gets one via `connectionMethods`) lets an operator set
`account_id` manually even on a day the discovery PR is still queued behind reviewer bandwidth.

### Google Ads: the manager hierarchy

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

### Shared: credential resolution and decrypt classification

`creds.resolve` classifies each of its failure branches, and the splits are deliberate. A connection
row with an EMPTY credential blob is permanently unusable as it stands, so it carries
`domain.ErrConnectionNotUsable` (→ 400) alongside `domain.ErrCredentialsAbsent` for the reason
token — without that second sentinel the most trivially diagnosable state in the set logs as
`reason=unclassified`. Two branches do not:
`domain.ErrNotFound` means there is no connection at all (→ 404, and the caller should create one,
not edit one), and a repository failure is a genuine "try again later" (→ 503). Flattening either
into "not usable" would lose a distinction the service layer depends on.

**A decrypt failure is not one condition, and it splits again.** Only a blob the encryptor could not
even ATTEMPT to authenticate — `domain.ErrCredentialsMalformed`, for AES-GCM a ciphertext shorter
than a nonce PLUS the authentication tag (`Seal` appends `Overhead()` bytes to every message,
including an empty one, so anything shorter is provably truncated) — is proven bad ROW data, and
only that branch earns `ErrConnectionNotUsable` → 400. Getting that boundary wrong is not cosmetic:
a blob between the two lengths reaches `Open`, fails authentication, and is then classified as the
key condition below. A GCM AUTHENTICATION failure carries `domain.ErrCredentialDecryptionFailed`
instead: it means a wrong or rotated APPLICATION key, or tampering or corruption of that one row
(`internal/infrastructure/crypto/aesgcm.go` states both), and the tag check CANNOT distinguish them.
The blast radius therefore is not decided by the sentinel — a wrong deployment key fails every
project at once, one corrupted row fails only that row, and the COUNT of failures is what tells a
responder which. Reported as "not usable as configured" it would answer 400 to a whole deployment's
worth of operators, each told to go fix a connection that is fine, and would erase the 500 that is
the only signal a key rotation went wrong; answering 500 for one corrupted row over-escalates, which
is the recoverable direction. An unrecognized decrypt error takes the authentication path on
purpose: an `Encryptor` that proves nothing about the row must not be read as accusing it.

**Which defect it was is carried by a second sentinel, and the log line is why.** Alongside
`ErrConnectionNotUsable`, each stored-connection defect wraps one of
`domain.ErrConnectionInactive`, `ErrCredentialsAbsent`, `ErrCredentialsUndecodable`,
`ErrCredentialsIncomplete`, or `ErrProviderConfigInvalid`. The status is still decided by the one
sentinel; these only name the
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
dependency. Note the decrypt branches wrap BOTH a sentinel and the decrypt error (`%w: %w`), and
the service layer never returns that cause to a caller — but whether it LOGS it is a property of
the HANDLER, not of the resolver. On the campaign toggle and metrics handlers, as of LFXV2-3065,
neither arm logs it. Authenticated-decryption failure (`ErrCredentialDecryptionFailed` → 500)
previously logged the cause, on the reasoning that the error is constructed by the encryptor from
ciphertext and key material only; that holds for the SENTINEL, but the whole chain is what reaches
the log and `domain.Encryptor` is a PORT whose implementations may quote the ciphertext or key
material they failed on. Malformed ciphertext reaches `ErrConnectionNotUsable`, which these two
handlers answer with 409 (account discovery classifies the same sentinel as 400 for its own
caller); that arm has always suppressed the cause and logs `reason=credential_blob_malformed`
alone, because the conditions on it include one detected by decoding the DECRYPTED blob. Those two handlers
are pinned by `Test{ToggleCampaignStatus,GetCampaignMetrics}_DecryptFailureLogsNoErrorText`.
Account discovery (`internal/service/connection.go`) still logs the full cause on its 500 arm and
is not covered by those tests, so this is a per-handler property rather than a service-wide
guarantee — see `internal-service.md`.

### HubSpot: email search, not account discovery

`HubSpotDispatcher.SearchEmails` implements `service.EmailSearcher`, a capability that sits
alongside `AccountLister` rather than inside it. The distinction is the point.

**There is no account to discover.** A HubSpot connection is scoped to the portal its
private-app token authenticates against — `Client.AuthenticatedPortalID` reads it back from the
token itself, not from the optional operator-supplied `portal_id`, which a credential swap
leaves untouched. So the question every ad platform's discovery answers ("which account may this
credential act as?") has no HubSpot analogue.

**What has no default is the TEMPLATE.** `Dispatch` stages an email by CLONING a caller-specified
source (`hubspotConfig.SourceEmailID`, required, no default), so without a way to find one the
channel cannot be driven from a UI at all. That is a per-campaign choice travelling in the
dispatch config, not a per-connection one stored on the row — which is why `MarketingEmail` is
its own model type rather than a reuse of `AccessibleAccount`. Sharing the type would only make
two unrelated lifetimes look interchangeable.

**The status mapping IS shared, deliberately.** `classifyDiscoveryError` was lifted out of
`listAccounts` unchanged so both endpoints answer 404/400/500/503 from one switch — the helper's
own contract is that a provider gets the judgements reasoned about there or none of them, and a
second copy is where one of them quietly diverges. Only the operation noun differs, carried by
`accountDiscovery.operation`: telling a caller who searched for an email template that "account
discovery could not be completed" describes an operation they did not perform.

One arm is NOT shared. A dispatcher with no `EmailSearcher` yields `ErrEmailSearchUnsupported`,
a separate sentinel from `ErrAccountsUnsupported`, because the two capabilities are genuinely
independent: HubSpot searches emails and has no ad accounts, while Google Ads and Meta are the
reverse — they are the only `AccountLister` implementors, and the remaining ad platforms
(LinkedIn, Reddit, X, Microsoft) implement neither capability. Folding the two sentinels into one
would make "this platform cannot do X" ambiguous about which X.

**Draft emails are returned, with their state — archived ones are absent.** Same reasoning as Meta's disabled
accounts: filtering the row the user is looking for answers "your portal has no such email"
about an email sitting right there. The caller gets `state` and decides — a DRAFT is the case
worth surfacing, since cloning an unfinished template is the mistake a picker can prevent.

Archived rows are a different matter and cannot be warned about at all: they are absent from the
result, so there is no row to carry a state. Anyone looking for one will not find it here, and
the endpoint has no way to say why — which is the honest limit of this contract rather than
something `state` can express.

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

## The system account is a connection row, not a second mechanism

A project that has connected no ad account of its own dispatches through the LF-owned system
account: an ordinary connection row at the reserved scope `model.SystemProjectID`
(`system:linuxfoundation`), not an `LFX_SYS_*` env block, because a system account needs exactly
what a project account needs — encryption at rest, an account id, provider config, a status, an
`If-Match` version, an `updated_by` trail.

**Only a genuine absence falls back, and that asymmetry is the whole safety argument.** A repo
error, an empty blob, a decrypt failure each mean the project HAS a connection needing attention,
and running its campaign on the LF account spends LF money on a request the project believed was
billed to itself. `resolveConn` is shared, so a failed FALLBACK lookup is not an absence either.

**And `ErrNotFound` is not by itself a genuine absence.** `Delete` SOFT-deletes
(`status = 'deleted'`) and `ConnectionRepo.Get` filters those rows out, so a project that
deliberately DISCONNECTED its ad account arrives at the fallback wearing the same sentinel as one
that never connected — and the branch above reads that as licence to spend LF budget on its
campaigns. Absence of a statement is what this fallback is for; a statement to the contrary is
not absence. `connReader.Disconnected` is the probe that separates them, and it sits on the
INTERFACE rather than behind a type assertion so a reader that cannot answer fails to compile
instead of silently inheriting the fallback the assertion's else-branch would give it. A probe
that ERRORS fails closed: an unanswered "was this disconnected?" is not a no, and failing open
there would restore the whole defect on any database blip.

**Origin and classification are two questions, and one sentinel cannot answer both.**
`ErrSystemConnectionNotUsable` answers "who must fix this" and only rides along with
`ErrConnectionNotUsable`, so a failure it does not classify — a blob that fails authenticated
decryption — reached the service layer indistinguishable from the same failure on the caller's own
row. That arm logs a project id at ERROR and asks whether one row or every connection is broken;
naming the caller sent whoever was paged to inspect a row that project does not have, and N
projects failing over ONE corrupt system row read as N failing rows, which is the deployment-wide
conclusion the arm is written not to assert. `ErrSystemConnectionOrigin` is wrapped onto every
error the fallback produces, at the single site that knows the fallback was taken.

No create endpoint can plant a row at the reserved scope (`projectSlugProblem` rejects it), and
`rejectSystemScope` closes get/update/delete/test/set-credential — which stay permissive on
`project_id` for historical UUID rows — at the shared helpers in `connection_handler.go`,
answering 404 not 403. **A choke point only covers what passes through it**: account discovery is
a SEVENTH endpoint taking a caller-supplied `project_id` and carries the guard inline; left open,
a `GET` there enumerates the LF's own accounts.

Nothing can install that scope over HTTP, so the installer is a REQUIRED part of the feature: the
`bootstrap-system-account` subcommand (`cmd/campaign-service/sysacct.go` → `internal/bootstrap`),
not a second binary, since ko publishes only `cmd/campaign-service`, resolving its DSN through
`config.ResolveDatabaseURL` because the chart injects `PG*` and leaves `DATABASE_URL` unset. It
reads the credential from stdin (a flag lands in shell history and every `ps`), and only
`ErrNotFound` may create. A rotation is ONE write — `UpdateWithCredential`, gated on the row's
version — because account id, config and credential must not be separately observable by
dispatch. As two writes there was no safe order: a crash between them left either the new account
carrying the old credential or the new credential on the old account, and two concurrent runs
could commit one run's account beside the other's because `SetCredential` is not version-gated. A
losing writer now gets `ErrPreconditionFailed` and is told nothing was written; the command stays
idempotent, so rerunning it converges.
Keys are folded because stored blobs and dispatch structs are both untagged, so `encoding/json`
falls back to a case-insensitive match that cannot bridge the underscore in the documented
snake_case wire form. The config an adapter refuses to create without (LinkedIn `org_id`, Meta
`page_id`, X `funding_instrument_id`) is required of the map about to be WRITTEN — on rotation the
existing columns MERGED with the flags, since `Update` rewrites every config column.

## `CampaignAdopter` (optional capability)

`CampaignAdopter` is a fourth OPTIONAL dispatcher interface, alongside `StatusToggler`,
`MetricsReader` and `AccountLister`, declared in `internal/service/orchestrator.go` and
discovered by the same type assertion. A dispatcher that does not implement it makes the
platform answer `ErrAdoptionUnsupported` (400) with no network call. **Google Ads is the
only implementation today.**

```go
LookupCampaign(ctx, projectID, platform, platformCampaignID) (*model.PlatformCampaignRef, error)
```

The contract has one rule that matters more than the signature: **`(nil, nil)` means the
platform answered and the campaign is genuinely absent.** Anything an adapter could not
verify — a transport failure, an unhonoured filter, an undecodable row, a status outside the
known set — must be an ERROR, because the service turns absence into a 404 and an operator
acts on a 404 by creating a duplicate paid campaign. Never reduce an unverifiable response to
a clean absence (the `continue`-on-mismatch shape: skipping every non-matching row yields zero
matches, exactly the licence-to-create answer the check existed to prevent).

`GoogleAdsDispatcher.LookupCampaign` resolves through `resolveOwnedGoogleAdsClient`, which is the
ordinary `resolveGoogleAdsClient` with the LF system fallback removed: it calls
`credsSource.resolveOwned`, which consults the project's own scope and nothing else, and reports
the resulting absence as `domain.ErrAdoptionRequiresOwnConnection` (409). Two separate isolation
problems sit behind that. Discovery credentials see every account the login
customer administers, so adopting through them could bind a campaign belonging to a different
project — which is why this is not the discovery client. And the system fallback puts MANY
projects inside ONE LF-owned ad account, where an endpoint that takes a caller-supplied arbitrary
campaign id lets project A bind, meter and pause a campaign project B created there; the
account-mismatch guard cannot see it, because both projects resolve to the same customer id.
No upstream metadata settles ownership either — a campaign's name, labels and budget are set by
whoever created it. Requiring a project-owned connection is what this layer can enforce, and it
forbids nothing real: a project with no ad account of its own has no campaign to adopt. It is not
an ownership PROOF, and must not be read as one — inside a shared customer, a project holding its
own connection can still name a campaign another project created. Nothing here can prevent that,
because the project's credential already confers read and pause on every campaign in that customer
straight through the provider's API, and adoption cannot be more restrictive than the credential it
uses; account tenancy is where that boundary lives (see migration 000020). What the gate does
guarantee is narrower and still worth having: adoption never borrows the LF fallback, so it can
never reach an account the project has no credential of its own for. Every
OTHER platform call keeps the fallback, because each names a campaign this service already has a
project-scoped row for, and that row is the authorization.

Declining to RESOLVE the fallback, rather than resolving it and rejecting a `resolved.fromSystem`
value, is load-bearing rather than stylistic. `resolve` loads, validates and DECRYPTS the LF row
before returning, so an LF connection with no credential blob — or one that no longer decrypts —
comes back as `domain.ErrSystemConnectionNotUsable` INSTEAD of a value, and the ownership gate
never runs. Under the earlier `fromSystem` shape that surfaced as a 500 blaming an LF row for a
request whose remedy was "connect your own ad account", and about a row adoption would have
refused in perfect health. `resolveOwned` makes the refusal independent of the fallback's state,
so no future failure mode of the system scope can leak onto this path and need a new sentinel arm.
`TestAdoptionRefusesTheSystemFallback` pins the outcome for an unusable system row AND, more
strongly, that `Get(model.SystemProjectID)` is never called at all — the second assertion is what
keeps the first true for failure modes nobody has thought of yet.

It drops the platform's `ENABLED`/`PAUSED` — reaching the mapping already means the
campaign is live, since `googleads.GetCampaign` filters `REMOVED` server-side and errors on any
status outside its known set — and it fills `PlatformCampaignRef.Result` with the resolved
customer id, so the adopted row records the account it was verified under and the existing
`googleAdsCreationCustomerID` mismatch guards keep working for adopted rows. A
`googleads.ErrNotACampaignID` is re-tagged `domain.ErrInvalidPlatformCampaignID` (400): it was
rejected locally with no network call, so it is permanent input, not an unreachable platform.
**Campaign ID validation happens before resolving the connection, so a malformed ID always returns
400 regardless of connection state — the permanent input fault masks any contingent connection fault.**

It also establishes the SLOT the adopted campaign occupies. The adopt request names no campaign
type, so the only evidence is what the platform reports: the lookup selects
`campaign.advertising_channel_type` and `googleAdsVariantForChannelType` maps it onto
`PlatformCampaignRef.Variant`, which the adopt path persists. Before this, every adopted Google
campaign was stored as `default` whatever it was — so adopting a Demand Gen campaign left the
`demand-gen` slot free and the next Demand Gen dispatch created a SECOND paid campaign for the
same brief. The mapping fails CLOSED: only the types this service can create are mappable, and
`PERFORMANCE_MAX`, `VIDEO`, an unrecognised future value or an absent field are refused rather
than defaulted, since defaulting is what produces the duplicate. An adapter that returns an empty
variant is refused by the service layer too, so a contract violation cannot fall back to `default`.

The Google dispatcher resolves the CHANNEL before it composes the campaign name, before adoption
looks anything up, and before any create. Each of those depends on which campaign type is being
dispatched, and doing it last meant all three ran assuming Search: `adoptExisting` on a Demand Gen
dispatch searched for the Search campaign's name, and an unsupported channel was refused only
after adoption could already have bound a campaign. `googleads.CampaignKindSearch` and
`CampaignKindDemandGen` are exported so the dispatcher composes the same name the client writes.

