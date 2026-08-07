# 2026-08-06 — The claim's advisory lock is contention, not ownership (LFXV2-2901)

**Update** — `ClaimCampaignVersion`'s doc comment now states its durability boundary
explicitly, and `TestClaimVersionIsBackedByACompareAndSwap` pins the mechanism that
actually carries correctness across the external call.

Copilot read the claim as durable ownership and found the hole in that reading. It is
right about the mechanism: `pg_advisory_lock` is SESSION-scoped, so a failover, a pool
eviction, or a severed TCP connection releases it server-side while the holder is still
inside its platform call. A successor can then claim the same version — the claim
deliberately leaves `version` unchanged — and issue a second paid call.

Two mechanisms carry correctness past that point, and it is worth being exact about which
one covers which failure, because an earlier draft of this entry got it backwards.

If the loss of the lock is the loss of the CONNECTION — the failover or severed-socket case
— the first writer does not race anyone. `ReplaceCampaign` begins its transaction on the
claimant's own connection, taken from `lockToken` rather than from the pool
(`campaign_repo.go:400-405`), precisely so a write can never attach itself to a successor's
session. That connection is the dead one, so the first writer's persist fails outright and
only the successor reaches the write. Nothing diverges in the row; what is lost is the
first writer's record of a platform call that may already have landed.

The compare-and-swap is what covers every OTHER way two writers can arrive at the write at
the same version — a lock released without its connection dying, a future caller that
forgets to claim, a bug in the map bookkeeping. `replaceCampaignQuery` pairs
`WHERE ... version=$12` with `version=version+1`, so whichever commits first bumps the
version and the other matches zero rows and gets `ErrPreconditionFailed`. Two persisted
writes at one version, two outbox rows, and a stale overwrite stay impossible only while
BOTH halves of that statement are present, which is what the new test holds.

The residual exposure is therefore one duplicated or unrecorded platform call, never a
diverged row. Every mutation guarded by this claim today is DECLARATIVE — set run status to
active or paused — not incremental, so a repeat converges on the same upstream state rather
than compounding.

The general lesson, and the reason this correction is worth keeping: a durability argument
has to name the connection each statement runs on. "The claim's lock died" and "the
claimant's write died" are the same event here only because `ReplaceCampaign` was
deliberately bound to the claim's connection — change that binding and both halves of this
entry change with it.

Making ownership itself durable means a lease row with expiry and reconciliation, the same
shape the outbox drain already uses (`TestClaimStampsALeaseThatOutlastsAPass`). That is a
design change with its own failure modes — a lease that expires mid-call reintroduces the
same race one layer up — and does not belong grafted onto a soft-delete fix. Recorded here
so the boundary is a decision on the record rather than something the next reader has to
rediscover from the finding.
