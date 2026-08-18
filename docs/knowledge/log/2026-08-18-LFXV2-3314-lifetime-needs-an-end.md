# 2026-08-18 — LFXV2-3314 a lifetime budget with no end date was prorated against now

**Fix** — the fourth member of the same family, and the guard that was meant to catch it only
covered its mirror image.

An earlier fix added `flight.Start == nil && flight.End != nil` → `Unknown`. The opposite
combination — a start present, **no end** — fell through: `end` defaults to `now`, so `total`
collapses into `elapsed` and the whole lifetime budget is treated as due today. A campaign ten
days into an open-ended $1000 budget having spent $100 read **10% underspending**, a
HIGH-priority item telling an operator to raise spend on a campaign that may be pacing perfectly
for a flight nobody has given an end.

**The guard is deliberately NOT symmetric.** It applies to `BudgetLifetime` only. A lifetime
budget is a total to spread across a flight, so prorating it needs the flight's LENGTH; a daily
budget's rate is explicit, so days elapsed is all the arithmetic needs and a missing end date
costs it nothing. Mutation-verified in both directions: removing the guard fails 4 tests,
widening it to daily fails 1. The second is what proves the scope, not merely the presence.

`end_date` is nullable in migration 000002, so this is storable rather than theoretical.

**Two doc corrections in the same pass.**

`isActive`'s docstring enumerated the provisioning vocabulary and said "only two of those",
which the run-state fix silently falsified when it added `active` to the switch. It now names
both axes `Campaign.Status` carries, and states why `paused` is excluded: zero spend is the
INTENDED outcome of pausing, so a finding against it argues with the person who chose it.

`docs/api-catalog.md`'s legacy per-provider block listed a `pacingLabel` of `severe` and omitted
`unknown`. Neither matched what the service emits, and this ticket is what made the two versions
contradict — pinning the vocabulary in the Goa design while the older block still advertised the
other one. `unknown` is the load-bearing member: it is what stops "could not derive" being
rendered as a confident `0%`.

**Raised by dealako.** He reproduced the mechanism, agreed with Copilot's independent finding on
the same line, and correctly scoped everything else on the review as non-blocking.
