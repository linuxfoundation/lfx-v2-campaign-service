# Log

## 2026-07-20

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
**Creation** — Added the `internal/platform/hubspot` Go package (email-channel
scaffold, LFXV2-2778 under epic LFXV2-2770). HubSpot's auth is the simplest of any
client — a STATIC private-app bearer token (no OAuth token-exchange flow), attached
directly by `doRequest`. The request layer mirrors the googleads/reddit/meta/twitter
discipline: no-follow redirects (fresh-client rebuild so a `WithHTTPClient` caller
isn't mutated), bounded 10 MiB reads, typed `apiError` (method/path/status only,
body never surfaced) + `transportError` (URL-free via `safeCause`, cause retained
via Unwrap), `isPreSendDialError` pre-send classification, and 429 retry gated on an
explicit `idempotent` flag (a non-idempotent create is never retried — no idempotency
key → double-create risk). Concept doc + code index added. Next: marketing-email ops
(LFXV2-2779) and CRM-lists/event-defs ops (LFXV2-2780) build on `doRequest`.

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
