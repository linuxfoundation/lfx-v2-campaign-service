# Log

## 2026-07-20

**Update** — Extended the Meta ad-set ambiguity to the 2xx-no-id case (LFXV2-2641,
PR #30 review by Copilot). The ad-set create's error path already routed through
`createOutcomeAmbiguous`, but a 2xx response with an empty `id` fell through to a
definite "returned no ad set ID" — the same duplicate-create risk as the campaign
and twitter no-id paths. Now surfaces UNCONFIRMED (verify before retrying). Test
added. Also fixed a CI `check-fmt` failure (gofmt comment alignment in the meta
test).

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
status-stripping existed in the OVERSIZED-body branch (>1 MiB), which returned a
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
UNCONFIRMED only on a mutating method; a definite 4xx or pre-send error → not
ambiguous). `isDuplicatePromotedTweetErr` now matches the typed error code
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
