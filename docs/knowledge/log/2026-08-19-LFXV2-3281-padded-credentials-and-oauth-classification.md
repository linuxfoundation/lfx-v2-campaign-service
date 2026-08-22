# 2026-08-19 — LFXV2-3281 padded credentials, OAuth error classification, and a corrupted catalog table

**Fix** — Five never-triaged Copilot findings from PR #148's suppressed-comment blocks, all
verified live at the current head and fixed as CLASSES rather than one site at a time. Several
had been raised four and five times because an earlier round repaired one site and left its twin.

**Whitespace-padded credentials passed every validator and then failed at LinkedIn forever.**
`Credentials.CanRefresh()` gates on `strings.TrimSpace(...) != ""`, but the store keeps a
credential VERBATIM and `fetchToken` wrote `client_id`/`client_secret` raw into the exchange
form. So a stored `" client-id "` satisfied every presence check in the system, looked correct in
the row, and made LinkedIn answer `invalid_client` on every refresh — an unrecoverable state a
validator claimed to prevent, and one no reconnect repairs because nothing looks wrong.

Normalisation belongs at the WRITE boundary, and the fix refuses rather than trims. A stored
padded value stays wrong for every future reader, so trimming at each read leaves the next reader
re-exposed; and because a credential is opaque to this service, silently rewriting one would hide
a truncated paste. That also matches the rule `canonicalCredentials` already applied to required
keys. Both boundaries now refuse: `validateLinkedInRefreshCredentials`
(`internal/service/connection.go`) returns a 400, and `validateConditionalGroups`
(`internal/bootstrap/sysacct.go`) refuses the install.

**The bootstrap site is the twin that kept being missed, and it is the higher blast radius.**
`requiredCredentialKeys[linkedin-ads]` is `{"access_token"}` ONLY, so the existing padding check
— which loops the required keys — never saw `refresh_token`, `client_id` or `client_secret`. The
trio is reached exclusively through the conditional-group loop, whose membership test also
trimmed. The row it guards is the LF system account, the fallback for every project with no
connection of its own. `fetchToken` additionally trims the two client fields as defence in depth
for rows written before these validators existed; `NewClient` already trimmed `RefreshToken`.

**A rejected token exchange was classified on status alone, sending operators to the wrong
remedy.** Any 400/401 became `credentialsExpiredError`, whose message says to re-authorize the
connection. But OAuth token endpoints return those same statuses for `invalid_client` — a typo in
a stored `client_id` — and re-authorizing the member can never fix an application credential.
`fetchToken` now parses RFC 6749 §5.2's `error` code and splits `invalid_client` (operator
misconfiguration, deliberately NOT tagged `ErrCredentialsExpired`) from everything else, which
keeps the expired/revoked/invalid reading. An absent or unparseable code falls to the expired arm,
preserving today's behaviour on a body this client cannot read. Only the parsed code is compared
against a local constant — `error_description` is never read, so no upstream byte reaches an
error that can be persisted into a campaign's Steps.

**The known-good-access-token fast path is dead code, and is now labelled as such.** `token.go`'s
second branch reuses an injected access token when `AccessTokenExpiresAt` is non-zero. Nothing
writes that field: `design/connection.go`'s `linkedin-ads-credentials` declares no expiry
attribute, so the encrypted blob cannot carry one; `internal/dispatch/linkedin.go`'s
`linkedinCreds` declares `AccessTokenExpiresAt` and copies it into `Credentials`, but the JSON it
decodes never contains the key, so the value is always the zero time and the `!IsZero()` test
always fails. The bootstrap installer writes no expiry either. The consequence is that EVERY
refresh-capable client exchanges a token on its first request, and since a client is constructed
per operation, a brief-level fan-out is one OAuth exchange per campaign.

The branch is kept with a comment naming the absent writer rather than deleted, because it is the
correct behaviour the moment an expiry is persisted and removing it would read as a decision that
reuse is wrong. Persisting the expiry or the rotated refresh token is a schema and behaviour
change and was deliberately NOT made here. One coupling is recorded for whoever makes it live:
`invalidateAccessToken` clears only the cache and not `c.creds`, which is sound ONLY while this
branch is dead — once an injected expiry is real, a 401 would clear the cache and this branch
would immediately re-serve the same rejected token.

**Two comments stated the opposite of the invariant they described.** `metrics.go` justified
going through `accessTokenValue` partly to avoid "racing the refresher's creds update", but
`c.creds` is never written after construction — that is precisely why a rotated refresh token is
adopted into `c.refreshToken`/`c.refreshExpiry` instead. The reason is correctness of the value,
not memory safety. Separately, `internal/service/auth.go` and this bundle's `internal-service`
concept both attributed the pre-fix harm — telling "a caller holding a perfectly valid token"
their credential was bad — to the 400 branch. That branch fires when a credential is ABSENT or
genuinely refused, where the answer is correct; the harm belonged to the `ErrKeyUnavailable`/503
branch, where a JWKS outage means no token is checked at all.

**`docs/api-catalog.md`'s optimization table was structurally corrupt.** A bad merge
(`a1aa965d`) collapsed three rows into a single line carrying seven cells against a five-column
header, concatenating three Description fragments and deleting the campaign-scoped
`GET .../campaigns/{id}/metrics` row entirely. That left three references dangling: the
`MetricsReader` note below the table, the brief-level row's "same read-through as the
campaign-scoped row above", and the Monitoring section's pointer telling readers where the
single-campaign read is documented. The fragments also disagreed with each other — one claimed
"Google Ads is the only adapter that verifies account identity" while another said an account
mismatch is "raised by every paid-ads adapter". Code settles it: `ErrCampaignAccountMismatch` is
raised by google-ads, microsoft, linkedin, meta, reddit and twitter (plus hubspot for a portal),
so the Google-Ads-only claim is the stale one, superseded by LFXV2-3050. The table is restored to
three rows, the deleted row is back with LFXV2-3050's corrected wording, and every dangling
reference resolves again.
