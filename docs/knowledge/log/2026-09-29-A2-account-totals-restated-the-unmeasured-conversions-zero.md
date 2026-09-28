# 2026-09-29 — A2: the account totals restated the unmeasured-conversions zero

**Fix** — response-shape and behaviour change, all four platforms, Reddit-visible.
Found by the pre-PR local review over the whole A2 range; the aggregate half of
`linuxfoundation/lfx-self-serve#3020`.

`#3020` stopped Reddit's rule input from claiming a measured zero: since that fix every Reddit
row carries `Conversions == nil`, and the per-campaign `conversions` attribute is optional
precisely so absent and zero stay different claims on the wire.

`monitorTotals` skipped the nil rows — correctly — but summed into a plain `float64` on a
**required** response attribute. So a Reddit response omitted `conversions` on every campaign
and still emitted `"totals": {"conversions": 0}` beside them. The same false measurement the
row-level fix removed, rebuilt one level up, in the one place a reader is most likely to take
at face value.

`model.AccountMonitorTotals.Conversions` is now `*float64`, set only when at least one summed
row reported a measurement, and `conversions` comes off the totals type's `Required` list in
`design/connection.go` (with `make apigen`). A measured `0` still reports `0` — the
distinction is "did anything measure", not "is the sum non-zero", which is why
`TestMonitorAccount_TotalsConversionsAbsentWhenNoRowMeasuredThem` includes a row carrying an
explicit `0.0` and asserts the total is present.

## The pattern worth naming

Both halves of this were written in the same range and neither was wrong in isolation. Making
a field optional at the row level and leaving the aggregate over those rows required is a
shape the type system does not catch: the sum compiles, the zero is well-typed, and nothing
fails. When a per-item field becomes "absent means unmeasured", every aggregate over it
inherits that contract in the same change.

## Also in this fix round

Documentation the same A2 range made stale, all raised by the same review:

- `docs/api-catalog.md`'s account-monitor row still said the four rule engines are
  **deliberately not unified**, that their quirks are preserved verbatim, and that Reddit's
  separate account-totals call is the one exception with a row-sum fallback. Every clause of
  that was undone by this branch. Rewritten to describe `monitor_shared.go`, the fixed quirks,
  row-summed totals on every platform, the removal of `derived_from_rows`, and the two
  threshold groups that stay per-platform with their reason.
- `design/connection.go`'s `pacing_unknown` description named only missing flight dates. Since
  the budget-less fix it is also set when there is no usable budget to pace against. Corrected
  in the design attribute and in `model.AccountCampaignMetrics`, and regenerated.
- The exported comments on `MonitorPriority`, `MonitorPacingLabel` and
  `AccountMonitorActionItem`, and `EvaluateLinkedInMonitor`'s, still told a reader the ported
  bugs were deliberately preserved and that each platform computes pacing with its own
  literals. They now describe the shared ladder and rank, and what genuinely remains
  per-platform.
