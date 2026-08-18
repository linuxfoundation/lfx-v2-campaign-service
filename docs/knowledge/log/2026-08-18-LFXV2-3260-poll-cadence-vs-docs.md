# 2026-08-18 — LFXV2-3260: Microsoft's documented cadence invalidates the sync path

**Note** — A code comment claimed Microsoft "documents a recommended floor of ~1s" for
polling `PollGenerateReport`. No such statement exists in Microsoft's documentation. The
primary source (Request and Download a Report, step 6) says the opposite:

> "Because most reports should complete within minutes, polling at two to 15-minute
> intervals should be appropriate for most cases. If the overall polling period exceeds
> 60 minutes, consider saving the report identifier, exiting the loop, and trying again
> later."

The consequence is larger than a wrong constant. The arithmetic does not close:

| Bound | Value | Set by |
|---|---|---|
| Report completion | **minutes** | Microsoft |
| `reportPollBudget` | 15s | this client |
| `metricsCallTimeout` | 20s | the orchestrator |
| Platform ingress | 60s | Traefik |

The budget cannot be raised to reach "minutes" without exceeding the caller timeout above
it, and that timeout cannot be raised past the ingress above that. **A synchronous
read-through can therefore essentially never observe a finished Microsoft report.**

Microsoft's own prescription for this case — persist the `ReportRequestId`, exit the loop,
return later — is exactly the resumable-report capability `ErrReportNotReady` already
records as absent, and as requiring a persisted job row plus async completion.

The interval was deliberately NOT changed. Moving 1s → 5s reduces the number of doomed
polls inside a budget that expires regardless, while implying the synchronous path works.
It does not, on Microsoft's own published numbers. The comment now states this, so the next
reader does not spend the time re-deriving it.

This is why the entire read stays behind `MICROSOFT_METRICS_ENABLED` (default off). Shipping
it enabled would mean a metrics endpoint that returns `not_ready` for nearly every Bing
campaign — a failure legible as a failure, but a failure nonetheless. The honest fix is the
async design, which is a schema and orchestration decision rather than a tuning one.
