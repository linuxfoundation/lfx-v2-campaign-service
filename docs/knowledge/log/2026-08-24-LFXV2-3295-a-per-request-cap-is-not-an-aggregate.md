# 2026-08-24 — a per-request cap is not an aggregate, and the comment said otherwise

**Fix** — `dispatch.MaxConcurrentVariantAssetBytes` bounds the creative-asset bytes that ALL
concurrent dispatches may hold, closing a gap `maxVariantAssetBytes` never covered.

`maxVariantAssetBytes` (240 MiB) caps ONE dispatch. Nothing capped how many ran at once, and the
orchestrator's semaphore is a single process-wide channel with **no per-provider partition**, so
all five slots can be Meta:

```
before: 5 x (240 MiB retained + 30 MiB materialised) = 1.32 GiB = 2.6x a 512 MiB pod
after:      240 MiB retained + 30 MiB materialised   =  270 MiB = 53% of the pod
```

It never needed the worst case: **two** asset-heavy dispatches already exceed the pod before
multipart copies and ordinary process memory.

## The comment that hid it

`orchestrator.go` said *"bounds concurrent per-platform campaign creation"*. Read that way,
`maxParallelDispatch = 5` means one Meta dispatch at a time and the per-dispatch cap IS the
aggregate. It is wrong in the direction that matters — it understates same-provider concurrency
by 5x, and a memory bound was sized against the misreading. The comment now states the semaphore
is process-wide with no per-provider split, and names the aggregate that covers it.

**A wrong comment on a shared limit is a bug in every bound derived from it.** The per-dispatch
cap was not carelessly written; it was written against a false statement of how many callers
there could be.

## Why the budget equals the per-dispatch cap

The tempting fix — size the budget so five dispatches fit — is the trap. Any budget below
`maxVariantAssetBytes` means a single config carrying eight maximum-size assets, legal today and
accepted by the per-dispatch ceiling, can **never acquire** and is refused. A bound that meets
its arithmetic by rejecting work the contract accepts is not a fix; it is the same error shape as
pricing a permit so cheaply nothing legal fits, which this PR already made once with the upload
admission floor.

Equal to the per-dispatch cap is the **smallest value that refuses no legal config**: exactly one
maximum-size dispatch fits, and every smaller one shares the remainder — the same "priced for the
worst legal input, shared by everything smaller" shape as the upload and decode budgets. What it
removes is the multiplier, which is where the 2.6x came from.

Rejected alternatives, and why:

- **Stream/release per variant** — the only option that fixes the peak rather than rationing it,
  and materially larger than it sounds: `resolveVariantAssets` hands the whole resolved slice
  back and the Meta client POSTs from it later, so streaming means restructuring the
  dispatcher/client contract and giving up dedupe-across-variants.
- **Lower `maxVariantAssetBytes`** — rejects legal campaigns. The trap above.
- **Lower `maxParallelDispatch`** — throttles every provider to fix a Meta-only problem.

## Lifetime, and the mutation that survived

The reservation is released at the END of the dispatch, not at the end of the resolve, because
the bytes live that long — `resolveVariantAssets` returns them and the client POSTs them to
`/adimages` later in the same call. Releasing early hands back budget for memory that is still
resident.

Three behavioural tests pin the bound: one dispatch of exactly the budget is admitted (or the
bound rejects legal work), a second is refused while the first is held, release returns capacity,
and eight realistic 512 KiB dispatches still run concurrently (a budget that serialized
everything would pass the first test and ruin the common case).

**One mutation survived them all**, and it is recorded rather than hidden: changing
`defer releaseAssets()` to an immediate `releaseAssets()` left every behavioural test green.
Pinning it end-to-end needs a decryptable connection, a credentials source and an HTTP round trip
to Meta, so a failure would not localise to the lifetime. It is guarded instead by a SOURCE
assertion on the call site — **weaker evidence than the behavioural tests, and labelled as such
in the test itself rather than folded into a pass count.**
