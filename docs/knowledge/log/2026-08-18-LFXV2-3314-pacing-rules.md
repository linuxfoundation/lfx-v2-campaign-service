# 2026-08-18 — LFXV2-3314 pacing and action-item rules

**Creation** — `internal/service/rules` derives a pacing band and a list of operator action
items from a campaign's measured state. It replaces four UI implementations that disagreed on
the underspending floor three ways; Reddit disagreed with itself, labelling at 50 and alerting
at 40, so a campaign could render as healthy while raising an alert.

**Pacing is against plan-to-date, not the whole budget.** A campaign three days into a
thirty-day flight is expected to have spent a tenth. Comparing against the full budget reports
every healthy campaign as severely underspending for most of its life.

**`ComputePacing` takes `spendDays` as a required argument.** The only spend this service can
read is window-scoped — `CampaignMetrics.CostMicros` covers `last_7_days` and friends — while a
lifetime `BudgetAmount` describes the whole flight. Without the period stated explicitly, a
7-day spend divides into a 30-day plan and yields a confident "23% of plan" for a campaign that
is exactly on track, which then raises an underspending item against it. Both arguments are
bare `float64`, so this is the one error the function cannot detect for itself. Expected spend
is computed over `min(spendDays, elapsed)`: a window wider than the flight cannot manufacture
plan days that have not elapsed.

**Incomputable is kept distinct from zero.** `Computable=false` means nothing was measured, not
that the campaign spent nothing. Four inputs reach it that a naive implementation renders as a
confident `0%`:

- a **non-finite budget** — NaN and +Inf both pass a `<= 0` test, and +Inf is the dangerous one
  because it is silent: expected spend goes infinite, `Pct` lands on 0, and the campaign raises
  a HIGH-priority underspending item indistinguishable from a real one;
- a **future-dated flight** — the one-day elapsed floor would otherwise invent a day of expected
  spend and flag a campaign scheduled to start next week;
- an **underflowed expected-spend** — a denormal stays strictly positive, so an `expected <= 0`
  guard misses it and the division overflows to `+Inf`. The non-finite check on the *result* is
  what covers this, not a guard on the inputs;
- no budget, or a zero-length flight.

**Thresholds carry two boundaries plus an absolute overspend edge.** The shared constants also
define `normal: 90`, but it names the top of a band `labelFor` derives from `Constrained` — as a
struct field it would be a knob a caller could turn with no effect. `Overspending` is absolute
rather than a multiple of `Constrained`, because deriving it moves a boundary nobody asked to
change whenever a platform overrides the constrained cap.

**Band boundaries are half-open upward.** A value exactly on a threshold lands in the healthier
band. At 100% this is the whole point: a campaign spending precisely what its flight expects by
now is on plan, and labelling it `constrained` raises a budget item against the only campaign
that needs none.

**Zero delivery requires both signals** — no impressions AND no spend. Impressions alone is an
unbilled serve; spend alone is a billing entry with no serve. Flagging either trains operators
to ignore the rule. It fires only for statuses where the service believes the campaign reached
the platform, so a stalled dispatch is not reported as a targeting problem.
