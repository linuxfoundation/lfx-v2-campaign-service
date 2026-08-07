# 2026-08-07 — LFXV2-2023: the restored `success` field was never assigned, so every batch reported false

**Update** — The plan restored the deprecated batch-level `success` boolean for the UI already
consuming it, defined it as `succeeded == total`, and made it `Required` on
`CampaignKeywordActionResult`. The handler sample that builds that result never assigned it.

**Fix** — Omitting a field from a Go struct literal does not fail to compile. It takes the zero
value, so the response would have carried `success: false` for every batch — including one where
every action applied. That is strictly worse than the redundancy the field was accepted as: a UI
still reading it would show a failed batch that fully succeeded, and the earlier log fragment
claiming the field was restored would have been describing something that never worked. A
deprecated field is only harmless while it is correct.

**Fix** — It is assigned as `succeeded == total`, verbatim from the design, with `total` hoisted so
the comparison and the emitted count cannot drift. An empty batch makes that trivially true; the
case is unreachable because validation rejects an empty actions list, and adding a `total > 0` term
to guard it would make the field disagree with its own documented definition — which is how a
deprecated field starts needing tests of its own.
