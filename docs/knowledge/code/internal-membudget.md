---
type: "Go Package"
title: "internal/membudget"
description: "One per-pod memory budget that creative-asset uploads draw on: the upload middleware's wire charge and the decode reservation's pixel charge come out of the same weighted semaphore, sized from UPLOAD_MEMORY_BUDGET_BYTES or 40% of the cgroup memory limit, with a bounded wait and an in-use figure for the metrics gauge."
resource: "internal/membudget"
---

# internal/membudget

One per-pod memory budget that creative-asset uploads draw on: the upload middleware's wire charge and the decode reservation's pixel charge come out of the same weighted semaphore, sized from UPLOAD_MEMORY_BUDGET_BYTES or 40% of the cgroup memory limit, with a bounded wait and an in-use figure for the metrics gauge.

## Why one budget (#179)

Before #179 the creative-asset upload had two independent aggregate bounds, each a quarter of the
pod: `middleware.UploadAdmission` (a semaphore priced from `Content-Length`) and
`service.DecodeReserver` (a semaphore priced from the image header). They could not be added up to
reason about the pod, because the wire weight (then 128 MiB) also counted the 80 MiB pixel buffer
the decode reservation charged again. Now both charge ONE `*Budget`, built by the container
(`Container.UploadBudget`), and the wire weight (`constants.UploadAdmissionWeightBytes`, 112 MiB)
excludes the pixels, so the sum of what every admitted upload holds can never exceed `Total()`.

## Sizing

`Resolve` picks the size: `UPLOAD_MEMORY_BUDGET_BYTES` (bytes or `Ki`/`Mi`/`Gi`) when set,
else 40% of the container memory limit read from cgroup v2 `memory.max` or v1
`memory.limit_in_bytes` (an unlimited cgroup counts as no limit), else 40% of
`constants.PodMemoryLimitBytes`. A malformed env value is an error that stops startup. The result
is never below `constants.UploadWorstCaseBytes` (192 MiB, one maximum-size upload's wire charge
plus pixel reservation); raising it is logged as a warning.

## Behaviour

**Two stages, no hold-and-wait.** An upload takes its wire charge with `Acquire` (first stage)
and holds it for the whole handler, then asks for its pixel charge with `AcquireSecondStage`
while still holding it. On a single FIFO semaphore that was hold-and-wait: the pixel request
queued behind a new upload's wire request, which could not be admitted until the first upload
finished, so admitted uploads whose bodies were already read stalled and shed. Now the first
stage may fill the budget only up to `Total - reserve` (`FirstStageLimit`; the container sets the
reserve to `CreativeDecodePeakBytes`, 80 MiB), and second-stage requests queue ahead of, and are
never blocked by, first-stage waiters. A pixel request therefore waits only on other pixel holders,
which release when their decode returns. `TestBudget_SecondStageIsNotQueuedBehindAWaitingFirstStage`,
`TestBudget_FirstStageAlwaysLeavesRoomForOnePixelCharge` and
`TestBudget_ReleaseServesSecondStageFirst` pin this.

**Cancelled callers hold nothing.** A caller whose context is already done is refused by both
stages even when capacity is free, and a queued request that is granted in the same moment its
caller is cancelled gives the charge back instead of returning it (#445 review). Otherwise a
disconnected upload would still read its body and run `image.Decode`, which cannot be cancelled,
holding up to 80 MiB a live upload could use. Every queued outcome is settled under the budget's
lock, so a racing grant is seen exactly once. `TestBudget_CancelledContextIsRefusedWithCapacityFree`
and `TestBudget_GrantRacingCancellationIsGivenBack` pin this; the hold-and-wait tests wait for the
queue to hold the expected waiters (`awaitWaiters`) rather than sleeping, so they cannot pass
before the request they are ordering has queued.

Both stages wait at most the given duration (250 ms each, so at most ~500 ms per upload), refuse
a want above their limit at once, and return an idempotent release. On exhaustion both call sites
answer `503` with `Retry-After: 1` (the middleware adds the header to the handler's own `503`).
`InUse()` feeds `campaign_upload_memory_budget_in_use_bytes`. The bound is on live charges: a
pixel buffer is released when `image.Decode` returns but freed by the next GC, a lag the 60% of
the limit outside the budget absorbs.

Not covered: the dispatch-side `dispatch.AssetReserver`, which bounds asset bytes held by
background dispatches, remains its own budget.
