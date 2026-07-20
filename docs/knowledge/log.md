# Log

## 2026-07-20

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

## 2026-07-13

**Creation** — Added OKF concept doc for internal/platform/meta (Meta Ads Graph
API client) with `tags`/`timestamp` frontmatter (queryable fields per OKF v0.1
§4.1), listed in the code index.

**Update** — Added OKF-recommended `tags` and `timestamp` frontmatter to the
internal/platform/reddit concept doc (queryable fields per OKF v0.1 §4.1).

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
