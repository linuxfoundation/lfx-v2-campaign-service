# 2026-08-22 — a guard that runs "first" only on the paths that reach it

**Fix** — three Copilot findings on the settings readback (PR #154). One was a real
error-precedence defect; two were prose falsified by this branch's own earlier fixes.
Continues the shape recorded in
[2026-08-22-a-doc-outlives-the-behaviour-it-described](2026-08-22-a-doc-outlives-the-behaviour-it-described.md);
that fragment's rule is intact and unchanged.

## Ordering is a property of the CALL, not of the comment above it

`ReadSettings` documented its absent-provenance guard as "checked BEFORE the client is
consulted", and the guard was written to fail closed with
`ErrCampaignProvenanceUnknown` joined to `ErrCampaignAccountMismatch` — a deterministic 409
whose only remedy is re-dispatching the row. The comment was accurate about intent and wrong
about ordering. The function opened with `resolveGoogleAdsClient`, which immediately calls
`resolveExisting`, which resolves and DECRYPTS the project (or system) connection.

So the guard was first only along the paths where resolution SUCCEEDED. On an unstamped row
read while the connection was disconnected, undecodable, or simply down, the upstream failure
surfaced instead — a 404/500/different 409 — and the operator was pointed at a retry that can
never succeed, because the actual defect is a row that records no creating customer. The
happy path was correct; the adjacent failure arm returned something else entirely.

The two facts are not the same KIND of fact, which is what decides the ordering:

- **absence** is purely LOCAL — the row records no creating customer, and no answer Google
  could give would change it. It needs no client.
- **mismatch** is a COMPARISON against the account the connection currently resolves to. It
  cannot be answered without a client.

The guards are now split around the resolve accordingly, rather than sitting together after
it. HubSpot's email metrics read already carried this exact correction, with the same
reasoning written out above its portal lookup — the rule existed and had simply not been
applied here.

**The generalisation.** "Checked before X" in a comment is a claim about the call sequence,
and a fix that hoists a check must be verified against the arm where the intervening call
FAILS. A test whose fixture is healthy cannot discriminate: resolution succeeds, the guard is
reached wherever it sits, and the test passes identically before and after. The discriminating
fixture is the unhealthy one.

## The class was narrower than the finding, and the sweep is what established that

The obvious generalisation — "every provenance guard sitting after a resolve" — enumerates
four other sites: `ToggleStatus` and `ReadMetrics` in the same file, plus the Microsoft and
X/Twitter dispatchers. None is the same defect. Every one of them spells its guard
`created == "" || created == current` (or `created != "" && created != current`), which waves
an unstamped row THROUGH by design, so absence never produces an error whose precedence could
be inverted. `ReadSettings` is the only fail-closed absence guard in the codebase that sat
behind a resolve, and being stricter than its siblings is exactly what its own doc comment
says it is for.

Confirmed by mutation rather than by reading: making `ReadMetrics` fail closed on absence
broke two existing tests, so the siblings' deliberate wave-through is already pinned and was
never in scope. A sweep that finds the class is bounded at one site is still a sweep — the
result is the boundary, not the count.

## An enumerating comment falsified from below

The const block introducing the four upstream-only settings fields said all four "have NO
counterpart on the campaign row". `settingsFieldChannelType` is compared against a value
recovered from `ConfigSnapshot`, and the call site that builds `rb.Fields` says so explicitly
twelve lines away — three fields pass a literal `nil` recorded side, channel type passes
`recordedChannelType`. The comment was true when written and was falsified by the change that
started recording the channel, which updated the call site and not the declaration.

Rewritten to describe the SHAPE and name the authority — which fields have a recorded
counterpart is decided at the call site where each pair is written out — so a config that
later learns to express one of these falsifies a list nobody is relying on rather than a claim
a reader would trust.

The same failure one level up: `docs/api-catalog.md` line 69 still asserted **"No endpoint
here reads the platform's run state back"**, which this PR's own `/settings` endpoint
contradicts — it reports Google's `ENABLED`/`PAUSED`/`REMOVED` as an upstream-only
observation. The paragraph now carves out `/settings` and keeps the claim that matters
intact: the run state is never written BACK to the row, so a console change is still invisible
to everything except this readback.

## Mutation notes

- Restoring the pre-fix ordering (resolve hoisted back above the guard) was **killed** by the
  new test. Restoring it is the only mutation that reproduces the reported defect, and it must
  be applied as a MOVE of the resolve — editing the guard instead changes a different thing.
- Dropping the `errors.Join` so the guard returns `ErrCampaignProvenanceUnknown` alone was
  **killed** by two tests, including the pre-existing before-contact one. The join is what
  keeps existing `errors.Is(err, ErrCampaignAccountMismatch)` callers matching.
- No survivors this round. The `git diff` after each mutation was read before running the
  suite: the previous round on this repo produced a "survivor" that was an edit landing eighty
  lines off target, and confirming the edit is cheaper than re-deriving the finding.
