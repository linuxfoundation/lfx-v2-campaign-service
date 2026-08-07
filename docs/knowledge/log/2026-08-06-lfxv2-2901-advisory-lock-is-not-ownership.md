# 2026-08-06 — The claim's advisory lock is contention, not ownership (LFXV2-2901)

**Update** — `ClaimCampaignVersion`'s doc comment now states its durability boundary
explicitly, and `TestClaimVersionIsBackedByACompareAndSwap` pins the mechanism that
actually carries correctness across the external call.

Copilot read the claim as durable ownership and found the hole in that reading. It is
right about the mechanism: `pg_advisory_lock` is SESSION-scoped, so a failover, a pool
eviction, or a severed TCP connection releases it server-side while the holder is still
inside its platform call. A successor can then claim the same version — the claim
deliberately leaves `version` unchanged — and issue a second paid call.

Where the reading goes wrong is the consequence. The finding predicted the first writer
"cannot persist on its dead connection", but `ReplaceCampaign` takes its own pooled
connection; the claim's connection is held only for the lock. So both writers reach the
write, and what separates them is the compare-and-swap in `replaceCampaignQuery`:
`WHERE ... version=$12` together with `version=version+1`. Whichever commits first bumps
the version; the other matches zero rows and gets `ErrPreconditionFailed`. Two persisted
writes at one version, two outbox rows, and a stale overwrite are all impossible — and
they stay impossible only while BOTH halves of that statement are present, which is what
the new test holds.

That leaves exactly one duplicated platform call as the residual exposure. Every mutation
guarded by this claim today is DECLARATIVE — set run status to active or paused — not
incremental, so a repeat converges on the same upstream state rather than compounding.

Making ownership itself durable means a lease row with expiry and reconciliation, the same
shape the outbox drain already uses (`TestClaimStampsALeaseThatOutlastsAPass`). That is a
design change with its own failure modes — a lease that expires mid-call reintroduces the
same race one layer up — and does not belong grafted onto a soft-delete fix. Recorded here
so the boundary is a decision on the record rather than something the next reader has to
rediscover from the finding.
