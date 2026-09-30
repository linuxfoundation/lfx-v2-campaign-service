# 2026-09-30 — LFXV2-2665: the authorization field that validated, returned 200, and stored NULL

**Fix** — three items from the round-16 review of PR #231. One is a live defect: the
`as_user_id` publishing-identity control was inert for every connection created through the
API. The other two are documentation and tests that had drifted behind the retry policy the
code actually implements.

## 1. `as_user_id` reached everything except the map literal

Every generic half of the feature was in place. `design/connection.go` declares the attribute on
the X config and result types, so the generated contracts carry it and Goa enforces
`^[0-9]+$` / `MaxLength 32`. `model.Provider.ConfigKeys()` lists it, so
`internal/infrastructure/postgres` — which builds its column list from that map — writes and
reads the column migration `000034` added. `internal/dispatch`'s `authorizedTwitterAsUserID`
reads `res.providerConfig["as_user_id"]` and makes the connection authoritative where it
declares one.

What is not generic is the `ProviderConfig` map literal written by hand in each provider's
create and update, and the read-back in `build<Platform>Result`. X's three carried only
`funding_instrument_id`. So a POST supplying a valid `as_user_id` passed validation, answered
200, and stored NULL; GET never returned the value; and the dispatch check read `""` for every
API-created connection — which is its "this connection declares no identity" branch, the exact
pre-feature behaviour. The control was inert while looking, from outside, like one that worked.

The failure direction is what makes this worth an entry rather than a line. A dropped ROUTING
value fails loudly on first use, because something downstream needs it. A dropped
AUTHORIZATION value fails silently and OPEN, because the code reading it is written to treat
absence as "no restriction configured" — that is the correct reading of a genuinely unset
field, and indistinguishable from this.

`TestAsUserIDIsAStorableConfigKey` in `internal/dispatch` already pinned the other half of the
same near-miss, recorded in the round-13 entry: the check was first written against a key
`ConfigKeys()` did not list. Neither test catches this one alone — that one passes against a
service that never writes the key, and a service test asserting only the RESULT passes against
a handler that echoes the payload back without persisting it. So
`TestTwitterAds_AsUserIDRoundTripsThroughTheService` asserts the STORED row, which is what
dispatch resolves, and checks the result beside it.

`TestUpdateTwitterAds_OmittedAsUserIDClearsIt` pins the update direction separately. PUT is a
full replace on these endpoints, so an omitted `as_user_id` CLEARS the declared identity — the
same semantics `account_id` and Meta's `app_id` already carry, and not a third convention. It
still earns a test with its reasoning written down, because clearing an authorization control
by omission WIDENS what dispatch accepts, and a later "preserve when omitted" tweak should have
to argue with that comment rather than slip past.

## 2. The retry policy was right; four statements about it were not

`docs/knowledge/code/internal-platform-twitter.md` states the rule correctly and has for some
time: retry eligibility is an explicit per-endpoint `idempotent` flag, and NONE of this
client's four creates qualifies. Four things around it said otherwise.

- `createRequest`'s godoc read as though tweet authoring were the only endpoint passing
  `false`.
- `createNullcastTweet`'s own comment said `promoted_tweets` is declared retry-safe —
  contradicting its call site three hundred lines up, which passes `false` with a comment
  explaining that `DUPLICATE_PROMOTABLE_ENTITY` does not name the line item holding the tweet.
- `docs/api-catalog.md` advertised "exponential backoff retry on 429 responses" with no
  qualification, under a heading an integrator reads to decide whether a create is safe to
  repeat.
- The two transport retry tests entered the loop via
  `createRequest(…, "campaigns", …, true /* idempotent: found-or-created by name */)`.

The tests were the most expensive of the four. A test is where a policy claim is normally
CHECKED rather than merely stated, so one asserting the opposite of production is the form of
this drift hardest to notice and easiest to cite. They now reach the retry loop through
`request`, the GET-only helper that is idempotent by construction — which is the only caller
shape production still has for it, and leaves the tests exercising exactly what they are for
(the backoff loop and the 429-body drain) without claiming anything false about an endpoint.

The `idempotent` parameter stays, all four creates passing `false` notwithstanding. What
differs between them is the REASON, and only one is permanent: the by-name lookups run above
the retry loop, `promoted_tweets` is refused on a repeat but not informatively, and tweet
authoring publishes a second tweet under the LF handle. Only the last can never be relaxed.

## 3. The schema reference omitted migration 000034's column

`docs/channel-connections-schema.md` is the canonical per-provider column table, and its
`twitter_ads_connections` entry still listed only `funding_instrument_id`. `as_user_id` is
there now with its publishing-identity purpose and the reason it is nullable with no backfill:
the shared LF system row must be seeded deliberately, and an invented value would pin every
project's tweets to one handle.

## Coverage

- `TestTwitterAds_AsUserIDRoundTripsThroughTheService` revert-verified: removing the two map
  entries and the result read-back fails it on the STORED value, naming
  `authorizedTwitterAsUserID` in the failure message.
  `TestUpdateTwitterAds_OmittedAsUserIDClearsIt` passes under that revert by construction — it
  pins semantics, not the mapping — which is why both exist.
- `TestRetryOn429` and `TestRetryOn429ReusesTheConnection` pass through the read helper; the
  429-drain assertion (retry arriving from the same client port) is unchanged and still the
  point of the second one.

Gates: `make check-fmt`, `golangci-lint run`, `go test -race ./...`, and
`go run ./cmd/okfvalidate ./docs/knowledge`.

Concept files updated: `internal-service.md` (hand-assembled `ProviderConfig` as the seam, and
the authorization-field failure direction) and `internal-platform-twitter.md` (the four
corrected statements, and why the parameter survives).

## Not fixed, and why

Three further findings from the same review are declined rather than outstanding:

- **`text` and `as_user_id` as query parameters** — declined in rounds 4, 6 and 7 and unchanged
  here: X Ads v12 create endpoints take their parameters as query parameters, so this is the
  contract, not a choice this client makes.
- **The `digits:digits@host` clock exemption** — deliberate, added in round 14 with its own
  regression row.
- **Host masking in the redactors** — declined consistently; reducing to scheme+host is the
  chosen position, not an oversight.

Refs: LFXV2-2665
