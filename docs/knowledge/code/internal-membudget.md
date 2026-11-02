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

`Acquire` waits at most the given duration (250 ms at both call sites), refuses a want above the
whole budget at once, and returns an idempotent release. The semaphore is FIFO, so an upload that
already holds its wire charge and waits for its pixel charge is not overtaken by new arrivals. On
exhaustion both call sites answer `503` with `Retry-After: 1` (the middleware adds the header to
the handler's own `503`). `InUse()` feeds `campaign_upload_memory_budget_in_use_bytes`.

Not covered: the dispatch-side `dispatch.AssetReserver`, which bounds asset bytes held by
background dispatches, remains its own budget.
