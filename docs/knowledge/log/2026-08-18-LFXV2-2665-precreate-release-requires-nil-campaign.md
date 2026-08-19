# 2026-08-18 — LFXV2-2665: releasing a dispatch claim now requires a nil campaign

**Fix** — `Orchestrator.dispatchPlatform` released the single-flight dispatch claim on
`dispatchErrIsPreCreate(derr)` alone, ignoring the campaign returned beside the error. The
release path deletes the `pending` row and frees the `(brief, platform)` slot, so a dispatcher
returning a non-nil partial alongside a `NoUpstreamCreate` error would have reopened the slot
for a duplicate PAID create. The guard is now
`dispatchErrIsPreCreate(derr) && campaign == nil`.

`docs/knowledge/code/internal-dispatch.md` already specified this contract — "the decision keys
on `result == nil` ALONE — NOT on whether the campaign id is populated" — as a rule for
adapters. What was missing was the orchestrator-side enforcement: the doc described the
intended behaviour of a check the code did not make. The concept file now records where both
preconditions are enforced and why they are independent.

`NoUpstreamCreate` is the ADAPTER's assertion about its own error; a non-nil campaign is
evidence from the same return that something was built upstream. When the two disagree the
evidence wins, because the failure modes are asymmetric: retaining a claim that could have been
released costs a stuck row an operator can clear, while releasing one that should have been
retained spends real money on a second paid campaign.

The condition deliberately tests `campaign == nil` and NOT
`campaign.PlatformCampaignID != ""`. An ambiguous first-create and LinkedIn's group-orphan both
return a non-nil partial whose id is empty BY DESIGN while a real upstream resource exists;
keying on the id would route exactly those back into the release path.

**Reachability:** latent, not live. Every current adapter returns `preCreateError` with a nil
campaign (`internal/dispatch/creds.go`), so no dispatcher produces the contradictory pairing
today. The guard protects a future adapter, and its cost is one comparison.

## Scope note — the reap half stays blocked

This does NOT unblock automatic reaping of stuck claims, and nothing here reclaims, steals or
deletes a claim. Reaping still requires distinguishing a claim that never reached the provider
from one that did, which no predicate on the current schema can do — see PR #125, withdrawn for
asserting exactly that. Unblocking it needs a durable transition committed BEFORE `Dispatch`
(e.g. a `provider_call_started_at` column), which is a schema plus money-path design decision,
not a fix to land quietly. No migration is added here.

## Testing

`TestOrchestrator_PreCreateErrorWithPartialRetainsClaim` supplies the fixture the guard must
REFUSE — a dispatcher returning a non-nil partial with a `NoUpstreamCreate` error — in both
shapes, id-carrying and id-less. This is the lesson PR #125 paid for: its live-Postgres check
passed while proving nothing, because every fixture matched the predicate's own premise, so the
test could only confirm the belief under test. A guard whose safety argument is a predicate is
pinned only by a case the predicate must exclude.

`TestOrchestrator_ConcurrentPreCreatePartialsKeepOneClaim` covers the contended path. It does
NOT use an n-party rendezvous, and cannot: the single-flight claim means exactly ONE caller ever
reaches `Dispatch` for a `(brief, platform)` pair, so a barrier waiting for N arrivals would
wait for arrivals that never come. Instead the winner is held INSIDE the provider call by a
gate while the N-1 losers run their claim attempts, so each loser resolves against a live
`pending` row — the ordering the skip path is specified against.

Settling that state needs a poll, not a snapshot: `Start` returns before the asynchronous
dispatch has recorded its outcome, so the test waits until exactly one job remains unfinalized
(the gated winner) before counting. The loser count is asserted against a LITERAL 3 rather than
`parties-1`, so shrinking `parties` fails the test instead of quietly degenerating it into the
serial case — an earlier draft asserted only the surviving claim and still passed at
`parties = 1`, which is to say it was not testing concurrency at all.

Both mutations were verified with COMPILING changes: reverting to `if
dispatchErrIsPreCreate(derr) {` fails both tests, and narrowing to
`campaign == nil || campaign.PlatformCampaignID == ""` still fails the id-less subtest, which
is why that subtest is separate from the id-carrying one.

Suite green under `-race`. The Postgres-backed tests SKIPPED — no live database was used for
this change, and none is needed: the fix and its tests are entirely orchestrator-level.
