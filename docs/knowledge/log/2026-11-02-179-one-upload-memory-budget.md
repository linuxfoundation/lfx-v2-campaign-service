# 2026-11-02 — 179: creative uploads draw on one per-pod memory budget

**Update** — the upload admission middleware (wire bytes) and the decode reservation (pixel
bytes) used to have independent quarter-of-the-pod semaphores, and the wire weight (128 MiB) also
counted the pixel buffer the decode reservation charged again. They now both charge one
`internal/membudget` budget built by the container. The wire weight is 112 MiB (body buffer +
base64 string + decoded slice; amplification 3) and excludes the pixels, so the two charges never
overlap.

The budget is `UPLOAD_MEMORY_BUDGET_BYTES` when set, else 40% of the cgroup memory limit, else
40% of 512 MiB (~205 MiB), and never below one worst-case upload (192 MiB). A malformed value
fails startup. Exhaustion waits up to 250 ms, then answers 503 with `Retry-After`; the handler's
own 503 from the decode reservation now carries `Retry-After` too. New gauges:
`campaign_upload_memory_budget_in_use_bytes` and `campaign_upload_memory_budget_bytes`. The env
var is documented in `values.yaml` and the README.

`TestUploadMemoryBudgetComposes` asserts the composed property and its effective concurrency
(1 worst-case upload, 1 undeclared upload, ≥16 small ones on the fallback budget).
`TestDecodeReserver_SharesTheUploadBudget`, `TestUploadAdmissionShared_ChargesTheSharedBudget` and
`TestBuildHandler_ChargesTheContainersUploadBudget` pin that both admission points charge the same
instance.
