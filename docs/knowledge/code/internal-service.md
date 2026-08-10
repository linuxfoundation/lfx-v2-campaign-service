---
type: "Go Package"
title: "internal/service"
description: "Campaign service business logic, including Readyz (DB-backed readiness) and Livez (process-only liveness)."
resource: "internal/service"
---

# internal/service

Package service contains the campaign service business logic, including the
implementation of the generated Goa service interfaces.

`GET /readyz` ANDs an optional `ReadinessChecker` (the PostgreSQL pool when
wired) into readiness with a ~2s timeout and returns `503` when the dependency
is unhealthy. `GET /livez` remains process-only so database outages do not
trigger Kubernetes restarts.

`AudienceService` implements the built-audience endpoints (the "B2" resource,
epic LFXV2-2770): create/get/list/update of a `campaign_audiences` row subordinate
to a brief. An audience is a POINTER + provenance to a platform-side audience (a
HubSpot master contact list), NOT its contents. Update is optimistic-concurrency
gated on `If-Match` (same strong-validator parsing as briefs); the ETag mirrors the
row version. Like the other services it late-binds via `SetBackend` after a
cold-start DB retry and returns a typed `503` (routes mounted) when no repo is wired.

## Event URL metadata (LFXV2-3043)

`FetchEventURL` (`event_url.go`) fetches an event page and returns the metadata extracted
from it, for pre-filling a brief form. It bridges `internal/platform/eventurl`'s fetcher and
parser to the API surface.

**It creates and persists NOTHING.** The caller reviews what was extracted and submits it
through the ordinary `create-brief`. There is no `EventDetails` → `CampaignBrief` mapper in
this service, and the absence is deliberate: a page's metadata is a *suggestion* to a human
authoring a brief, not a brief. Writing the mapper before a caller existed produced code that
compiled, passed and linted while being reachable from nothing, so it was removed rather than
shipped. Add it with the caller that needs it, not before.

- The collaborators are injected by `SetEventURL`, separately from `NewBriefService`, for the
  same reason as `SetIndexer` — the ~40 existing constructor call sites keep compiling, and a
  `BriefService` without them still serves every other method.
- Because it consults no repository, it stays available during the cold-start window before
  the database binds. Its `503` covers only the case where the fetcher itself was never wired.
- An absent field is a nil pointer, never a pointer to `""`: "the page did not say" and "the
  page said nothing" are different answers to a UI deciding whether to leave a field free.
- `mapEventURLErr` maps the `eventurl` sentinels onto the advertised errors. A forbidden
  address is `400`, not `403` — nothing about the caller's permissions is at issue; the URL
  they supplied names an address this service will not connect to.

`BriefService` implements brief CRUD and campaign endpoints. `FindBrief` looks a brief up by
`(project_id, event_slug)` rather than by id — the key a caller holds when re-visiting an event
page — returning `ErrNotFound` when the event has no brief yet. That 404 is an ordinary
outcome (first-time generation), not a failure. It never generates or mutates: regeneration is
an explicit `UpdateBrief`, so edits to the AI-generated copy are never silently overwritten. Campaign creation
(`CreateCampaigns`) requires an approved brief, rejects empty and duplicate
platform sets (a duplicate would create two paid upstream campaigns), then hands
off to the `Orchestrator`, which persists a job and dispatches per platform
asynchronously (bounded concurrency). Dispatch is idempotent: a brief already
carrying a COMPLETED campaign for a platform is reused rather than re-created. The
idempotency fast-path lookup (`GetCampaignByPlatform`) distinguishes its outcomes: an
existing campaign with an upstream id AND a terminal status (`created` /
`created_degraded`) short-circuits to reuse; a `pending` row — even one carrying an
upstream id or a Result reconcile blob — is a retained partial ORPHAN, not a
completed campaign, so it does NOT short-circuit (on retry it is reported as
reconciliation-required rather than a false success); `ErrNotFound` (no row yet) falls
through to `ClaimCampaignDispatch`; but a REAL DB error (anything else) is surfaced as
a platform failure (logged at ERROR) rather than silently treated like "no existing
campaign" — proceeding to
claim/dispatch when an existing campaign merely couldn't be loaded could duplicate an
upstream create, so it fails loud instead. Replacing a brief's
content resets it to `draft` (re-approval required). Optimistic concurrency is enforced via
version/If-Match (`428` when missing, `412` on mismatch).

Dispatch is durable (LFXV2-2665): single-flight per (brief, platform) is
enforced by an atomic claim — `ClaimCampaignDispatch` does INSERT ... ON CONFLICT
DO NOTHING of a `pending` campaign row, so exactly one worker across replicas
wins the claim (the unique index arbitrates) with no held connection or blocking
lock. A worker that loses the claim reuses the existing row instead of dispatching
again; the pending row also survives an upstream-create-then-crash, making the
orphaned upstream campaign recoverable. The orchestrator tracks in-flight runs
and its `Shutdown` drains them (bounded) before the DB pool closes, and on
startup jobs left non-terminal beyond a staleness cutoff are failed-forward (they
cannot be safely resumed without provider idempotency keys).

## Campaign status toggle

`BriefService.ToggleCampaignStatus` (backing `PATCH .../campaigns/{id}/status`
{active|paused}) pauses/resumes a campaign ON THE PLATFORM, then persists. Unlike
`UpdateCampaign` (DB-only), the platform call happens FIRST via
`Orchestrator.ToggleCampaignStatus` → the platform's `StatusToggler`; the DB row is written
only after the platform confirms. Only a fully-created campaign (`created`, or one already `active`/`paused`) may be toggled
(`model.CampaignStatusToggleable`); a `pending` ambiguous orphan or a `created_degraded`
campaign is rejected 409 — toggling one would activate an incomplete campaign and/or
overwrite its reconciliation marker with the run state (a non-empty `PlatformCampaignID`
alone is not sufficient, since a partial/degraded campaign can carry an upstream id).
Write ownership of the row is claimed via `CampaignRepo.ClaimCampaignVersion` BEFORE the
paid platform call, not by comparing the read-time version in memory (LFXV2-2901): an
in-memory comparison only rejects a stale caller, it does nothing to stop a SECOND
caller that read the same version from also passing its own check and also calling
the platform, so two toggles — or a toggle and `UpdateCampaign` — could both mutate
the platform before either persisted.

**The claim is a LOCK, not a version bump.** `ClaimCampaignVersion` takes a Postgres session
advisory lock (keyed by campaign id, on a dedicated pooled connection), reads the row at
`expectedVersion`, and leaves the version UNCHANGED. The increment happens later, in
`ReplaceCampaign`, inside the same transaction that writes the outbox event — which is what
preserves the invariant that every campaign write co-commits its index event. An earlier
design bumped at claim time (`UPDATE ... SET version=version+1 WHERE version=$expected`) and
could not hold that invariant; do not reintroduce it. Because ownership is the lock and not
the version, it survives the caller's external I/O: a second writer is REFUSED for the whole
duration of the first one's platform call instead of racing between the claim and the
post-platform persist. Refused, not queued — the claim is `pg_try_advisory_lock`, so the
loser gets `ErrCampaignWriteInProgress` immediately and the service returns a retryable 409.
Nothing resumes after release; the client retries as a new request. Queuing was rejected
because a waiter would pin a pool connection for the length of the holder's platform call. Callers MUST release via
`ReleaseCampaignLock` (deferred), and the not-found vs. stale-version classification is made
while the lock is still held — releasing first would let a concurrent delete turn a
stale-version caller's 412 into a 404. `UpdateCampaign`, which has no I/O between its read
and its write, claims first for the same reason — without it, its single-statement
`ReplaceCampaign` could win the row out from under an in-flight toggle's claim,
losing the toggle's own post-platform persist even though each write was
individually consistent. Both handlers validate BEFORE claiming, so a request that will be
rejected anyway never takes the lock and never blocks legitimate writers. A stale
`If-Match` fails BEFORE the paid platform call;
failures are classified (`ErrCampaignNotProvisioned` → 409 — a campaign with no upstream id yet,
OR one that on ACTIVATE lacks the child ad group/ad ids needed to serve, so not every 409 means
an unfinished create; `ErrToggleUnsupported` → 400, an UNCONFIRMED outcome → 503 "verify before
retrying", a definite platform failure → 503 "not modified") rather than all blamed on the
platform. An
UNCONFIRMED outcome is a transport/5xx/redirect error the PATCH may have applied — the client
exposes it via `reddit.IsOutcomeUnconfirmed`, the dispatcher wraps it in an error whose
`Unconfirmed()` reports true (same behavioral-interface pattern as `NoUpstreamCreate`), and
the handler surfaces it without lying either way and without writing the row. The post-platform
`ReplaceCampaign` runs on a `context.WithoutCancel` context BOUNDED by `persistResultTimeout`,
so the row can't diverge from the platform if the request is cancelled after the PATCH commits
and a stuck DB can't hang shutdown; a persist failure after the platform changed is logged as a
divergence reconcile signal.

**Actor attribution:** The toggle records WHO performed it (the person who paused/resumed the campaign)
in the `updated_by` column of the row, capturing the actor from the request context BEFORE the
detached context persists the row. A system-initiated toggle (e.g., a scheduled remediation, no authenticated
principal) records no actor rather than substituting a stand-in — the campaign's creator, a literal
"system" — because "not recorded" is a distinct state from naming a principal that never acted.
Note what the repo then does with that nil: `replaceCampaignQuery` writes
`updated_by=COALESCE($9, updated_by)`, so an unattributed toggle LEAVES the previous mover in
place rather than clearing the column. Nil means "this write records nothing", not "forget what
you knew" — the column only reads NULL if no attributed write ever reached the row.
The `created_by` column (the person who authorized the spend) is never touched
by a toggle. See `campaign_actor_test.go` and the `000016` migration (campaigns' actor columns).

## Campaign metrics read

`BriefService.GetCampaignMetrics` (backing `GET .../campaigns/{id}/metrics`) reads live
performance metrics (impressions, clicks, cost, CTR) directly from the campaign's ad
platform. Unlike `GetCampaign`, this is a pure read — `model.CampaignMetrics` is never
persisted, so there is no `If-Match`/version to check, mirroring `ToggleCampaignStatus`'s
always-live-not-cached semantics. `Orchestrator.ReadCampaignMetrics` type-asserts the
platform's dispatcher for the optional `MetricsReader` capability at call time (the same
optional-capability pattern as `StatusToggler`) rather than requiring every dispatcher to
implement it; a dispatcher that isn't a `MetricsReader` — or a platform with no dispatcher
registered at all — returns `ErrMetricsUnsupported` (400) without ever contacting the
platform. An unprovisioned campaign (`PlatformCampaignID` empty, or `campaign == nil`)
returns `ErrCampaignNotProvisioned` (409) before any platform call, same as the toggle. A
connection the dispatcher refuses BEFORE contacting the platform — `ErrCampaignAccountMismatch`,
`ErrAccountNotSelected`, `ErrConnectionNotUsable` — is also a 409 (see the classification
section below). Everything else propagates as-is (503) — a read has no ambiguous mutation to
protect, so there is no UNCONFIRMED classification here. The call is
bounded by `metricsCallTimeout` (20s, distinct from `toggleCallTimeout`'s 45s — reads should
fail fast rather than hold a request open).

The `window` query parameter is a closed, platform-agnostic vocabulary
(`model.MetricsWindow`: `today`, `yesterday`, `last_7_days`, `last_14_days`,
`last_30_days` [default], `this_month`, `last_month`) — never a platform's own dialect
(e.g. Google Ads' GAQL `DURING` literals, Meta's Insights `date_preset`). Each platform's
`MetricsReader` adapter is responsible for mapping this vocabulary to its own platform's
query syntax; the mapping (and any platform-specific validation, e.g. an allow-list guard
against GAQL injection) lives in the platform client package, not here.

When the caller omits `window`, `defaultMetricsWindowFor` (`internal/service/brief.go`)
picks the default PER PLATFORM rather than applying one global constant: `last_30_days`
for every platform except X Ads, which defaults to `last_7_days` because its stats
endpoint caps queryable date ranges at 7 days per request — `last_30_days` is simply
unreachable there (see `internal/platform/twitter` and `internal/dispatch/twitter.go`
below). A single global default would make every omitted-window request against an X
campaign fail with a guaranteed 400.

## Account discovery

`ConnectionService.ListGoogleAdsAccounts` (backing `GET .../connection-google-ads/accounts`)
and `ListMetaAdsAccounts` (`GET .../connection-meta-ads/accounts`) enumerate the ad accounts
reachable UPSTREAM with the connection's stored credential, so an operator can pick one instead
of pasting an account id by hand.

**Both handlers are three lines over one `listAccounts` helper**, parameterized by an
`accountDiscovery{provider, displayName, notUsableRemedy}` value. The mapping below encodes
several judgements that are individually easy to get wrong — 404 rather than 503 for a missing
connection, a 500 that logs but never echoes a decryption failure, a 400 rather than 503 for a
connection no waiting will fix — and a second copy is where one of them quietly diverges. What
IS per-provider is the caller-facing text: Meta's remedy names `access_token`, Google's names
`login_customer_id`, and pointing the second handler at the first's `accountDiscovery` would
tell a Meta operator to check a field their connection does not have.
`TestListMetaAdsAccounts_MessagesNameMetaNotGoogleAds` is the test for exactly that, because
every status-code assertion passes with the wiring wrong. It is a live read on the same
never-persisted discipline as `GetCampaignMetrics`, and `Orchestrator.ReadAccounts` uses the
same optional-capability pattern: it type-asserts the platform's dispatcher for `AccountLister`
at call time and returns `ErrAccountsUnsupported` (400) without contacting the platform when
that capability isn't wired.

Note what it does NOT list: anything this service stores. A project holds at most one connection
per provider, read via `GET .../connection-{provider}`.

Five outcomes are distinguished deliberately, because collapsing them misdirects the caller.

- `ErrAccountsUnsupported` → **400** — the platform has no discovery capability.
- `domain.ErrNotFound` → **404** — the project has no stored connection. A setup state; a 503
  here would tell the caller to retry something that cannot succeed until a connection exists.
- `domain.ErrCredentialDecryptionFailed` → **500** — a well-formed credential blob failed
  AUTHENTICATED decryption, which means a wrong or rotated application encryption key, or tampered
  or corrupted data — and GCM's tag check CANNOT tell those apart. This arm sits ABOVE the
  `ErrConnectionNotUsable` one and is checked first on purpose: a wrong deployment key would fail
  every project's connection at once, and a 400 would send each of their operators to go fix a row
  that is fine, while a 503 would promise that waiting helps. That does NOT mean this status
  asserts an outage — one tampered or corrupted row produces exactly the same failure, and which
  it was is answered by the COUNT of failing projects, not by the status. The 500 is chosen because
  the ambiguity is asymmetric: over-escalating one bad row is recoverable, under-reporting a broken
  key is not. It logs at ERROR because it is the arm that should page someone, and the cause IS
  logged here — it is produced by the encryptor from ciphertext and key material only, never from
  plaintext.
- `domain.ErrConnectionNotUsable` → **400** — the connection EXISTS but cannot be used as it
  stands: inactive, an incomplete or undecodable credential blob, or a malformed stored config
  value such as a dashed `login_customer_id`. The platform is never contacted. This arm is what
  keeps the 503 below honest: a 503 promises that waiting might help, and none of these conditions
  change until a human edits the connection. The distinction cannot be made here — a setup failure
  and an upstream one arrive as the same type — so `internal/dispatch/googleads.go` wraps the
  pre-send failures with the sentinel and this arm reads it. The wrap has two owners:
  `validateGoogleAdsCredentials` tags the credential-state three (inactive, undecodable,
  incomplete), which is why they reach callers beyond discovery — but the SHAPE they reach them in
  depends on whether the caller is synchronous. The **status toggle** and the **metrics read**
  (`internal/service/brief.go`) are synchronous, so they answer a **409** off this same sentinel.
  **Campaign create is not**: it answers `202` and the identical failure surfaces later in the
  polled job result, never as a 409 — see `docs/api-catalog.md`. Do not describe "campaign
  dispatch" as receiving a 409; the dispatch layer produces the error, and only the two
  synchronous readers turn it into a status code.
  `resolveGoogleAdsDiscoveryClient` tags the dashed `login_customer_id`. Neither the cause NOR its text leaves this function — not in the response and not in
  the log line. One of the wrapped errors is computed over the decrypted credential blob, and
  `encoding/json` quotes its input, so logging the cause would put credential-derived bytes into
  centralized logs for exactly the connection whose credentials are malformed. What the log line
  carries instead is `reason=`, from `unusableConnectionReason` — a fixed token
  (`connection_inactive`, `credentials_absent`, `credentials_undecodable`,
  `credentials_incomplete`, `provider_config_invalid`, `credential_blob_malformed`,
  `account_not_selected`, `unclassified`) read off the reason
  sentinel the dispatch layer wraps alongside `ErrConnectionNotUsable`. A closed vocabulary is what
  a log line wants anyway: greppable, alertable, and with no payload to carry a secret in.
- Anything else → **503** — the platform was reached and did not answer.

**`account_not_selected` is the one reason in that vocabulary that is not a fault.** Every other
token describes something WRONG with stored state; this one describes state that is merely
UNFINISHED, and it is the only one a caller reaches by doing exactly what the API told them to
do. It is NAMED because `GoogleAdsConnectionConfig` dropped `Required("account_id")` to allow the
credentials-first bootstrap (`design/connection.go`): a connection can now be created with
credentials alone, discovery run against it, and the chosen account PUT back afterwards.

It was not, however, previously impossible — and the distinction matters, because the guard that
produces this token predates the bootstrap and used to return a bare error. Goa's `Required`
checks that the JSON KEY is present; the generated validator was `if body.AccountID == nil`, and
the Go field is a plain string, so `"account_id": ""` (or whitespace) satisfied it and was stored.
An account-less row was reachable all along as an unintended, undocumented state. What this change
did was turn it into a supported, omission-based lifecycle state — and, in doing so, make the
mis-classified 503 the common case rather than a latent one.

Note that `status=active` on such a connection is deliberate, not a gap in the lifecycle.
**`active` says the connection is ENABLED for credential-based operations — it does not say the
credentials were verified.** Nothing verifies them: `createConn` serializes, encrypts and
persists exactly what was supplied, and `testConn` says so itself (upstream verification is not
implemented), so an active row can hold OAuth material the platform will reject. What `active`
buys is reachability — `validateGoogleAdsCredentials` refuses a non-active connection, so a
distinct "pending" status would make discovery unreachable for exactly the connections that need
it, and the bootstrap would dead-end at step two. Readiness to run a campaign is a separate,
derived fact — `account_id` being non-empty — and the operations that need it report its absence
with this reason rather than inventing a second status to carry the same bit.

**Only the two campaign handlers see this sentinel, and both answer 409.** The campaign status
toggle and the per-campaign metrics read each match `ErrAccountNotSelected` *before* the general
`ErrConnectionNotUsable` arm — it is always wrapped alongside that sentinel, so a broad match
would swallow it and return the ambiguous "no account selected, or the credentials need
attention" message for a connection whose credentials are fine. The CAMPAIGN is the resource
there, and an unfinished connection is a precondition conflict — the same classification those
handlers already give `ErrCampaignNotProvisioned`. Non-retryable is the property a client acts
on, and the one the original code got wrong: before this sentinel existed the empty-id guard
returned a bare error, which fell through to each handler's `default:` arm and answered
**503 "the platform did not respond"** — for a platform never contacted, with a remedy (wait)
that can never work, since only a human choosing an account changes the state.

**Account discovery does not map it at all.** Discovery calls `validateGoogleAdsCredentials`,
not `validateGoogleAdsConnection`, so the account-id check never runs: accepting an account-less
connection is exactly what makes the bootstrap work, since discovery is how the operator finds
the account to select. Discovery's 400 is reserved for its *other* unusable states (inactive,
credential blob absent/incomplete/malformed, provider config invalid).

The distinction is carried in the response **message**, not a field. `ConflictError` is a shared
Goa type with exactly `code` and `message`, so exposing a machine-readable `reason` would mean
changing a type every 409 in this service returns; the reason token reaches operators through
the log instead.

Two DIFFERENT guards protect the empty-vs-nil distinction, and they fail in opposite directions —
document them separately so a future change preserves each for its own reason:

1. **A nil from an `AccountLister`** is a contract violation, and `ReadAccounts` maps it to 503.
   A credential that legitimately reaches zero accounts must return an EMPTY list, so a nil from
   a dispatcher means the dispatcher did not answer, not that the answer was "none".
2. **A nil introduced LATER, by the service layer's own conversion loop**, never reaches that
   guard — it is created after it. Nothing rejects it, so it serializes as a successful `null`
   response body: clients were promised a list and get a null instead.

Hence the rule that covers both: every layer builds its slice with `make(..., 0, n)`. Guard 1
turns a missing answer into 503; guard 2 does not exist, which is exactly why the construction
convention has to hold on the service side rather than being checked there.

Both cold-start guards on this handler return 503 but mean different things:
`resolveBackendWithOrch` checks the repo first ("connection storage is unavailable") and the
orchestrator second ("account discovery service is unavailable"). A test that leaves BOTH unset
only ever exercises the first.

## Campaign delete

`BriefService.DeleteCampaign` (backing `DELETE .../campaigns/{id}`, `If-Match` required)
SOFT-deletes a campaign locally. Its purpose is slot recovery: the `(brief_id, platform)`
uniqueness that makes dispatch idempotent also meant a campaign row occupied its brief's
slot for that platform permanently, so a wrong-budget campaign or an ambiguously-failed
upstream create blocked the pair forever. The partial unique index from `000010` excludes
deleted rows, so deleting frees the slot for a legitimate re-dispatch.

**It never touches the ad platform, by design.** No provider client in
`internal/platform/*` implements a campaign delete/remove call, and inventing an
unverified one is worse than not offering it: removing the local row while a real paid
campaign keeps spending is the worst available outcome. The API description says so
explicitly — a campaign already created upstream keeps running until it is stopped there
(pause it via the status toggle first). That is also why the delete is SOFT: the retained
row holds `platform_campaign_id`, the only local pointer to whatever may still exist
upstream, so a hard delete would free the slot but destroy the sole record needed to find
and stop the campaign still spending.

Guards, evaluated under a `SELECT … FOR UPDATE` row lock in one transaction: absent or
already-deleted → `ErrNotFound` (404, matching `GetCampaign`, which hides deleted rows);
mid-dispatch `pending` → `ErrConflict`, re-mapped by the service to an ACTIONABLE 409
("being dispatched … wait and retry") rather than `mapBriefErr`'s generic "already
exists"; then the version check → 412. The version check runs LAST so a stale ETag on a
dispatching campaign reports the dispatch, not a misleading "reload and retry". The row
lock is required, not decorative: a `pending` row is an active dispatch claim, and under
READ COMMITTED a plain guarded `UPDATE` cannot see a claim that commits just before the
statement (and the claim INSERTs rather than updates, so there is no row conflict to
serialize on) — deleting under an in-flight dispatch could let a concurrent claim
double-create upstream.

## FetchEventURL does not consult the repositories

`event_url.go` holds the one brief-service handler that does not call `ready()`. It needs
a fetcher and a parser, not a database, so it stays available through the cold-start
window when the backend has not yet bound — and its own 503 therefore means only "the
fetcher was never wired", which is a configuration fact rather than a transient one.

The collaborators arrive through `SetEventURL` for the same reason `SetIndexer` exists:
the ~40 `NewBriefService` call sites (nearly all tests) must keep compiling. The
difference from the indexer is that there is no Noop stand-in — a fetcher that silently
did nothing would report "no event details" for a page that is perfectly fine — so the
handler checks for nil and reports unavailable instead.

`EventFetcher` is narrow on purpose. `eventurl.NewFetcher` is the only constructor in
non-test code, so nothing can reach this seam with an unguarded HTTP client; the interface
exists so tests need no listening socket, not so the SSRF guard becomes swappable.

`mapEventURLErr` matches with `errors.Is`, because `eventurl` returns a multi-unwrap error
carrying both a sentinel and a redacted cause — a type switch sees only the wrapper. Its
default branch returns a FIXED message rather than formatting the cause: `eventurl` builds
URL-free messages because they are rendered to callers and to logs, and an unrecognized
error is exactly the one whose text nothing vouched for. Forbidden maps to 400 and not
403: nothing about the caller is at issue, so 403 would send an operator to look at tokens.

See [internal/service](../../../internal/service).
