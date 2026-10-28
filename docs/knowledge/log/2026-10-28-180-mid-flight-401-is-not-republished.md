# 2026-10-28 — #180: a 401 that lands while a token refresh is on the network is not republished

**Fix** — The Google Ads, Microsoft Ads, Reddit and LinkedIn token caches no longer undo a
401 that arrives while a refresh is still in flight.

**The race.** A refresh is published as a flight whose `inflight.token` is `""` until the
exchange returns. A 401 that landed in that window cleared the cache, but there was nothing on
the flight to match. When the exchange came back holding the same value (an authorization
server may re-issue a still-unexpired access token), `fetchToken` wrote it back into the cache
with a fresh lifetime, and the leader handed it to every waiter. A cached client then
re-presented the rejected token to every later call until it expired. On a spending campaign
that is a stuck campaign, not one failed call.

**The fix.** `fetchToken` returns the token and its expiry and no longer writes the cache. The
leader writes the cache, checks the flight, sets `inflight.token` and tears the flight down in
ONE critical section under the token mutex. An invalidation that finds a published flight
marks it:

- Google Ads, Microsoft and Reddit record the presented token in `tokenRefresh.rejected`. The
  check is by identity, so a late 401 naming some other token costs the flight nothing (ABA
  preserved).
- LinkedIn's invalidation is unconditional (its 401 arms do not say which token was refused),
  so it sets `tokenRefresh.poisoned`. That costs at most one extra exchange per mid-flight 401.

A marked result is neither cached nor handed out. The flight publishes a blank, and every
waiter re-leads one fresh exchange.

The record is scoped to the flight the 401 raced. If the recovery exchange re-issues the same
value, that value is served and cached: it is the endpoint's answer after the 401. The next
401 for it clears the cache again, so nothing is stuck until expiry. Carrying rejections
across flights would turn one transient 401 into a refusal of a token the endpoint still
issues. `TestRefresh_ReissuedRecoveryIsStillClearedByTheNext401` pins this for Google Ads,
Microsoft and Reddit.

**Not affected:** X/Twitter (OAuth 1.0a with static, signed credentials), Meta (static bearer)
and HubSpot (static private-app token). They have no refreshed-token cache. HubSpot's
`linkPortalLookups` single-flight caches a portal id, not a credential.

**Tests.** `token_flight_race_test.go` in each of the four packages drives the real leader.
The token server's handler blocks the refresh, and the 401 lands while it is held. The test
asserts that neither the waiter nor the cache fast path returns the rejected token. It failed
20/20 at `origin/main` and passes under `-race -count=20`. `TestFetchToken_DoesNotWriteTheCache`
pins where the cache write lives. Mutation proofs: removing the record, the check or the
selectivity, or moving the cache write back into `fetchToken`, fails the matching test.

**Supersedes** the sentence in
`2026-08-24-LFXV2-3033-a-cached-token-survives-its-own-401.md` that "an in-flight single-flight
refresh is deliberately left alone: it is already fetching a new token". That refresh can come
back holding the rejected value, and is now marked.
