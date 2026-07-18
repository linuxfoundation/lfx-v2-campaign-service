# Log

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
