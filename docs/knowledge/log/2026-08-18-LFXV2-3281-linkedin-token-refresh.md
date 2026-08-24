# 2026-08-18 — LFXV2-3281 LinkedIn access-token refresh

**Fix** — LinkedIn access tokens last 60 days and nothing renewed them. On 2026-08-14 the
system token aged out and `GET /api/campaigns/linkedin/monitor` answered 500 with only
`401 {"code":"EXPIRED_ACCESS_TOKEN","serviceErrorCode":65602}` in the server log to say why.
Reddit and Meta returned live data from the same page, so it was not an outage; a freshly
pasted token turned the same call into a 200. `internal/platform/googleads` referenced refresh
tokens across five files and `internal/platform/microsoft` across three — `internal/platform/linkedin`
across zero.

**The vendor contract was verified against LinkedIn's primary docs before any code was written,
and one load-bearing claim did not survive.** Refresh tokens are NOT universally issued:
LinkedIn supports them "for all approved Marketing Developer Platform (MDP) partners" only. So
refresh is built as an OPTIONAL capability, not an assumption. A connection carrying an access
token alone keeps working exactly as before — bearer-only, no exchange attempted — because
`Credentials.CanRefresh()` gates the whole path on refresh_token + client_id + client_secret all
being present. Building an unconditional refresh loop would have broken every non-MDP connection.

Confirmed from the same source: the access token's 60-day and the refresh token's 365-day
lifetimes; the exchange is `POST https://www.linkedin.com/oauth/v2/accessToken`,
`x-www-form-urlencoded`, carrying `grant_type=refresh_token`, `refresh_token`, `client_id`,
`client_secret`, and returning `access_token`, `expires_in`, `refresh_token`,
`refresh_token_expires_in`.

**The refresh token's TTL does NOT reset when it is used.** LinkedIn's doc is explicit: after a
refresh "the lifespan of the refresh token remains the same as specified in the initial OAuth
flow (365 days)". The connection therefore hard-stops one year after the member last authorized
it, and only a human re-authorization clears it — refresh defers the outage, it cannot remove
it. That is why the client warns 30 days ahead of the refresh token's own deadline rather than
treating a successful refresh as proof the connection is healthy.

**65602 is one expiry signal, not the complete one.** The ticket treated it as the test. LinkedIn's
error-handling guide documents "expired access token", "the token has been revoked" and "invalid
access token" as three distinct 401 conditions and publishes NO subcode for the latter two, so a
65602-only predicate would miss revocation entirely — the case LinkedIn explicitly reserves the
right to trigger "at any time due to technical or policy reasons". `isTokenExpiryResponse` uses the
subcode as a positive signal when present and otherwise treats a 401 on an authenticated call as
the signal, while 403/429 are excluded. The refresh exchange itself is classified on STATUS, not on
a parsed reason: LinkedIn returns the same `400 invalid_request` for an expired, revoked or invalid
refresh token, and the remedy for all three is identical.

**Absence had to keep meaning "unknown".** No connection stores a token expiry today, so a zero
`AccessTokenExpiresAt` means "not known", never "expired" — reading absence as expiry would have
failed every existing connection closed on the first call. The client fails closed only on a
`KNOWN`-past expiry. A mutation removing the `IsZero()` guard is caught by two tests.

**The single-flight discipline is copied from the siblings, not reinvented.** The lock is never
held across the network call; the caller finding the cache empty becomes the leader and fetches on
a `context.WithoutCancel` context so one caller's cancellation cannot tear down a refresh the
others depend on. The property is tested by asserting the SERVER-SIDE exchange COUNT under 25
concurrent callers — a per-caller refresh still returns a valid token to everyone and would pass
any weaker assertion while hammering the token endpoint.

**The credential schema had nowhere to put a refresh token, so the feature was
unreachable until the API changed too.** `LinkedInAdsCredentials` accepted only
`access_token`, and the persisted blob is that Goa struct marshalled verbatim — so no
supported input could populate `RefreshToken`/`ClientID`/`ClientSecret`, `CanRefresh()`
was false on every real connection, and the client always took the bearer-only branch.
The client-side half alone would have shipped as "done" while the 60-day outage stayed
exactly as it was. `refresh_token`, `client_id` and `client_secret` are now OPTIONAL
attributes on that type — deliberately unlike Microsoft's, where `refresh_token` is
`Required`, because requiring them here would reject every non-MDP connection.

**Three defects were found only by reviewing the branch rather than the feature.** The
metrics read path — `doAdAnalyticsAttempt`, the one that produced the outage — set its
bearer header from `c.creds.AccessToken` directly, so the headline fix missed the
headline path; `doRequest` had been converted and it had not. `fetchToken` wrote the
rotated refresh token back into `c.creds`, which `validateCredentialShape` and the
`CanRefresh` fast path read WITHOUT the lock: a data race, and precisely where this
diverged from the siblings it claims to mirror — google-ads and microsoft only ever
READ `c.creds`, which is what makes their unlocked reads safe. And
`domain.ErrCredentialsExpired` had no reader in `unusableConnectionReason`, so the new
reason landed as `unclassified` on the async create path — the status improved while
the diagnosis did not.

**A partial refresh credential set is rejected, not stored.** The trio is all-or-none:
`CanRefresh()` gates on all three, so saving a `refresh_token` without its client
credentials would pass validation, store cleanly, and silently degrade the connection
to bearer-only — the operator sees a saved refresh token, believes renewal is
configured, and meets the same 60-day expiry anyway. Goa's `Required` cannot express a
conditional group, so `validateLinkedInRefreshCredentials` enforces it at create and
set-credential time and answers 400.

**The token exchange's transport error had to be REBUILT, not wrapped.** That request's
body carries `client_secret` and `refresh_token`, and `WithHTTPClient` accepts an
arbitrary `RoundTripper` whose error text is caller-controlled and can echo it;
`http.Client.Do` then wraps that in a `*url.Error`, so peeling one layer still leaves
the credential-bearing error reachable. A `%w` wrap leaked the entire form body — the
original leak test only covered response bodies and passed throughout. It now routes
through the package's existing `redactHTTPDoError`, which reconstructs the cause from
its classification alone, and the test walks EVERY layer via `errors.Unwrap` rather
than only the outermost `Error()`.

**A 401 needs handling that no expiry timestamp can provide.** LinkedIn "reserves the
right to revoke Refresh Tokens or Access Tokens at any time", so a token valid when the
request was built can be rejected mid-flight. `doRequest` now classifies a 401 as an
expiry, invalidates the cached access token so the next call re-exchanges rather than
replaying a dead one, and surfaces the actionable error. Only the CACHE is cleared,
never the refresh material: a revoked access token does not imply a revoked refresh
token, and discarding the latter would turn a recoverable state into one needing a
human.

**Nothing is persisted and nothing is logged.** The package still never touches the database, env
or disk; credentials are injected. A rotated refresh token is adopted in-memory for the next
exchange within the client's lifetime but is deliberately not written back — and it does not need
to be, precisely because the TTL does not reset. Callers construct a Client per dispatch, so that
adoption matters only WITHIN one dispatch; there is no cross-request token cache, and adding one
would belong in a shared token store rather than in this package. No error echoes the token-endpoint body: that
request carried `client_secret` and the refresh token, these errors are persisted into a campaign's
`Steps`, so a leak would be durable. Status only.

**The expiry now surfaces as a named, non-retryable defect.** `linkedin.ErrCredentialsExpired` is
re-tagged at the dispatch boundary as `domain.ErrConnectionNotUsable` + `domain.ErrCredentialsExpired`,
which keeps it out of the retryable 503 bucket and out of the opaque 500 that started this. The
message names the connection, and distinguishes the LF SYSTEM row from a project-owned one — one
expired system token disables LinkedIn for every project falling back to it, and "the LinkedIn
connection" would send each of those operators to a row they do not own.
