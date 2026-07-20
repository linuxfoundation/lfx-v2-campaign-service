# Log

## 2026-07-21

**Update** — PR #40 review (round 11): two fixes. (1) Archived-brief lifecycle
inconsistency (cursor): `ListAudiences` 404s on an archived parent brief, but
`GetAudience`/`UpdateAudience` only matched the audience row and never re-checked the
brief was active — so after archiving, list failed while get/patch still succeeded on
the same nested resource. Added an `EXISTS(active brief)` predicate to `GetAudience`'s
query (Update loads via Get, so the patch path is covered too), consistent with List +
Create. (2) Doc drift: `internal-infrastructure-postgres.md` still showed the old
`btrim(...) <> ''` 000006 constraint; updated it to the `~ '[^[:space:]]'` expression.

**Update** — PR #40 review (copilot, round 10, after David's approval): two fixes.
(1) UpdateAudience checked If-Match only via the repo's atomic write, AFTER the merge +
built-invariant Validate() — so a patch valid against the client's fetched version but
content-invalid once merged onto a NEWER stored version returned 400 instead of 412
(stale ETag). Added an explicit `cur.Version != version → 412` check right after
GetAudience (before merge/validate); the repo's atomic check still catches a read→write
race. Added a regression test (`TestAudienceService_Update_StaleIfMatchIs412NotContent400`).
(2) The built-invariant CHECK (000006) used `btrim(x) <> ''`, which strips only ordinary
spaces — a tab/newline-only master-list id passed the DB CHECK but `Validate()`
(strings.TrimSpace) rejects it. Switched to `platform_master_list_id ~ '[^[:space:]]'`
(requires a non-whitespace char), matching the app.

**Update** — PR #40 review (copilot, round 9): two fixes. (1) Cross-tenant integrity gap:
`campaign_audiences.brief_id` referenced only `campaign_briefs(id)`, so the copied
`project_id` was unchecked — a worker/backfill/direct write could persist an audience
whose `project_id` differed from its brief's, and `GetAudience` (trusts the stored
`project_id` for tenant scoping) could expose it under the wrong tenant. Added migration
000007: a composite FK `(brief_id, project_id) → campaign_briefs(id, project_id)` (plus
the `UNIQUE (id, project_id)` on campaign_briefs the composite FK requires). The API
create path already guarded this via `INSERT … WHERE EXISTS` an active project-scoped
brief; the FK makes the DB the source of truth for all writers. (2) Doc drift: updated
`cmd-campaign-service.md` to say `buildMux` mounts health/campaign, connection, brief,
AND audience servers (it said only health + connection).

**Update** — PR #40 human review (David CHANGES_REQUESTED + Rashad). Fixed the one
blocking defect: `CreateAudience` stored `created_by` as the JSONB literal `null` for an
unattributed row — `actorFromCtx` returns a typed-nil `*model.Actor` that slips past
`marshalAny(any)`'s `v == nil` guard (a typed nil boxed in an interface is not `== nil`)
and JSON-marshals to `"null"`. Added a `marshalActor(*model.Actor)` helper that checks
the concrete pointer, so no actor → SQL NULL. Also (agreeing with both reviewers) added a
DB CHECK `campaign_audiences_platform_valid` (`platform IN ('hubspot')`) to migration
000006 so the platform enum is datastore-enforced like `status`, not only at request
time. Clarified `audienceFromInput` status handling to an explicit if/else (behaviorally
identical — `StatusOrDefault()` was already a no-op when set — but a reviewer misread the
unconditional call as an overwrite; the false positive is now un-misreadable). Dropped
the dead `id` parameter from `audienceFromInput`. Added tests: nil-actor→NULL created_by,
and explicit-status-preserved-on-create.

**Update** — PR #40 follow-up review: two fixes. (1) The "explicit empty list clears
suppressions" contract couldn't round-trip: `suppression_list_ids` is an optional array,
so the generated client encodes it `json:"...,omitempty"` and a non-nil `[]string{}` is
dropped on the wire — the clear silently didn't work. Replaced the empty-slice signal with
an explicit `clear_suppression_lists` boolean in `AudienceUpdateInput` (always encodes;
takes precedence over a supplied list), regenerated `gen/`, updated `applyAudiencePatch`/
`hasAudiencePatch`, and added a service test for replace/clear/precedence. (2) `mapAudienceErr`
mapped `ErrNotFound` → "the audience was not found", but on create/list that error comes
from a missing/cross-project/archived PARENT BRIEF — made the shared message
resource-neutral ("the audience or its parent brief was not found").

**Update** — Route + authz for campaign_audiences (LFXV2-2783). Verified the audiences
endpoints need NO new gateway wiring: they nest under `/briefs/{briefId}/audiences`, so
the HTTPRoute `briefs(/.*)?` regex already forwards them and the single Heimdall
`project-api` rule (`/projects/:projectId/briefs/**`) already authorizes them on
`campaign_manager` (confirmed by running the RE2 regex against real audiences paths).
Added explicit audiences rows to the route/rule PARITY test (parity_test.go accepted
table) so a future narrowing of the briefs match/rule can't silently unroute or
de-authorize them, and documented the inheritance in api-catalog.md. No chart change.

**Update** — PR #40 follow-up review: two fixes. (1) `AudienceRepo.UpdateAudience` did
`UPDATE` then a SEPARATE `GetAudience` re-read to return the row — a race where a
concurrent version N+1 could land between the two statements and hand the first caller
the other writer's row + ETag. Switched to `UPDATE … RETURNING audienceCols` scanned
atomically, so the caller always gets the state its OWN write produced; the re-read
survives only on the no-row path to classify 404 vs 412 (it never becomes the returned
row, so it can't race). (2) Tightened the migration-000006 CHECK to reject blank/
whitespace master-list ids (`btrim(...) <> ''`), not just NULL — via the API empties are
written as NULL, but a direct/build-worker write could persist `''`, and the DB is meant
to be the source of truth for all writers.

**Update** — PR #40 review: updated `internal-container.md` to include the audiences
service in the no-DB and cold-start-503/late-binding mode enumerations (it was still
listing only connection + brief). The container wires `AudienceService` in all four
paths and late-binds it via `AudienceService.SetBackend` (same RWMutex/`ready()` pattern
as the brief service), so the OKF concept now matches the container behavior.

**Update** — PR #40 follow-up review: enforce the built-audience invariant. `AudienceBuilt`
is DEFINED as "the platform master list exists", but `status:"built"` was accepted with no
`platform_master_list_id` — persisting a row that claims a list its pointer is NULL. Added
`CampaignAudience.Validate()` (built ⇒ non-empty master-list id, evaluated on the EFFECTIVE
status) and call it before persisting on BOTH create AND update-after-merge, so no path (a
create with built+no-id, a status-only patch to built on an id-less row, or clearing the id
on an already-built row) can leave "built" meaning nothing — each is now a 400. Model +
service tests cover all three. Backed the app-level 400 with a DB CHECK constraint
(migration 000006: `status <> 'built' OR platform_master_list_id IS NOT NULL`) so the
platform build worker and direct writes can't violate it either — the datastore is the
source of truth, the API 400 a friendly early reject. (Reviewer-sim follow-ups: fixed a
godoc regression where `audienceValidationErr`'s doc comment detached `mapAudienceErr`'s;
documented the deliberate content-400-before-concurrency-412 precedence in UpdateAudience.)

**Update** — PR #40 follow-up review (two rounds): fixed the campaign_audiences PATCH
contract. (1) The update method reused `AudienceInput`, where `platform` is Required —
so the generated validator rejected a status-only/suppression-only patch unless the
caller also resent the immutable `platform`, defeating the "only supplied fields change"
contract. Added a dedicated `AudienceUpdateInput` (all mutable fields optional, no
`platform`), pointed `update-audience` at it, regenerated `gen/`, retyped
`applyAudiencePatch`. (2) But then every field being optional meant `{"audience":{}}`
passed the validator as a no-op that still bumps version/updated_at → invalidates other
clients' ETags → spurious 412s. Added a service-level `hasAudiencePatch` guard rejecting
an all-omitted patch as a 400 (with a test asserting the version is NOT bumped). Updated
the service tests to send platform-free patches and fixed the `AudienceInput` doc comment
(it is the CREATE payload; updates use `AudienceUpdateInput`). design.md notes the split.

**Update** — PR #40 review: extended the container startup tests to cover the new
audiences service (typed-503 in both no-DB and cold-start-503 modes + successful
`SetBackend` late-binding), and updated the architecture index for accuracy —
`design.md` now says four services and describes the audiences service, and
`api-catalog.md` gained a Campaign Audiences section listing the four nested routes.

**Creation** — Added the campaign_audiences Goa API (LFXV2-2782, epic LFXV2-2770) on
top of the existing DB layer (migration 000005 + model.CampaignAudience +
AudienceRepository + repo). `design/audience.go` defines the audiences service
(create/get/list/update) nested under a brief
(`/projects/{project_id}/briefs/{brief_id}/audiences[/{audience_id}]`), reusing the
shared design helpers (bearerToken/projectIDAttr/briefIDAttr/ifMatchAttr, JWTAuth,
the standard error set). Regenerated gen/ via goa. `internal/service/audience.go`
implements the handlers: maps payloads ↔ model, optimistic-concurrency update gated on
If-Match (same strong-validator parsing as briefs), ETag = version, typed error
mapping, and RWMutex `SetBackend` late-binding + typed-503 mode mirroring the brief
service. Wired into the container (no-db / 503-boot / live / cold-start-retry paths)
and mounted in the server (`buildMux` + a route-mount test asserting
`GET …/audiences` resolves non-404 + a nil-endpoints fail-loud case). Service-layer
tests cover create/defaults/If-Match(428/412/success)/404/late-binding. Full gate green.

## 2026-07-20

**Update** — Fixed "briefs stay broken after a cold-start DB retry" (PR #28 review,
cursor High, surfaced after #11 merged into #28). After #11 added the brief service +
orchestrator to the container, the 503-mode background retry only late-bound the
CONNECTION service + readiness — it never re-wired the BRIEF service, so brief/job
routes returned 503 for the whole pod lifetime while `/readyz` flipped to healthy
(readiness OK but routes 503 — worse than "unavailable"). Fixed: (1) gave
`BriefService` a `SetBackend(briefs, campaigns, jobs, orch)` late-binding setter
guarded by an RWMutex, with handlers now snapshotting collaborators via `ready()`
(so a mid-request swap can't race); (2) the retry goroutine now fully re-wires — brief
`SetBackend` + orchestrator + `FailStuckJobs` + `StartRecoverySweeper` — and flips
readiness LAST so `/readyz` never reports OK while brief routes still 503; (3) 503-mode
boot now wires a nil-repo brief service (routes mounted → typed 503, not a nil panic).
Added `TestBriefService_SetBackend_LateBinding` + a container 503-mode assertion.
Race-clean.

**Update** — Documented the Traefik `RegularExpression` HTTPRoute version requirement
(PR #28 review, copilot). Copilot claimed Traefik's Gateway API provider doesn't
support `RegularExpression` path matches (only Exact/PathPrefix) → the project-nested
route would be silently unrouted. VERIFIED WRONG against Traefik's source
(`buildPathRule`, every v3.1.0+ tag): a `RegularExpression` match is translated to a
native `PathRegexp(...)` rule (RE2/Go-regexp), GA, not gated. BUT two real nuances:
(1) **v3.0.x does NOT support it** (returns "unsupported path match"), so it requires
Traefik >= v3.1.0 — now stated in the template comment + concept doc; (2) the feature
is NOT in Traefik's Gateway API conformance report even though the code implements
it, so the render alone doesn't prove routing — added a note to verify the deployed
HTTPRoute's `Accepted` status condition is True. Replaced the vague "custom
conformance" wording. No route change (works on the platform's v3.1.0+ gateway).
NOTE: no other LFX service uses RegularExpression HTTPRoute (query-service uses
PathPrefix/Exact) because they route on their own top-level prefix; campaign-service
can't (project-service owns /projects/), hence the regex.

**Update** — Corrected the "re-run after a partial migration is harmless" doc claim
(PR #28 review, copilot). The container concept doc and the `Migrate` doc comment
said migrations are idempotent so a re-run after a partial is harmless — but that's
wrong for a PARTIAL (dirty) migration: golang-migrate marks the schema dirty
precisely because partial migration SQL is not assumed idempotent, and a re-run then
hits `ErrDirty` (needs manual `force`, exactly the permanent-failure path documented
above). Reworded both to scope the "skipped/harmless" claim to a CLEAN schema and
describe partial failure as the dirty/manual-recovery state.

**Update** — Fail fast on a PERMANENT migration failure instead of 503-looping
forever (PR #28 review, copilot + cursor). The 503-mode retry loop retried
`initDatabase` on ANY error — so a dirty schema (`migrate.ErrDirty`, set when a prior
migration failed partway) would loop forever behind a 503, with no fail-fast signal.
A dirty schema can't clear by re-running Migrate; it needs an operator to force the
version. Added `postgres.IsPermanentMigrationErr` (classifies a wrapped
`migrate.ErrDirty`); the synchronous fast path now returns an error (process exits
loud) and the background retry loop logs ERROR + stops looping on it. Connectivity /
lock / deadline failures are deliberately still transient (they retry). Note: the
overlapping-migration half of these findings was already fixed earlier (migrateMu +
pool-first-then-Migrate); these older bot comments predate that. Test added.

**Update** — Made the pgx DSN-parse errors DSN-free (PR #28 review, copilot). Both
`NewPool` and `ValidateMigrationDSN` wrapped `pgxpool.ParseConfig`'s error with `%w`;
NewContainer propagates it and main logs it, so a malformed credential-bearing
DATABASE_URL risked logging the connection string. VERIFIED that pgx's
`ParseConfigError` already redacts the password (`redactPW`) across every malformed
DSN shape I probed (bad port, space-in-host→url.Parse-fails-falls-to-keyword-regex,
bad connect_timeout/sslmode, keyword form) — so the finding's literal "leaks the
password" claim is not currently true. BUT we shouldn't depend on a dependency's
best-effort redaction for a secret, so wrapped both sites in a `dsnParseError` whose
Error() renders a STATIC DSN-free message and whose Unwrap() keeps the pgx cause for
errors.Is/As + diagnostics. Test asserts a password/DSN never reaches Error() while
the cause stays unwrappable.

**Update** — Added the route/rule PARITY test (PR #28 review, copilot). The PR
described an RE2 route/RuleSet parity regression guard, but none was committed — the
HTTPRoute regex and the Heimdall RuleSet path list are two hand-maintained matchers
with nothing coupling them, so a drift (a forwarded-but-unruled path) would skip the
campaign_manager FGA check unnoticed. Added `TestRouteRuleSetParity`
(`charts/lfx-v2-campaign-service/parity_test.go`): renders both templates via `helm
template`, extracts the RE2 regex + the RuleSet's project-nested patterns (translating
Traefik `:projectId`/`*`/`**`), and asserts a curated accepted/rejected path table
matches identically in both matchers (skips if helm absent; fails on render error).
Verified non-vacuous by flipping an expectation. httproute concept doc updated.

**Update** — Scoped the parity test to the campaign_manager rule (PR #28 review,
copilot). `extractRulePatterns` treated ANY `/projects/` path anywhere in the RuleSet
as "authorized", so a path moved into an allow_all/deny_all/differently-scoped rule
would still satisfy parity — but the actual invariant is campaign_manager on
project:{projectId}, not just "some rule matches". Now extraction is scoped to the
`project-api` rule BLOCK (isolated from its `- id:` to the next), and a new
`TestProjectAPIRuleEnforcesCampaignManager` (also called from both parity tests)
asserts that rule's authorizer is openfga_check with relation campaign_manager +
object project:{projectId}. A rule downgrade/re-scope now fails the security test.

**Update** — Strengthened the parity test to couple to matcher CONTENT (PR #28
review, copilot). The curated table only sampled fixed paths, so a one-sided
matcher edit that no case exercised (copilot's example: adding `tiktok-ads/metrics`
to the route regex only) would still pass. Added `TestRouteRuleSetParityWitnesses`:
it enumerates concrete example paths from the route regex's AST (`regexp/syntax`
walker — one witness per alternation leaf, `[^/]+`/`.*` collapsed to literals) and
requires each to be RULED, and builds a witness from every RuleSet pattern and
requires the route to FORWARD it. A route-only new branch now yields an unruled
witness → fail; a RuleSet-only entry yields an unforwarded witness → fail. Verified
against copilot's exact scenario (`/projects/x/tiktok-ads/metrics` is caught).

**Update** — Bounded the migration step with the startup deadline (PR #28 follow-up
review, cursor Medium). After the earlier pool-first fix, `initDatabase` still ran
`postgres.Migrate` (no context) synchronously with no time bound, so a reachable
but slow/lock-blocked migration could block `NewContainer` indefinitely. Now
Migrate runs in a goroutine under a package `migrateMu` (serializes runs so a retry
never starts a second migration while a prior deadline-abandoned one is finishing)
and the caller returns on the startup deadline. Also cleaned a union-merge artifact
in this log (duplicated oversized-body line).

**Update** — Hardened the #28 503-mode cold-start fix after review (cursor HIGH +
copilot). (1) `initDatabase` started `postgres.Migrate` (uncancellable Up()) in a
goroutine and returned on the 15s deadline WITHOUT waiting — so the retry loop
launched another migration while the previous was still blocked, leaking goroutines
and racing concurrent migrations. Reworked to open the pool FIRST (NewPool does a
context-bounded Ping) and run Migrate only after a reachable ping, so Migrate never
blocks against a down DB and retries never overlap. (2) A malformed DATABASE_URL
(keyword DSN) is deterministic, so `NewContainer` now fails fast via
`postgres.ValidateMigrationDSN` instead of 503-looping forever. (3) Corrected the
service.go comments/doc that claimed a NIL readiness dep makes /readyz not-ready —
a nil dep is treated as READY (no-DB mode); cold-start uses the non-nil notReady{}
checker. (4) The connection 503 message "not configured" → "unavailable" (during
cold start the DB is configured, just unavailable). Tests + concept doc updated.

**Update** — Made the DB cold-start startupProbe budget real (PR #28 review,
LFXV2-2558). `NewContainer` capped migration+pool init at 15s and `main` exited
on failure, so an unreachable DB at boot crash-looped the pod and the ~90s
startupProbe budget never applied. Now a *transient* DB-init failure boots the
services in 503 mode (a `notReady` health dep so `/readyz` returns 503, distinct
from no-DB mode; connection service nil-repo) and a background goroutine retries
migration/pool, swapping the live pool/repo in via `SetReadinessDep`/`SetBackend`
(mutex-guarded against concurrent request reads) once it opens. Config errors
(invalid DB settings, bad encryption key) still fail fast. `Close` cancels the
retry goroutine. Updated the container + deployment concept docs and the
startupProbe comment.
**Creation** — Added the `campaign_audiences` resource — DB layer (LFXV2-2773 subtask
2781, email epic LFXV2-2770). Migration `000005` creates `campaign_audiences` (a built
audience subordinate to a brief: `brief_id` FK to `campaign_briefs`, columns store a
POINTER + provenance — `platform_master_list_id`, `suppression_list_ids`,
`inclusion_summary`, `status` building/built/failed, `version` — NOT the audience
contents, which stay in HubSpot). This is the "B2" decision: a built audience is a
first-class, inspectable, reusable, versioned LFX resource. Added `model.CampaignAudience`
(+ AudienceStatus, StatusOrDefault), `domain.AudienceRepository` interface, and
`postgres.AudienceRepo` (create/get/list/update; project-scoped; optimistic-concurrency
update gated on version → ErrPreconditionFailed, matching ReplaceCampaign). Indexed on
brief_id + project_id (no natural uniqueness — a brief may have many audiences). The
Goa API/handlers + route/rule wiring are the sibling subtasks (2782/2783); the repo
isn't consumed until the service exists. Model unit test added; per repo convention
(no DB unit tests here — repos are covered via service-layer fakes) the migration is
validated on boot. Whole-module build/vet/test green; concept doc + log updated.

**Update** — Idempotency-lookup errors no longer silently fall through to dispatch
(PR #11 review, cursor Medium). In `dispatchPlatform`, the fast path treated ANY
non-nil error from `GetCampaignByPlatform` like "no row" and fell through to
claim/dispatch — so a transient/real DB failure that hid an existing campaign could
trigger a duplicate upstream create, with no log/signal. Now the outcomes are
distinguished: existing-with-upstream-id → reuse; `ErrNotFound` → fall through to the
claim; any OTHER error → surface as a platform failure (logged ERROR), not a blind
dispatch. Corrected the concept doc, which had documented the old swallow-the-error
behavior as intentional. Test added (`TestOrchestrator_IdempotencyLookupErrorIsFailure`).

**Update** — Addressed dealako's 4 [minor] review items on PR #11 (LFXV2-2626).
(1) `GetCampaignByPlatform` was the one campaign_repo method not scoped by
project_id — added a `projectID` param + `AND project_id=$3` (matching
GetCampaign/ClaimCampaignDispatch) for tenant-isolation defense-in-depth; updated
the domain interface + the orchestrator call site. (2) The rare double-fault in
`ClaimCampaignDispatch` (post-insert read AND rollback both fail) orphans a
`status='pending'` row that permanently blocks the (brief,platform) pair — now
logs at ERROR with project_id/brief_id/platform/job_id for alerting/manual
reconcile. (3) Added `TestClaimCampaignDispatch_ConcurrentSingleWinner` — N
goroutines racing the claim path, asserting exactly one wins and losers cleanly
no-op (the prior claim tests were single-threaded). (4) `design/brief.go`: `Brief`
now `Reference()`s `BriefInput` for the 8 shared attributes instead of
duplicating them — this also fixed a latent drift the manual copy had already
caused (Brief's `program_type` was missing BriefInput's Enum, so the generated
OpenAPI had no enum + gibberish examples on the Brief response; regenerated).
**Update** — Closed the second half of the X/Twitter URL leak (PR #31 review,
copilot). The transportError fix covered the AMBIGUOUS branch, but the PRE-SEND
branch (`isPreSendDialError` → DNS/connect-refused) still did a raw
`fmt.Errorf("... %w", err)` of the `*url.Error`, so a DNS/refused failure on a
create still rendered the request URL (X puts create params in the query string)
into persisted Steps. Added a `preSendError` type mirroring transportError's
URL-free `Error()` (via `safeTransportCause`) but semantically DEFINITE (request
never sent → not applied, unlike ambiguous transportError); `Unwrap()` retains the
cause so `isPreSendDialError`/`errors.Is` still match. Test added
(`TestPreSendError_DoesNotLeakURL`). NOTE: reddit/meta (merged) have the SAME raw
`%w` pre-send render — same follow-up as the transportError leak applies there.

**Update** — Fixed a URL leak + stale docs on the X/Twitter client (PR #31 review,
cursor Medium + dealako + copilot). (1) `transportError.Error()` rendered `%v` of
the wrapped `httpClient.Do` error — typically a `*url.Error` embedding the full
request URL (which can carry request material / a destination's secret query) —
and that string was copied into `PromotedTweetWarning` + persisted `Steps`. Added
`safeTransportCause` which unwraps a `*url.Error` to its underlying cause
(timeout/EOF/reset) with no URL; `Error()` now renders method/path + that. Test
added. NOTE: reddit/meta (merged) have the same `%v` transportError render —
follow-up to apply the same URL-suppression there. (2) Corrected the stale
`createOutcomeAmbiguous` header comment that still claimed "NOT gated on the HTTP
method" after the 3xx gate was re-added. (3) Documented CreateCampaign's
non-standard `(non-nil result, non-nil error)` contract so callers inspect the
result on error (for reconcile) instead of discarding it.
**Creation** — Added the `internal/platform/snowflake` Go package (email channel,
LFXV2-2772 under epic LFXV2-2770): a READ-ONLY Snowflake client that resolves
past-edition EVENT_NAME/EVENT_ID from `ANALYTICS.PLATINUM_LFX_ONE.event_registrations`
for HubSpot BEHAVIORAL_EVENT filters. Read-only BY CONSTRUCTION — no arbitrary-SQL
entry point (unlike the reference app's `snowflake_query(sql)`); the one method
`ResolvePastEventNames` builds a fixed, fully-parameterized SELECT DISTINCT (terms
bind as ILIKE ?/NOT ILIKE ?, never interpolated; identifiers are constants guarded by
`ident`; LIMIT-capped). Source is PLATINUM (not the reference's Silver_Segment).
Fail-closed on error/empty (callers must NOT substitute guessed names). Key-pair (JWT)
auth via injected PKCS8 PEM, with `.env`-mangling tolerance (quotes/`\n`/CRLF); pool
opens lazily; DSN never quoted into errors. Tested with a hand-rolled in-process
database/sql driver fake (no new test dep) — 9 cases asserting query shape,
injection-safety, fail-closed, and key parsing. **DEPENDENCY:** adds
`github.com/snowflakedb/gosnowflake` v1.19.1 (the only official Go Snowflake driver;
no shared Go Snowflake service exists — the LFX One UI's Snowflake service is
TypeScript). Concept doc + code index added; `go mod tidy` run.
**Update** — Two more GA-2 partial/pre-send fixes (PR #33 review, copilot). (1) The
ambiguous/duplicate BUDGET partial exposed only `CampaignName`, but the resource that
may exist is a budget created under a DIFFERENT name (`LFX | Budget | …`) — with no id
yet, a caller couldn't reconcile it. Added `CampaignBudgetName` to `CampaignResult`
and populated it in every partial. (2) A pre-send contract hole: with a CACHED OAuth
token, an already-cancelled context reached `httpClient.Do`, got wrapped as a
`transportError`, and was reported UNCONFIRMED — but nothing was sent, so it's a clean
failure. Added an explicit `ctx.Err()` check immediately before the first mutate →
`(nil, err)`. (Without a cached token the token fetch surfaced the ctx error pre-send
anyway; the cached-token path reaches Do directly, hence the explicit guard.) Tests
added for both (the pre-send test warms the token cache first).

**Update** — Added `networkSettings` to the GA-2 SEARCH campaign create (PR #33
review, copilot — verified against v23 docs before applying). A SEARCH campaign that
targets NO network is rejected with
`CampaignError.CAMPAIGN_MUST_TARGET_AT_LEAST_ONE_NETWORK`, and an omitted
`networkSettings` resolves to exactly that (proto3 bools default false) — Google
documents no protective default and every official create sample sets it. The
rejection lands on `campaigns:mutate` AFTER the budget commits, so it would orphan the
budget. Now sends `networkSettings{targetGoogleSearch: true, targetSearchNetwork:
false, targetContentNetwork: false}` — Google Search only (conservative for a PAUSED
broker shell; targetSearchNetwork=true would require targetGoogleSearch AND opt into
Search Partners). Happy-path test now asserts the networkSettings block. Concept doc
updated.

**Update** — Corrected the GA-2 name-length limits after re-verifying the v23 docs
(PR #33 review round 3, copilot — TWO contradictory claims: one said 255, one said
128; BOTH wrong for Campaign). Authoritative from the v23 System Limits table + RPC
field refs: `Campaign.name` = up to **256 CHARACTERS** (`StringLengthError.TOO_LONG`);
`CampaignBudget.name` = **1..255 UTF-8 BYTES** (trimmed). Different number AND unit.
My earlier "128 chars" campaign cap was simply wrong (over-strict, rejecting valid
names). Fixed: `maxCampaignNameRunes=256` (validated via `utf8.RuneCountInString`),
`maxBudgetNameBytes=255` (validated via `len`); `validateEntityName` now takes the
measured length + unit label so each name is measured in its correct unit (a
multibyte name would otherwise slip past the budget's byte ceiling). Also confirmed
v23 forbids NUL/LF/CR in `Campaign.name` — already handled by the control-char
stripping in `sanitizeNamePart`. Replaced the 128-overflow test with a byte-limit
preflight test + a units (bytes-vs-runes) test. LESSON: when two AI reviewers give
contradictory numbers, verify against the primary source before implementing either.

**Update** — Fixed several GA-2 correctness bugs from PR #33 review (copilot +
cursor), verified against the v23 docs: (1) campaign create now sets the REQUIRED
`containsEuPoliticalAdvertising: DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` —
omitting it fails every create with FieldError.REQUIRED (and since 2026-04-01 an
undeclared account has ALL mutates rejected), which would have orphaned the budget.
(2) The campaign duplicate check used `DUPLICATE_NAME` (the BUDGET code); campaigns
use `CampaignError.DUPLICATE_CAMPAIGN_NAME` — split into isDuplicateBudgetNameErr /
isDuplicateCampaignNameErr so the campaign branch actually fires. (3) A mutating
429 is now UNCONFIRMED (doRequest suppresses its retry precisely because it may
have committed — was mis-classified as a clean failure → double-create risk). (4)
Error codes are now parsed from the FULL body in doRequest and retained on
`apiError.ErrorCodes`; hasErrorCode reads that field instead of re-parsing the
truncated `Body` (a real error JSON exceeds maxErrorBodyChars, so the old on-demand
parse of the truncated snapshot silently dropped codes, breaking all duplicate
detection). (5) A ctx check between the budget and campaign mutates skips the
campaign create on a done context, returning the budget as a reconcilable partial.
(6) Clarified docs: a campaign-create 4xx doesn't mean nothing was created (the
budget exists); the non-shared-budget name-reuse-on-retry corollary is undocumented
so retry-safety relies on a stable NameSuffix. Concept doc + index updated (GA-1→GA-2).

**Update** — Second GA-2 review round on PR #33 (5 fixes): (1) split the name-length
limit into `maxBudgetNameLen=255` / `maxCampaignNameLen=128` and validate each name
against its own limit — v23 permits a 255-char budget name but only 128 for a
campaign, so the collapsed single limit let a 129–255-char campaign name pass
preflight and get rejected by the paid campaigns:mutate AFTER the budget was
created (avoidable orphan). (2) Require BOTH Project AND EventName independently (was
either-or): Project is the attribution key the pipeline parses from the name, so a
one-segment name is mis-attributed. (3) Added `sanitizeNamePart` to strip the `|`
delimiter from caller segments before composing — a raw `|` would inject extra
pipe-fields and break name-based reconciliation/attribution. (4) `firstResourceName`
now returns (resourceName, id) and errors on a present-but-MALFORMED resourceName
(no id segment, e.g. `customers/1/campaigns/`) → UNCONFIRMED, instead of continuing
with an empty unreconcilable id. (5) Fixed the RejectsBadInput test (its budget
cases now set Project+EventName so they exercise the budget checks, not the new
attribution checks that run first) + added tests for the 128-overflow, pipe-strip,
malformed-resourceName, and firstResourceName cases. Concept doc updated.

**Update** — GA-2 PR #33 follow-up (copilot): renamed `CampaignInput.BudgetUSD` →
`Budget` (and `maxBudgetUSD` → `maxBudget`). Google applies `amountMicros` in the ad
account's OWN currency and this client does no FX conversion, so the `USD` suffix
was a false promise — 50 on a EUR account is 50 EUR/day, not ~54. Field comment now
states it's account-currency (NOT USD), and the budget-created step no longer
hardcodes a `$` sign. Mirrors the meta client, which renamed the same field for the
same reason. No behavior change (the value was already sent as-is).

**Update** — GA-2 PR #33 follow-up (cursor Bugbot): the both-fields-required check
validated the RAW input (`strings.TrimSpace`), but composeName only includes a
segment when its `sanitizeNamePart` is non-empty — so a delimiter-only value like
`"|||"` passed validation yet sanitized to nothing, dropping the Project segment
while still creating a paid budget/campaign. Fixed by validating the SANITIZED
value (`sanitizeNamePart(in.Project/EventName) == ""`) so validation and
composition stay consistent; added pipe-only test cases.

**Creation** — Added Google Ads campaign creation (GA-2, LFXV2-2637) in
`internal/platform/googleads/campaign.go`: `CreateCampaign` creates a PAUSED SEARCH
campaign as two sequential `:mutate` calls — a non-shared STANDARD `campaignBudget`
(amountMicros = budget×1e6) then a `campaign` referencing it with a `manualCpc {}`
bidding strategy. Both resource ids surfaced. Because `:mutate` has no idempotency
key, added `createOutcomeAmbiguous` (5xx/transport ambiguous always; 3xx only on a
mutating method) + `isDuplicateNameErr` (4xx DUPLICATE_NAME → already-exists) +
machine-readable error-code parsing (`error.details[GoogleAdsFailure].errors[].errorCode`,
body never surfaced, codes bounded): an ambiguous or 2xx-no-resourceName outcome →
UNCONFIRMED + reconcilable partial (carries the budget id once created); a definite
4xx → clean failure. Deterministic composed names so a retry collides on
DUPLICATE_NAME rather than double-creating. Table-driven httptest coverage for
every branch. Concept doc updated.
**Update** — Extended the Meta ad-set ambiguity to the 2xx-no-id case (LFXV2-2641,
PR #30 review by Copilot). The ad-set create's error path already routed through
`createOutcomeAmbiguous`, but a 2xx response with an empty `id` fell through to a
definite "returned no ad set ID" — the same duplicate-create risk as the campaign
and twitter no-id paths. Now surfaces UNCONFIRMED (verify before retrying). Test
added. Also fixed a CI `check-fmt` failure (gofmt comment alignment in the meta
test).

**Update** — Extended the X/Twitter create-outcome ambiguity to the INITIAL
CAMPAIGN create (LFXV2-2642, PR #31 review by Cursor + Copilot) — the last
uncovered create step. The campaign POST returned a bare `(nil, err)` on an
ambiguous 3xx/5xx/transport failure and a plain error on a 2xx-no-id, discarding
the deterministic campaign name; X may have committed the PAUSED campaign, so a
caller got no reconcile signal and could retry into a duplicate. Now returns a
name-carrying partial result + UNCONFIRMED (verify before retrying) for both cases
(a definite 4xx/pre-send error still returns plain `(nil, err)`), mirroring the
meta/reddit clients' name-only partial for the first create step. The whole
twitter flow (campaign → line item → promoted tweet) now classifies every create
outcome consistently. Tests added.

**Update** — Extended the X/Twitter create-outcome ambiguity to the LINE-ITEM
create (LFXV2-2642, PR #31 review by Cursor). The line-item POST always returned a
definite "line item creation failed" (even on a 5xx/mutating-3xx/transport error
where X may have committed it) and a definite "returned no line item ID" on a
2xx-no-id — the same blind-retry/duplicate risk already fixed for the campaign,
promoted-tweet, and meta ad-set paths. Both now surface UNCONFIRMED (verify before
retrying) when ambiguous; a definite 4xx/pre-send error still reads "failed".
Also updated the `PromotedTweetWarning` field contract (it told consumers the
promoted tweet "may need to be added manually", which for an UNCONFIRMED outcome is
the duplicate risk this exists to prevent — now it requires verifying before adding
or retrying) and corrected the twitter concept doc's "shallow copy" wording to the
fresh-client construction.

## 2026-07-19

**Update** — Fixed an http.Client copy-after-use in the Meta client's no-follow
enforcement (LFXV2-2641, PR #30 review by Copilot). `NewClient` value-copied a
`WithHTTPClient`-supplied client (`hc := *c.httpClient`) to override CheckRedirect
— but an `http.Client` must not be copied after first use (the copy duplicates its
internal mutex while sharing the request-cancellation map, so concurrent use of
the caller's client and the copy can race). Now builds a FRESH `*http.Client`
carrying only the exported reusable fields (Transport, Jar, Timeout) with
`CheckRedirect: noFollow`. The no-follow test asserts Transport/Timeout are
preserved and the fresh client is a distinct pointer. Also made the campaign
UNCONFIRMED step reason-neutral ("ambiguous response — timeout, server error, or
an unfollowed redirect") since a 3xx now routes there too. NOTE: the reddit client
(merged) has the same value-copy pattern — follow-up tracked to apply the same
fresh-client fix there. The twitter client gets the same fix on PR #31.

**Update** — Closed two more Meta ambiguity gaps (LFXV2-2641, PR #30 review by
Copilot). (1) `doRequest` returned a plain error when a NON-2xx response body
failed to read, stripping the HTTP status — so a mutating 3xx/5xx with an
unreadable body (the create may have committed) was mis-seen as a definite failure
by `createOutcomeAmbiguous` (which keys on the `*APIError` status). It now returns
an `*APIError` preserving the status on a non-2xx read failure (2xx read failures
stay `transportError`). (2) The ad-set create returned its error directly without
the ambiguity check the campaign and ad/creative creates use, so a surfaced 3xx/5xx
read as a definite "ad set creation failed" — risking a duplicate ad set on retry.
It now routes through `createOutcomeAmbiguous`: ambiguous → UNCONFIRMED (verify
before retrying), definite 4xx → "failed". Tests added for both. (3) The same
status-stripping existed in the OVERSIZED-body branch (> maxResponseBody, 10 MiB), which returned a
plain error before recording the status — a mutating 3xx/5xx over the cap was still
mis-classified as a definite failure. Now the oversized-body branch preserves the
status the same way (2xx → transportError, non-2xx → *APIError), with a regression
test. Updated the meta concept doc to describe the fresh-client + status-preservation.

**Update** — Gated the Meta client's 3xx create-outcome ambiguity on a mutating
method (LFXV2-2641, PR #30 review by Cursor Bugbot). `createOutcomeAmbiguous`
treated EVERY 3xx as UNCONFIRMED without checking the method, diverging from the
reddit client (which gates 3xx on `isMutatingMethod`) despite claiming to mirror
it. All call sites pass POST today so behavior was unchanged, but the helper's
contract was wrong for any future GET caller — a GET redirect is not a create.
Added `isMutatingMethod` to the meta client and gated the 3xx branch (5xx and
transport errors stay ambiguous regardless of method); extended the ambiguity test
with GET/POST/DELETE method cases. Now genuinely identical to reddit.

**Update** — Fixed the http.Client copy-after-use in the X/Twitter client's
no-follow enforcement (LFXV2-2642, PR #31), matching the meta fix (PR #30):
`NewClient` now builds a fresh `*http.Client` (Transport/Jar/Timeout + noFollow)
instead of value-copying the caller's; the no-follow test asserts Transport/Timeout
preservation and a distinct pointer.

**Update** — Gated the X/Twitter client's 3xx create-outcome ambiguity on a
mutating method (LFXV2-2642, PR #31), matching the same fix applied to the meta
client (PR #30, Cursor review) and the reddit client. `createOutcomeAmbiguous`
had treated every 3xx as UNCONFIRMED regardless of method; now a 3xx is ambiguous
only on a mutating method (a GET redirect is not a create), while 5xx and
transport errors stay ambiguous regardless of method. Added `isMutatingMethod`
and GET/POST/DELETE test cases. All three clients (reddit/meta/twitter) now share
an identical method-gated contract.
**Update** — Fixed several GA-2 correctness bugs from PR #33 review (copilot +
cursor), verified against the v23 docs: (1) campaign create now sets the REQUIRED
`containsEuPoliticalAdvertising: DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING` —
omitting it fails every create with FieldError.REQUIRED (and since 2026-04-01 an
undeclared account has ALL mutates rejected), which would have orphaned the budget.
(2) The campaign duplicate check used `DUPLICATE_NAME` (the BUDGET code); campaigns
use `CampaignError.DUPLICATE_CAMPAIGN_NAME` — split into isDuplicateBudgetNameErr /
isDuplicateCampaignNameErr so the campaign branch actually fires. (3) A mutating
429 is now UNCONFIRMED (doRequest suppresses its retry precisely because it may
have committed — was mis-classified as a clean failure → double-create risk). (4)
Error codes are now parsed from the FULL body in doRequest and retained on
`apiError.ErrorCodes`; hasErrorCode reads that field instead of re-parsing the
truncated `Body` (a real error JSON exceeds maxErrorBodyChars, so the old on-demand
parse of the truncated snapshot silently dropped codes, breaking all duplicate
detection). (5) A ctx check between the budget and campaign mutates skips the
campaign create on a done context, returning the budget as a reconcilable partial.
(6) Clarified docs: a campaign-create 4xx doesn't mean nothing was created (the
budget exists); the non-shared-budget name-reuse-on-retry corollary is undocumented
so retry-safety relies on a stable NameSuffix. Concept doc + index updated (GA-1→GA-2).

**Creation** — Added Google Ads campaign creation (GA-2, LFXV2-2637) in
`internal/platform/googleads/campaign.go`: `CreateCampaign` creates a PAUSED SEARCH
campaign as two sequential `:mutate` calls — a non-shared STANDARD `campaignBudget`
(amountMicros = budget×1e6) then a `campaign` referencing it with a `manualCpc {}`
bidding strategy. Both resource ids surfaced. Because `:mutate` has no idempotency
key, added `createOutcomeAmbiguous` (5xx/transport ambiguous always; 3xx only on a
mutating method) + `isDuplicateNameErr` (4xx DUPLICATE_NAME → already-exists) +
machine-readable error-code parsing (`error.details[GoogleAdsFailure].errors[].errorCode`,
body never surfaced, codes bounded): an ambiguous or 2xx-no-resourceName outcome →
UNCONFIRMED + reconcilable partial (carries the budget id once created); a definite
4xx → clean failure. Deterministic composed names so a retry collides on
DUPLICATE_NAME rather than double-creating. Table-driven httptest coverage for
every branch. Concept doc updated.

## 2026-07-18

**Creation** — Added the `internal/platform/googleads` Go package (GA-1 scaffold,
LFXV2-2636): a Google Ads REST client (not gRPC) with OAuth2 refresh-token auth
(single-flight leader/follower, secret-safe errors), a request layer (no-follow
redirects, bounded reads, pre-send/ambiguous/definite classification, 429 retry
gated on an explicit idempotent flag since GAQL search is POST-but-read-only), and
cursor-paginated GAQL search with page/row caps. customer_id validated digits-only.
GAQL gotcha documented: v23 replaced campaign.start_date/end_date with
campaign.start_date_time/end_date_time. Concept doc + code index updated. Campaign
creation (:mutate), metrics/keywords/audience, and keyword actions follow in
GA-2..GA-5.

**Update** — Routed the project-nested campaign API through the gateway and gave it
real authz (PR #28, LFXV2-2558). The chart previously routed only `/campaigns`, so
the actual contract paths (`/projects/{projectId}/…`) were unreachable. httproute
now uses a `RegularExpression` match selecting this service's project-nested
subpaths (`connection-*`, `briefs`, `jobs`, `{provider}/metrics`,
`google-ads/keywords|audience`, `hubspot`), leaving `project-service`'s `/projects/`
routes untouched. ruleset replaces the `/campaigns` `deny_all` placeholders with a
single `project-api` rule gating every routed family on the project
`campaign_manager` relation (`openfga_check` scoped to `project:{projectId}`, D2),
with `oidc` + `anonymous_authenticator` paired (openfga_check is what rejects the
anonymous subject) and an `allow_all` fallback when OpenFGA is disabled (local dev).
A separate `campaigns-placeholder` rule keeps the still-routed `/campaigns` /
`/_campaigns/*` prefixes fail-closed (`deny_all`), preserving the chart↔route parity
invariant (every heimdall-routed path has a matching rule). deployment readiness
`failureThreshold` relaxed 1→3 for CloudNativePG cold start. Concepts updated:
`httproute`, `ruleset`.
**Update** — Also strengthened the no-follow regression tests (meta + twitter):
they injected a nil-`CheckRedirect` client, which couldn't prove the override is
UNCONDITIONAL (a "fill only nil callbacks" impl would pass). Now they inject a
caller client carrying a SENTINEL `CheckRedirect` and assert the client the code
uses returns `http.ErrUseLastResponse` despite it, while the caller's original
still returns the sentinel (shallow copy, not mutation). (PR #30 review by Copilot.)

**Update** — Typed the X/Twitter Ads client's errors and added outcome
classification (LFXV2-2642). doRequest previously returned a bare fmt.Errorf for
every non-2xx AND echoed the response body into the error string (which can carry
signed URLs / destination secrets and gets persisted into Steps). Added a typed
`apiError` (status/method/path + X's machine-readable error codes, NO body),
`transportError` (ambiguous), `isPreSendDialError`, and `createOutcomeAmbiguous`
(a 5xx apiError or a transportError → UNCONFIRMED regardless of method; a 3xx →
UNCONFIRMED only on a mutating method, since a GET redirect is not a create; a
definite 4xx or a pre-send error → not ambiguous). `isDuplicatePromotedTweetErr`
now matches the typed error code
(DUPLICATE_PROMOTABLE_ENTITY, gated to a 4xx) instead of the no-longer-surfaced
body. Brings X to parity with the reddit/meta/googleads clients. Concept doc updated.

**Update** — Extended the X/Twitter create-outcome classification to the 2xx
edge (LFXV2-2642, PR #31 review by Copilot): a promoted_tweets POST returning a
2xx with no `data.id` was warning "add it manually" — but a 2xx means the POST
succeeded and X MAY have created the association, so a manual re-add risks the
duplicate the classifier prevents. Now that case is surfaced as UNCONFIRMED
(verify before retrying), same wording as the ambiguous-error branch;
`TestPromotedTweetMissingIDWarns` updated to assert the distinction.

**Update** — Gated the X/Twitter duplicate classification to a 4xx (LFXV2-2642,
PR #31 review): `isDuplicatePromotedTweetErr` matched `DUPLICATE_PROMOTABLE_ENTITY`
on any status and ran before `createOutcomeAmbiguous`, so a mutating 3xx/5xx
carrying that code was reported as a known duplicate instead of UNCONFIRMED (the
create may have committed on a 5xx). Now requires a definite 4xx; 3xx/5xx falls
through to ambiguous. Also reworded an UNCONFIRMED warning from "reached X" to
"may have reached X" (a transportError is only plausibly sent), and corrected the
`createOutcomeAmbiguous` log description (status/type-based + caller-scoped, NOT
"any GET failure → clean").

**Update** — Closed a no-body-leak regression in that same X/Twitter `apiError`
(LFXV2-2642, PR #31 review by Copilot): `Error()` was rendering the retained
`ErrorCodes` from the untrusted response body, re-opening the leak channel into
persisted Steps (an untrusted body can place secrets even inside `errors[].code`).
Now `Error()` renders method/path/status only; codes are kept solely for
`hasErrorCode` classification, and `parseErrorCodes` drops over-long values and
caps the count. Mirrors the reddit client's Body-for-classification-only pattern.

**Update** — Disabled HTTP redirect following on the Meta and X/Twitter Ads
clients (LFXV2-2641), closing a duplicate-create gap: both built their
`*http.Client` (and accepted `WithHTTPClient` clients) with no `CheckRedirect`, so
the stdlib could follow a 3xx on a mutating POST after the create was committed and
muddy outcome classification (for X, a followed redirect also resends an OAuth-1.0a
request signed for the original URL). Added a shared `noFollow`
(`http.ErrUseLastResponse`) policy set on the default client and enforced
unconditionally after options via a shallow copy (so a caller's client isn't
mutated) — matching the reddit/linkedin/googleads clients. Regression tests added.
**Update** — Reddit no-follow enforcement now builds a fresh `*http.Client` for a
`WithHTTPClient`-supplied client instead of value-copying it (LFXV2-2641).
`NewClient` did `hc := *c.httpClient; hc.CheckRedirect = noFollow`. The rebuild
carries over only the caller's documented exported fields (Transport, Jar, Timeout)
and sets `CheckRedirect: noFollow`, so it depends on the type's public API rather
than the struct's internal shape (layout-independent) and won't silently carry any
future unexported field. NOTE: this is NOT a race fix — on the repo's Go target
`http.Client` is just those four exported fields with no internal synchronization
state, so the old value copy was also correct (`go vet` copylocks does not flag
it). It's a defensive/clarity change. Strengthened the no-follow test to assert
Jar preservation (in addition to Transport/Timeout) and the caller-not-mutated
guarantee. Scope: reddit only — reddit is the sole client on main enforcing
no-follow on a caller-supplied client (merged via PR #27). The separately-proposed
PRs #30 (meta) and #31 (twitter), still open against main, ADD no-follow to those
clients and construct the client the same way.

## 2026-07-15

**Update** — Hardened the Reddit Ads client's ambiguous-outcome classification
(PR #27): `isPreSendDialError` now proves pre-send ONLY for DNS resolution and
connect-time dial failures (ECONNREFUSED/EHOSTUNREACH/ENETUNREACH). NO TLS error
is treated as pre-send, matching the merged Meta client — a TLS error is not a
reliable pre-send proof for an arbitrary caller-supplied transport (renegotiation,
or a wrapping RoundTripper surfacing a cert/record error while reading a response
after forwarding the POST), so both `*tls.CertificateVerificationError` and
`tls.RecordHeaderError` flow to the UNCONFIRMED path — the safe classification.
Redirect following is still force-disabled on every client used, including one
supplied via `WithHTTPClient` (`CheckRedirect` overridden to
`http.ErrUseLastResponse` UNCONDITIONALLY on a shallow copy, so the caller's
client is not mutated), which keeps 3xx handling well-defined. A 3xx on a MUTATING
request is classified UNCONFIRMED (it reached a responder and may have committed
before redirecting); a 3xx on a GET is not a create. A context error surfaced
from an IN-FLIGHT `Do` stays UNCONFIRMED (the per-attempt ctx wraps the whole
round trip, so it can fire after the POST reached Reddit) — but a cancellation
returned while waiting for token refresh is a proven pre-POST failure
(`refreshToken` returns `ctx.Err()` directly) and remains non-ambiguous.
5xx/mid-flight transport failures also stay UNCONFIRMED. Reworded the
manual-fallback UTM step to SET/REPLACE the utm_* params (matching
`buildRedditUTMURL`'s `url.Values.Set`), keeping all other query params and
dropping a trailing path slash.

## 2026-07-13

**Creation** — Added OKF concept doc for internal/platform/meta (Meta Ads Graph
API client) with `tags`/`timestamp` frontmatter (queryable fields per OKF v0.1
§4.1), listed in the code index.

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/reddit concept doc (queryable fields per OKF v0.1 §4.1).

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/linkedin concept doc (queryable fields per OKF v0.1 §4.1).

## 2026-07-10

**Update** — Addressed Copilot review on the X/Twitter Ads client (PR #19):
create calls now send params as URL query parameters (not a JSON body) per the
X Ads v12 contract, use `entity_status=PAUSED`, and line items carry the
required `start_time`/`end_time` with `bid_strategy` (not `bid_type`); dates are
strictly parsed to reject impossible calendar values; name lookups propagate
errors instead of masking them as not-found. Added the
`internal/platform/twitter` code concept and index entry.

**Update** — Mount connection routes in the HTTP server (LFXV2-2556): the
`cmd/campaign-service` concept now notes that every container-wired service
must also be mounted in `server.go`, or its routes 404 despite compiling.

**Creation** — Added the `internal/platform/reddit` concept doc for the new
Reddit Ads API v3 client (OAuth2 token refresh + Campaign -> Ad Group -> Ad
creation) and listed it in the code index.
**Update** — Hardened claim-based dispatch: resolve the dispatcher and reuse an
already-completed campaign BEFORE claiming (so a no-dispatcher platform never
leaves a permanent pending claim), release the pending claim if dispatch fails
before the upstream campaign is created, and bound concurrent provider calls with
a process-wide semaphore (previously the per-job errgroup limit let N concurrent
jobs each get maxParallelDispatch slots). Shutdown cancels in-flight runs on
drain timeout.

**Update** — Reworked LFXV2-2665 single-flight from a held-connection advisory
lock to an atomic claim row (INSERT ON CONFLICT DO NOTHING of a `pending`
campaign), removing the pool-exhaustion/blocking hazards of holding a connection
across the HTTP dispatch. The pending row is also the recovery signal for an
upstream-create-then-crash. Recovery scan uses a staleness cutoff so a rolling
deploy can't fail a job the old replica is still dispatching.

**Update** — Durable campaign dispatch (LFXV2-2665): per-platform single-flight
via an atomic claim row (ClaimCampaignDispatch — INSERT ON CONFLICT DO NOTHING of
a 'pending' campaign; see the later hardening entries above for the final shape,
which superseded an initial advisory-lock attempt), so concurrent
create-campaigns can't double-create upstream; the orchestrator drains in-flight
runs on graceful shutdown before the pool closes; and startup fails-forward jobs
left non-terminal by a restart. Added CampaignRepository.ClaimCampaignDispatch /
DeleteDispatchClaim and JobRepository.FailStuckJobs.

**Update** — PR #11 review round 3: validate brief_id/campaign_id/job_id path
params as UUIDs (400 instead of a PostgreSQL cast 500); make brief approval
version-gated via If-Match (rejects approving stale content, 412/428); type the
job-poll result (PlatformResult array, replacing Any); and stop applying
debug.LogPayloads to the connection/brief/health endpoints so DEBUG can't leak
BearerTokens or plaintext provider credentials into logs (debug.HTTP header/status
logging is retained). Reconciled api-catalog (PlatformResult; CampaignCreateResult
marked as the future richer shape).

**Update** — Brief + campaign API and async orchestrator (LFXV2-2626):
updated `design`, `internal/service`, and `internal/container` concepts for
the Project → Brief → Campaigns hierarchy, async job dispatch, and idempotent
per-platform creation. Behavior hardened per review: brief content replace
resets status to `draft` and persists `event_slug`; duplicate platform sets are
rejected; dispatch reuses an existing upstream campaign instead of re-creating;
brief responses carry `event_details`/`copy`/`keywords`/`targeting`; the
`(project_id, event_slug)` archived-aware partial unique index moved to a new
migration `000003` (never edit an applied migration in place); `platforms` is
enum-constrained and every brief method declares `BadRequest` (JWTAuth can 400).

**Creation** — Added OKF concept doc for internal/platform/linkedin (LinkedIn
Marketing API client), listed in the code index.

**Update** — Dropped the Goa CLI path allowlist; twitter-api-secret FP is
fingerprint-only in `.gitleaksignore`. Clarified `.grype.yaml` rationale
(Engine fixes exist; Go module path not yet upgradeable via migrate/dktest).

**Update** — Absorbed PR #18 grype fixes into the MegaLinter secrets work:
added `.grype.yaml` (ignore five transitive test-only `docker/docker`
CVEs) and `REPOSITORY_GRYPE_ARGUMENTS` in `.mega-linter.yml`. Kept the
narrower gitleaks allowlists from PR #24 (not #18's broad `^gen/`).

**Update** — Documented local MegaLinter/Docker workflow and tightened
`.gitleaks.toml` allowlists (narrow Goa CLI path + `.gitleaksignore`
fingerprint for twitter-api-secret false positive; sample AES key limited
to docs + `values.local.example.yaml`). Added architecture concept
`megalinter-secrets.md`.

## 2026-07-09

**Update** — Wired `CREDENTIAL_ENCRYPTION_KEY` into the Helm chart and local docs (required whenever a DB URL is configured so `/readyz` can start). Documented a non-production local sample key.

**Update** — Documented PostgreSQL readiness on `/readyz` (LFXV2-2559): updated service/config/container/constants concepts, added `internal/infrastructure/postgres` concept, noted PG* secret injection on Deployment, and added the `002-db-conn-check` feature-spec subtree.

**Creation** — initial OKF knowledge bundle generated from existing docs, Helm charts, Go packages, and speckit specs.
