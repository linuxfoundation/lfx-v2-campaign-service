# 2026-09-30 — LFXV2-2665: the publishing identity, and a retry that was never safe

**Fix** — the two items the round-14 entry listed under *Surfaced, not decided* are now
decided and fixed. That entry is superseded on those two points and correct on everything
else; it is left as written because a log entry records what was true when it was made.

Both are authorization-shaped rather than parsing-shaped, and both turned out to rest on the
same mistake: a check that answers a NEARBY question was being read as if it answered the one
that mattered.

## 1. `asUserId` came from caller JSON and nothing asked whether the caller was entitled to it

`AsUserID` names which of the ad account's promotable users a nullcast tweet is authored
UNDER. It arrived in the campaign config and went to the client, where
`resolvePromotableUser` confirmed the id is promotable **by this ad account**.

That is a different question, and on the SHARED LF system connection it is true of every LF
handle. So a project supplying another project's handle passed the check and published as
them — the caller names the identity, the service supplies the authority.

`as_user_id` is now a stored X Ads `ProviderConfig` key (migration `000034`, optional), and
`authorizedTwitterAsUserID` makes the connection authoritative where it declares one: a
caller value must match it, and a caller that sends none inherits it rather than falling
through to an auto-resolve that could land on a different handle. This is LinkedIn's `org_id`
rule and the brief's `Project` rule — identity is stamped from the authenticated scope, never
read from caller JSON.

Where the connection declares nothing, behaviour is unchanged. That is the compatibility
guarantee and also the honest limit of the fix: **the check is inert until an operator seeds
`as_user_id` on the shared LF system row**, via `bootstrap -config as_user_id=...`. Until
then the gap is narrow rather than closed — the client already refuses to guess when several
candidates exist and the caller pinned nothing.

The refusal names neither the requested nor the configured id. Promotable-user ids in
persisted error text is a defect this branch has already fixed once, and naming the
connection's own identity back to a caller who guessed wrong would confirm the guess.

### The near-miss worth recording

The check was first written reading `res.providerConfig["as_user_id"]` while `as_user_id` was
not a storable key. The lookup would have returned `""` for every connection and the refusal
could never have fired — a check that reads as protection and is not one, which is worse than
no check. Making the key real is why this change reaches the design, the model, a migration
and the bootstrap installer rather than one file.

`TestAsUserIDIsAStorableConfigKey` pins the seam: `ConfigKeys()` is what the connection
repository builds its column list from, so a dropped migration or a reverted model change now
fails a test instead of silently disarming the authorization.

## 2. `promoted_tweets` passed `idempotent: true`, and the file contradicted itself

`doRequestAbs`'s own doc said only **server-side convergence** justifies `true`, and named a
repeated `promoted_tweets` POST returning `DUPLICATE_PROMOTABLE_ENTITY` as the one create that
qualifies.

It does not. The client says so itself, a few hundred lines away: X returns that same code
when the tweet is promoted by a **different** line item, so the refusal does not say the
association this call wanted exists — which is exactly why the client deliberately turns it
into a manual-verification warning rather than success.

So the retry converts a transient throttle into a permanent *"verify this by hand"* on an
association that may well have been made correctly on the first attempt. It passes `false`
now, and all four creates do. `createOutcomeAmbiguous` still classifies the 429 as
UNCONFIRMED, so the operator is told to check — the change is that they are told it once,
about one attempt, instead of after four.

## Coverage

Each fix was verified by reverting it alone from a scratchpad copy:

- Removing the `authorizedTwitterAsUserID` call from the create path leaves
  `TestTwitter_MismatchedAsUserIDIsPreCreate` failing with *"the refusal reached X 2 times"* —
  the unit tests on the helper keep passing, which is the point of having the call-site test
  as well. A first draft of that test passed under the revert because its config JSON was not
  nested under `twitterConfig` and was therefore never decoded; the counter is what exposed it.
- Removing `as_user_id` from `providerConfigKeys` fails
  `TestAsUserIDIsAStorableConfigKey`, printing the key list that remains.
- Restoring `idempotent: true` on the `promoted_tweets` call fails
  `TestPromotedTweetsIsNotRetriedOn429` with *"expected exactly 1 POST, got 4"*. That test
  drives a full `CreateCampaign` flow rather than calling `createRequest` directly: calling it
  directly pins `doRequestAbs`'s contract, not the call site, which is the thing under test.

Concept files updated: `internal-platform-twitter.md` (all four creates pass `false`, and why
the duplicate code is not convergence), `internal-dispatch.md` (the publishing identity as a
connection fact), `internal-infrastructure-postgres.md` (migration `000034`) and
`internal-bootstrap.md` (why `as_user_id` needs a shape rule). `docs/api-catalog.md` records
the connection-wins rule for API consumers.

Refs: LFXV2-2665
