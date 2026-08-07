# 2026-08-06 — Reddit metrics: default-off gate and the missing counter coverage

**Update** — Copilot argued that merely defining `ReadMetrics` on `RedditDispatcher` IS the
capability switch: `Orchestrator.ReadCampaignMetrics` discovers `MetricsReader` by type
assertion, and the published endpoint then invokes it. Wiring it therefore promotes a guessed
request shape, response shape, and spend currency unit to production metrics that return 200
and look authoritative. The caveats live in code comments and docs; the HTTP response carries
none of them. That is right, and the two remedies offered were "leave it unwired" or "gate it
behind a disabled-by-default flag".

Took the flag. `ReadMetrics` now returns `domain.ErrMetricsUnsupported` — mapping to the same
400 a platform with no metrics support at all produces, which is the accurate answer while the
contract is unverified — unless `REDDIT_METRICS_ENABLED` is exactly `"true"`. Any other value,
including unset, fails closed, so a typo'd `"1"` or `"yes"` does not silently open the path.
`TestReddit_ReadMetrics_DisabledByDefault` pins all of those cases.

The gate is read at call time rather than at construction, so a deployment flips it without a
rebuild and the disabled path costs one env read. `REDDIT_METRICS_ENABLED` is declared in
`pkg/constants` and wired in `charts/lfx-v2-campaign-service/values.yaml` with `value: "false"` —
a flag the chart never injects would be unusable. Once the shape is verified against a live
Reddit ad account the gate is deleted and the values key goes with it.

**Update** — dealako flagged two coverage gaps in `internal/platform/reddit/metrics_test.go`,
both correct and both inconsistencies rather than omissions: the spend-validation path already
had negative/NaN/oversized coverage while the counter path had none.

`TestGetCampaignMetrics_CounterGuardsAreDecodeErrors` covers all four counter branches — negative
impressions, negative clicks, and the two checked additions. The overflow cases need TWO rows,
not one: the running total starts at zero, so `metrics.Impressions > MaxInt64-row.Impressions` is
unreachable on the first row regardless of how large it is.

`TestGetCampaignMetrics_MultipleRowsAccumulate` covers the decode loop with three rows. Every
prior test used a single-element `data` array, so nothing pinned the accumulation itself — and a
row-per-day response is a plausible real shape this scaffold may meet first in production. It
asserts CTR is recomputed from the TOTALS (80/2000 = 0.04) rather than averaged per row, which
would give 0.0333.

Both verified binding: neutering the four guards fails the first test with all four diagnostics,
and changing `metrics.Impressions += row.Impressions` to `=` fails the second with
`expected 2000 impressions summed across rows, got 400`.
