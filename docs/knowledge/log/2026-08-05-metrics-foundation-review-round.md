# 2026-08-05 — Metrics foundation: dealako review round

**Fix** — `GetCampaignMetrics`'s `ErrMetricsWindowUnsupported` branch
(`internal/service/brief.go`) concatenated the wrapped adapter error
(`merr.Error()`) directly into the client-facing 400 message, leaking
internal adapter detail into an API response. Now logs `merr` server-side via
`slog.WarnContext` (matching the existing `default:` branch's pattern a few
lines below) and returns a fixed client-facing message.

**Fix** — `TestOrchestrator_ReadCampaignMetrics_EnforcesCallTimeout`
(`internal/service/orchestrator_test.go`) computed its deadline-tolerance
window from `time.Now()` taken only after the call returned, so slow CI
scheduling between the call and the check could widen the window without
tightening the actual tolerance on the deadline's derivation. Now brackets
the call with `beforeCall`/`afterCall` timestamps and asserts the observed
deadline falls within `[beforeCall, afterCall] + metricsCallTimeout`.

**Fix** — Restored an explanatory comment on
`ctxAssertingCampaignRepo.UpsertCampaign`'s pass-through of the index-payload
builder, deleted without replacement in an earlier commit. The comment
documents a non-obvious invariant: swallowing the builder there would hide
whether a shutdown-window persist still co-commits its index message.

**Fix** — Added `TestBriefService_GetCampaignMetrics_DefaultWindowIsLast30Days`,
covering the previously-untested happy path where `window` is omitted:
asserts both the dispatcher's received window and the response's `Window`
field resolve to `last_30_days`. Every prior test that omitted `Window`
returned an error before a metrics result was produced.

**Fix** — Added `TestOrchestrator_ReadCampaignMetrics_NilNilReaderResultIsError`,
pinning `ReadCampaignMetrics`'s existing `(nil, nil)`-from-`MetricsReader`
contract-violation guard, which had no dedicated test.

**Question** — dealako asked whether `cost_micros`'s platform-dependent
currency (USD for LinkedIn/Reddit, X's billing unit for Twitter, etc.) should
be normalized via a `currency_code` field on `CampaignMetrics` now, rather
than deferred. Left as-is for this PR: the four adapter PRs (#72–#75) are
already rebased onto this foundation without a currency field, and adding one
here would require a follow-up in all four rather than a foundation-only
change. Replied on the thread recommending it be scoped as a follow-up once a
UI consumer actually needs cross-platform cost comparison, since no adapter
currently reports a non-USD-equivalent currency.
