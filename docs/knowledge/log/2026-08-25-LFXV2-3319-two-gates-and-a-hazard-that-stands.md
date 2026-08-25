# 2026-08-25 — X goes credentials-first across BOTH gates; Meta and LinkedIn join the client cache

**Update** — Two independent changes that share one property: each was recorded in prose as
"remaining work", and in each case the prose had to be re-derived rather than trusted.

## X credentials-first takes two gates, and both moved together

Making a provider credentials-first is gated in two separate places, and admitting it to one
leaves half a flow:

1. `accountDiscoveryProviders` (`internal/bootstrap/sysacct.go`) gates whether an account-less
   SYSTEM row is installable by the bootstrap CLI. X is now a member.
2. `Required("account_id")` on `TwitterAdsConnectionConfig` (`design/connection.go`) gates the
   PUBLIC connection APIs. Dropped.

Relaxing only the map would have left X credentials-first for the CLI and still un-creatable over
HTTP; relaxing only the design would have left the system row uninstallable. `docs/api-catalog.md`
already warned that naming only the map "would send the next change to do half the work", and that
warning was accurate — both gates were verified independently against the code before either moved.

**A third gate was checked for and does not exist for X.** LinkedIn additionally needs its create
path to tag the missing choice, because `LinkedInDispatcher.Dispatch` resolves inline and answers
an empty account id with a bare `notCreated`. X does not have that problem: `Dispatch` resolves
through `validateTwitterConnection` (`internal/dispatch/twitter.go`), which tags an empty account
id with BOTH `ErrConnectionNotUsable` and `ErrAccountNotSelected`. That is the Microsoft shape, and
it was confirmed by reading the call graph rather than by trusting the catalog's claim.

`funding_instrument_id` stays Required at both gates, and that is the boundary of the change:
credentials-first defers the ACCOUNT choice, which discovery can finish, and nothing else. No
discovery endpoint supplies a funding instrument, so relaxing it would create a row nothing in this
API could complete — the same objection that keeps Reddit's `account_id` required.

Relaxing `Required` also changes the generated Go type from `string` to `*string`, which is what
makes the contract change legible in code: two call sites in `internal/service/connection.go` now
deref through `strVal`, matching Google Ads and Meta. A mutation confirmed the new test binds —
restoring `Required("account_id")` and regenerating fails compilation at exactly those sites.

Only the missing-KEY case moved in `internal/apivalidation`. Goa's `Required` is a presence check
on the JSON key, so absence became legal while a malformed PRESENT value did not: the empty-string,
path-injection (`8r7gb/../x`) and overlong cases all still reject, and the LFXV2-2642 injection
guard is untouched.

## Meta and LinkedIn wired to the client cache; X deliberately not

`clientCache`'s roster recorded Meta, LinkedIn and X as unwired under LFXV2-3033, "deferred only
because open PRs owned those files at the time (cs#148, cs#152, cs#158)". Those PRs have merged, so
that reason is gone for Meta and LinkedIn, and both are now wired on their TOGGLE and METRICS
paths after a per-client safety analysis:

- **Meta** is IMMUTABLE once built — every field is written at construction and read-only after,
  with no token cache and no in-flight refresh handle. That is stronger than the mutex-guarded
  property Reddit and Microsoft rely on.
- **LinkedIn** writes its cached token, expiry, rotated refresh token and single-flight handle
  exclusively under `c.tokenMu`, which is never held across the network call.

Their `Dispatch` paths stay UNWIRED on purpose. Meta's create client carries a fuller
`AccountConfig` (page id, plus the account a create requires) and LinkedIn's a `RuntimeConfig` with
`DefaultOrgID` and per-request `TargetingProfiles`/`EmployerExclusions` — so LinkedIn's client
VARIES per call under a cache key that does not. Caching either would let one campaign's targeting
serve another's.

**X remains unwired, and its reason is technical rather than procedural — it survives the
merge of those PRs.** The X client documents itself as safe for SEQUENTIAL use only, and that is
not a stale doc comment: `twitter.Client.pace`/`writeDelay` paces its own writes with an
inter-request sleep to stay under X's ~1 write-per-second limit, a scheme that assumes one dispatch
at a time drives the instance. Sharing one across concurrent callers would interleave two
dispatches through that single pacing assumption and break it, on the money-spending create path.
Wiring it needs a concurrency argument this pattern does not supply.

The wiring tests assert client IDENTITY rather than a token hit count, and the difference is a
property of the provider rather than a weaker test: Meta and LinkedIn are handed an already-minted
bearer token and perform no exchange at construction, so a count-based test would read 0 with and
without the cache and prove nothing. A mutation forcing an unconditional rebuild fails both reuse
tests. Rotation tests pin the other half — a client built from credential version N must not
survive a bump to N+1, or the cache would serve a revoked credential through a live client.

## The Google Ads create-path bypass is real and was left alone

The same roster records that `GoogleAdsDispatcher.Dispatch` builds its client inline rather than
through `cachedGoogleAdsClient`, so a dispatch burst re-mints an OAuth token per campaign. That was
verified: `Dispatch` and `googleAdsClientFor` resolve identically and construct identically (same
credentials, `CustomerID`, `LoginCustomerID`, `Label`), so unlike Meta and LinkedIn it is genuinely
cacheable, and unlike them it would save a real token exchange.

It was NOT wired here. `TestClientCache_GoogleAdsDispatchBypassesTheCache` pins the current
behaviour precisely so that wiring it fails that test and forces the rosters to be updated in the
same change — it is a separate behaviour change on the paid create path, and folding it into a
ticket about X account discovery would bury it. It stays recorded as the deliberate bypass it is.

## A roster change is not done until every sentence that named the old state agrees

Changing X's status in two rosters falsified claims in four other places, none of which the
compiler or `okfvalidate` can see. They were found by grepping the CLAIM rather than the changed
files — case-insensitively, in both directions ("X is excluded" AND "X is Required"):

- `internal/service/connection.go` — the Google Ads `AccountID` comment said optionality belonged
  to "Google Ads and, as of LFXV2-3061, Meta … the other four still require it".
- `internal/service/connection_force_system_test.go` — a test comment asserting `account_id` is
  Required on "LinkedIn, Reddit, X and Microsoft", used to explain why the force-system guard
  fires on CHANGE rather than presence.
- `docs/knowledge/code/internal-service.md` — the same four-provider claim about that guard.
- `docs/knowledge/code/internal-bootstrap.md` — "Of LinkedIn, Microsoft, Reddit and X, both
  Microsoft and X have BOTH halves", which still implied X was outside the map it had just joined.

The force-system ones were the interesting pair, because the roster was doing load-bearing work in
the argument: the comments justified the change-based check by pointing at providers that CANNOT
omit the id. That reasoning survives X leaving the group, but it is no longer the whole reason —
re-sending a stored id must stay a no-op for a credentials-first provider too. Both comments now
say that, so the next provider to go credentials-first does not read the roster as the
justification and conclude the check can be narrowed.

Log fragments under `docs/knowledge/log/` were deliberately left alone: they are dated history of
what was true when written, not statements about the current tree.
