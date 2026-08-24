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
asynchronously (bounded concurrency). The orchestrator records dispatch outcomes,
job state transitions and upstream platform latency through the `DispatchMetrics`
interface — declared in THIS package rather than importing
[internal/infrastructure/metrics](internal-infrastructure-metrics.md), so orchestrator
tests do not drag in a Prometheus registry, and injected via `SetMetrics` from the one
`newOrchestrator` helper both construction paths route through. It defaults to a no-op,
never nil, so every record site is unconditional. A recovered dispatcher panic gets its
OWN outcome rather than folding into `failure`: a panic is a bug in this service, not an
upstream refusal, and the two want different responses from whoever is on call. A panic
raised AFTER a dispatch has completed is a different case: a `dispatched` flag makes the
recover arm leave the stored result alone, because the campaign really was created
upstream and reporting it failed would invite a retry that could double-create a PAID
campaign — losing one metric is strictly cheaper. Upstream
calls are timed only AFTER the pre-platform guards pass, so local refusals (which return
in nanoseconds) do not drag the latency quantiles toward zero.

The RUNNING and TERMINAL job transitions are recorded with deliberately OPPOSITE rules.
RUNNING is recorded on **attempt** (dispatch proceeds whether or not the status write
lands, so gating it would under-count during a database blip). The terminal one is
recorded only after a **successful** write, via the single `terminalize` helper both
finalize paths route through: `campaign_job_transitions_total` exists so a stuck job
shows up as the gap between `running` and the terminal statuses, and a job whose terminal
write failed is still `running` in the database — counting its terminal would close the
gap for exactly the rows the alert hunts. The recovery sweeper (`runRecoverySweep`)
then records one terminal transition per row it recovers — that is where the gap
CLOSES. Both halves are needed: guarding only the finalize side would leave the gap
permanently open, so a stuck-job alert would keep firing after the rows were already
terminal in the database. Dispatch is idempotent: a brief already
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

### Background sweepers

The orchestrator runs TWO periodic sweepers, both tracked by `wg` and both owned by
`sweeperCtx` — which `Shutdown` cancels FIRST, before the dispatch drain, so a sweep
already blocked in the database is interrupted promptly and its shutdown never eats
into the drain budget. Both run on every replica with no leader election.

- `StartRecoverySweeper` (every 5m) fails forward jobs stuck past `staleJobCutoff`,
  complementing the one-time startup scan.
- `StartJobRetentionSweeper` (hourly, LFXV2-3222) prunes TERMINAL `campaign_jobs`
  rows past the retention window via `JobRepo.PruneTerminalJobs`, bounding a table
  that is otherwise append-only. The window comes from `CAMPAIGN_JOB_RETENTION` via
  `SetJobRetention`, which IGNORES a non-positive value: zero is what an unset or
  unparseable variable produces, and it must mean "use the long repository default",
  never "retain nothing". A prune error is logged and dropped — retention failing
  costs disk, never correctness — and the goroutine keeps running rather than exiting
  on the first transient error. See the postgres concept for why only terminal
  statuses are eligible and why they are an allow-list.


## Creative asset upload (LFXV2-3295)

`BriefService.UploadCreativeAsset` (backing `POST .../briefs/{briefId}/creative-assets`)
validates and stores an uploaded image so a Meta ad creative can later reference it by id. It
touches NO ad platform — the bytes are held until dispatch, where Meta's per-ad-account
`image_hash` is resolved (see [internal/platform/meta](internal-platform-meta.md)).

The generated validator enforces what the CONTRACT can express — `content_type` is one of the
allowed MIME strings, and the base64 string is within `[1, 41,943,040]` CHARACTERS — so what this
handler ADDS is the DECODED-size ceiling and the proof that the BYTES are actually a decodable
image of the DECLARED type.

The `MaxLength` figure is the ENCODED ceiling on purpose, and the reason is a representation
mismatch worth stating. Goa publishes that attribute as `type: string` and emits `MaxLength` as
`maxLength` on the JSON string, where it counts CHARACTERS; base64 expands by 4/3, so declaring the
decoded 30 MiB there published a constraint rejecting uploads at ~22.5 MiB decoded — inside what
this endpoint accepts. Server and schema agreed only because the generated validator applied the
same number to the decoded slice, so both were wrong together and nothing local disagreed. The
design now declares `41943040` (= `base64.StdEncoding.EncodedLen(30 MiB)`), the unit that schema
actually measures, and the DECODED 30 MiB ceiling is **stage 0** in this handler
(`maxCreativeStoredBytes`) — a `len()` on an already-decoded slice, so it costs nothing and refuses
an oversize upload before `DecodeConfig` reads its header.

Note also what neither bound does: neither one bounds the bytes read off the WIRE, because both are
applied after the JSON decoder has read the whole request body. That bound is a separate, inbound
one — `constants.MaxRequestBodyBytes`, applied by `middleware.MaxBodyBytes` (see
[internal/middleware](internal-middleware.md)). Validation then runs in three stages, and the ORDER is
the security property. **Stage 1**, `image.DecodeConfig`, reads only the header — enough to name
the format and read the declared dimensions — and rejects garbage a declared `content_type`
alone would wave through. **Stage 2** refuses an image whose decoded pixel buffer would
exceed `maxCreativeDecodedBytes` (80 MiB), or whose sides exceed `maxCreativeDimension`
(10,000 each, which also rejects a degenerate 1x20,000,000 strip inside the byte budget). The
budget is in BYTES, not pixels, because the pixels→bytes factor depends on bit depth: Go decodes
a 16-bit colour-type-6 PNG to `*image.NRGBA64` at EIGHT bytes per pixel, so a pixel-only cap
silently permits twice the memory it advertises. `bytesPerPixelFor` prices the image from
`DecodeConfig`'s `ColorModel` — available before any allocation — and charges any unrecognised
model the wide rate. 80 MiB admits ~21M pixels at 8-bit and ~10M at 16-bit, against a 4K UHD
creative of 8.29M pixels, which is accepted at both depths. This is the decompression-bomb gate:
PNG and JPEG
both compress a flat image enormously, so a body well inside the 42-MiB request cap can declare
dimensions decoding to gigabytes, and the check spends only the header read. **Stage 3** decodes
in FULL and discards the result, because stage 1 proves only that a HEADER parses — a PNG
truncated immediately after its IHDR passes `DecodeConfig` while carrying no recoverable pixel
data, and storing it yields a corrupt asset that fails much later at dispatch. Stage 2 exists so
that stage 3's allocation is bounded by the DECODED-BYTE budget rather than by whatever the
header claims — a byte budget, not a pixel count, because a 16-bit image decodes at 8 bytes per
pixel against an 8-bit image's 4, so a pixel-only cap silently admits twice the memory for a
16-bit upload;
running the decode first would make the gate worthless, since the allocation IS the attack.

**Stage 2b** sits between them and bounds the AGGREGATE. Stage 2 bounds ONE image and says
nothing about how many decode at once, and the upstream `middleware.UploadAdmission` cannot
cover it: that permit is priced from `Content-Length`, and compression severs the link between
wire bytes and decoded bytes (a flat 4000x4000 PNG is ~68 KiB on the wire and 61 MiB decoded, an
amplification over 900x, admitted deliberately by the dimension gate). Wire-priced admission
charges such an image the floor, so without a second bound enough of them decode concurrently to
exhaust the pod while the upload budget still reads as unspent. `DecodeReserver` therefore
reserves the DECLARED pixel cost — the same figure stage 2 computes from the header — against
`constants.DecodeAdmissionBudgetBytes` (128 MiB, a quarter of the pod, sized like the upload
budget and for the same reason). The two budgets are additive in the worst case and together cap
uploads at half the pod. The wait is bounded by `DecodeAdmissionWait` rather than by the caller's
context, because net/http gives a handler's `r.Context()` no deadline — `ReadTimeout`/
`WriteTimeout` are SOCKET deadlines — so an unbounded acquisition would hold the request's outer
upload permit until the client disconnected, converting a memory guard into permit exhaustion.
Capacity that cannot be reserved answers the same retryable `503` the admission middleware sheds
with. The reservation is released the moment `image.Decode` returns, on BOTH the success and the
400 arm, and NOT deferred to the method's return: the decoded image is discarded there, so
holding it across the checksum and the insert would shed concurrent uploads for memory already
free. Stage 2b is not redundant with the wire admission — they bound different quantities with
different worst cases, and neither subsumes the other.

The set of registered decoders (`image/png`,
`image/jpeg`, blank-imported) is only the UPPER bound; `mimeForImageFormat` is the authoritative
allow-list, so another package importing `image/gif` cannot widen what this endpoint accepts.
That authority is TESTED rather than merely asserted: the service test package imports
`image/gif` itself — registering the decoder for the test binary only, reproducing exactly the
condition the claim must survive — feeds a real, decodable GIF, and requires the refusal. Without
it, widening `mimeForImageFormat` to accept any recognised format changed no test result. The
stored `mime_type` is the SNIFFED one, and a declared/sniffed mismatch is REFUSED (400), not
silently corrected. Three distinct 400s are kept apart: bytes that do not decode at all, bytes
that decode to a format outside the allow-list, and a declared type that disagrees with the
sniff. Meta's creative POLICY (minimum dimensions, aspect ratio) is deliberately NOT checked
here — it is Meta-specific and belongs at dispatch; this endpoint is storage integrity.

Unlike `CreateCampaigns`/`AdoptCampaign` there is no `validateProjectSlug`: an asset's
`project_id` is only a tenant-scoping predicate, never an attribution or connection-lookup key,
so it stays UUID-or-slug like the other nested brief routes. The `checksum` is the
lowercase-hex SHA-256 of the bytes (`sha256Hex`), which is the `(brief_id, checksum)` dedupe key
— a repeat upload of the same image returns the existing asset, and the RESPONSE STATUS says so:
**201** when this request stored the asset, **200** when it resolved to one that already existed.

That distinction cannot be read off the returned row — it is fully populated and identical on both
paths, carrying the same id and the FIRST upload's `created_at` — so it is recovered in SQL and
carried up. `createCreativeAssetQuery`'s `RETURNING` includes `(xmax = 0) AS inserted`, true for
exactly the rows the statement inserted (an INSERT leaves `xmax` 0; the `ON CONFLICT DO UPDATE` arm
writes a row version whose `xmax` is the current transaction). `CreateAsset` returns it as its
second value, and the handler renders it into the `created` attribute the generated encoder
switches on. An unconditional 201 gave a retrying client false creation semantics.

`created_by` is the attributed actor (NULL when none decodes, same as `CreateBrief`); on an
idempotent re-upload the repo preserves the FIRST uploader, so this attributes creation, not
re-sending. The `bytes` are
deliberately NOT echoed in the result (`creativeAssetResult` returns metadata only — the caller
already has the bytes it sent, and a multi-megabyte base64 body on every upload would be pure
overhead). Like the other late-bound methods, an unwired repo returns a typed `503`
(availability-neutral wording, since in the cold-start window the database is configured but the
repo has not bound yet); `SetCreativeAssetRepo` is on `briefBackendSetter` so the cold-start path
binds it in the same step as the brief repos, and BOTH startup paths bind through the single
`bindBriefLiveBackends` helper — the interface forces the method to exist, only the shared helper
forces it to be CALLED, and the handler's 503 is deliberately indistinguishable from the
no-database mode's, so a mis-wired live container cannot be detected from the outside.

## Campaign status toggle

`BriefService.ToggleCampaignStatus` (backing `PATCH .../campaigns/{id}/status`
{active|paused}) pauses/resumes a campaign ON THE PLATFORM, then persists. Unlike
`UpdateCampaign` (DB-only), the platform call happens FIRST via
`Orchestrator.ToggleCampaignStatus` → the platform's `StatusToggler`; the DB row is written
only after the platform confirms. Only a fully-created campaign (`created`, or one already
`active`/`paused`) may be toggled FREELY (`model.CampaignStatusToggleable`); a `pending`
ambiguous orphan or a `created_degraded` campaign is rejected 409 on ACTIVATE — activating one
would put an incomplete campaign in front of an audience and overwrite its reconciliation
marker with the run state (a non-empty `PlatformCampaignID` alone is not sufficient, since a
partial/degraded campaign can carry an upstream id).

**PAUSE is the one exception, and only for `created_degraded`.** That status means the campaign
definitely EXISTS upstream while the service does not know its full wiring — and an ADOPTED
campaign (LFXV2-3042) reaches it by binding a campaign the lookup found, where `ENABLED` and
`PAUSED` are alike live. So an adopted campaign can already be serving and spending, and
refusing every toggle made the campaign most likely to need stopping the one campaign the
service could not stop, even though the dispatchers explicitly support pausing a campaign with
no child ids. A pause costs the guard nothing: it cannot activate anything. **The marker is
preserved rather than written over** — pausing reconciles nothing, so the row keeps
`created_degraded` (no version bump, no index event) and the response reports that unchanged
status. The exception lives at the call site, not inside `CampaignStatusToggleable`, which
stays direction-blind: a `toggleable(status, direction)` shape would invite an unrelated caller
to pass the wrong direction and silently gain the exception. `pending` and the partial-orphan
statuses stay refused in BOTH directions — they do not mean "exists upstream", so there is
nothing for a pause to act on.
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
connection the dispatcher refuses before the TENANT-SCOPED metrics request itself —
`ErrCampaignAccountMismatch`, `ErrAccountNotSelected`, `ErrConnectionNotUsable` — is also a 409
(see the classification section below). This is not the same as "before any platform call":
HubSpot's `ErrCampaignAccountMismatch` path calls `AuthenticatedPortalID`
(`POST /oauth/v2/private-apps/get/access-token-info`) to resolve the token's portal before it can compare, so the
platform is reached even though the metrics read itself never runs. `ErrNoMetricsInWindow` is a fourth 409, and the one that is not about a
connection at all: the platform answered successfully and reported no data. It is kept off
the 503 default deliberately, because for the email channel it is the ORDINARY state (a
staged draft nobody has sent yet) and calling that an outage would send an operator to
investigate a healthy integration. Everything else propagates as-is (503) — a read has no
ambiguous mutation to protect, so there is no UNCONFIRMED classification here. The call is
bounded by `metricsCallTimeout` (20s, distinct from `toggleCallTimeout`'s 45s — reads should
fail fast rather than hold a request open).

The `window` query parameter is a closed, platform-agnostic vocabulary
(`model.MetricsWindow`: `today`, `yesterday`, `last_7_days`, `last_14_days`,
`last_30_days` [default], `this_month`, `last_month`) — never a platform's own dialect
(e.g. Google Ads' GAQL `DURING` literals, Meta's Insights `date_preset`). Each platform's
`MetricsReader` adapter is responsible for mapping this vocabulary to its own platform's
query syntax; the mapping (and any platform-specific validation, e.g. an allow-list guard
against GAQL injection) lives in the platform client package, not here.

## Brief-wide metrics read

`BriefService.GetBriefMetrics` (backing `GET .../briefs/{id}/metrics`) reads every campaign on
a brief in one request. It is the same read-through as the campaign-scoped handler above,
fanned out over `ListCampaignsForBrief` with an `errgroup` bounded by
`briefMetricsConcurrency` (4). That limit is deliberately local rather than the dispatch
semaphore: these are read-only GETs that create nothing, so they need none of dispatch's
spend-safety ceilings, and borrowing that semaphore would let a metrics read starve a paid
create waiting for a slot.

**The failure model is the whole point of the endpoint.** A brief spans several platforms and
each read can fail independently, so the states the campaign-scoped handler expresses as
distinct HTTP responses cannot be HTTP responses here — one campaign's 409 must not fail the
other five. `classifyBriefMetricsErr` maps each sentinel onto a per-row status instead:
`unsupported` (the 400s), `not_ready` (a 409 — the campaign has nothing to report yet),
`connection_problem` (a 404, three 409s or a 500 depending on the defect; they collapse because
the remedy is identical, an operator repairs the connection) and `failed` (the 503 default). `not_ready` and `failed` are
deliberately NOT merged — a staged email draft and an ad-platform outage produce the same
absence of numbers and want opposite responses.

`domain.ErrServiceDefect` takes `failed`, NOT `connection_problem`, and its own arm above the
connection ones. This row carries no status code — it carries the STRING an operator reads — and
`connection_problem` renders as "go fix your connection" for a connection that is fine, which is
the same provably useless remedy a 409 would have been. `failed` already means "not this
campaign's configuration, and retrying is the caller's only move"; the MESSAGE is what
distinguishes it, and it names the fault as ours and no remedy the reader owns.

Two sentinels reach this path WITHOUT `ErrConnectionNotUsable` and so need their own arms
above the connection ones, or they fall through to the retryable default and tell an operator
to retry something only a human edit clears: `domain.ErrNotFound` (no connection row for this
project and provider — a 404 on the campaign-scoped endpoint) and
`domain.ErrCredentialDecryptionFailed` (a corrupted row or a rotated
`CREDENTIAL_ENCRYPTION_KEY` — a 500). The decrypt case logs at ERROR rather than WARN, because
the cheap discriminator between one bad row and a key rotation breaking every project is the
COUNT of those lines, and it logs NO error text: `domain.Encryptor` is an interface, so its
error may quote ciphertext or key material, and `safeErrSummary` normalises rather than
redacts. `domain.ErrSystemConnectionNotUsable` keeps its own arm above the general
connection one for a different reason: the general wording tells the operator to reconnect,
and this fires precisely when the project has NO connection of its own to reconnect — it fell
back to the shared LF row. It logs at ERROR alongside the decrypt case, since a broken LF
system row fails every project depending on it.

`domain.ErrSystemConnectionMissing` needs an arm above **`ErrNotFound`** specifically, which is
the one ordering constraint the others do not have: it is wrapped ALONGSIDE `ErrNotFound` (the
absence is real), so the broad arm wins if placed first and answers "connect your project" for a
deployment-wide, operator-owned fault — force-system mode is on and nobody installed the LF row,
which is precisely the connection a project cannot create. 500 + ERROR log, like the
not-usable case; the repair differs (install a row, not fix one).

**The brief-wide row logs a defect at the SAME level as the synchronous paths, and the rule
is a property rather than a list.** Every sentinel whose remedy belongs to nobody the request
can reach — the shared-infrastructure ones above, and `domain.ErrServiceDefect` for a fault in
this service's own code — logs at ERROR in the fan-out, matching the 500 + ERROR the
campaign-scoped handler, the toggle and the discovery handler answer for the identical error.
The reason this endpoint in particular cannot afford to diverge: it returns a SUCCESSFUL
aggregate (a `failed` row inside a 200), so unlike every other consumer there is no status
code carrying the alarm and the log line is the entire signal that the defect happened. A row
logged at WARN sits below the threshold anyone watches while the caller is told the request
succeeded. Stated as the property because the arm previously carried a comment counting "all
three", which the next sentinel added to it falsified without failing anything.

**A non-`ok` row omits `metrics` entirely rather than carrying zeroes.** A zero is a
measurement; substituting one for a campaign that could not be read is indistinguishable from
a campaign that genuinely served nothing, and that substitution is what turns an outage into
an apparent performance result. `ok_count` exists so a consumer can see a cross-campaign total
covers 2 of 6 before presenting it.

Each `g.Go` returns `nil` even on error, and that is load-bearing rather than sloppy:
returning the error would cancel the errgroup's context and abandon campaigns whose reads had
not yet started, so the aggregate would report failures it never actually attempted.

`reason` is a fixed sentence chosen in `classifyBriefMetricsErr`, never the adapter's error
text, which can carry a platform's own response body or operator-supplied account identifiers
— the same redaction rule the campaign-scoped handler's account-mismatch arm follows.

There is no cross-channel cost total. `cost_micros` is micro-units of each platform's OWN
native currency and this service performs no FX conversion, so a sum would carry no currency.

## Window semantics, shared by both metrics reads

Everything below applies to `GetCampaignMetrics` AND `GetBriefMetrics` — it describes the
window vocabulary and the per-platform defaults, not either handler's own behaviour. It is
its own section so a reader tracing the campaign-scoped read does not miss it by stopping at
the brief-wide section above.

One caveat the vocabulary cannot express: for the HubSpot email channel the window selects
which EMAILS are in scope by send date, not which events are counted, so the counters are
the email's totals to date and two different windows containing the send date return
identical numbers. `Window` in the response records what was ASKED. The email channel also
adds an optional `email` object to the result, rendered by `emailMetricsResult` — a function
rather than an inline literal precisely so nil is the case the type system enforces, since
no ad adapter ever populates it and a dereference here would turn every ad-platform read
into a 500.

When the caller omits `window`, `defaultMetricsWindowFor` (`internal/service/brief.go`)
picks the default PER PLATFORM rather than applying one global constant: `last_30_days`
for every platform except X Ads, which defaults to `last_7_days` because its stats
endpoint caps queryable date ranges at 7 days per request — `last_30_days` is simply
unreachable there (see `internal/platform/twitter` and `internal/dispatch/twitter.go`
below). A single global default would make every omitted-window request against an X
campaign fail with a guaranteed 400.

## Keyword actions: which fault wins, and what an ambiguous one answers

`BriefService.ApplyKeywordActions` (`brief_keyword_actions.go`) pauses or removes Google Ads
keywords on one campaign. It MUTATES what serves, takes no `If-Match` and no write lock (it
persists nothing), and the batch is atomic upstream. Two ordering rules govern its error
surface, and both exist because a caller reads a status code as an instruction.

**A permanent input fault dominates a contingent state fault.** The Google Ads adapter validates
the batch BEFORE it checks provisioning or resolves a connection, deliberately — "so a permanent
input fault masks any contingent connection fault rather than the other way round"
(`dispatch/googleads.go`). `Orchestrator.ApplyKeywordActions` used to run its own
`ErrCampaignNotProvisioned` guard AHEAD of the dispatcher, which inverted that: a malformed batch
against an unprovisioned campaign answered 409 ("try later") instead of 400 ("fix your request"),
so the caller retried forever on input only they could correct. The orchestrator now refuses only
a nil campaign — not an input fault, and not a state any adapter should have to nil-check around
— and DELEGATES the empty-`PlatformCampaignID` case to the adapter, which raises the same
sentinel in more detail (it also requires an ad group) and raises it in the right order. A VALID
batch against an unprovisioned campaign still answers 409; only the malformed case moved. The two
layers must not disagree about which fault wins.

**An ambiguous outcome must not answer what a definite one answers.** The classifier's `default`
arm previously swallowed UNCONFIRMED errors — its own comment admitted it — so a mutate that MAY
already have been applied returned the same "could not be applied" 503 a definite failure gets.
A 503 reads as "retry", and retrying a `REMOVE` that already ran is precisely the wrong remedy:
Google cannot re-enable a removed criterion, only create a new one with a new id. UNCONFIRMED now
has its own arm, matched by BEHAVIOUR (`errors.As` on an `Unconfirmed() bool` method — the
dispatch wrapper is unexported, so no shared sentinel crosses the package boundary), sitting
ABOVE the default. It keeps the 503 rather than inventing a status: the endpoint's declared error
set (`commonBriefErrors`) is unchanged, so no design or `gen/` churn rides along, and the
ambiguity is carried by the MESSAGE, which tells the caller to VERIFY before retrying. This
mirrors the status toggle's unconfirmed arm exactly. A client must therefore read the message on
a 503 here rather than branch on the status alone, and `docs/api-catalog.md` says so.

## Campaign adoption

`BriefService.AdoptCampaign` (backing `POST .../campaigns/adopt`) binds a campaign that ALREADY
exists on the ad platform to an approved brief; its one platform call is a read. Without it,
campaigns launched in a platform's own console are unreachable by the metrics read, the toggle
and delete, all of which resolve a campaign through its stored row.

`Orchestrator.LookupPlatformCampaign` discovers the optional `CampaignAdopter` capability by
type assertion (see `internal-dispatch.md`) and bounds the call with `adoptLookupTimeout`
(20s, matching `metricsCallTimeout`).

**Absence and unverifiability are distinct, and that is the whole design.** The
`CampaignAdopter` contract says `(nil, nil)` means the platform ANSWERED and there is no such
campaign; every other failure is an error. `LookupPlatformCampaign` converts a nil ref into
`ErrPlatformCampaignAbsent` at the boundary so no caller can dereference nil on a nil error,
and the handler maps that sentinel — and only that sentinel — to 404; everything unclassified
becomes 503. An operator told "no such campaign" goes and creates one, so a false absence
costs a duplicate PAID campaign while a false 503 costs a retry.

Every check that can be made locally precedes the platform call: project slug, platform
validity, a `TrimSpace` re-check of `platform_campaign_id` (the design's `MinLength(1)`
rejects `""` but not `" "`, and an effectively-empty filter on a lenient client returns
somebody else's campaign as the adoption target), then the brief load and its approved gate.
Loading the brief first stops adoption being an oracle for campaign ids on an ad account the
caller cannot otherwise see, and stops approval being bypassed.

What gets persisted:

- **`ref.ID`, which `LookupPlatformCampaign` has already proven equal to the requested id.**
  The first version of this rule was "record what the platform echoed, not what was asked for",
  which sounds like faithfulness and is not: a lookup answering with a DIFFERENT campaign then
  produced a 201 binding a real paid campaign nobody named. A mismatch is refused, and refused
  as UNVERIFIABLE rather than as a 404 — nothing in that response establishes the requested
  campaign is missing, and 404 is the answer an operator resolves by creating a duplicate. The
  Google Ads lookup already errors when its own id filter comes back unhonoured, so this is the
  second of two checks on the same hazard, in the layer that owns the contract rather than in
  one adapter. The comparison is `TrimSpace` on both sides and nothing else: any looser rule
  would be this service inventing an equivalence between two ids in the platform's vocabulary.
- **`Status` is `model.CampaignStatusCreated`, never the platform's run state.** That column is
  this service's lifecycle vocabulary, which `CampaignStatusDeletable` and
  `CampaignStatusNeedsReconciliation` both default-deny on an unknown value, so a stored
  `ENABLED` would be undeletable AND never reconciled. `model.PlatformCampaignRef` therefore
  carries no status: adoptability is the ADAPTER's decision, in its own vocabulary.
- **`ref.Result`, the adapter's provenance blob, and the adopting actor.** Google Ads puts the
  resolved customer id in `Result`; `googleAdsCreationCustomerID` reads it back on every later
  toggle and metrics read, and an empty `Result` reads there as "unknown", which those guards
  treat as permission to proceed. `created_by`/`updated_by` are stamped from
  `attributedActor`, as on every other campaign write.

Persistence goes through `CampaignRepository.AdoptCampaign`, deliberately not `UpsertCampaign`
— see `internal-infrastructure-postgres.md`. An already-live `(brief, platform)` pair is a 409.
The connection arms are ordered as in the metrics and toggle switches, and for the same reason:
`ErrAccountNotSelected` (409) then the broad `ErrConnectionNotUsable` (409) — WRAPPED alongside
each other, so a broad match placed first wins and names a scope the caller cannot address.
Ahead of both sits `ErrAdoptionRequiresOwnConnection` (409), which is not a defect at all: the
project has no connection of its own and adoption alone cannot accept the shared LF account
(`internal-dispatch.md` has the isolation argument). Placing it first keeps the connection arms
from sending an operator to repair something that is working as designed.

This switch has NO `ErrSystemConnectionNotUsable` arm, and that is the one place it departs from
the metrics and toggle switches. Those resolve with the LF system fallback, so a defect in the LF
row genuinely reaches their callers and earns a 500 aimed at an operator. Adoption resolves the
project scope only (`credsSource.resolveOwned`), so the LF row is never loaded on this path and
cannot fail on it. An arm would be unreachable — and wrong even so, since it would answer a
caller whose remedy is "connect your own ad account" with a 500 about a row they do not own.
The arm existed until review caught it; it survived because
`TestAdoptCampaign_ConnectionDefectsAreDistinguished` injects errors straight into the adopter
fake, which asserts a switch's behaviour without establishing that anything can produce the
input. The mechanism is now pinned where it is real, in
`dispatch.TestAdoptionRefusesTheSystemFallback`, which asserts the system scope is never read.
`ErrInvalidPlatformCampaignID`
is a 400: the adapter rejected the id locally and issued no query, so the 503 would invite a
retry of input that can never succeed. Two more 409s come back from the repository, which is
where they can be checked atomically: `ErrPlatformCampaignAlreadyBound` (the campaign is bound
to a different brief) and `ErrStaleApproval` (the brief lost approval during the platform read).
The handler passes `brief.Version` down for the second, and answers the first ahead of the plain
`ErrConflict` arm so the message names the OTHER binding rather than this brief.

What adoption does NOT buy is activation. On Google Ads the toggle refuses `ACTIVATE` unless the
row carries the ad-group, ad and keyword-criterion ids that prove targeting was provisioned, and
adoption records only the campaign it was asked about — it does not walk that campaign's
children. So an adopted row supports metrics, delete and pause, and answers
`ErrCampaignNotProvisioned` on activate. That is the guard working, not a gap in it: this service
has not verified the campaign can deliver, and reporting a successful activation of something
that cannot serve is exactly what the sentinel exists to prevent.

## Account discovery

`ConnectionService.ListGoogleAdsAccounts`, `ListMetaAdsAccounts`, `ListLinkedinAdsAccounts` and
`ListMicrosoftAdsAccounts` (backing `GET .../connection-{google-ads,meta-ads,linkedin-ads,microsoft-ads}/accounts`)
enumerate the ad accounts reachable UPSTREAM with the connection's stored credential, so an
operator can pick one instead of pasting an account id by hand. LinkedIn and Microsoft joined in
LFXV2-3064; Reddit and X have no handler because their clients expose no `ListAdAccounts`, so
there is nothing for one to call.

**Every handler is three lines over one `listAccounts` helper**, parameterized by an
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
- `domain.ErrSystemConnectionMissing` → **500** — force-system mode is on and the LF system row
  is not installed for this provider. It must sit ABOVE the `ErrNotFound` arm below, which it is
  wrapped alongside: the broad arm would otherwise answer 404 "connect your project" for a fault
  the project cannot fix, since forced mode ignores its connection by construction.
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
- `domain.ErrServiceDefect` → **500** — matched ABOVE every connection arm below. The failure is
  in a request THIS SERVICE constructed, so nothing the caller or the operator owns is broken.
  Each arm below names a remedy belonging to someone who has nothing to repair: the 400 tells an
  operator their stored connection "cannot be used as configured" and lists fields to check that
  are all correct. That is the audit-a-correct-configuration outcome the reason vocabulary exists
  to prevent, and tagging the error `ErrConnectionNotUsable` reintroduced it while the reason token
  reached only the log. **The reason sentinel and the STATUS sentinel are separate axes.** A 5xx
  because the only actor who can act reads the log, not the response; the body names no remedy
  because the caller has none. Produced today by `linkedinExpiry` for a token request LinkedIn
  refused on protocol grounds (`ErrTokenRequestRejected`), and wrapped ALONGSIDE that reason so
  `unusableConnectionReason` keeps reporting `token_request_rejected`.
- `domain.ErrConnectionNotUsable` → **400** — the connection EXISTS but cannot be used as it
  stands: inactive, an incomplete or undecodable credential blob, or a malformed stored config
  value such as a dashed `login_customer_id`. The platform is never contacted. This arm is what
  keeps the 503 below honest: a 503 promises that waiting might help, and none of these conditions
  change until a human edits the connection. The distinction cannot be made here — a setup failure
  and an upstream one arrive as the same type — so the dispatch layer wraps the pre-send failures
  with the sentinel and this arm reads it. Six adapters do:
  `internal/dispatch/{googleads,reddit,twitter,microsoft,meta,linkedin}.go`, each in its own shared
  resolve/validate helper, so every path that reaches THIS arm is covered rather than just the one
  that happened to be fixed. Meta joined them in LFXV2-3061 (`resolveMetaCredentials` for the
  credential-state three, `requireMetaAccountID` for the missing account). LinkedIn joined
  them in LFXV2-3196 (`resolveLinkedInCredentials`, covering the credential-state three plus the
  missing account, which it needs because its client is constructed with a RuntimeConfig naming
  the account) — for its two synchronous entry points, `ToggleStatus` and `ReadMetrics`.
  LinkedIn's `Dispatch` deliberately keeps its own inline validation and does NOT route through
  that helper: it wraps failures in `notCreated()` to release the dispatch claim, a contract the
  helper does not carry, and it is asynchronous so it never reaches this arm anyway
  (`internal-dispatch.md` records the same split). In Google Ads the wrap has three owners:
  `validateGoogleAdsCredentials` tags the credential-state three (inactive, undecodable,
  incomplete), which is why they reach callers beyond discovery — but the SHAPE they reach them in
  depends on whether the caller is synchronous. The **status toggle** and the **metrics read**
  (`internal/service/brief.go`) are synchronous, so they answer a **409** off this same sentinel.
  **Campaign create is not**: it answers `202`, so the same failure can only surface later, in
  the polled job result, never as a 409 — see `docs/api-catalog.md`. Be precise about what
  reaches that result, because it is less than the sentinel carries:
  `Orchestrator.dispatchPlatform` collapses every dispatcher error into one generic string, so
  the job result says the platform dispatch failed and does NOT say which of the classified
  reasons it was. The reason token survives only in that path's log line, via
  `unusableConnectionReason`. Do not describe "campaign dispatch" as receiving a 409; the
  dispatch layer produces the error, only the two synchronous readers turn it into a status
  code, and the async path turns it into a log attribute rather than into anything the caller
  polling the job can read.
  `validatedLoginCustomerID` in `internal/dispatch/googleads.go` tags the dashed `login_customer_id`,
  and it is now called by all three readers (toggle resolver, discovery resolver, and create dispatcher).
  Neither the cause NOR its text leaves the dispatch layer — not in the response and not in
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

`createConn` and `updateConn` also carry the force-system reversibility guard,
`rejectForcedSystemAccountWrite(c.Provider, c.AccountID, currentAccountID)`. It takes the PROVIDER
as a parameter rather than reading `c.AccountID` alone, and that is a correctness requirement
rather than tidiness:
`model.Connection.AccountID` is SHARED by every provider, and `CreateHubspot`/`UpdateHubspot` copy
HubSpot's Required `account_id` — a list/audience id — into the same field. A provider-blind guard
therefore rejected every HubSpot create and update with 400 while the flag was on, blocking CRM
connection setup entirely, over an id no ad-account discovery ever produced and that turning the
flag off could not strand. The guard asks `IsPaidAds()` rather than naming HubSpot, per `Kind()`'s
own guidance: a provider added later answers false and is left alone instead of inheriting a
paid-ads policy. See internal-dispatch's Reversibility section for what the guard protects.

**The third parameter is the row's CURRENT `account_id`, and the guard fires on a CHANGE rather
than on presence.** The distinction is the whole endpoint, not an edge of it. `account_id` is
`Required` on LinkedIn, Reddit, X and Microsoft (`design/connection.go`, generated as a
non-pointer `string`), and PUT is a full replace, so a caller editing only the label MUST resend
the id already stored — the schema will not decode a body without it. A presence check therefore
returned 400 for **every update those four providers can express**. Google Ads and Meta, whose
`account_id` is optional, could satisfy it only by omitting the field — which, PUT being a full
replace, CLEARS the column. So the presence check's single permitted way to rename a Google or
Meta connection was to destroy the account selection a rollback depends on: the guard, obeyed,
caused the loss it exists to prevent.

Re-sending the stored value persists nothing, so allowing it cannot violate the invariant. What
the invariant forbids is a system-discovered id ARRIVING on a project row, and every route to that
is a change — newly set, or different from what is stored.

Two sub-decisions are load-bearing:

- **"Unchanged" compares against the STORED VALUE, not recorded provenance.** `model.Connection`
  has no provenance for the account selection and no column records which credential discovered
  it, so there is nothing else to compare against. The stored value answers the only question the
  guard asks: does this write move the row?
- **The comparison is exact past a trim — deliberately NOT `internal/dispatch`'s
  `matchesAccount`.** That helper is permissive by design in two ways this guard must not be: an
  empty creation id returns `true` (here, empty→non-empty is precisely the newly-set case), and it
  folds Meta's `act_` prefix, so `act_123` and `123` compare equal. Meta's `account_id` is pinned
  to canonical `act_<digits>` by `Pattern`, so accepting the bare-digit form as "unchanged" would
  wave through a write that changes the column into a schema-invalid shape.

`createConn` passes `""`, and that is the accurate answer rather than a placeholder: create has no
prior row by construction, so every non-empty id on a create is newly set and stays refused. The
consequence is deliberate — while the flag is on those four `Required("account_id")` providers
cannot be CONNECTED at all. That is the invariant working, not a second instance of the update
defect: the id would be landing fresh on a project row, which is exactly the write that outlives
the flag.

`updateConn` reads the current row between `parseIfMatch` and `repo.Update`. Both boundaries are
chosen: AFTER the precondition parse, so a caller who omitted `If-Match` still gets 428 without
paying for a read; BEFORE the write, because a rejected update must not reach the database. A read
FAILURE is returned as-is rather than defaulting the current value to `""` — that default would be
the reverse of fail-closed, making every resubmission look newly set and reporting a transient
database fault as a 400 about the caller's body.

The read is gated on **`forcedSystemGuardApplies`**, the guard's own scope predicate
(`IsPaidAds() && forcedSystemAdsAccount()`), extracted into a named function precisely so the gate
and the guard cannot be one expression written twice. The gate matters because the flag is
default-off: without it every connection update on every deployment would pay a round-trip for a
policy that is not running, and a missing row would start surfacing through the new read rather
than through `repo.Update`'s error, moving the 404 ahead of the version check. Naming the
predicate matters because the duplicated form is behaviour-preserving when it drifts — one copy
losing `IsPaidAds()` only buys a useless read on HubSpot updates, which no outcome-based test can
see — so the drift would have been invisible rather than caught.

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

**Only the two campaign handlers MAP this sentinel to a status, and both answer 409.** They are
not its only consumers — the asynchronous pre-create dispatch path reads it too (see the
job-result paragraph above), but it runs after the 202 and so records the reason in its log
rather than in any response. What follows is about the two that do answer a caller. The campaign
status toggle and the per-campaign metrics read each match `ErrAccountNotSelected` *before* the general
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

The distinction here is carried in the response **message**, not a field, and that is now a
choice rather than a limitation. `ConflictError` (`design/connection.go`) carries an OPTIONAL
`reason` slug alongside `code` and `message`; being optional is what let it be added without
touching the eighteen other sites that construct the shared type. The audience build populates
it — three 409s with three opposite remedies — and these connection-usability 409s do not,
because their remedy is the same one in every case (fix the connection, the message says how)
and a slug per unusable state would be a taxonomy with no client reading it. The reason token
reaches operators here through the log. A client must treat an absent `reason` as "unspecified
conflict"; see `mapAudienceErr` in `internal/service/audience.go` for the populated case.

**The message names no accounts endpoint**, and that constraint is load-bearing rather than
stylistic. FOUR providers now have one — Google Ads, Meta, LinkedIn and Microsoft Ads
(`design/connection.go`) — but Reddit and X/Twitter still do not, and they tag this defect too,
so they reach the same arm. A message pointing them at `.../accounts` would prescribe a route
that 404s, which reads as a service bug rather than a value the caller has to supply. "Save an ad
account id on the connection" is true of every provider, which is why the shared message says
that and nothing more. `assertNoAccountsEndpointPromised` pins it.

Note the earlier wording claimed Microsoft's `/accounts` would 404. That was true before
LFXV2-3064 and false after it — the constraint survives on Reddit and X alone, and stating it in
terms of a provider that has since gained the route is how a correct rule ends up cited as
evidence for a wrong fact.

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
orchestrator second. A test that leaves BOTH unset only ever exercises the first.

The orchestrator message names the OPERATION the caller attempted — `"<operation> service is
unavailable"` — rather than a fixed string. It was hard-coded to "account discovery" while that
was the only caller, and the HubSpot email search (LFXV2-3197) inherited it: a search hitting the
pre-wiring window was told about an operation it never performed. A 503 is read by someone
deciding whether to retry, so naming the wrong one sends them to the wrong subsystem. Callers pass
`accountDiscovery.label()`, the same value `classifyDiscoveryError` uses one layer up, so the two
messages a single request can produce always agree.

## HubSpot email search (LFXV2-3197)

`ListHubspotEmails` serves `GET /projects/{project_id}/connection-hubspot/emails`, returning the
marketing emails reachable through the stored HubSpot connection so a caller can pick the one an
email campaign will CLONE.

It is a TEMPLATE picker, not account discovery, and is deliberately not modelled as one: a HubSpot
connection is already scoped to the portal its private-app token authenticates against, so there
is no account to choose. What has no default is `hubspotConfig.SourceEmailID`, which campaign
create requires — without this endpoint the email channel cannot be driven from the UI at all.

The STATUS MAPPING is shared with discovery. `classifyDiscoveryError` was lifted out of
`listAccounts` unchanged and both callers pass it a descriptor, so 400/404/500/503 cannot drift
between the two; only the operation noun differs. One arm is not shared: a dispatcher with no
`EmailSearcher` yields `ErrEmailSearchUnsupported`, a separate sentinel from
`ErrAccountsUnsupported` because the capabilities are independent — HubSpot searches emails and
has no ad accounts, while the ad platforms that implement `AccountLister` are the reverse
(Google Ads, Meta, LinkedIn and Microsoft as of LFXV2-3064; Reddit and X implement neither,
having no `ListAdAccounts` in their clients). Folding them into one sentinel would
make "this platform cannot do X" ambiguous about which X.

An omitted `q` lists rather than fails, because the first screen of a picker nobody has typed into
is the useful default. That screen is BOUNDED — see `internal-platform-hubspot.md` for the cap and
why a filtered search is deliberately not bounded — and the bound is part of the published
contract, since the endpoint has no pagination fields and a caller otherwise cannot tell a
complete portal listing from a capped one.

Draft emails are RETURNED with their `state`, for the same reason Meta returns disabled accounts:
hiding the row the user is looking for answers "your portal has no such email" about an email
sitting right there. Archived rows are a different case and are simply absent — HubSpot models
archival as a separate flag rather than a lifecycle state, so no `state` value can describe them.

## Campaign delete

`BriefService.DeleteCampaign` (backing `DELETE .../campaigns/{id}`, `If-Match` required)
SOFT-deletes a campaign locally. Its purpose is slot recovery: the `(brief_id, platform, variant)`
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

## Authentication is one guard, embedded three times (LFXV2-3053)

`JWTAuth` used to base64-decode the token payload and believe it. It now calls
[internal/infrastructure/auth](internal-infrastructure-auth.md) and refuses anything that
does not verify. `authGuard` (`auth.go`) holds the verifier and is EMBEDDED in the three
authenticated services — Goa wires a security handler per service, and three copies of a
security check is three places to drift; each keeps a thin `JWTAuth` only because the
error type is generated per package.

Two details that look incidental and are not. The mutex is `authMu`, not `mu`: every
embedding service already has a `mu`, and two same-named fields at different depths
resolve **silently** to the outer one, so a lock taken in the wrong place would compile
and protect nothing. And a nil verifier REJECTS, making missing wiring an outage rather
than a silent return to trusting unverified claims —
`TestNewContainer_AllPathsInjectTheTokenVerifier` pins that all three services get one on
the boot paths a test can reach — no-database and 503-mode — which is the only place that
bug is visible at runtime. It cannot reach the LIVE path: `wireLiveBackends` needs a
reachable PostgreSQL and constructs all three services independently, so "every boot path"
was a claim about code that had been read, not tested.
`TestNoServiceIsConstructedOutsideItsVerifierInjectingHelper` closes that half in the
source instead: it parses the `container` package and fails if any non-test call to
`service.NewBriefService` / `NewConnectionService` / `NewAudienceService` sits outside its
verifier-injecting helper. Reachability is a source property, so a new construction site is
caught whether or not a unit test can boot the path it is on.

Rejections are **401** as of LFXV2-3057, carrying a `WWW-Authenticate: Bearer` challenge
(RFC 9110 §15.5.2). `UnauthorizedError` is declared in `design/connection.go` and reaches
every method through `authErrors()` (connections) and `commonBriefErrors()` (briefs and
audiences). 400 conflated a refused credential with a malformed payload: a client could not
tell "token expired, refresh and retry" from "payload invalid, do not retry" — opposite
handling, and a refresh is exactly the retry a 401 should trigger — and status-based
alerting counted a REFUSED CREDENTIAL as a client payload error, so an expired or
malformed token read as a spike in malformed requests rather than an auth incident. (A
JWKS outage was never part of that: `domain.ErrKeyUnavailable` has always taken the
`unavailable` branch and answered 503, and this change does not touch it — see the 503
split below.)
BadRequest stays declared everywhere alongside it: it is still the status for payload and
path-parameter validation, which the bodyless reads reach through the generated decoder.

The challenge is a FIELD of the error, not a framework constant — Goa's Response-level
`Header()` maps an attribute onto a response header and has no fixed-string form, so
`www_authenticate` is a Required attribute on the type, mapped in
`connectionAuthErrorResponses`/`briefErrorResponses`, and filled from the one shared
`bearerChallenge` constant in `auth.go`. Drop that Header mapping and the field silently
serializes into the JSON body: the status is still 401, every encoder still has its
Unauthorized case, and the response violates the RFC —
`TestEveryConnectionUnauthorizedEncoderSetsTheChallenge` is what catches it. The challenge
is the bare scheme, with no `realm` and no `error="invalid_token"`: an `error` code names
WHICH check failed, which is exactly the reason-specific signal the opaque message keeps
off the wire.

But not every rejection is about the token. `authenticate` returns a third value, an
`unavailable bool`, saying whose fault the failure is: true when THIS service could not
perform the check — no verifier wired, or Heimdall's JWKS unreachable
(`domain.ErrKeyUnavailable`) — false when the token itself was refused. The three `JWTAuth`
impls map the first to `ConnServiceUnavailableError` (**503**) and the second to
`UnauthorizedError` (401). The SECOND was 400 before, which classified an absent or refused
credential as a MALFORMED REQUEST, and told the caller not to retry a token a refresh would
fix. The 503 case was never a 400 — only that branch can involve a credential that is
genuinely valid, because during a JWKS outage no token is checked at all. The 401 branch is
not that case: it answers a credential that was absent or genuinely refused.
The verdict is returned separately rather than sniffed from the message: "invalid bearer
token" is deliberately the *same* string for every token-side refusal, so it cannot carry
the distinction. The nil-actor branch — a verifier that accepts but names nobody — stays on
the 401 side on purpose: it is indistinguishable from a refusal seen from outside, and
`TestAuthenticate_RejectionMessagesAreOpaque` pins that a caller cannot learn which of the
two happened.
`attributedActor` still warns on a nil actor although no served route can reach it with
one — a tripwire for a future entry point wired without the security scheme, which would
present only as NULL attribution.

See [internal/service](../../../internal/service).
